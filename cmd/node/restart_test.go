package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"testing"
	"time"

	"BluePods/internal/consensus"
	"BluePods/internal/genesis"
	"BluePods/internal/storage"
)

// waitForCommit blocks until the DAG advances its commit cursor at least minRound
// rounds past start, or fails after the deadline. A committed round is what writes the
// live validator set to storage, so the test waits for one after its mutations.
func waitForCommit(t *testing.T, dag *consensus.DAG, start uint64, minRounds uint64) {
	t.Helper()

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if dag.LastCommittedRound() >= start+minRounds {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}

	t.Fatalf("bootstrap node did not commit past round %d within the deadline (cursor=%d)",
		start+minRounds, dag.LastCommittedRound())
}

// TestBuildValidatorSetRestoresLiveSetOnRestart is co-factor (a) of the bootstrap
// restart bug at the init seam. A bootstrap node seeds genesis, the founder bonds extra
// self-stake above its genesis figure, a second validator joins the live set, and the
// node commits a round — persisting that live set with the commit cursor. On restart,
// buildValidatorSet over the same data directory must rebuild the full live set: the
// founder with its bonded (not genesis) self-stake and the second validator with its
// own stake and reward coin. Before the fix the restart started from the bare local
// identity, collapsing total_bonded to the founder's genesis self-stake.
func TestBuildValidatorSetRestoresLiveSetOnRestart(t *testing.T) {
	dir := t.TempDir()
	_, privKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	// Session 1: seed genesis, diverge the founder's live stake, add a second validator,
	// and commit so the live set is persisted through the normal path.
	n1, db1 := bootstrapTestNode(t, dir, privKey)
	n1.seedGenesisState()

	owner := deriveOwner(privKey)
	is := genesis.BuildInitialState(n1.genesisConfig(), owner)
	founder := consensus.Hash(is.Pubkey)

	const founderBond = uint64(7_000)
	wantFounderStake := is.SelfStake + founderBond
	n1.dag.ValidatorSet().SetSelfStake(founder, wantFounderStake)

	var other consensus.Hash
	other[0], other[31] = 0xAB, 0xCD
	var otherBLS [48]byte
	otherBLS[0] = 0x09
	otherCoin := consensus.Hash{0x77, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x42}
	const otherStake = uint64(55_000)
	n1.dag.ValidatorSet().AddWithStake(other, "10.0.0.9:9009", otherBLS, otherStake, 0, false)
	n1.dag.ValidatorSet().SetRewardCoin(other, otherCoin)

	start := n1.dag.LastCommittedRound()
	waitForCommit(t, n1.dag, start, 1)

	n1.dag.Close()
	if err := db1.Close(); err != nil {
		t.Fatalf("close first session storage: %v", err)
	}

	// Session 2: fresh buildValidatorSet over the same data directory.
	db2, err := storage.New(dir)
	if err != nil {
		t.Fatalf("reopen storage: %v", err)
	}
	defer db2.Close()

	n2 := &Node{cfg: &Config{Bootstrap: true, PrivateKey: privKey}, storage: db2}
	vs := n2.buildValidatorSet()

	if vs.Len() != 2 {
		t.Fatalf("restored %d validators, want 2 (validator-set membership lost on restart)", vs.Len())
	}

	fInfo := vs.Get(founder)
	if fInfo == nil {
		t.Fatal("founder absent from validator set after restart")
	}
	if fInfo.SelfStake != wantFounderStake {
		t.Errorf("founder self-stake after restart = %d, want %d (bond discarded / collapsed to genesis)",
			fInfo.SelfStake, wantFounderStake)
	}

	oInfo := vs.Get(other)
	if oInfo == nil {
		t.Fatal("second validator absent from validator set after restart")
	}
	if oInfo.SelfStake != otherStake {
		t.Errorf("second validator self-stake after restart = %d, want %d", oInfo.SelfStake, otherStake)
	}
	if oInfo.RewardCoin != otherCoin {
		t.Errorf("second validator reward coin after restart = %x, want %x", oInfo.RewardCoin[:4], otherCoin[:4])
	}
}
