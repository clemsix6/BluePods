package consensus

import (
	"testing"

	"BluePods/internal/genesis"
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

// TestDeclaredOpFeesConserveSupply verifies that a declared-operation
// transaction moves value without creating or destroying any: everything the
// gas coin loses — the compute floor, the flat operation fee and the rent —
// enters the epoch pool, and nothing is withheld as a deposit. Rent is a
// consumed fee, so a domain lease must never leave a locked balance behind that
// no object accounts for.
func TestDeclaredOpFeesConserveSupply(t *testing.T) {
	env := newOpsFeeEnv(t)
	defer env.dag.Close()

	sender := Hash{0x01}
	gasCoin := Hash{0xCC}
	const startBalance = 1_000_000
	env.coins.SetObject(buildTestCoinObject(gasCoin, startBalance, sender, 0))

	obj := Hash{0x11}
	ops := []genesis.DeclaredOp{
		registerOp("alpha", obj, 7),
		renewOp("alpha", 3),
		{Kind: deleteOp, ObjectID: obj[:]},
	}

	atxBytes := env.atx(t, opsFeeTx{sender: sender, gasCoin: gasCoin, maxGas: env.params.MinGas, ops: ops})
	atx := types.GetRootAsAttestedTransaction(atxBytes, 0)
	tx := atx.Transaction(nil)

	split, storage, proceed := env.dag.deductFees(tx, atx, env.producer.pubKey)
	if !proceed {
		t.Fatal("deductFees must proceed for a funded gas coin")
	}

	balance, err := readCoinBalance(env.coins.GetObject(gasCoin))
	if err != nil {
		t.Fatalf("read gas coin: %v", err)
	}

	debited := uint64(startBalance) - balance
	if pooled := split.Epoch + split.Burned + storage; pooled != debited {
		t.Errorf("debited %d but accounted %d (epoch %d + burned %d + withheld %d)",
			debited, pooled, split.Epoch, split.Burned, storage)
	}
	if storage != 0 {
		t.Errorf("withheld storage = %d, want 0: declared operations create no object", storage)
	}

	want := env.params.MinGas*env.params.GasPrice + 10*env.params.RentalRatePerEpoch + env.params.DeleteFee
	if debited != want {
		t.Errorf("debited %d, want %d (floor + 7+3 epochs of rent + delete fee)", debited, want)
	}
}

// TestEpochTransitionCarriesUndistributablePool verifies that when the epoch
// reward pool has no reward weight to land on (no validator produced a round this
// epoch, so total reward weight is zero), transitionEpoch carries the pool
// forward into the next epoch's pool instead of zeroing it at clearEpochState: a
// reward that cannot be delivered must stay accounted, never vanish. A coinless
// validator WITH weight is no longer this case — its share now compounds into its
// self-stake (see creditValidatorReward); the genuine carry is a pool with no
// weight to distribute at all.
func TestEpochTransitionCarriesUndistributablePool(t *testing.T) {
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

	// A funded pool but zero reward weight: the settled epoch has no committed
	// production (epochRoundsProduced unset), so totalRewardWeight is 0 and the pool has
	// no weight to distribute against. Settlement is deferred one boundary, so drive it
	// from epoch 1 with epoch 0's pool pending.
	dag.setCurrentEpoch(1)
	dag.validators.SetSelfStake(pk, 100)
	dag.epochFees[0] = 500

	dag.transitionEpoch(20)

	if dag.totalEpochFees() != 500 {
		t.Errorf("in-flight pool after boundary = %d, want 500 (undistributable pool carried over, not lost)", dag.totalEpochFees())
	}
}
