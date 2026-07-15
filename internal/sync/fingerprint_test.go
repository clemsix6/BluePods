package sync

import (
	"crypto/ed25519"
	"crypto/rand"
	"os"
	"path/filepath"
	"testing"

	"BluePods/internal/consensus"
	"BluePods/internal/genesis"
	"BluePods/internal/podvm"
	"BluePods/internal/state"
	"BluePods/internal/storage"
)

// fingerprintTestPod is a placeholder system-pod ID for fingerprint tests; its
// value is irrelevant since no vertex is ever produced or validated.
var fingerprintTestPod = consensus.Hash{0xF9}

// generateTestKey generates a random ed25519 key pair for a fingerprint test DAG.
func generateTestKey(t *testing.T) ed25519.PrivateKey {
	t.Helper()

	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	return priv
}

// newFingerprintTestPair creates a fresh DAG and State pair backed by the same
// temporary storage (mirroring production wiring, where both share one
// storage.Storage), with an empty validator set. The DAG's commit and liveness
// loops are stopped via t.Cleanup.
func newFingerprintTestPair(t *testing.T) (*consensus.DAG, *state.State) {
	t.Helper()

	dir, err := os.MkdirTemp("", "fingerprint_test_*")
	if err != nil {
		t.Fatalf("mkdir temp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })

	db, err := storage.New(filepath.Join(dir, "db"))
	if err != nil {
		t.Fatalf("create storage: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	pool := podvm.New()
	t.Cleanup(func() { pool.Close() })

	st := state.New(db, pool)

	dag := consensus.New(db, consensus.NewValidatorSet(nil), nil, fingerprintTestPod, 0, generateTestKey(t), st)
	t.Cleanup(func() { dag.Close() })

	return dag, st
}

// TestComputeFingerprint_StableAcrossIdleComputes verifies two computes with no
// state change in between are byte-identical.
func TestComputeFingerprint_StableAcrossIdleComputes(t *testing.T) {
	dag, st := newFingerprintTestPair(t)

	fp1 := ComputeFingerprint(dag, st)
	fp2 := ComputeFingerprint(dag, st)

	if fp1.Checksum != fp2.Checksum {
		t.Errorf("checksum changed with no state change: %x vs %x", fp1.Checksum, fp2.Checksum)
	}
}

// TestComputeFingerprint_SingletonContentHashedStandardNot verifies that
// mutating a singleton's content changes the checksum, while mutating a
// standard (replicated) object's content, with its tracker entry unchanged,
// does not: only the tracker entry (not holder-scoped content) represents a
// replicated object in the hash.
func TestComputeFingerprint_SingletonContentHashedStandardNot(t *testing.T) {
	dag, st := newFingerprintTestPair(t)

	var singletonID, standardID [32]byte
	singletonID[0] = 0x01
	standardID[0] = 0x02

	dag.TrackObject(singletonID, 1, 0, 0)
	dag.TrackObject(standardID, 1, 5, 0)

	st.SetObject(buildTestObjectWithReplication(singletonID, 1, []byte("v1"), 0))
	st.SetObject(buildTestObjectWithReplication(standardID, 1, []byte("v1"), 5))

	base := ComputeFingerprint(dag, st)

	// Mutating the singleton's content (tracker entry unchanged) must change
	// the checksum: singleton bytes are hashed directly.
	st.SetObject(buildTestObjectWithReplication(singletonID, 1, []byte("v2"), 0))

	afterSingletonEdit := ComputeFingerprint(dag, st)
	if afterSingletonEdit.Checksum == base.Checksum {
		t.Error("checksum unchanged after mutating a singleton's content")
	}

	// Revert the singleton, then mutate the standard object's content with its
	// tracker entry unchanged: the checksum must NOT change.
	st.SetObject(buildTestObjectWithReplication(singletonID, 1, []byte("v1"), 0))
	st.SetObject(buildTestObjectWithReplication(standardID, 1, []byte("completely different content"), 5))

	afterStandardEdit := ComputeFingerprint(dag, st)
	if afterStandardEdit.Checksum != base.Checksum {
		t.Error("checksum changed after mutating a standard object's content with its tracker entry unchanged")
	}
}

// TestComputeFingerprint_SupplyInvariantAtGenesis verifies the fingerprint's
// supply terms satisfy coins_total + total_bonded + deposits + fees_in_flight
// == total_supply on a freshly seeded genesis state: coins_total starts at the
// coin's balance and total_bonded at the founder's seeded stake (Task 5), so
// the two sum exactly to total_supply with zero deposits and zero fees.
func TestComputeFingerprint_SupplyInvariantAtGenesis(t *testing.T) {
	dag, st := newFingerprintTestPair(t)

	params := consensus.DefaultFeeParams()
	dag.SetFeeSystem(st, &params, nil)

	var owner [32]byte
	owner[0] = 0x42

	cfg := genesis.Config{InitialMint: 1_000_000, GenesisStake: 300_000, QUICAddress: "quic://founder:9000"}
	is := genesis.BuildInitialState(cfg, owner)

	dag.SeedGenesis(is)

	fp := ComputeFingerprint(dag, st)

	sum := fp.CoinsTotal + fp.TotalBonded + fp.Deposits + fp.FeesInFlight
	if sum != fp.TotalSupply {
		t.Errorf("supply mismatch: coinsTotal(%d) + totalBonded(%d) + deposits(%d) + feesInFlight(%d) = %d, want totalSupply %d",
			fp.CoinsTotal, fp.TotalBonded, fp.Deposits, fp.FeesInFlight, sum, fp.TotalSupply)
	}
}
