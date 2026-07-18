package consensus

import "testing"

// TestDeregistrationCreditsRewardCoin_PreservesSupplyIdentity drives the
// epoch-boundary removal path for a validator whose bond is released: the
// released self-stake must land in the validator's reward coin so coins_total
// grows by exactly the bond that left total_bonded, keeping the supply identity
// coins_total + total_bonded + deposits + fees_in_flight == total_supply exact.
func TestDeregistrationCreditsRewardCoin_PreservesSupplyIdentity(t *testing.T) {
	const (
		startBalance = uint64(1000)
		selfStake    = uint64(5000)
	)

	dag, store, sender, coinID := bondTestDAG(t, startBalance)
	defer dag.Close()

	// The departing validator carries a bonded self-stake and designates its coin
	// as the reward coin (the target the credit must land on).
	dag.validators.SetSelfStake(sender, selfStake)
	dag.validators.SetRewardCoin(sender, coinID)

	// Supply identity holds before the boundary: coins_total (the coin) plus
	// total_bonded (the self-stake) equals total_supply, with no deposits or fees.
	store.SetCoinsTotal(startBalance)
	store.SetTotalSupply(startBalance + selfStake)
	if got := dag.totalBonded(); got != selfStake {
		t.Fatalf("totalBonded before removal = %d, want %d", got, selfStake)
	}

	dag.pendingRemovals[sender] = true
	removed := dag.applyPendingRemovals()

	if len(removed) != 1 {
		t.Fatalf("removed count = %d, want 1", len(removed))
	}
	if dag.validators.Contains(sender) {
		t.Fatal("validator should be gone after removal")
	}

	if got := store.CoinsTotal(); got != startBalance+selfStake {
		t.Fatalf("coins_total after removal = %d, want %d (grew by the released self-stake)",
			got, startBalance+selfStake)
	}
	if got := coinBalance(t, store, coinID); got != startBalance+selfStake {
		t.Fatalf("reward coin balance after removal = %d, want %d", got, startBalance+selfStake)
	}

	requireSupplyIdentity(t, dag, store)
}

// TestDeregistrationNoRewardCoin_KeepsValidatorBonded checks the no-reward-coin
// rule: a departing validator with a bonded self-stake but no reward coin cannot
// re-home its principal, so the removal is refused this boundary. The validator
// stays active and bonded, the deregistration stays pending for a later boundary,
// and the supply identity stays exact (no bond is dropped uncredited).
func TestDeregistrationNoRewardCoin_KeepsValidatorBonded(t *testing.T) {
	const (
		startBalance = uint64(1000)
		selfStake    = uint64(5000)
	)

	dag, store, sender, _ := bondTestDAG(t, startBalance)
	defer dag.Close()

	dag.validators.SetSelfStake(sender, selfStake)
	// No SetRewardCoin: the validator designates no reward coin.

	store.SetCoinsTotal(startBalance)
	store.SetTotalSupply(startBalance + selfStake)

	dag.pendingRemovals[sender] = true
	removed := dag.applyPendingRemovals()

	if len(removed) != 0 {
		t.Fatalf("removed count = %d, want 0 (removal refused with no reward coin)", len(removed))
	}
	if !dag.validators.Contains(sender) {
		t.Fatal("validator should stay active when its bond cannot be re-homed")
	}
	if !dag.pendingRemovals[sender] {
		t.Fatal("deregistration should stay pending for a later boundary")
	}
	if got := store.CoinsTotal(); got != startBalance {
		t.Fatalf("coins_total = %d, want %d (unchanged: nothing credited)", got, startBalance)
	}

	requireSupplyIdentity(t, dag, store)
}

// Fixture amounts for the deregistration-with-delegator tests: a validator with a
// bonded self-stake and a designated reward coin, plus one delegator whose coin
// funds a delegation into that validator.
const (
	delegatedValidatorBalance = uint64(1000) // delegatedValidatorBalance is the validator reward coin's starting balance
	delegatedSelfStake        = uint64(5000) // delegatedSelfStake is the validator's bonded self-stake
	delegatorCoinBalance      = uint64(2000) // delegatorCoinBalance is the delegator coin's starting balance
	delegatedAmount           = uint64(800)  // delegatedAmount is the stake delegated (and later withdrawn)
)

