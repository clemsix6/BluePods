package consensus

import (
	"encoding/binary"
	"testing"

	flatbuffers "github.com/google/flatbuffers/go"

	"BluePods/internal/types"
)

// TestDelegationID checks the position ID is deterministic and distinct per pair.
func TestDelegationID(t *testing.T) {
	delegator := [32]byte{0x01}
	validator := [32]byte{0x02}

	a := DelegationID(delegator, validator)
	b := DelegationID(delegator, validator)
	if a != b {
		t.Fatalf("DelegationID is not deterministic: %x != %x", a[:4], b[:4])
	}

	// Distinct per delegator.
	other := DelegationID([32]byte{0x09}, validator)
	if a == other {
		t.Fatal("DelegationID must differ for a different delegator")
	}

	// Distinct per validator.
	otherVal := DelegationID(delegator, [32]byte{0x09})
	if a == otherVal {
		t.Fatal("DelegationID must differ for a different validator")
	}

	// Swapping the pair must not collide.
	if a == DelegationID(validator, delegator) {
		t.Fatal("DelegationID must be order-sensitive between delegator and validator")
	}
}

// TestDelegationContentRoundTrip checks the content codec round-trips.
func TestDelegationContentRoundTrip(t *testing.T) {
	validator := [32]byte{0xAB, 0xCD}
	amount := uint64(123456789)

	content := encodeDelegationContent(validator, amount)

	gotValidator, gotAmount, ok := decodeDelegationContent(content)
	if !ok {
		t.Fatal("decodeDelegationContent failed on valid content")
	}
	if gotValidator != validator {
		t.Fatalf("validator round-trip = %x, want %x", gotValidator[:4], validator[:4])
	}
	if gotAmount != amount {
		t.Fatalf("amount round-trip = %d, want %d", gotAmount, amount)
	}
}

// TestDecodeDelegationContentRejectsShort checks malformed content is rejected.
func TestDecodeDelegationContentRejectsShort(t *testing.T) {
	if _, _, ok := decodeDelegationContent(make([]byte, 39)); ok {
		t.Fatal("content shorter than 40 bytes must be rejected")
	}
}

// TestBuildDelegationObject checks the position object carries the delegator as
// owner, the deterministic ID, and the decodable content.
func TestBuildDelegationObject(t *testing.T) {
	delegator := [32]byte{0x11}
	validator := [32]byte{0x22}
	amount := uint64(500)

	data := buildDelegationObject(delegator, validator, amount)

	obj := types.GetRootAsObject(data, 0)

	wantID := DelegationID(delegator, validator)
	if got := obj.IdBytes(); string(got) != string(wantID[:]) {
		t.Fatalf("object ID = %x, want %x", got[:4], wantID[:4])
	}
	if got := obj.OwnerBytes(); string(got) != string(delegator[:]) {
		t.Fatalf("owner = %x, want delegator %x", got[:4], delegator[:4])
	}

	gotValidator, gotAmount, ok := decodeDelegationContent(obj.ContentBytes())
	if !ok || gotValidator != validator || gotAmount != amount {
		t.Fatalf("content decode = (%x, %d, %v), want (%x, %d, true)", gotValidator[:4], gotAmount, ok, validator[:4], amount)
	}
}

