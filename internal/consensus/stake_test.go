package consensus

import "testing"

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
