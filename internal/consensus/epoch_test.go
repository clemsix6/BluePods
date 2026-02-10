package consensus

import (
	"bytes"
	"crypto/ed25519"
	"sort"
	"testing"

	flatbuffers "github.com/google/flatbuffers/go"
	"github.com/zeebo/blake3"

	"BluePods/internal/types"
)

// TestIsEpochBoundary tests epoch boundary detection at correct rounds.
func TestIsEpochBoundary(t *testing.T) {
	db := newTestStorage(t)
	validators, vs := newTestValidatorSet(4)

	dag := New(db, vs, nil, testSystemPod, 0, validators[0].privKey, nil,
		WithEpochLength(100),
	)
	defer dag.Close()

	tests := []struct {
		round    uint64
		expected bool
	}{
		{0, false},     // round 0 is never an epoch boundary
		{1, false},     // not a multiple
		{50, false},    // not a multiple
		{99, false},    // not a multiple
		{100, true},    // first epoch boundary
		{200, true},    // second epoch boundary
		{300, true},    // third epoch boundary
		{101, false},   // not a multiple
		{1000, true},   // large multiple
	}

	for _, tt := range tests {
		got := dag.isEpochBoundary(tt.round)
		if got != tt.expected {
			t.Errorf("isEpochBoundary(%d) = %v, want %v", tt.round, got, tt.expected)
		}
	}
}

// TestIsEpochBoundary_Disabled tests that epochLength=0 never triggers.
func TestIsEpochBoundary_Disabled(t *testing.T) {
	db := newTestStorage(t)
	validators, vs := newTestValidatorSet(4)

	dag := New(db, vs, nil, testSystemPod, 0, validators[0].privKey, nil)
	defer dag.Close()

	// epochLength defaults to 0
	for _, round := range []uint64{0, 1, 100, 1000} {
		if dag.isEpochBoundary(round) {
			t.Errorf("isEpochBoundary(%d) should be false when disabled", round)
		}
	}
}

// TestTransitionEpoch_IncrementsEpoch tests that the epoch counter is incremented.
func TestTransitionEpoch_IncrementsEpoch(t *testing.T) {
	db := newTestStorage(t)
	validators, vs := newTestValidatorSet(4)

	dag := New(db, vs, nil, testSystemPod, 0, validators[0].privKey, nil,
		WithEpochLength(10),
	)
	defer dag.Close()

	if dag.Epoch() != 0 {
		t.Fatalf("expected epoch 0, got %d", dag.Epoch())
	}

	dag.transitionEpoch(10)

	if dag.Epoch() != 1 {
		t.Fatalf("expected epoch 1 after transition, got %d", dag.Epoch())
	}

	dag.transitionEpoch(20)

	if dag.Epoch() != 2 {
		t.Fatalf("expected epoch 2 after second transition, got %d", dag.Epoch())
	}
}

// TestTransitionEpoch_FreezesValidatorSet tests that epochHolders matches validators at boundary.
func TestTransitionEpoch_FreezesValidatorSet(t *testing.T) {
	db := newTestStorage(t)
	validators, vs := newTestValidatorSet(4)

	dag := New(db, vs, nil, testSystemPod, 0, validators[0].privKey, nil,
		WithEpochLength(10),
	)
	defer dag.Close()

	dag.transitionEpoch(10)

	holders := dag.EpochHolders()
	if holders.Len() != 4 {
		t.Fatalf("expected 4 epoch holders, got %d", holders.Len())
	}

	// Verify all validators are in the epoch holders
	for _, v := range validators {
		if !holders.Contains(v.pubKey) {
			t.Errorf("epoch holders missing validator %x", v.pubKey[:4])
		}
	}
}

// TestTransitionEpoch_AppliesPendingRemovals tests that removed validators are excluded.
func TestTransitionEpoch_AppliesPendingRemovals(t *testing.T) {
	db := newTestStorage(t)
	validators, vs := newTestValidatorSet(4)

	dag := New(db, vs, nil, testSystemPod, 0, validators[0].privKey, nil,
		WithEpochLength(10),
	)
	defer dag.Close()

	// Mark validator 3 for removal
	dag.pendingRemovals[validators[3].pubKey] = true

	dag.transitionEpoch(10)

	// Validator 3 should be removed from both validators and epoch holders
	if dag.validators.Contains(validators[3].pubKey) {
		t.Error("removed validator should not be in active set")
	}

	if dag.EpochHolders().Contains(validators[3].pubKey) {
		t.Error("removed validator should not be in epoch holders")
	}

	if dag.validators.Len() != 3 {
		t.Errorf("expected 3 validators, got %d", dag.validators.Len())
	}
}

// TestMidEpochRegistration_NotInEpochHolders tests that new validators are NOT in the frozen set.
func TestMidEpochRegistration_NotInEpochHolders(t *testing.T) {
	db := newTestStorage(t)
	validators, vs := newTestValidatorSet(3)

	dag := New(db, vs, nil, testSystemPod, 0, validators[0].privKey, nil,
		WithEpochLength(100),
	)
	defer dag.Close()

	// First epoch transition freezes the 3 validators
	dag.transitionEpoch(100)

	if dag.EpochHolders().Len() != 3 {
		t.Fatalf("expected 3 epoch holders, got %d", dag.EpochHolders().Len())
	}

	// Add a new validator mid-epoch
	newVal := newTestValidator()
	dag.validators.Add(newVal.pubKey, "", "", [48]byte{})
	dag.epochAdditions = append(dag.epochAdditions, newVal.pubKey)

	// New validator is in active set but NOT in epoch holders
	if !dag.validators.Contains(newVal.pubKey) {
		t.Error("new validator should be in active set")
	}

	if dag.EpochHolders().Contains(newVal.pubKey) {
		t.Error("mid-epoch addition should NOT be in epoch holders")
	}
}

// TestMidEpochRegistration_InEpochHoldersAfterTransition tests new validator included after transition.
func TestMidEpochRegistration_InEpochHoldersAfterTransition(t *testing.T) {
	db := newTestStorage(t)
	validators, vs := newTestValidatorSet(3)

	dag := New(db, vs, nil, testSystemPod, 0, validators[0].privKey, nil,
		WithEpochLength(100),
	)
	defer dag.Close()

	// First epoch transition
	dag.transitionEpoch(100)

	// Add new validator mid-epoch
	newVal := newTestValidator()
	dag.validators.Add(newVal.pubKey, "", "", [48]byte{})

	// Second epoch transition should include the new validator
	dag.transitionEpoch(200)

	if !dag.EpochHolders().Contains(newVal.pubKey) {
		t.Error("new validator should be in epoch holders after next transition")
	}

	if dag.EpochHolders().Len() != 4 {
		t.Errorf("expected 4 epoch holders, got %d", dag.EpochHolders().Len())
	}
}

