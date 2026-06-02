package consensus

import "testing"

// TestCappedWeight checks the per-validator voting cap: a validator above the cap
// is clamped, one below is unchanged, and the equal-share floor keeps a small set
// at a reachable 2/3 quorum (including the total%setSize truncation edge).
func TestCappedWeight(t *testing.T) {
	// Cap at 10% (100 per-mille) of a large total.
	// total=1000, capMille=100 → fraction ceiling 100; equal share 1000/4=250.
	// Floor wins (250 > 100), so the ceiling is 250.
	if got := cappedWeight(400, 1000, 100, 4); got != 250 {
		t.Fatalf("above cap: cappedWeight = %d, want 250 (equal-share floor)", got)
	}

	if got := cappedWeight(200, 1000, 100, 4); got != 200 {
		t.Fatalf("below cap: cappedWeight = %d, want 200 (unchanged)", got)
	}

	// Large set where the fraction ceiling exceeds the equal share.
	// total=10000, capMille=100 → fraction 1000; equal share 10000/100=100.
	// Fraction wins (1000 > 100), so a validator at 5000 clamps to 1000.
	if got := cappedWeight(5000, 10000, 100, 100); got != 1000 {
		t.Fatalf("fraction cap: cappedWeight = %d, want 1000", got)
	}

	// Truncation edge: total % setSize != 0. total=100, setSize=3 → equal share 33.
	// capMille=100 → fraction 10; floor wins. A validator at 50 clamps to 33.
	if got := cappedWeight(50, 100, 100, 3); got != 33 {
		t.Fatalf("truncation edge: cappedWeight = %d, want 33", got)
	}

	// Small-set reachability: 3 validators, 10% cap. Without the equal-share floor
	// the per-validator ceiling would be 10% of total and three of them could never
	// sum to 2/3. With the floor (total/3 each), three full shares reach the total.
	const total = 300
	var sum uint64
	for i := 0; i < 3; i++ {
		sum += cappedWeight(100, total, 100, 3)
	}
	if !quorumReachedTestHelper(sum, total) {
		t.Fatalf("small-set quorum unreachable: cappedSum=%d total=%d", sum, total)
	}

	// Degenerate guards: setSize<=0 or total==0 returns effective unchanged.
	if got := cappedWeight(42, 1000, 100, 0); got != 42 {
		t.Fatalf("setSize<=0: cappedWeight = %d, want 42", got)
	}
	if got := cappedWeight(42, 0, 100, 4); got != 42 {
		t.Fatalf("total==0: cappedWeight = %d, want 42", got)
	}
}

// quorumReachedTestHelper mirrors the 2/3 threshold for use before quorumReached
// itself is introduced (Task 5.2). It keeps TestCappedWeight self-contained.
func quorumReachedTestHelper(cappedSum, total uint64) bool {
	return 3*cappedSum >= 2*total
}

// TestEffectiveStake checks that effective stake is self plus delegated, and
// that a jailed or nil validator contributes zero.
func TestEffectiveStake(t *testing.T) {
	if got := EffectiveStake(&ValidatorInfo{SelfStake: 100, DelegatedTotal: 50}); got != 150 {
		t.Fatalf("EffectiveStake = %d, want 150", got)
	}

	if got := EffectiveStake(&ValidatorInfo{SelfStake: 100, DelegatedTotal: 50, Jailed: true}); got != 0 {
		t.Fatalf("jailed EffectiveStake = %d, want 0", got)
	}

	if got := EffectiveStake(nil); got != 0 {
		t.Fatalf("nil EffectiveStake = %d, want 0", got)
	}
}

// TestTotalBonded sums effective stake over the active set, excluding jailed.
func TestTotalBonded(t *testing.T) {
	db := newTestStorage(t)
	validators, vs := newTestValidatorSet(3)

	dag := New(db, vs, nil, testSystemPod, 0, validators[0].privKey, nil)
	defer dag.Close()

	dag.validators.SetSelfStake(validators[0].pubKey, 100)
	dag.validators.AddDelegated(validators[0].pubKey, 50)
	dag.validators.SetSelfStake(validators[1].pubKey, 200)
	dag.validators.SetSelfStake(validators[2].pubKey, 300)
	dag.validators.Jail(validators[2].pubKey) // jailed → excluded

	// 150 (v0) + 200 (v1) + 0 (v2 jailed) = 350
	if got := dag.totalBonded(); got != 350 {
		t.Fatalf("totalBonded = %d, want 350", got)
	}
}
