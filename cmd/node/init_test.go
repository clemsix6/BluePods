package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/binary"
	"testing"

	flatbuffers "github.com/google/flatbuffers/go"

	"BluePods/internal/aggregation"
	"BluePods/internal/consensus"
	"BluePods/internal/genesis"
	"BluePods/internal/state"
	"BluePods/internal/storage"
	"BluePods/internal/types"
)

// bootstrapTestNode builds a bootstrap-mode Node's storage/state/dag wiring the
// way initConsensus does, without the network layer (seedGenesisState never
// touches it), so seedGenesisState can be exercised directly. db is opened at
// dir; the caller closes it (directly, or by building a fresh session over the
// same dir to simulate a restart).
func bootstrapTestNode(t *testing.T, dir string, privKey ed25519.PrivateKey) (*Node, *storage.Storage) {
	t.Helper()

	db, err := storage.New(dir)
	if err != nil {
		t.Fatalf("storage: %v", err)
	}

	blsKey, err := aggregation.DeriveFromED25519(privKey)
	if err != nil {
		t.Fatalf("derive BLS key: %v", err)
	}

	st := state.New(db, nil)

	var pubHash consensus.Hash
	copy(pubHash[:], privKey.Public().(ed25519.PublicKey))
	validators := consensus.NewValidatorSet([]consensus.Hash{pubHash})

	systemPod := consensus.Hash{0x01, 0x02, 0x03}
	dag := consensus.New(db, validators, nil, systemPod, 0, privKey, nil, consensus.WithBootstrap())

	params := consensus.DefaultFeeParams()
	dag.SetFeeSystem(st, &params, nil)

	n := &Node{
		cfg: &Config{
			Bootstrap:   true,
			PrivateKey:  privKey,
			InitialMint: 1_000_000,
		},
		storage:   db,
		state:     st,
		dag:       dag,
		blsKey:    blsKey,
		systemPod: systemPod,
	}

	return n, db
}

// coinBalanceFor reads a coin object's little-endian balance directly, mirroring
// the consensus package's private readCoinBalance (not exported outside it).
func coinBalanceFor(t *testing.T, st *state.State, id [32]byte) uint64 {
	t.Helper()

	data := st.GetObject(id)
	if data == nil {
		t.Fatalf("coin %x not found", id[:4])
	}

	obj := types.GetRootAsObject(data, 0)
	content := obj.ContentBytes()
	if len(content) < 8 {
		t.Fatalf("coin content too short: %d bytes", len(content))
	}

	return binary.LittleEndian.Uint64(content[:8])
}

// buildCoinWithBalance serializes a singleton Coin object, mirroring the
// genesis package's private buildGenesisCoin, so the test can simulate spend
// activity between the two seedGenesisState calls without depending on
// unexported helpers of another package.
func buildCoinWithBalance(id, owner [32]byte, balance uint64) []byte {
	content := make([]byte, 8)
	binary.LittleEndian.PutUint64(content, balance)

	b := flatbuffers.NewBuilder(256)
	idVec := b.CreateByteVector(id[:])
	ownerVec := b.CreateByteVector(owner[:])
	contentVec := b.CreateByteVector(content)

	types.ObjectStart(b)
	types.ObjectAddId(b, idVec)
	types.ObjectAddVersion(b, 1)
	types.ObjectAddOwner(b, ownerVec)
	types.ObjectAddReplication(b, 0)
	types.ObjectAddContent(b, contentVec)
	types.ObjectAddFees(b, 0)
	b.Finish(types.ObjectEnd(b))

	return b.FinishedBytes()
}

// TestSeedGenesisState_RestartDoesNotReseedLedger simulates a bootstrap node
// restarting over its own data directory: seedGenesisState seeds the ledger on
// first boot, activity changes the reserve coin balance and the supply
// counters, the node "restarts" (storage is closed and reopened at the same
// path, with a fresh state and dag built over it), and seedGenesisState runs
// again. The reserve coin balance and the supply counters must be left exactly
// as activity left them — a re-seed would silently roll them back to their
// genesis values. The founder's validator entry must still come back with its
// genesis self-stake: nothing else restores it on a bootstrap-mode restart.
func TestSeedGenesisState_RestartDoesNotReseedLedger(t *testing.T) {
	dir := t.TempDir()
	_, privKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	// First boot: seeds the ledger and the validator.
	n1, db1 := bootstrapTestNode(t, dir, privKey)
	n1.seedGenesisState()

	owner := deriveOwner(privKey)
	coinID := genesis.GenesisCoinID(owner)

	genesisBalance := coinBalanceFor(t, n1.state, coinID)
	genesisSupply := n1.state.TotalSupply()
	if genesisBalance == 0 || genesisSupply == 0 {
		t.Fatalf("test misconfigured: genesis balance=%d supply=%d must be nonzero", genesisBalance, genesisSupply)
	}

	// Simulate activity since genesis: spend part of the reserve coin and burn
	// some supply, so the persisted state diverges from the genesis-seed values.
	const spent = 12345
	spentBalance := genesisBalance - spent
	n1.state.SetObject(buildCoinWithBalance(coinID, owner, spentBalance))
	n1.state.SubSupply(spent)
	n1.state.SubCoins(spent)
	spentSupply := n1.state.TotalSupply()

	// Stop the first session's background production loop before closing its
	// storage handle, or it panics trying to write to closed storage.
	n1.dag.Close()
	if err := db1.Close(); err != nil {
		t.Fatalf("close first session storage: %v", err)
	}

	// Restart: fresh storage handle, state, and dag over the same data directory.
	n2, db2 := bootstrapTestNode(t, dir, privKey)
	defer db2.Close()
	defer n2.dag.Close()

	n2.seedGenesisState()

	if got := coinBalanceFor(t, n2.state, coinID); got != spentBalance {
		t.Errorf("reserve coin balance after restart = %d, want %d (unchanged by re-seed)", got, spentBalance)
	}
	if got := n2.state.TotalSupply(); got != spentSupply {
		t.Errorf("total_supply after restart = %d, want %d (unchanged by re-seed)", got, spentSupply)
	}

	// The founder's validator entry must still be restored: nothing else installs
	// the live self-stake on a bootstrap-mode restart.
	is := genesis.BuildInitialState(n2.genesisConfig(), owner)
	info := n2.dag.ValidatorSet().Get(is.Pubkey)
	if info == nil {
		t.Fatal("founder missing from validator set after restart")
	}
	if info.SelfStake != is.SelfStake {
		t.Errorf("founder self-stake after restart = %d, want %d (genesis stake, re-installed)", info.SelfStake, is.SelfStake)
	}
}