// TestDeregister_StaysActiveUntilEpoch tests that deregistered validator stays active until boundary.
func TestDeregister_StaysActiveUntilEpoch(t *testing.T) {
	db := newTestStorage(t)
	validators, vs := newTestValidatorSet(4)

	dag := New(db, vs, nil, testSystemPod, 0, validators[0].privKey, nil,
		WithEpochLength(10),
	)
	defer dag.Close()

	// Mark for removal but don't transition
	dag.pendingRemovals[validators[2].pubKey] = true

	// Validator should still be in the active set
	if !dag.validators.Contains(validators[2].pubKey) {
		t.Error("pending removal should still be in active set before epoch boundary")
	}
}

// TestDeregister_RemovedAfterEpoch tests that deregistered validator is gone after boundary.
func TestDeregister_RemovedAfterEpoch(t *testing.T) {
	db := newTestStorage(t)
	validators, vs := newTestValidatorSet(4)

	dag := New(db, vs, nil, testSystemPod, 0, validators[0].privKey, nil,
		WithEpochLength(10),
	)
	defer dag.Close()

	dag.pendingRemovals[validators[2].pubKey] = true
	dag.transitionEpoch(10)

	if dag.validators.Contains(validators[2].pubKey) {
		t.Error("deregistered validator should be removed after epoch transition")
	}

	if dag.EpochHolders().Contains(validators[2].pubKey) {
		t.Error("deregistered validator should not be in epoch holders")
	}
}

// TestMultipleEpochTransitions tests 3+ epoch transitions in sequence.
func TestMultipleEpochTransitions(t *testing.T) {
	db := newTestStorage(t)
	validators, vs := newTestValidatorSet(5)

	dag := New(db, vs, nil, testSystemPod, 0, validators[0].privKey, nil,
		WithEpochLength(10),
	)
	defer dag.Close()

	// Epoch 1: remove validator 4
	dag.pendingRemovals[validators[4].pubKey] = true
	dag.transitionEpoch(10)

	if dag.Epoch() != 1 {
		t.Fatalf("expected epoch 1, got %d", dag.Epoch())
	}
	if dag.validators.Len() != 4 {
		t.Fatalf("expected 4 validators after epoch 1, got %d", dag.validators.Len())
	}

	// Epoch 2: add new validator, remove validator 3
	newVal := newTestValidator()
	dag.validators.Add(newVal.pubKey, "", "", [48]byte{})
	dag.pendingRemovals[validators[3].pubKey] = true
	dag.transitionEpoch(20)

	if dag.Epoch() != 2 {
		t.Fatalf("expected epoch 2, got %d", dag.Epoch())
	}
	if dag.validators.Len() != 4 {
		t.Fatalf("expected 4 validators after epoch 2, got %d", dag.validators.Len())
	}

	// Epoch 3: no changes
	dag.transitionEpoch(30)

	if dag.Epoch() != 3 {
		t.Fatalf("expected epoch 3, got %d", dag.Epoch())
	}

	// Verify final epoch holders
	if !dag.EpochHolders().Contains(newVal.pubKey) {
		t.Error("new validator should be in epoch holders")
	}
	if dag.EpochHolders().Contains(validators[3].pubKey) {
		t.Error("removed validator 3 should not be in epoch holders")
	}
	if dag.EpochHolders().Contains(validators[4].pubKey) {
		t.Error("removed validator 4 should not be in epoch holders")
	}
}

// TestEpochTransition_CallbackFired tests that the epoch transition callback is called.
func TestEpochTransition_CallbackFired(t *testing.T) {
	db := newTestStorage(t)
	validators, vs := newTestValidatorSet(4)

	dag := New(db, vs, nil, testSystemPod, 0, validators[0].privKey, nil,
		WithEpochLength(10),
	)
	defer dag.Close()

	var calledEpoch uint64
	dag.OnEpochTransition(func(epoch uint64) {
		calledEpoch = epoch
	})

	dag.transitionEpoch(10)

	if calledEpoch != 1 {
		t.Errorf("expected callback with epoch 1, got %d", calledEpoch)
	}

	dag.transitionEpoch(20)

	if calledEpoch != 2 {
		t.Errorf("expected callback with epoch 2, got %d", calledEpoch)
	}
}

// TestInitEpochHolders tests that InitEpochHolders sets up the initial epoch state.
func TestInitEpochHolders(t *testing.T) {
	db := newTestStorage(t)
	validators, vs := newTestValidatorSet(4)

	dag := New(db, vs, nil, testSystemPod, 0, validators[0].privKey, nil,
		WithEpochLength(10),
	)
	defer dag.Close()

	// Before InitEpochHolders, epochHolders is nil
	if dag.epochHolders != nil {
		t.Fatal("epochHolders should be nil before init")
	}

	dag.InitEpochHolders()

	// After init, epochHolders should have all validators
	if dag.epochHolders == nil {
		t.Fatal("epochHolders should not be nil after init")
	}

	if dag.epochHolders.Len() != 4 {
		t.Errorf("expected 4 epoch holders, got %d", dag.epochHolders.Len())
	}
}

// TestInitEpochHolders_DisabledWhenNoEpochs tests that InitEpochHolders is a no-op without epochs.
func TestInitEpochHolders_DisabledWhenNoEpochs(t *testing.T) {
	db := newTestStorage(t)
	validators, vs := newTestValidatorSet(4)

	dag := New(db, vs, nil, testSystemPod, 0, validators[0].privKey, nil)
	defer dag.Close()

	dag.InitEpochHolders()

	if dag.epochHolders != nil {
		t.Error("epochHolders should remain nil when epochs are disabled")
	}
}

// TestChurnLimit_CapsRemovals tests that excess removals are deferred.
func TestChurnLimit_CapsRemovals(t *testing.T) {
	db := newTestStorage(t)
	validators, vs := newTestValidatorSet(6)

	dag := New(db, vs, nil, testSystemPod, 0, validators[0].privKey, nil,
		WithEpochLength(10),
		WithMaxChurnPerEpoch(2),
	)
	defer dag.Close()

	// Mark 4 validators for removal (exceeds max churn of 2)
	dag.pendingRemovals[validators[2].pubKey] = true
	dag.pendingRemovals[validators[3].pubKey] = true
	dag.pendingRemovals[validators[4].pubKey] = true
	dag.pendingRemovals[validators[5].pubKey] = true

	dag.transitionEpoch(10)

	// Only 2 should have been removed (max churn = 2)
	if dag.validators.Len() != 4 {
		t.Fatalf("expected 4 validators (6 - 2 churn), got %d", dag.validators.Len())
	}

	// 2 should remain in pending removals
	if len(dag.pendingRemovals) != 2 {
		t.Fatalf("expected 2 deferred removals, got %d", len(dag.pendingRemovals))
	}

	// Second epoch transition should apply 2 more
	dag.transitionEpoch(20)

	if dag.validators.Len() != 2 {
		t.Fatalf("expected 2 validators after second transition, got %d", dag.validators.Len())
	}

	if len(dag.pendingRemovals) != 0 {
		t.Fatalf("expected 0 deferred removals after second transition, got %d", len(dag.pendingRemovals))
	}
}

