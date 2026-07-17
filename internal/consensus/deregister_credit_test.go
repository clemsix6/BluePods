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