// buildDelegateTx builds a system-pod delegate/undelegate transaction: sender,
// the system pod, the function name, Borsh args (validator || amount for
// delegate; validator only for undelegate), and one mutable_ref to the coin.
func buildDelegateTx(t *testing.T, sender, coinID, validator [32]byte, funcName string, amount uint64, withAmount bool) *types.Transaction {
	t.Helper()

	builder := flatbuffers.NewBuilder(512)

	mutVec := buildObjectRefVector(builder, []objectRef{{id: coinID, version: 1}}, true)

	args := make([]byte, 0, 40)
	args = append(args, validator[:]...)
	if withAmount {
		amt := make([]byte, 8)
		binary.LittleEndian.PutUint64(amt, amount)
		args = append(args, amt...)
	}

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

// TestHandleDelegate_AtomicDebitPositionAndTotal checks a valid delegation
// strictly debits the coin, creates the position owned by the delegator, and
// raises the validator's DelegatedTotal — all together.
func TestHandleDelegate_AtomicDebitPositionAndTotal(t *testing.T) {
	dag, store, delegator, coinID := bondTestDAG(t, 1000)
	defer dag.Close()

	validator := delegator // the registered validator is the delegation target

	tx := buildDelegateTx(t, delegator, coinID, validator, "delegate", 300, true)
	if !dag.handleDelegate(tx) {
		t.Fatal("delegate to a known validator should be applied")
	}

	if got := coinBalance(t, store, coinID); got != 700 {
		t.Fatalf("coin balance after delegate = %d, want 700", got)
	}
	if got := dag.validators.Get(validator).DelegatedTotal; got != 300 {
		t.Fatalf("DelegatedTotal after delegate = %d, want 300", got)
	}

	posID := DelegationID(delegator, validator)
	posData := store.GetObject(posID)
	if posData == nil {
		t.Fatal("delegation position object must be created")
	}
	obj := types.GetRootAsObject(posData, 0)
	if string(obj.OwnerBytes()) != string(delegator[:]) {
		t.Fatalf("position owner = %x, want delegator %x", obj.OwnerBytes()[:4], delegator[:4])
	}
	gotVal, gotAmt, ok := decodeDelegationContent(obj.ContentBytes())
	if !ok || gotVal != validator || gotAmt != 300 {
		t.Fatalf("position content = (%x, %d, %v), want (%x, 300, true)", gotVal[:4], gotAmt, ok, validator[:4])
	}
}

// TestHandleDelegate_UnderFundedRejectedNoZeroing checks an under-funded
// delegation is rejected without touching the coin, position, or total.
func TestHandleDelegate_UnderFundedRejectedNoZeroing(t *testing.T) {
	dag, store, delegator, coinID := bondTestDAG(t, 100)
	defer dag.Close()

	validator := delegator
	tx := buildDelegateTx(t, delegator, coinID, validator, "delegate", 300, true)
	if dag.handleDelegate(tx) {
		t.Fatal("under-funded delegate should be rejected")
	}

	if got := coinBalance(t, store, coinID); got != 100 {
		t.Fatalf("coin must be untouched on rejected delegate, got %d, want 100", got)
	}
	if got := dag.validators.Get(validator).DelegatedTotal; got != 0 {
		t.Fatalf("DelegatedTotal must stay 0 on rejected delegate, got %d", got)
	}
	if store.GetObject(DelegationID(delegator, validator)) != nil {
		t.Fatal("no position must be created on rejected delegate")
	}
}

// TestHandleDelegate_UnknownValidatorRejected checks delegation to a validator
// not in the set is rejected (no debit, no total change).
func TestHandleDelegate_UnknownValidatorRejected(t *testing.T) {
	dag, store, delegator, coinID := bondTestDAG(t, 1000)
	defer dag.Close()

	unknown := [32]byte{0xDE, 0xAD}
	tx := buildDelegateTx(t, delegator, coinID, unknown, "delegate", 300, true)
	if dag.handleDelegate(tx) {
		t.Fatal("delegate to an unknown validator should be rejected")
	}
	if got := coinBalance(t, store, coinID); got != 1000 {
		t.Fatalf("coin must be untouched on unknown-validator delegate, got %d", got)
	}
}

// TestHandleDelegate_JailedValidatorRejected checks delegation to a jailed
// validator is rejected.
func TestHandleDelegate_JailedValidatorRejected(t *testing.T) {
	dag, store, delegator, coinID := bondTestDAG(t, 1000)
	defer dag.Close()

	validator := delegator
	dag.validators.Jail(validator)

	tx := buildDelegateTx(t, delegator, coinID, validator, "delegate", 300, true)
	if dag.handleDelegate(tx) {
		t.Fatal("delegate to a jailed validator should be rejected")
	}
	if got := coinBalance(t, store, coinID); got != 1000 {
		t.Fatalf("coin must be untouched on jailed-validator delegate, got %d", got)
	}
	if got := dag.validators.Get(validator).DelegatedTotal; got != 0 {
		t.Fatalf("DelegatedTotal must stay 0 on jailed-validator delegate, got %d", got)
	}
}

// TestHandleUndelegate_RemovesPositionAndLowersTotal checks undelegate deletes
// the position, credits the coin, and lowers DelegatedTotal.
func TestHandleUndelegate_RemovesPositionAndLowersTotal(t *testing.T) {
	dag, store, delegator, coinID := bondTestDAG(t, 1000)
	defer dag.Close()

	validator := delegator
	if !dag.handleDelegate(buildDelegateTx(t, delegator, coinID, validator, "delegate", 400, true)) {
		t.Fatal("delegate should be applied")
	}
	// coin: 600, DelegatedTotal: 400, position exists.

	if !dag.handleUndelegate(buildDelegateTx(t, delegator, coinID, validator, "undelegate", 0, false)) {
		t.Fatal("undelegate should be applied")
	}

	if got := dag.validators.Get(validator).DelegatedTotal; got != 0 {
		t.Fatalf("DelegatedTotal after undelegate = %d, want 0", got)
	}
	if got := coinBalance(t, store, coinID); got != 1000 {
		t.Fatalf("coin balance after undelegate = %d, want 1000 (principal returned)", got)
	}
	if store.GetObject(DelegationID(delegator, validator)) != nil {
		t.Fatal("position must be removed after undelegate")
	}
}

// TestDelegationsFor_EnumeratesPositions checks the enumerator returns every
// delegation position targeting a validator after multiple delegate txs.
func TestDelegationsFor_EnumeratesPositions(t *testing.T) {
	dag, store, validator, vCoin := bondTestDAG(t, 1000)
	defer dag.Close()

	// First delegator is the validator itself (self-delegation) using its coin.
	if !dag.handleDelegate(buildDelegateTx(t, validator, vCoin, validator, "delegate", 200, true)) {
		t.Fatal("self-delegation should be applied")
	}

	// Second delegator with its own funded coin.
	d2 := [32]byte{0xB2}
	d2Coin := [32]byte{0xC2}
	store.SetObject(buildTestCoinObject(d2Coin, 1000, d2, 0))
	if !dag.handleDelegate(buildDelegateTx(t, d2, d2Coin, validator, "delegate", 300, true)) {
		t.Fatal("second delegation should be applied")
	}

	if dag.delegations == nil {
		t.Fatal("SetFeeSystem must store the DelegationEnumerator")
	}

	entries := dag.delegations.DelegationsFor(validator)
	if len(entries) != 2 {
		t.Fatalf("DelegationsFor returned %d entries, want 2", len(entries))
	}

	byOwner := map[[32]byte]uint64{}
	for _, e := range entries {
		byOwner[e.Delegator] = e.Amount
	}
	if byOwner[validator] != 200 || byOwner[d2] != 300 {
		t.Fatalf("enumerated amounts = %v, want {validator:200, d2:300}", byOwner)
	}
}

// TestSplitValidatorReward checks the proportional split: self-stake share plus
// commission to the validator, post-commission remainder pro-rata to delegators,
// conserving the reward exactly with the rounding remainder going to the validator.
func TestSplitValidatorReward(t *testing.T) {
	d1 := [32]byte{0xD1}
	d2 := [32]byte{0xD2}

	validatorAmt, dels := splitValidatorReward(1000, 600, 1000, []delegatorShare{
		{Delegator: d1, Amount: 100},
		{Delegator: d2, Amount: 300},
	})

	// self share 600 + commission 40 = 640; delegators 90 and 270.
	if validatorAmt != 640 {
		t.Fatalf("validator amount = %d, want 640", validatorAmt)
	}
	if len(dels) != 2 || dels[0].Amount != 90 || dels[1].Amount != 270 {
		t.Fatalf("delegator amounts = %+v, want [90 270]", dels)
	}

	total := validatorAmt
	for _, d := range dels {
		total += d.Amount
	}
	if total != 1000 {
		t.Fatalf("split does not conserve: sum = %d, want 1000", total)
	}
}

// TestSplitValidatorReward_RemainderToValidator checks integer-division loss is
// awarded to the validator so the split conserves exactly.
func TestSplitValidatorReward_RemainderToValidator(t *testing.T) {
	d1 := [32]byte{0xD1}
	d2 := [32]byte{0xD2}
	d3 := [32]byte{0xD3}

	// reward 100, no self-stake, no commission, three equal delegators: 100/3
	// each truncates to 33, leaving a remainder of 1 for the validator.
	validatorAmt, dels := splitValidatorReward(100, 0, 0, []delegatorShare{
		{Delegator: d1, Amount: 1},
		{Delegator: d2, Amount: 1},
		{Delegator: d3, Amount: 1},
	})

	total := validatorAmt
	for _, d := range dels {
		total += d.Amount
	}
	if total != 100 {
		t.Fatalf("split does not conserve: sum = %d, want 100", total)
	}
	if validatorAmt != 1 {
		t.Fatalf("validator remainder = %d, want 1", validatorAmt)
	}
}

// TestSplitValidatorReward_NoDelegators checks the validator keeps the whole
// reward when there are no delegators.
func TestSplitValidatorReward_NoDelegators(t *testing.T) {
	validatorAmt, dels := splitValidatorReward(500, 600, 1000, nil)
	if validatorAmt != 500 {
		t.Fatalf("validator amount = %d, want 500", validatorAmt)
	}
	if len(dels) != 0 {
		t.Fatalf("delegator amounts = %+v, want none", dels)
	}
}

// TestSplitValidatorReward_SelfDelegation checks a validator that also appears as
// one of its own delegators is paid its self-stake share AND its self-owned
// delegation share, on distinct capital, with no double-credit and exact
// conservation.
func TestSplitValidatorReward_SelfDelegation(t *testing.T) {
	self := [32]byte{0x5E, 0x1F}
	other := [32]byte{0x07}

	// selfStake 600; the validator also self-delegated 200; another delegator 200.
	// Total stake 1000, reward 1000, commission 10%.
	// self-stake share = 600; delegated portion = 400; commission = 40 → validator
	// keeps 640. Remaining 360 split pro-rata of {200, 200}: 180 each.
	validatorAmt, dels := splitValidatorReward(1000, 600, 1000, []delegatorShare{
		{Delegator: self, Amount: 200},
		{Delegator: other, Amount: 200},
	})

	if validatorAmt != 640 {
		t.Fatalf("validator self-stake+commission amount = %d, want 640", validatorAmt)
	}

	// The self-delegation entry is paid as a normal delegator (180), on the
	// delegation-position coin — NOT folded into the 640 self-stake reward.
	var selfDelShare, otherShare uint64
	for _, d := range dels {
		switch d.Delegator {
		case self:
			selfDelShare = d.Amount
		case other:
			otherShare = d.Amount
		}
	}
	if selfDelShare != 180 || otherShare != 180 {
		t.Fatalf("delegator shares: self=%d other=%d, want 180/180", selfDelShare, otherShare)
	}

	// Conservation: the validator's 640 (self-stake reward) and the 180
	// self-delegation share are distinct credits to distinct objects; together
	// with the other delegator the whole 1000 is accounted for exactly once.
	total := validatorAmt
	for _, d := range dels {
		total += d.Amount
	}
	if total != 1000 {
		t.Fatalf("self-delegation split does not conserve: sum = %d, want 1000", total)
	}
}