// TestChurnLimit_CapsAdditions tests that excess additions are excluded from epochHolders.
func TestChurnLimit_CapsAdditions(t *testing.T) {
	db := newTestStorage(t)
	validators, vs := newTestValidatorSet(4)

	dag := New(db, vs, nil, testSystemPod, 0, validators[0].privKey, nil,
		WithEpochLength(10),
		WithMaxChurnPerEpoch(2),
	)
	defer dag.Close()

	// First epoch to initialize holders
	dag.transitionEpoch(10)

	if dag.EpochHolders().Len() != 4 {
		t.Fatalf("expected 4 epoch holders, got %d", dag.EpochHolders().Len())
	}

	// Add 5 new validators mid-epoch (exceeds max churn of 2)
	newVals := make([]testValidator, 5)
	for i := 0; i < 5; i++ {
		newVals[i] = newTestValidator()
		dag.validators.Add(newVals[i].pubKey, "", "", [48]byte{})
		dag.epochAdditions = append(dag.epochAdditions, newVals[i].pubKey)
	}

	if dag.validators.Len() != 9 {
		t.Fatalf("expected 9 validators (4 + 5), got %d", dag.validators.Len())
	}

	// Epoch 2: only 2 of the 5 new additions should be in epochHolders
	dag.transitionEpoch(20)

	expectedHolders := 4 + 2 // original + allowed additions
	if dag.EpochHolders().Len() != expectedHolders {
		t.Fatalf("expected %d epoch holders (4 original + 2 churn), got %d",
			expectedHolders, dag.EpochHolders().Len())
	}

	// All 4 original validators should still be in epochHolders
	for _, v := range validators {
		if !dag.EpochHolders().Contains(v.pubKey) {
			t.Errorf("original validator %x should be in epoch holders", v.pubKey[:4])
		}
	}

	// Exactly 2 new validators should be in epochHolders
	newInHolders := 0
	for _, v := range newVals {
		if dag.EpochHolders().Contains(v.pubKey) {
			newInHolders++
		}
	}

	if newInHolders != 2 {
		t.Fatalf("expected 2 new validators in epoch holders, got %d", newInHolders)
	}

	// epochAdditions should be cleared
	if len(dag.epochAdditions) != 0 {
		t.Fatalf("epochAdditions should be cleared, got %d", len(dag.epochAdditions))
	}
}

// TestChurnLimit_NoLimitWhenDisabled tests that maxChurn=0 means unlimited.
func TestChurnLimit_NoLimitWhenDisabled(t *testing.T) {
	db := newTestStorage(t)
	validators, vs := newTestValidatorSet(6)

	dag := New(db, vs, nil, testSystemPod, 0, validators[0].privKey, nil,
		WithEpochLength(10),
		// maxChurnPerEpoch defaults to 0 (unlimited)
	)
	defer dag.Close()

	// Mark all 6 for removal
	for _, v := range validators {
		dag.pendingRemovals[v.pubKey] = true
	}

	dag.transitionEpoch(10)

	// All should be removed at once
	if dag.validators.Len() != 0 {
		t.Fatalf("expected 0 validators with unlimited churn, got %d", dag.validators.Len())
	}
}

// TestValidatorSetRemove tests the Remove method on ValidatorSet.
func TestValidatorSetRemove(t *testing.T) {
	validators, vs := newTestValidatorSet(4)

	// Remove validator 2
	removed := vs.Remove(validators[2].pubKey)
	if !removed {
		t.Fatal("Remove should return true for existing validator")
	}

	if vs.Len() != 3 {
		t.Fatalf("expected 3 validators after remove, got %d", vs.Len())
	}

	if vs.Contains(validators[2].pubKey) {
		t.Error("removed validator should not be in set")
	}

	// Remove non-existent validator
	unknown := newTestValidator()
	if vs.Remove(unknown.pubKey) {
		t.Error("Remove should return false for non-existent validator")
	}

	// Verify remaining validators are still accessible
	for _, i := range []int{0, 1, 3} {
		if !vs.Contains(validators[i].pubKey) {
			t.Errorf("validator %d should still be in set", i)
		}
	}
}

// TestEpochHolders_FallsBackToValidators tests that EpochHolders returns validators if not initialized.
func TestEpochHolders_FallsBackToValidators(t *testing.T) {
	db := newTestStorage(t)
	validators, vs := newTestValidatorSet(4)

	dag := New(db, vs, nil, testSystemPod, 0, validators[0].privKey, nil)
	defer dag.Close()

	// Without epoch initialization, EpochHolders should return validators
	holders := dag.EpochHolders()
	if holders.Len() != vs.Len() {
		t.Errorf("expected epoch holders to fall back to validators, got %d vs %d",
			holders.Len(), vs.Len())
	}
}

// =============================================================================
// Frozen independence: epochHolders must be fully independent from validators
// =============================================================================

// TestEpochHolders_FrozenIndependence verifies that modifying the live validators
// after a snapshot does NOT affect the frozen epochHolders.
func TestEpochHolders_FrozenIndependence(t *testing.T) {
	db := newTestStorage(t)
	validators, vs := newTestValidatorSet(4)

	dag := New(db, vs, nil, testSystemPod, 0, validators[0].privKey, nil,
		WithEpochLength(10),
	)
	defer dag.Close()

	dag.transitionEpoch(10)

	// After snapshot, epochHolders has 4
	if dag.EpochHolders().Len() != 4 {
		t.Fatalf("expected 4 epoch holders, got %d", dag.EpochHolders().Len())
	}

	// Add a new validator to the live set
	newVal := newTestValidator()
	dag.validators.Add(newVal.pubKey, "", "", [48]byte{})

	// epochHolders should STILL be 4, NOT 5
	if dag.EpochHolders().Len() != 4 {
		t.Fatalf("epoch holders should remain frozen at 4, got %d", dag.EpochHolders().Len())
	}

	// Remove a validator from the live set
	dag.validators.Remove(validators[0].pubKey)

	// epochHolders should STILL be 4
	if dag.EpochHolders().Len() != 4 {
		t.Fatalf("epoch holders should remain frozen at 4 after live removal, got %d", dag.EpochHolders().Len())
	}

	// Original validator should still be in frozen set even though removed from live
	if !dag.EpochHolders().Contains(validators[0].pubKey) {
		t.Error("removed-from-live validator should still be in frozen epoch holders")
	}

	// New validator should NOT be in frozen set
	if dag.EpochHolders().Contains(newVal.pubKey) {
		t.Error("newly added validator should NOT be in frozen epoch holders")
	}
}

// =============================================================================
// Edge cases
// =============================================================================

