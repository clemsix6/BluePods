package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"os"
	"testing"
	"time"

	"BluePods/internal/consensus"
	"BluePods/internal/events"
	"BluePods/internal/network"
	"BluePods/internal/state"
	"BluePods/internal/storage"
	"BluePods/internal/sync"
)

// syncTestProvider is a minimal sync.SnapshotProvider backed by a real storage
// snapshot, mirroring internal/sync's own manager_test.go provider: enough for
// CreateSnapshot to collect a real (empty) consistent cut.
type syncTestProvider struct {
	round uint64
	db    *storage.Storage
}

// Round returns the mock provider's fixed round.
func (p *syncTestProvider) Round() uint64 { return p.round }

// ExportConsistentCut returns a cut whose cursor/round are the mock round and
// whose storage snapshot is a real, empty consistent view.
func (p *syncTestProvider) ExportConsistentCut(historyRounds uint64) consensus.ConsistentCut {
	return consensus.ConsistentCut{
		Cursor:     p.round,
		Round:      p.round,
		DBSnapshot: p.db.Snapshot(),
	}
}

// TestRequestAndApplySnapshot_EmitsSnapshotApplied exercises requestAndApplySnapshot
// end to end over a real QUIC connection: a server node serves its snapshot
// manager's latest snapshot, the client node requests and applies it, and the
// test asserts sync.snapshot.applied fires carrying the applied round, a
// well-formed checksum, and the object count.
func TestRequestAndApplySnapshot_EmitsSnapshotApplied(t *testing.T) {
	serverDir, err := os.MkdirTemp("", "sync_test_server_*")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	defer os.RemoveAll(serverDir)

	serverDB, err := storage.New(serverDir)
	if err != nil {
		t.Fatalf("storage: %v", err)
	}
	defer serverDB.Close()

	provider := &syncTestProvider{round: 5, db: serverDB}
	snapManager := sync.NewSnapshotManager(serverDB, provider)
	snapManager.Start()
	defer snapManager.Stop()

	// Wait for the manager's initial snapshot (2s startup delay, see manager.go).
	time.Sleep(3 * time.Second)
	if data, _ := snapManager.Latest(); data == nil {
		t.Fatal("snapshot manager did not produce an initial snapshot")
	}

	_, serverKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate server key: %v", err)
	}

	server, err := network.NewNode(network.Config{PrivateKey: serverKey, ListenAddr: "127.0.0.1:0"})
	if err != nil {
		t.Fatalf("create server: %v", err)
	}
	defer server.Close()

	server.OnRequest(func(p *network.Peer, data []byte) ([]byte, error) {
		if sync.IsSnapshotRequest(data) {
			return sync.HandleSnapshotRequest(data, snapManager)
		}
		return nil, fmt.Errorf("unexpected request")
	})

	if err := server.Start(); err != nil {
		t.Fatalf("start server: %v", err)
	}

	_, clientKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate client key: %v", err)
	}

	client, err := network.NewNode(network.Config{PrivateKey: clientKey, ListenAddr: "127.0.0.1:0"})
	if err != nil {
		t.Fatalf("create client: %v", err)
	}
	defer client.Close()

	if err := client.Start(); err != nil {
		t.Fatalf("start client: %v", err)
	}

	peer, err := client.Connect(server.Addr())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}

	clientDir, err := os.MkdirTemp("", "sync_test_client_*")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	defer os.RemoveAll(clientDir)

	clientDB, err := storage.New(clientDir)
	if err != nil {
		t.Fatalf("storage: %v", err)
	}
	defer clientDB.Close()

	n := &Node{storage: clientDB, state: state.New(clientDB, nil)}

	buf := captureEvents(t)

	result, err := n.requestAndApplySnapshot(peer)
	if err != nil {
		t.Fatalf("requestAndApplySnapshot: %v", err)
	}

	recs := eventsNamed(t, buf, events.EvSnapshotApplied)
	if len(recs) != 1 {
		t.Fatalf("want 1 %s event, got %d", events.EvSnapshotApplied, len(recs))
	}

	if recs[0]["round"] != float64(result.lastCommittedRound) {
		t.Errorf("round = %v, want %d", recs[0]["round"], result.lastCommittedRound)
	}

	checksum, ok := recs[0]["checksum"].(string)
	if !ok || len(checksum) != 64 {
		t.Errorf("checksum = %v, want 64-char hex string", recs[0]["checksum"])
	}

	if recs[0]["objects"] != float64(0) {
		t.Errorf("objects = %v, want 0 (empty snapshot)", recs[0]["objects"])
	}
}

// TestBuildValidatorSetFromSnapshot_CarriesRewardCoin guards against a
// fingerprint-forking regression: buildValidatorSetFromSnapshot calling only
// vs.AddWithStake per validator, never vs.SetRewardCoin, so RewardCoin is silently
// dropped rebuilding the live validator set from a synced snapshot, even
// though the snapshot wire format carries it correctly. A non-founder's own
// RewardCoin gets repaired later by its own register_validator replay, but
// the founder never re-registers — so on every synced (non-bootstrap) node
// the founder's RewardCoin stays zero forever, and any epoch-boundary credit
// that targets it lands on the bootstrap node but is silently skipped
// everywhere else, forking the fingerprint.
func TestBuildValidatorSetFromSnapshot_CarriesRewardCoin(t *testing.T) {
	n := &Node{}

	var founder, rewardCoin consensus.Hash
	founder[0] = 0xAA
	rewardCoin[0] = 0xBB

	synced := []*consensus.ValidatorInfo{
		{Pubkey: founder, QUICAddr: "quic://founder:9000", SelfStake: 1000, RewardCoin: rewardCoin},
	}

	vs := n.buildValidatorSetFromSnapshot(synced)

	got := vs.Get(founder)
	if got == nil {
		t.Fatal("founder missing from rebuilt validator set")
	}
	if got.RewardCoin != rewardCoin {
		t.Errorf("RewardCoin dropped rebuilding validator set from snapshot: got %x, want %x", got.RewardCoin, rewardCoin)
	}
}