// delegatedRemovalDAG builds a bond-test DAG whose single validator carries a
// bonded self-stake, a designated reward coin, and one outstanding delegation from
// a distinct delegator opened through the real delegate path. The supply counters
// are seeded so the identity coins_total + total_bonded == total_supply holds at
// baseline (no deposits or fees in these unit tests). It returns the DAG, the
// store, the validator pubkey, the delegator pubkey, and the delegator's coin —
// the fixture both deregistration-with-delegator tests share.
func delegatedRemovalDAG(t *testing.T) (*DAG, *mockCoinStore, [32]byte, [32]byte, [32]byte) {
	t.Helper()

	dag, store, validator, valCoin := bondTestDAG(t, delegatedValidatorBalance)

	dag.validators.SetSelfStake(validator, delegatedSelfStake)
	dag.validators.SetRewardCoin(validator, valCoin)

	delegator := [32]byte{0xD1}
	delCoin := [32]byte{0xC1}
	store.SetObject(buildTestCoinObject(delCoin, delegatorCoinBalance, delegator, 0))
	if !dag.handleDelegate(buildDelegateTx(t, delegator, delCoin, validator, "delegate", delegatedAmount, true)) {
		t.Fatal("delegation to the validator should be applied")
	}

	// Baseline coins_total is the two coins' balances after the delegate debit;
	// total_bonded is self plus delegated; total_supply is their sum.
	coinsBaseline := delegatedValidatorBalance + (delegatorCoinBalance - delegatedAmount)
	store.SetCoinsTotal(coinsBaseline)
	store.SetTotalSupply(coinsBaseline + delegatedSelfStake + delegatedAmount)

	return dag, store, validator, delegator, delCoin
}

// TestDeregistrationWithDelegators_PreservesSupplyIdentity drives the epoch-boundary
// removal path for a validator that still carries outstanding delegated stake. The
// delegated total is bonded capital its delegators own, counted in total_bonded;
// removing the validator drops it from the active set while only the self-stake is
// recredited, so the supply identity coins_total + total_bonded + deposits +
// fees_in_flight == total_supply would turn deflationary by exactly the delegated
// amount. The removal must be deferred while any delegation is outstanding, so the
// delegated stake stays bonded and the identity stays exact.
func TestDeregistrationWithDelegators_PreservesSupplyIdentity(t *testing.T) {
	dag, store, validator, _, _ := delegatedRemovalDAG(t)
	defer dag.Close()

	if got := dag.totalBonded(); got != delegatedSelfStake+delegatedAmount {
		t.Fatalf("totalBonded before removal = %d, want %d", got, delegatedSelfStake+delegatedAmount)
	}

	dag.pendingRemovals[validator] = true
	removed := dag.applyPendingRemovals()

	// The identity must hold immediately after the boundary, with the delegation
	// still in place: the delegated stake stays bonded rather than vanishing.
	requireSupplyIdentity(t, dag, store)

	// It holds because the removal is deferred while delegated stake is bonded: the
	// validator stays active and its deregistration stays pending for a later boundary.
	if len(removed) != 0 {
		t.Fatalf("removed count = %d, want 0 (removal deferred while delegated stake is bonded)", len(removed))
	}
	if !dag.validators.Contains(validator) {
		t.Fatal("validator must stay active while it carries delegated stake")
	}
	if !dag.pendingRemovals[validator] {
		t.Fatal("deregistration must stay pending for a later boundary")
	}
}

// TestDeregistrationCompletesAfterUndelegate checks the deferred departure completes
// once the delegation is withdrawn. Even while the deregistration is pending the
// validator stays in the set, so undelegate takes the normal bonded→coins path
// (credit the coin, delete the position, lower DelegatedTotal). With no delegated
// stake left, the next boundary removes the validator and returns its self-stake,
// with the supply identity exact throughout.
func TestDeregistrationCompletesAfterUndelegate(t *testing.T) {
	dag, store, validator, delegator, delCoin := delegatedRemovalDAG(t)
	defer dag.Close()

	dag.pendingRemovals[validator] = true
	if removed := dag.applyPendingRemovals(); len(removed) != 0 {
		t.Fatalf("first boundary removed %d, want 0 (deferred while delegated)", len(removed))
	}

	// The delegator withdraws through the normal path while the deregistration is
	// pending: undelegate credits the coin, deletes the position, and lowers
	// DelegatedTotal to zero, keeping the identity exact (bonded→coins).
	if !dag.handleUndelegate(buildDelegateTx(t, delegator, delCoin, validator, "undelegate", 0, false)) {
		t.Fatal("undelegate should apply while the deregistration is pending")
	}
	if got := dag.validators.Get(validator).DelegatedTotal; got != 0 {
		t.Fatalf("DelegatedTotal after undelegate = %d, want 0", got)
	}
	requireSupplyIdentity(t, dag, store)

	// With no delegated stake left, the next boundary completes the departure and
	// returns the self-stake to the reward coin.
	removed := dag.applyPendingRemovals()
	if len(removed) != 1 {
		t.Fatalf("second boundary removed %d, want 1 (departure completes once delegations clear)", len(removed))
	}
	if dag.validators.Contains(validator) {
		t.Fatal("validator should be gone once its delegations are withdrawn")
	}
	requireSupplyIdentity(t, dag, store)
}

// requireSupplyIdentity asserts the protocol supply identity holds exactly for a
// DAG whose only nonzero terms are coins_total and total_bonded (no deposits, no
// fees in flight in these unit tests).
func requireSupplyIdentity(t *testing.T, dag *DAG, store *mockCoinStore) {
	t.Helper()

	sum := store.CoinsTotal() + dag.totalBonded()
	if sum != store.TotalSupply() {
		t.Fatalf("supply identity broken: coins_total(%d)+total_bonded(%d)=%d != total_supply(%d)",
			store.CoinsTotal(), dag.totalBonded(), sum, store.TotalSupply())
	}
}