// TestEpochLength1_EveryRoundIsEpoch tests that epochLength=1 triggers every round.
func TestEpochLength1_EveryRoundIsEpoch(t *testing.T) {
	db := newTestStorage(t)
	validators, vs := newTestValidatorSet(4)

	dag := New(db, vs, nil, testSystemPod, 0, validators[0].privKey, nil,
		WithEpochLength(1),
	)
	defer dag.Close()

	if dag.isEpochBoundary(0) {
		t.Error("round 0 should never be epoch boundary")
	}

	for _, round := range []uint64{1, 2, 3, 10, 100} {
		if !dag.isEpochBoundary(round) {
			t.Errorf("round %d should be epoch boundary with epochLength=1", round)
		}
	}

	// 5 transitions in a row
	for i := uint64(1); i <= 5; i++ {
		dag.transitionEpoch(i)
	}

	if dag.Epoch() != 5 {
		t.Fatalf("expected epoch 5, got %d", dag.Epoch())
	}
}

// TestRegisterAndDeregisterSameEpoch tests registering then deregistering within one epoch.
func TestRegisterAndDeregisterSameEpoch(t *testing.T) {
	db := newTestStorage(t)
	validators, vs := newTestValidatorSet(3)

	dag := New(db, vs, nil, testSystemPod, 0, validators[0].privKey, nil,
		WithEpochLength(10),
	)
	defer dag.Close()

	// First epoch transition
	dag.transitionEpoch(10)

	// Mid-epoch: add a new validator
	newVal := newTestValidator()
	dag.validators.Add(newVal.pubKey, "", "", [48]byte{})
	dag.epochAdditions = append(dag.epochAdditions, newVal.pubKey)

	// Mid-epoch: also mark the same validator for removal
	dag.pendingRemovals[newVal.pubKey] = true

	// Transition should remove the validator before snapshotting
	dag.transitionEpoch(20)

	// The validator should have been removed from active set
	if dag.validators.Contains(newVal.pubKey) {
		t.Error("validator registered + deregistered in same epoch should be removed")
	}

	// And should NOT be in epoch holders
	if dag.EpochHolders().Contains(newVal.pubKey) {
		t.Error("validator registered + deregistered in same epoch should not be in epoch holders")
	}

	// epochAdditions should be cleared
	if len(dag.epochAdditions) != 0 {
		t.Errorf("epochAdditions should be cleared, got %d", len(dag.epochAdditions))
	}
}

// TestTransitionWithNoPendingChanges tests that epoch transition with no changes works cleanly.
func TestTransitionWithNoPendingChanges(t *testing.T) {
	db := newTestStorage(t)
	validators, vs := newTestValidatorSet(4)

	dag := New(db, vs, nil, testSystemPod, 0, validators[0].privKey, nil,
		WithEpochLength(10),
	)
	defer dag.Close()

	// Transition with no changes
	dag.transitionEpoch(10)

	if dag.Epoch() != 1 {
		t.Fatalf("expected epoch 1, got %d", dag.Epoch())
	}

	if dag.validators.Len() != 4 {
		t.Fatalf("expected 4 validators unchanged, got %d", dag.validators.Len())
	}

	if dag.EpochHolders().Len() != 4 {
		t.Fatalf("expected 4 epoch holders, got %d", dag.EpochHolders().Len())
	}
}

// =============================================================================
// Churn limit determinism
// =============================================================================

// TestChurnLimit_DeterministicOrder verifies that churn-limited removals always
// remove the same validators regardless of Go map iteration order.
// Runs multiple iterations to detect non-determinism.
func TestChurnLimit_DeterministicOrder(t *testing.T) {
	var firstRemoved []Hash

	for iter := 0; iter < 20; iter++ {
		db := newTestStorage(t)

		// Create 6 validators with fixed pubkeys (sorted order known)
		vs := NewValidatorSet(nil)
		pubkeys := make([]Hash, 6)
		for i := 0; i < 6; i++ {
			pubkeys[i] = Hash{byte(i + 1)} // deterministic keys: 01, 02, 03, 04, 05, 06
			vs.Add(pubkeys[i], "", "", [48]byte{})
		}

		// Use first validator's key as privKey (doesn't matter for this test)
		validators := make([]testValidator, 1)
		validators[0] = newTestValidator()

		dag := New(db, vs, nil, testSystemPod, 0, validators[0].privKey, nil,
			WithEpochLength(10),
			WithMaxChurnPerEpoch(2),
		)

		// Mark validators 3,4,5,6 for removal (4 removals, limit is 2)
		dag.pendingRemovals[pubkeys[2]] = true
		dag.pendingRemovals[pubkeys[3]] = true
		dag.pendingRemovals[pubkeys[4]] = true
		dag.pendingRemovals[pubkeys[5]] = true

		dag.transitionEpoch(10)

		// Collect which were removed (not in validators anymore)
		var removed []Hash
		for _, pk := range pubkeys[2:] {
			if !dag.validators.Contains(pk) {
				removed = append(removed, pk)
			}
		}

		sort.Slice(removed, func(i, j int) bool {
			return bytes.Compare(removed[i][:], removed[j][:]) < 0
		})

		if firstRemoved == nil {
			firstRemoved = removed
		} else {
			// Every iteration must remove the same validators
			if len(removed) != len(firstRemoved) {
				t.Fatalf("iteration %d: removed %d validators, expected %d", iter, len(removed), len(firstRemoved))
			}
			for i := range removed {
				if removed[i] != firstRemoved[i] {
					t.Fatalf("iteration %d: non-deterministic removal! got %x, expected %x",
						iter, removed[i][:4], firstRemoved[i][:4])
				}
			}
		}

		dag.Close()
	}
}

// TestChurnLimit_DeferredAppliedNextEpoch verifies deferred removals apply in subsequent epoch.
func TestChurnLimit_DeferredAppliedNextEpoch(t *testing.T) {
	db := newTestStorage(t)

	vs := NewValidatorSet(nil)
	pubkeys := make([]Hash, 5)
	for i := 0; i < 5; i++ {
		pubkeys[i] = Hash{byte(i + 1)}
		vs.Add(pubkeys[i], "", "", [48]byte{})
	}

	v := newTestValidator()
	dag := New(db, vs, nil, testSystemPod, 0, v.privKey, nil,
		WithEpochLength(10),
		WithMaxChurnPerEpoch(1),
	)
	defer dag.Close()

	// Mark 3 validators for removal, limit is 1 per epoch
	dag.pendingRemovals[pubkeys[2]] = true
	dag.pendingRemovals[pubkeys[3]] = true
	dag.pendingRemovals[pubkeys[4]] = true

	// Epoch 1: remove 1
	dag.transitionEpoch(10)
	if dag.validators.Len() != 4 {
		t.Fatalf("epoch 1: expected 4 validators, got %d", dag.validators.Len())
	}
	if len(dag.pendingRemovals) != 2 {
		t.Fatalf("epoch 1: expected 2 deferred, got %d", len(dag.pendingRemovals))
	}

	// Epoch 2: remove 1 more
	dag.transitionEpoch(20)
	if dag.validators.Len() != 3 {
		t.Fatalf("epoch 2: expected 3 validators, got %d", dag.validators.Len())
	}
	if len(dag.pendingRemovals) != 1 {
		t.Fatalf("epoch 2: expected 1 deferred, got %d", len(dag.pendingRemovals))
	}

	// Epoch 3: remove last one
	dag.transitionEpoch(30)
	if dag.validators.Len() != 2 {
		t.Fatalf("epoch 3: expected 2 validators, got %d", dag.validators.Len())
	}
	if len(dag.pendingRemovals) != 0 {
		t.Fatalf("epoch 3: expected 0 deferred, got %d", len(dag.pendingRemovals))
	}
}

