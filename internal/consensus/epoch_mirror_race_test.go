package consensus

import (
	"sync"
	"testing"
)

// TestEpochMirror_ConcurrentReadersDuringTransitions drives real epoch
// boundaries through the commit path while goroutines concurrently call
// EpochHolders, HoldersForEpoch, and Epoch — the accessors external readers
// (a status handler, an anchor bundle, a joiner's trusted-checkpoint judge)
// use off no lock. Run under -race, this is the regression for the
// currentEpoch/epochHolders/prevEpochHolders/nextEpochHolders mirror: before
// it, any of these accessors touched fields the commit loop mutates under
// commitMu, unsynchronized. The in-test assertions are a secondary sanity
// check; the race detector's silence is the actual guarantee.
func TestEpochMirror_ConcurrentReadersDuringTransitions(t *testing.T) {
	db := newTestStorage(t)
	validators, vs := newTestValidatorSet(1)
	mock := &mockBroadcaster{}

	// Listener mode plus a manual checkCommits drive, exactly as
	// TestEpochTransition_ViaCommitPath does, gives full control over which
	// rounds commit so the test can force several epoch boundaries quickly.
	dag := New(db, vs, mock, testSystemPod, 0, validators[0].privKey, nil,
		WithEpochLength(5),
		WithListenerMode(),
	)
	defer dag.Close()

	dag.InitEpochHolders()
	dag.validators.SetSelfStake(validators[0].pubKey, 100)
	freezeGenesis(dag) // freeze the genesis committee so the anchor path resolves epoch 0

	stop := make(chan struct{})
	var wg sync.WaitGroup

	reader := func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}

			epoch := dag.Epoch()
			if holders, ok := dag.HoldersForEpoch(epoch); ok && holders == nil {
				t.Error("HoldersForEpoch reported ok=true with a nil set")
			}
			if dag.EpochHolders() == nil {
				t.Error("EpochHolders returned nil")
			}
			_ = dag.LiveEpoch()
			_ = dag.EpochHoldersCount()
		}
	}

	const readers = 4
	wg.Add(readers)
	for i := 0; i < readers; i++ {
		go reader()
	}

	// With 1 staked validator holding the whole capped stake, round R commits
	// once a vertex at R+2 exists. epochLength=5 crosses two boundaries by
	// round 12.
	for round := uint64(0); round <= 12; round++ {
		data := buildVertexWithProperParents(t, dag, validators[0], round, 0)
		if !dag.AddVertex(data) {
			t.Fatalf("AddVertex failed at round %d", round)
		}
		dag.checkCommits()
	}

	close(stop)
	wg.Wait()

	if dag.Epoch() < 2 {
		t.Fatalf("expected at least 2 epoch transitions to have run concurrently with the readers, got epoch %d", dag.Epoch())
	}
}
