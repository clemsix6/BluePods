package consensus

import (
	"encoding/binary"
	"testing"

	flatbuffers "github.com/google/flatbuffers/go"

	"BluePods/internal/types"
)

// buildStakeTx builds a system-pod bond/unbond transaction: sender, the system
// pod, the function name, a Borsh u64 amount in args, and one mutable_ref
// pointing at the staked coin.
func buildStakeTx(t *testing.T, sender, coinID [32]byte, funcName string, amount uint64) *types.Transaction {
	t.Helper()

	builder := flatbuffers.NewBuilder(512)

	mutVec := buildObjectRefVector(builder, []objectRef{{id: coinID, version: 1}}, true)

	args := make([]byte, 8)
	binary.LittleEndian.PutUint64(args, amount)

	hashVec := builder.CreateByteVector(make([]byte, 32))
	senderVec := builder.CreateByteVector(sender[:])
	podVec := builder.CreateByteVector(testSystemPod[:])
	funcNameOff := builder.CreateString(funcName)
	argsVec := builder.CreateByteVector(args)

	types.TransactionStart(builder)
	types.TransactionAddHash(builder, hashVec)
	types.TransactionAddSender(builder, senderVec)
	types.TransactionAddPod(builder, podVec)
	types.TransactionAddFunctionName(builder, funcNameOff)
	types.TransactionAddArgs(builder, argsVec)
	types.TransactionAddMutableRefs(builder, mutVec)
	txOff := types.TransactionEnd(builder)

	builder.Finish(txOff)
	return types.GetRootAsTransaction(builder.FinishedBytes(), 0)
}

// coinBalance reads the balance of a coin in the store, failing if absent.
func coinBalance(t *testing.T, store CoinStore, id [32]byte) uint64 {
	t.Helper()

	data := store.GetObject(id)
	if data == nil {
		t.Fatalf("coin %x not found", id[:4])
	}
	bal, err := readCoinBalance(data)
	if err != nil {
		t.Fatalf("read balance: %v", err)
	}
	return bal
}

// bondTestDAG builds a DAG wired to a mock coin store with one registered
// validator owning a funded coin, ready for bond/unbond handler tests.
func bondTestDAG(t *testing.T, balance uint64, opts ...Option) (*DAG, *mockCoinStore, [32]byte, [32]byte) {
	t.Helper()

	db := newTestStorage(t)
	validators, vs := newTestValidatorSet(1)
	sender := validators[0].pubKey
	coinID := [32]byte{0xC0}

	store := newMockCoinStore()
	store.SetObject(buildTestCoinObject(coinID, balance, sender, 0))

	dag := New(db, vs, nil, testSystemPod, 0, validators[0].privKey, nil, opts...)
	params := DefaultFeeParams()
	dag.SetFeeSystem(store, &params, nil)

	return dag, store, sender, coinID
}

// TestHandleBond_DebitsCoinAndRaisesStake checks that a valid bond debits the
// coin and increases self-stake.
func TestHandleBond_DebitsCoinAndRaisesStake(t *testing.T) {
	dag, store, sender, coinID := bondTestDAG(t, 1000)
	defer dag.Close()

	tx := buildStakeTx(t, sender, coinID, "bond", 300)
	if !dag.handleBond(tx) {
		t.Fatal("bond should be applied")
	}

	if got := coinBalance(t, store, coinID); got != 700 {
		t.Fatalf("coin balance after bond = %d, want 700", got)
	}
	if got := dag.validators.Get(sender).SelfStake; got != 300 {
		t.Fatalf("self-stake after bond = %d, want 300", got)
	}
}

// TestHandleBond_UnderFundedRejectedNoZeroing checks that an under-funded bond
// is rejected WITHOUT zeroing the coin (strict debit, not deductCoinFee).
func TestHandleBond_UnderFundedRejectedNoZeroing(t *testing.T) {
	dag, store, sender, coinID := bondTestDAG(t, 100)
	defer dag.Close()

	tx := buildStakeTx(t, sender, coinID, "bond", 300)
	if dag.handleBond(tx) {
		t.Fatal("under-funded bond should be rejected")
	}

	if got := coinBalance(t, store, coinID); got != 100 {
		t.Fatalf("coin balance must be untouched on rejected bond, got %d, want 100", got)
	}
	if got := dag.validators.Get(sender).SelfStake; got != 0 {
		t.Fatalf("self-stake must stay 0 on rejected bond, got %d", got)
	}
}