// =============================================================================
// Epoch + scanner integration
// =============================================================================

// TestEpochTransition_ScannerSeesNewHolders tests that after an epoch transition
// where a new validator joins, the scanner correctly identifies that new objects
// need to be fetched (because Rendezvous now maps them to us).
func TestEpochTransition_ScannerSeesNewHolders(t *testing.T) {
	db := newTestStorage(t)
	validators, vs := newTestValidatorSet(3)

	dag := New(db, vs, nil, testSystemPod, 0, validators[0].privKey, nil,
		WithEpochLength(10),
	)
	defer dag.Close()

	// Track objects with replication=2
	obj1 := Hash{0x10}
	obj2 := Hash{0x20}
	obj3 := Hash{0x30}
	dag.tracker.trackObject(obj1, 1, 2, 0)
	dag.tracker.trackObject(obj2, 1, 2, 0)
	dag.tracker.trackObject(obj3, 1, 2, 0)

	// Epoch 1: initial state with 3 validators
	dag.transitionEpoch(10)

	// Simulate: before epoch transition, this node held obj1 and obj2
	localObjects := map[Hash]bool{obj1: true, obj2: true}

	// Now a new validator joins
	newVal := newTestValidator()
	dag.validators.Add(newVal.pubKey, "", "", [48]byte{})

	// Epoch 2: new validator in the set changes Rendezvous hashing
	dag.transitionEpoch(20)

	// After epoch 2, Rendezvous may have reassigned objects.
	// Simulate: now this node should hold obj1 and obj3 (not obj2)
	isHolderAfterEpoch := func(id [32]byte, rep uint16) bool {
		h := Hash(id)
		return h == obj1 || h == obj3
	}

	hasLocal := func(id [32]byte) bool {
		return localObjects[Hash(id)]
	}

	result := dag.ScanObjects(isHolderAfterEpoch, hasLocal)

	// obj3: should hold but don't have → NeedFetch
	foundFetch := false
	for _, id := range result.NeedFetch {
		if id == obj3 {
			foundFetch = true
		}
	}
	if !foundFetch {
		t.Error("expected obj3 in NeedFetch (new holder after epoch transition)")
	}

	// obj2: have locally but no longer holder → CanDrop
	foundDrop := false
	for _, id := range result.CanDrop {
		if id == obj2 {
			foundDrop = true
		}
	}
	if !foundDrop {
		t.Error("expected obj2 in CanDrop (no longer holder after epoch transition)")
	}
}

// TestEpochTransition_ScannerWithSingletons tests that singletons (replication=0)
// are always considered held and never appear in NeedFetch or CanDrop.
func TestEpochTransition_ScannerWithSingletons(t *testing.T) {
	db := newTestStorage(t)
	validators, vs := newTestValidatorSet(4)

	dag := New(db, vs, nil, testSystemPod, 0, validators[0].privKey, nil,
		WithEpochLength(10),
	)
	defer dag.Close()

	// Track a singleton and a standard object
	singleton := Hash{0xAA}
	standard := Hash{0xBB}
	dag.tracker.trackObject(singleton, 1, 0, 0) // replication=0 → singleton
	dag.tracker.trackObject(standard, 1, 3, 0)  // replication=3 → standard

	// isHolder: singletons always held (replication=0 → true), standard NOT held
	isHolder := func(id [32]byte, rep uint16) bool {
		if rep == 0 {
			return true // singletons always held
		}
		return false // this node doesn't hold the standard object
	}

	// hasLocal: we have the singleton but not the standard
	hasLocal := func(id [32]byte) bool {
		return Hash(id) == singleton
	}

	result := dag.ScanObjects(isHolder, hasLocal)

	// Singleton: holder + hasLocal → no action
	for _, id := range result.NeedFetch {
		if id == singleton {
			t.Error("singleton should never appear in NeedFetch")
		}
	}
	for _, id := range result.CanDrop {
		if id == singleton {
			t.Error("singleton should never appear in CanDrop")
		}
	}

	// Standard: NOT holder + NOT hasLocal → no action (ignore)
	if len(result.NeedFetch) != 0 {
		t.Errorf("expected 0 NeedFetch, got %d", len(result.NeedFetch))
	}
	if len(result.CanDrop) != 0 {
		t.Errorf("expected 0 CanDrop, got %d", len(result.CanDrop))
	}
}

// TestScannerReplicationPassedCorrectly tests that the scanner passes the correct
// replication value from the tracker to isHolder.
func TestScannerReplicationPassedCorrectly(t *testing.T) {
	db := newTestStorage(t)
	validators, vs := newTestValidatorSet(4)

	dag := New(db, vs, nil, testSystemPod, 0, validators[0].privKey, nil,
		WithEpochLength(10),
	)
	defer dag.Close()

	// Track objects with different replications
	obj1 := Hash{0x01}
	obj2 := Hash{0x02}
	obj3 := Hash{0x03}
	dag.tracker.trackObject(obj1, 1, 0, 0)  // singleton
	dag.tracker.trackObject(obj2, 1, 3, 0)  // replication=3
	dag.tracker.trackObject(obj3, 1, 10, 0) // replication=10

	// Record what replication values isHolder receives
	receivedRep := make(map[Hash]uint16)

	isHolder := func(id [32]byte, rep uint16) bool {
		receivedRep[Hash(id)] = rep
		return true
	}
	hasLocal := func(id [32]byte) bool { return true }

	dag.ScanObjects(isHolder, hasLocal)

	// Verify correct replication values were passed
	if receivedRep[obj1] != 0 {
		t.Errorf("obj1 replication: got %d, want 0", receivedRep[obj1])
	}
	if receivedRep[obj2] != 3 {
		t.Errorf("obj2 replication: got %d, want 3", receivedRep[obj2])
	}
	if receivedRep[obj3] != 10 {
		t.Errorf("obj3 replication: got %d, want 10", receivedRep[obj3])
	}
}

