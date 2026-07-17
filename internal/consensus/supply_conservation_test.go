package consensus

import (
	"testing"

	"BluePods/internal/types"
)

// TestPartialFeeCoveragePooled verifies that when a gas coin cannot fully cover
// a transaction's fee, the balance actually taken from the coin is pooled into
// the returned FeeSplit's Epoch share rather than discarded. The taken amount
// left the coin, so it must enter the epoch pool like any consumed fee, or
// total_supply would overstate the coins actually backing it.
func TestPartialFeeCoveragePooled(t *testing.T) {
	db := newTestStorage(t)
	validators, vs := newTestValidatorSet(3)
	mock := &mockBroadcaster{}

	dag := New(db, vs, mock, testSystemPod, 0, validators[0].privKey, nil)
	defer dag.Close()
	disableTxAuth(dag)

	coinStore := newMockCoinStore()
	params := DefaultFeeParams()
	dag.SetFeeSystem(coinStore, &params, nil)

	sender := Hash{0x01}
	gasCoinID := Hash{0xCC}

	const balance = 10 // far below the fee a singleton-creating tx incurs
	coinStore.SetObject(buildTestCoinObject(gasCoinID, balance, sender, 0))

	atxBytes := buildFeeTestATX(t, sender, gasCoinID, 500, []uint16{0})
	atx := types.GetRootAsAttestedTransaction(atxBytes, 0)
	tx := atx.Transaction(nil)

	split, _, proceed := dag.deductFees(tx, atx, validators[0].pubKey)
	if proceed {
		t.Fatal("expected proceed=false: the fee is not fully covered")
	}

	got, err := readCoinBalance(coinStore.GetObject(gasCoinID))
	if err != nil {
		t.Fatalf("read gas coin balance: %v", err)
	}
	if got != 0 {
		t.Fatalf("coin balance after partial deduction = %d, want 0", got)
	}

	if split.Epoch != balance {
		t.Errorf("split.Epoch = %d, want %d (the whole drained balance pooled)", split.Epoch, balance)
	}
}

// TestEpochTransitionCarriesUncreditableRemainder verifies that when the epoch
// reward pool cannot be credited anywhere (the sole, top-weight validator
// designates no reward coin), transitionEpoch carries the uncreditable amount
// forward into the next epoch's pool instead of zeroing it at clearEpochState:
// a reward that cannot be delivered must stay accounted, never vanish.
func TestEpochTransitionCarriesUncreditableRemainder(t *testing.T) {
	db := newTestStorage(t)
	validators, vs := newTestValidatorSet(1)
	pk := validators[0].pubKey

	store := newMockCoinStore()
	store.SetTotalSupply(1_000_000)

	dag := New(db, vs, nil, testSystemPod, 0, validators[0].privKey, nil,
		WithEpochLength(10),
	)
	params := DefaultFeeParams()
	dag.SetFeeSystem(store, &params, nil)
	defer dag.Close()

	// Nonzero reward weight, but NO reward coin designated: the validator's
	// liquid share and the remainder both land on an undesignated recipient.
	dag.validators.SetSelfStake(pk, 100)
	dag.epochRoundsProduced[pk] = 1
	dag.epochFees = 500

	dag.transitionEpoch(10)

	if dag.epochFees != 500 {
		t.Errorf("epochFees after boundary = %d, want 500 (uncreditable pool carried over, not lost)", dag.epochFees)
	}
}