// TestHandleBond_BelowMinStakeRejected checks a bond leaving self-stake under
// minStake is rejected.
func TestHandleBond_BelowMinStakeRejected(t *testing.T) {
	dag, store, sender, coinID := bondTestDAG(t, 1000, WithMinStake(500))
	defer dag.Close()

	tx := buildStakeTx(t, sender, coinID, "bond", 300) // 300 < minStake 500
	if dag.handleBond(tx) {
		t.Fatal("bond below minStake should be rejected")
	}
	if got := coinBalance(t, store, coinID); got != 1000 {
		t.Fatalf("coin must be untouched on minStake rejection, got %d", got)
	}

	// A bond meeting minStake is accepted.
	tx2 := buildStakeTx(t, sender, coinID, "bond", 500)
	if !dag.handleBond(tx2) {
		t.Fatal("bond meeting minStake should be accepted")
	}
	if got := dag.validators.Get(sender).SelfStake; got != 500 {
		t.Fatalf("self-stake = %d, want 500", got)
	}
}

// TestHandleUnbond_LowersStakeAndCreditsCoin checks unbond reduces self-stake
// and credits the coin back.
func TestHandleUnbond_LowersStakeAndCreditsCoin(t *testing.T) {
	dag, store, sender, coinID := bondTestDAG(t, 1000)
	defer dag.Close()

	if !dag.handleBond(buildStakeTx(t, sender, coinID, "bond", 600)) {
		t.Fatal("bond should be applied")
	}
	// coin: 400, stake: 600

	if !dag.handleUnbond(buildStakeTx(t, sender, coinID, "unbond", 200)) {
		t.Fatal("unbond should be applied")
	}

	if got := dag.validators.Get(sender).SelfStake; got != 400 {
		t.Fatalf("self-stake after unbond = %d, want 400", got)
	}
	if got := coinBalance(t, store, coinID); got != 600 {
		t.Fatalf("coin balance after unbond = %d, want 600", got)
	}
}

// TestHandleUnbond_MinStakeFloor checks the minimum-stake floor on unbond: an
// unbond that would leave a POSITIVE self-stake below minStake is rejected, while
// a full exit down to exactly 0 is allowed.
func TestHandleUnbond_MinStakeFloor(t *testing.T) {
	dag, store, sender, coinID := bondTestDAG(t, 1000, WithMinStake(500))
	defer dag.Close()

	if !dag.handleBond(buildStakeTx(t, sender, coinID, "bond", 800)) {
		t.Fatal("bond should be applied")
	}
	// coin: 200, stake: 800

	// Unbond 500 → remaining 300 (positive, below minStake 500) → rejected.
	if dag.handleUnbond(buildStakeTx(t, sender, coinID, "unbond", 500)) {
		t.Fatal("unbond leaving sub-minimum self-stake should be rejected")
	}
	if got := dag.validators.Get(sender).SelfStake; got != 800 {
		t.Fatalf("self-stake must be untouched on rejected unbond, got %d, want 800", got)
	}
	if got := coinBalance(t, store, coinID); got != 200 {
		t.Fatalf("coin must be untouched on rejected unbond, got %d, want 200", got)
	}

	// Unbond 300 → remaining 500 (== minStake) → allowed.
	if !dag.handleUnbond(buildStakeTx(t, sender, coinID, "unbond", 300)) {
		t.Fatal("unbond leaving exactly minStake should be allowed")
	}
	if got := dag.validators.Get(sender).SelfStake; got != 500 {
		t.Fatalf("self-stake = %d, want 500", got)
	}

	// Full exit: unbond the remaining 500 → 0 → allowed.
	if !dag.handleUnbond(buildStakeTx(t, sender, coinID, "unbond", 500)) {
		t.Fatal("full exit to zero self-stake should be allowed")
	}
	if got := dag.validators.Get(sender).SelfStake; got != 0 {
		t.Fatalf("self-stake after full exit = %d, want 0", got)
	}
}

// TestJailedValidatorCarriesZeroWeight confirms jailing zeroes effective stake.
func TestJailedValidatorCarriesZeroWeight(t *testing.T) {
	dag, _, sender, coinID := bondTestDAG(t, 1000)
	defer dag.Close()

	dag.handleBond(buildStakeTx(t, sender, coinID, "bond", 300))
	dag.validators.Jail(sender)

	if got := EffectiveStake(dag.validators.Get(sender)); got != 0 {
		t.Fatalf("jailed validator effective stake = %d, want 0", got)
	}
	if got := dag.totalBonded(); got != 0 {
		t.Fatalf("totalBonded with only-jailed validator = %d, want 0", got)
	}
}