// TestEpochTransition_ValidatorLeaves_ObjectsRedistributed tests the full flow:
// validator deregisters → epoch transition → scanner detects redistribution.
func TestEpochTransition_ValidatorLeaves_ObjectsRedistributed(t *testing.T) {
	db := newTestStorage(t)
	validators, vs := newTestValidatorSet(4)

	dag := New(db, vs, nil, testSystemPod, 0, validators[0].privKey, nil,
		WithEpochLength(10),
	)
	defer dag.Close()

	// Track objects
	obj1 := Hash{0x01}
	obj2 := Hash{0x02}
	dag.tracker.trackObject(obj1, 1, 2, 0)
	dag.tracker.trackObject(obj2, 1, 2, 0)

	// Initialize epoch holders
	dag.InitEpochHolders()

	// Record epoch holders before
	holdersBefore := dag.EpochHolders().Len()
	if holdersBefore != 4 {
		t.Fatalf("expected 4 holders before, got %d", holdersBefore)
	}

	// Mark validator 3 for removal (deregister)
	dag.pendingRemovals[validators[3].pubKey] = true

	// Epoch transition removes the validator
	dag.transitionEpoch(10)

	holdersAfter := dag.EpochHolders().Len()
	if holdersAfter != 3 {
		t.Fatalf("expected 3 holders after removal, got %d", holdersAfter)
	}

	// The removed validator should NOT be in epoch holders
	if dag.EpochHolders().Contains(validators[3].pubKey) {
		t.Error("removed validator should not be in epoch holders")
	}

	// Remaining validators should be there
	for i := 0; i < 3; i++ {
		if !dag.EpochHolders().Contains(validators[i].pubKey) {
			t.Errorf("validator %d should still be in epoch holders", i)
		}
	}

	// Scanner should now show redistribution needs
	// (exact results depend on the isHolder function, but the mechanism works)
	localObjs := map[Hash]bool{obj1: true}

	result := dag.ScanObjects(
		func(id [32]byte, rep uint16) bool {
			// After validator leaves, this node now holds everything
			return true
		},
		func(id [32]byte) bool {
			return localObjs[Hash(id)]
		},
	)

	// obj2 should be in NeedFetch (holder=true, hasLocal=false)
	if len(result.NeedFetch) != 1 || result.NeedFetch[0] != obj2 {
		t.Errorf("expected obj2 in NeedFetch, got %v", result.NeedFetch)
	}
}

// TestEpochTransition_ClearsEpochAdditions tests that epochAdditions is reset.
func TestEpochTransition_ClearsEpochAdditions(t *testing.T) {
	db := newTestStorage(t)
	validators, vs := newTestValidatorSet(4)

	dag := New(db, vs, nil, testSystemPod, 0, validators[0].privKey, nil,
		WithEpochLength(10),
	)
	defer dag.Close()

	// Simulate mid-epoch additions
	dag.epochAdditions = append(dag.epochAdditions, Hash{0x01}, Hash{0x02})

	if len(dag.epochAdditions) != 2 {
		t.Fatalf("expected 2 epoch additions, got %d", len(dag.epochAdditions))
	}

	dag.transitionEpoch(10)

	if len(dag.epochAdditions) != 0 {
		t.Fatalf("epochAdditions should be cleared after transition, got %d", len(dag.epochAdditions))
	}
}

// =============================================================================
// Rendezvous-based holder tests (inline BLAKE3 scoring)
// =============================================================================

// testComputeHolders mirrors aggregation.Rendezvous.ComputeHolders using inline BLAKE3.
// Score = BLAKE3(objectID || validatorPubkey), top N by score descending.
func testComputeHolders(objectID Hash, validators []Hash, replication int) []Hash {
	type scored struct {
		pubkey Hash
		score  [32]byte
	}

	if replication <= 0 {
		return nil
	}
	if replication > len(validators) {
		replication = len(validators)
	}

	items := make([]scored, len(validators))
	for i, v := range validators {
		h := blake3.New()
		h.Write(objectID[:])
		h.Write(v[:])
		var s [32]byte
		h.Sum(s[:0])
		items[i] = scored{pubkey: v, score: s}
	}

	sort.Slice(items, func(i, j int) bool {
		return bytes.Compare(items[i].score[:], items[j].score[:]) > 0
	})

	result := make([]Hash, replication)
	for i := 0; i < replication; i++ {
		result[i] = items[i].pubkey
	}
	return result
}

// testIsHolderRendezvous builds an isHolder function using real Rendezvous scoring.
func testIsHolderRendezvous(myPubkey Hash, validators []Hash) func([32]byte, uint16) bool {
	return func(objectID [32]byte, replication uint16) bool {
		if replication == 0 {
			return true // singletons always held
		}
		holders := testComputeHolders(Hash(objectID), validators, int(replication))
		for _, h := range holders {
			if h == myPubkey {
				return true
			}
		}
		return false
	}
}

// buildVertexWithProperParents creates a signed vertex with correct parent producer info.
// Unlike buildTestVertex which puts all-zero producers in VertexLinks, this helper
// looks up the actual producer from the DAG store so validateParentsQuorum passes.
func buildVertexWithProperParents(t *testing.T, dag *DAG, v testValidator, round uint64, epoch uint64) []byte {
	t.Helper()

	var parents []Hash
	if round > 0 {
		parents = dag.store.getByRound(round - 1)
	}

	builder := flatbuffers.NewBuilder(1024)

	// Build parents with correct producers
	parentOffsets := make([]flatbuffers.UOffsetT, len(parents))
	for i, p := range parents {
		var producer Hash
		if pv := dag.store.get(p); pv != nil {
			producer = extractProducer(pv)
		}

		hVec := builder.CreateByteVector(p[:])
		pVec := builder.CreateByteVector(producer[:])

		types.VertexLinkStart(builder)
		types.VertexLinkAddHash(builder, hVec)
		types.VertexLinkAddProducer(builder, pVec)
		parentOffsets[i] = types.VertexLinkEnd(builder)
	}

	types.VertexStartParentsVector(builder, len(parentOffsets))
	for i := len(parentOffsets) - 1; i >= 0; i-- {
		builder.PrependUOffsetT(parentOffsets[i])
	}
	parentsVec := builder.EndVector(len(parentOffsets))

	types.VertexStartTransactionsVector(builder, 0)
	txsVec := builder.EndVector(0)

	producerVec := builder.CreateByteVector(v.pubKey[:])

	// Build unsigned vertex for hash
	types.VertexStart(builder)
	types.VertexAddRound(builder, round)
	types.VertexAddProducer(builder, producerVec)
	types.VertexAddParents(builder, parentsVec)
	types.VertexAddTransactions(builder, txsVec)
	types.VertexAddEpoch(builder, epoch)
	vertexOff := types.VertexEnd(builder)
	builder.Finish(vertexOff)

	unsigned := builder.FinishedBytes()
	hash := hashVertex(unsigned)
	sig := ed25519.Sign(v.privKey, hash[:])

	// Rebuild with hash + signature
	builder.Reset()

	parentOffsets = make([]flatbuffers.UOffsetT, len(parents))
	for i, p := range parents {
		var producer Hash
		if pv := dag.store.get(p); pv != nil {
			producer = extractProducer(pv)
		}

		hVec := builder.CreateByteVector(p[:])
		pVec := builder.CreateByteVector(producer[:])

		types.VertexLinkStart(builder)
		types.VertexLinkAddHash(builder, hVec)
		types.VertexLinkAddProducer(builder, pVec)
		parentOffsets[i] = types.VertexLinkEnd(builder)
	}

	types.VertexStartParentsVector(builder, len(parentOffsets))
	for i := len(parentOffsets) - 1; i >= 0; i-- {
		builder.PrependUOffsetT(parentOffsets[i])
	}
	parentsVec = builder.EndVector(len(parentOffsets))

	types.VertexStartTransactionsVector(builder, 0)
	txsVec = builder.EndVector(0)

	hashVec := builder.CreateByteVector(hash[:])
	sigVec := builder.CreateByteVector(sig)
	producerVec = builder.CreateByteVector(v.pubKey[:])

	types.VertexStart(builder)
	types.VertexAddHash(builder, hashVec)
	types.VertexAddRound(builder, round)
	types.VertexAddProducer(builder, producerVec)
	types.VertexAddSignature(builder, sigVec)
	types.VertexAddParents(builder, parentsVec)
	types.VertexAddTransactions(builder, txsVec)
	types.VertexAddEpoch(builder, epoch)
	vertexOff = types.VertexEnd(builder)

	builder.Finish(vertexOff)

	return builder.FinishedBytes()
}

// TestRendezvousRedistribution_ValidatorJoin tests that when a new validator joins,
// Rendezvous hashing assigns some objects to the new validator that were previously
// held by existing validators.
func TestRendezvousRedistribution_ValidatorJoin(t *testing.T) {
	// Create 4 validators with stable pubkeys
	validators := make([]Hash, 4)
	for i := range validators {
		validators[i] = Hash{byte(i + 1)}
	}

	// Create 100 objects with replication=2
	objects := make([]Hash, 100)
	for i := range objects {
		objects[i] = Hash{0xA0, byte(i)}
	}

	// Compute holders before new validator
	holdersBefore := make(map[Hash][]Hash)
	for _, obj := range objects {
		holdersBefore[obj] = testComputeHolders(obj, validators, 2)
	}

	// Add a 5th validator
	newVal := Hash{0x05}
	validatorsAfter := append(append([]Hash{}, validators...), newVal)

	// Compute holders after new validator
	holdersAfter := make(map[Hash][]Hash)
	for _, obj := range objects {
		holdersAfter[obj] = testComputeHolders(obj, validatorsAfter, 2)
	}

	// Verify that SOME objects changed holders (redistribution happened)
	changedCount := 0
	for _, obj := range objects {
		before := holdersBefore[obj]
		after := holdersAfter[obj]

		same := len(before) == len(after)
		if same {
			for i := range before {
				if before[i] != after[i] {
					same = false
					break
				}
			}
		}
		if !same {
			changedCount++
		}
	}

	if changedCount == 0 {
		t.Fatal("no objects changed holders after adding a validator — redistribution failed")
	}

	// With 100 objects and 4→5 validators, we expect roughly 20% to move (1/5)
	if changedCount < 5 {
		t.Errorf("only %d/100 objects changed holders — expected more redistribution", changedCount)
	}

	t.Logf("redistribution: %d/100 objects changed holders (new validator)", changedCount)

	// Verify that new validator actually holds some objects
	newValHolds := 0
	for _, obj := range objects {
		holders := holdersAfter[obj]
		for _, h := range holders {
			if h == newVal {
				newValHolds++
				break
			}
		}
	}

	if newValHolds == 0 {
		t.Fatal("new validator holds zero objects — Rendezvous did not assign anything")
	}

	t.Logf("new validator holds %d/100 objects", newValHolds)
}

// TestRendezvousRedistribution_ValidatorLeave tests that when a validator departs,
// its objects are redistributed to remaining validators.
func TestRendezvousRedistribution_ValidatorLeave(t *testing.T) {
	validators := make([]Hash, 5)
	for i := range validators {
		validators[i] = Hash{byte(i + 1)}
	}

	objects := make([]Hash, 100)
	for i := range objects {
		objects[i] = Hash{0xB0, byte(i)}
	}

	// Compute holders before departure
	holdersBefore := make(map[Hash][]Hash)
	leavingVal := validators[4] // validator 5 leaves
	leavingHeld := 0
	for _, obj := range objects {
		holders := testComputeHolders(obj, validators, 2)
		holdersBefore[obj] = holders
		for _, h := range holders {
			if h == leavingVal {
				leavingHeld++
				break
			}
		}
	}

	// Remove leaving validator
	remaining := validators[:4]

	// Compute holders after departure
	holdersAfter := make(map[Hash][]Hash)
	for _, obj := range objects {
		holdersAfter[obj] = testComputeHolders(obj, remaining, 2)
	}

	// Verify: leaving validator no longer holds anything
	for _, obj := range objects {
		for _, h := range holdersAfter[obj] {
			if h == leavingVal {
				t.Fatalf("leaving validator still holds object %x", obj[:4])
			}
		}
	}

	// Verify: objects that were held by leaving validator are now held by others
	orphanedPickedUp := 0
	for _, obj := range objects {
		wasHeldByLeaving := false
		for _, h := range holdersBefore[obj] {
			if h == leavingVal {
				wasHeldByLeaving = true
				break
			}
		}

		if wasHeldByLeaving {
			// After departure, these objects should still have 2 holders (from remaining)
			after := holdersAfter[obj]
			if len(after) != 2 {
				t.Errorf("object %x: expected 2 holders after departure, got %d", obj[:4], len(after))
			}
			orphanedPickedUp++
		}
	}

	if orphanedPickedUp == 0 && leavingHeld > 0 {
		t.Fatal("no orphaned objects were picked up by remaining validators")
	}

	t.Logf("leaving validator held %d objects, all picked up by remaining validators", leavingHeld)
}

// TestFullEpochHappyPath_ObjectRedistribution is the complete integration test:
// 1. Start with 4 validators, track objects with replication
// 2. Use real Rendezvous to compute initial holders for validator[0]
// 3. Add a new validator, epoch transition
// 4. Verify: some objects dropped, some new ones need fetch
// 5. Remove a validator, epoch transition
// 6. Verify: redistribution again
func TestFullEpochHappyPath_ObjectRedistribution(t *testing.T) {
	db := newTestStorage(t)
	validators, vs := newTestValidatorSet(4)

	dag := New(db, vs, nil, testSystemPod, 0, validators[0].privKey, nil,
		WithEpochLength(10),
	)
	defer dag.Close()

	// Track 50 objects with replication=2
	objects := make([]Hash, 50)
	for i := range objects {
		objects[i] = Hash{0xC0, byte(i)}
		dag.tracker.trackObject(objects[i], 1, 2, 0)
	}

	// Epoch 1: initial snapshot
	dag.transitionEpoch(10)

	// Build initial holder set for validator[0] using real Rendezvous
	validatorPubkeys := make([]Hash, len(validators))
	for i, v := range validators {
		validatorPubkeys[i] = v.pubKey
	}

	isHolderBefore := testIsHolderRendezvous(validators[0].pubKey, validatorPubkeys)

	// Record what validator[0] holds initially
	initiallyHeld := make(map[Hash]bool)
	for _, obj := range objects {
		if isHolderBefore(obj, 2) {
			initiallyHeld[obj] = true
		}
	}

	t.Logf("validator[0] initially holds %d/50 objects", len(initiallyHeld))

	// --- Phase 2: Add a new validator ---
	newVal := newTestValidator()
	dag.validators.Add(newVal.pubKey, "", "", [48]byte{})
	dag.epochAdditions = append(dag.epochAdditions, newVal.pubKey)

	// Epoch 2: new validator included
	dag.transitionEpoch(20)

	// Updated pubkeys includes new validator
	updatedPubkeys := append(append([]Hash{}, validatorPubkeys...), newVal.pubKey)
	isHolderAfterAdd := testIsHolderRendezvous(validators[0].pubKey, updatedPubkeys)

	// Simulate local storage: validator[0] has all objects it held before
	localObjects := make(map[Hash]bool)
	for obj := range initiallyHeld {
		localObjects[obj] = true
	}

	// Scan with real Rendezvous
	resultAfterAdd := dag.ScanObjects(isHolderAfterAdd, func(id [32]byte) bool {
		return localObjects[Hash(id)]
	})

	// Some objects should have changed: dropped or need fetch
	totalChanges := len(resultAfterAdd.NeedFetch) + len(resultAfterAdd.CanDrop)
	t.Logf("after validator join: needFetch=%d, canDrop=%d",
		len(resultAfterAdd.NeedFetch), len(resultAfterAdd.CanDrop))

	// Verify NeedFetch objects are genuinely objects we should hold but don't
	for _, id := range resultAfterAdd.NeedFetch {
		if localObjects[id] {
			t.Errorf("NeedFetch contains object %x that we already have locally", id[:4])
		}
		if !isHolderAfterAdd(id, 2) {
			t.Errorf("NeedFetch contains object %x that we're NOT holder for", id[:4])
		}
	}

	// Verify CanDrop objects are genuinely objects we have but shouldn't hold
	for _, id := range resultAfterAdd.CanDrop {
		if !localObjects[id] {
			t.Errorf("CanDrop contains object %x that we DON'T have locally", id[:4])
		}
		if isHolderAfterAdd(id, 2) {
			t.Errorf("CanDrop contains object %x that we ARE still holder for", id[:4])
		}
	}

	// --- Phase 3: Remove a validator ---
	dag.pendingRemovals[validators[3].pubKey] = true

	// Epoch 3: validator removed
	dag.transitionEpoch(30)

	// Update local state: apply fetches, remove drops
	for _, id := range resultAfterAdd.NeedFetch {
		localObjects[id] = true
	}
	for _, id := range resultAfterAdd.CanDrop {
		delete(localObjects, id)
	}

	// Updated pubkeys after removal (validator[3] gone)
	finalPubkeys := make([]Hash, 0)
	for i := 0; i < 3; i++ {
		finalPubkeys = append(finalPubkeys, validatorPubkeys[i])
	}
	finalPubkeys = append(finalPubkeys, newVal.pubKey)

	isHolderAfterRemove := testIsHolderRendezvous(validators[0].pubKey, finalPubkeys)

	resultAfterRemove := dag.ScanObjects(isHolderAfterRemove, func(id [32]byte) bool {
		return localObjects[Hash(id)]
	})

	t.Logf("after validator leave: needFetch=%d, canDrop=%d",
		len(resultAfterRemove.NeedFetch), len(resultAfterRemove.CanDrop))

	// Verify correctness of scan results after removal
	for _, id := range resultAfterRemove.NeedFetch {
		if localObjects[id] {
			t.Errorf("NeedFetch contains object %x already local", id[:4])
		}
		if !isHolderAfterRemove(id, 2) {
			t.Errorf("NeedFetch contains object %x not holder", id[:4])
		}
	}

	for _, id := range resultAfterRemove.CanDrop {
		if !localObjects[id] {
			t.Errorf("CanDrop contains object %x not local", id[:4])
		}
		if isHolderAfterRemove(id, 2) {
			t.Errorf("CanDrop contains object %x still holder", id[:4])
		}
	}

	// After 3 epochs: verify objects are tracked and epochs are correct
	if dag.Epoch() != 3 {
		t.Fatalf("expected epoch 3, got %d", dag.Epoch())
	}

	if totalChanges == 0 {
		t.Log("WARNING: no redistribution after adding validator — this is possible but unlikely with 50 objects")
	}
}

// TestEpochTransition_ViaCommitPath tests that the epoch transition fires
// through the real commit path (checkCommits → isEpochBoundary → transitionEpoch)
// rather than calling transitionEpoch directly.
func TestEpochTransition_ViaCommitPath(t *testing.T) {
	db := newTestStorage(t)
	validators, vs := newTestValidatorSet(1)
	mock := &mockBroadcaster{}

	// Use listener mode to prevent the DAG from auto-producing vertices.
	// This gives us full control over which round vertices exist.
	dag := New(db, vs, mock, testSystemPod, 0, validators[0].privKey, nil,
		WithEpochLength(5),
		WithListenerMode(),
	)
	defer dag.Close()

	// Track the epoch via callback
	var lastEpoch uint64
	dag.OnEpochTransition(func(epoch uint64) {
		lastEpoch = epoch
	})

	dag.InitEpochHolders()

	// With 1 validator (quorum=1), round R is committed when there's
	// a validator vertex at round R+2.
	// Epoch boundary at round 5 → needs commit of round 5 → needs vertex at round 7.
	for round := uint64(0); round <= 7; round++ {
		data := buildVertexWithProperParents(t, dag, validators[0], round, 0)
		added := dag.AddVertex(data)
		if !added {
			t.Fatalf("AddVertex failed at round %d", round)
		}
	}

	// Manually trigger checkCommits since the timing-based loop may not have fired.
	dag.checkCommits()

	if lastEpoch == 0 {
		t.Fatal("epoch transition callback was not fired via commit path")
	}

	if dag.Epoch() < 1 {
		t.Fatalf("expected epoch >= 1, got %d", dag.Epoch())
	}

	t.Logf("epoch transition fired via commit path at epoch %d (last committed: %d)",
		lastEpoch, dag.LastCommittedRound())
}
