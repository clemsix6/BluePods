package consensus

import (
	"encoding/hex"
	"testing"

	flatbuffers "github.com/google/flatbuffers/go"

	"BluePods/internal/events"
	"BluePods/internal/types"
)

// TestExecuteTx_ReplicatedDeletionAccountsUniformly commits a replicated-object
// deletion through executeTx on both a holder and a non-holder of the object. A
// non-holder skips execution (execution sharding), so the deposit-release,
// refund, and burn accounting must run in the commit loop from the transaction's
// declared deleted_objects and the network-uniform tracker deposit — never in the
// holder-only execution path. Both nodes must release the tracked deposit, refund
// the gas coin, and burn the remainder identically, or their coins_total,
// deposits, and total_supply fork.
func TestExecuteTx_ReplicatedDeletionAccountsUniformly(t *testing.T) {
	const (
		deposit     = 10000
		refundBPS   = 9500 // DefaultFeeParams.StorageRefundBPS
		initialBal  = 5_000_000
		startSupply = 100_000_000
		replication = 3
	)
	expectedRefund := uint64(deposit) * refundBPS / 10000 // 9500
	expectedBurn := uint64(deposit) - expectedRefund       // 500

	owner := Hash{0xAA}
	objID := Hash{0x88}
	gasCoinID := Hash{0xEE}

	type result struct {
		trackerFees uint64
		supplyDelta uint64
		coinBalance uint64
		feeTotal    uint64
	}

	run := func(holder bool) result {
		db := newTestStorage(t)
		validators, vs := newTestValidatorSet(3)
		dag := New(db, vs, &mockBroadcaster{}, testSystemPod, 0, validators[0].privKey, nil)
		defer dag.Close()
		disableTxAuth(dag)

		coinStore := newMockCoinStore()
		coinStore.SetTotalSupply(startSupply)
		coinStore.SetCoinsTotal(initialBal)
		params := DefaultFeeParams()

		// A non-nil holder function keeps the replication-ratio fee path from
		// dereferencing nil; the exact holders do not matter here.
		dag.SetFeeSystem(coinStore, &params, func([32]byte, int) []Hash { return nil })

		// Execution-sharding gate: this node holds the replicated object only when
		// holder is true; a non-holder skips execution entirely.
		dag.isHolder = func([32]byte, uint16) bool { return holder }

		// The gas coin is a singleton owned by the sender.
		coinStore.SetObject(buildTestCoinObject(gasCoinID, initialBal, owner, 0))

		// Every node's tracker records the replicated object's storage deposit.
		dag.tracker.trackObject(objID, 0, replication, deposit, 0, Hash{})

		atxBytes := buildDeletionATX(owner, gasCoinID, 500, objID, replication, owner)
		atx := types.GetRootAsAttestedTransaction(atxBytes, 0)

		supplyBefore := coinStore.TotalSupply()
		fee := dag.executeTx(atx, 1, validators[0].pubKey, nil, Hash{})

		bal, _ := readCoinBalance(coinStore.GetObject(gasCoinID))

		return result{
			trackerFees: dag.tracker.getFees(objID),
			supplyDelta: supplyBefore - coinStore.TotalSupply(),
			coinBalance: bal,
			feeTotal:    fee.Total,
		}
	}

	h := run(true)
	n := run(false)

	// The deposit must be released from the tracker on BOTH nodes.
	if n.trackerFees != 0 {
		t.Errorf("non-holder did not release the tracked deposit: getFees=%d, want 0", n.trackerFees)
	}
	if h.trackerFees != 0 {
		t.Errorf("holder did not release the tracked deposit: getFees=%d, want 0", h.trackerFees)
	}

	// The remainder must be burned from total_supply on BOTH nodes.
	if n.supplyDelta != expectedBurn {
		t.Errorf("non-holder supply burn = %d, want %d", n.supplyDelta, expectedBurn)
	}

	// The refund must land in the gas coin on BOTH nodes: final balance is the
	// starting balance minus the consumed fee plus the refund.
	wantBal := uint64(initialBal) - n.feeTotal + expectedRefund
	if n.coinBalance != wantBal {
		t.Errorf("non-holder gas coin = %d, want %d (init %d - fee %d + refund %d)",
			n.coinBalance, wantBal, initialBal, n.feeTotal, expectedRefund)
	}

	// Holder and non-holder must move identically.
	if h != n {
		t.Errorf("holder vs non-holder diverged: %+v vs %+v", h, n)
	}
}

// TestSettleDeclaredDeletions_RefundEmitsEvents verifies a settled deletion with
// a landed refund emits fees.deposit.refunded (the refunded amount), supply.burned
// (only the burned remainder), and state.object.deleted (carrying the refund).
func TestSettleDeclaredDeletions_RefundEmitsEvents(t *testing.T) {
	db := newTestStorage(t)
	validators, vs := newTestValidatorSet(3)
	dag := New(db, vs, &mockBroadcaster{}, testSystemPod, 0, validators[0].privKey, nil)
	defer dag.Close()

	coinStore := newMockCoinStore()
	coinStore.SetTotalSupply(100000)
	params := DefaultFeeParams()
	dag.SetFeeSystem(coinStore, &params, nil)

	owner := Hash{0xAA}
	objID := Hash{0x74}
	gasCoinID := Hash{0xEE}
	coinStore.SetObject(buildTestCoinObject(gasCoinID, 5000, owner, 0))
	dag.tracker.trackObject(objID, 0, 3, 10000, 0, Hash{})

	tx := buildDeletionTx(owner, &gasCoinID, objID)

	buf := captureEvents(t)
	dag.settleDeclaredDeletions(tx, Hash{0x01})

	refunded := eventsNamed(t, buf, events.EvDepositRefunded)
	if len(refunded) != 1 {
		t.Fatalf("want 1 %s event, got %d", events.EvDepositRefunded, len(refunded))
	}
	if refunded[0]["object"] != hex.EncodeToString(objID[:]) {
		t.Errorf("refunded object = %v, want %s", refunded[0]["object"], hex.EncodeToString(objID[:]))
	}
	if refunded[0]["coin"] != hex.EncodeToString(gasCoinID[:]) {
		t.Errorf("refunded coin = %v, want %s", refunded[0]["coin"], hex.EncodeToString(gasCoinID[:]))
	}
	if refunded[0]["amount"] != float64(9500) { // 10000 * 9500 / 10000
		t.Errorf("refunded amount = %v, want 9500", refunded[0]["amount"])
	}

	burned := eventsNamed(t, buf, events.EvSupplyBurned)
	if len(burned) != 1 {
		t.Fatalf("want 1 %s event, got %d", events.EvSupplyBurned, len(burned))
	}
	if burned[0]["amount"] != float64(500) { // 10000 - 9500
		t.Errorf("burned amount = %v, want 500", burned[0]["amount"])
	}
	if burned[0]["reason"] != "deletion" {
		t.Errorf("burned reason = %v, want deletion", burned[0]["reason"])
	}

	deleted := eventsNamed(t, buf, events.EvObjectDeleted)
	if len(deleted) != 1 {
		t.Fatalf("want 1 %s event, got %d", events.EvObjectDeleted, len(deleted))
	}
	if deleted[0]["refund"] != float64(9500) {
		t.Errorf("deleted refund = %v, want 9500", deleted[0]["refund"])
	}
}

// TestSettleDeclaredDeletions_NoGasCoinBurnsFullDeposit verifies a deletion with no
// gas coin to receive the refund burns the WHOLE deposit (supply.burned with the
// full amount), never emits fees.deposit.refunded, and reports a zero refund.
func TestSettleDeclaredDeletions_NoGasCoinBurnsFullDeposit(t *testing.T) {
	db := newTestStorage(t)
	validators, vs := newTestValidatorSet(3)
	dag := New(db, vs, &mockBroadcaster{}, testSystemPod, 0, validators[0].privKey, nil)
	defer dag.Close()

	coinStore := newMockCoinStore()
	coinStore.SetTotalSupply(10000)
	params := DefaultFeeParams()
	dag.SetFeeSystem(coinStore, &params, nil)

	owner := Hash{0xAA}
	objID := Hash{0x76}
	dag.tracker.trackObject(objID, 0, 3, 1000, 0, Hash{})

	tx := buildDeletionTx(owner, nil, objID) // no gas coin: no refund recipient

	buf := captureEvents(t)
	dag.settleDeclaredDeletions(tx, Hash{0x01})

	if refunded := eventsNamed(t, buf, events.EvDepositRefunded); len(refunded) != 0 {
		t.Fatalf("want 0 %s events without a gas coin, got %d", events.EvDepositRefunded, len(refunded))
	}

	burned := eventsNamed(t, buf, events.EvSupplyBurned)
	if len(burned) != 1 {
		t.Fatalf("want 1 %s event, got %d", events.EvSupplyBurned, len(burned))
	}
	if burned[0]["amount"] != float64(1000) {
		t.Errorf("burned amount = %v, want 1000 (full deposit)", burned[0]["amount"])
	}

	if got := coinStore.TotalSupply(); got != 9000 {
		t.Errorf("total supply after full burn = %d, want 9000", got)
	}

	if fees := dag.tracker.getFees(objID); fees != 0 {
		t.Errorf("deposit not released from tracker: getFees=%d, want 0", fees)
	}
}

// buildDeletionTx builds a Transaction that mutably references delID and declares
// it deleted. A non-nil gasCoin is set as the refund recipient; a nil gasCoin
// models a deletion with no gas coin.
func buildDeletionTx(sender Hash, gasCoin *Hash, delID Hash) *types.Transaction {
	builder := flatbuffers.NewBuilder(512)

	refIDVec := builder.CreateByteVector(delID[:])
	types.ObjectRefStart(builder)
	types.ObjectRefAddId(builder, refIDVec)
	types.ObjectRefAddVersion(builder, 0)
	refOff := types.ObjectRefEnd(builder)

	types.TransactionStartMutableRefsVector(builder, 1)
	builder.PrependUOffsetT(refOff)
	mutVec := builder.EndVector(1)

	deletedVec := builder.CreateByteVector(delID[:])
	senderVec := builder.CreateByteVector(sender[:])
	podVec := builder.CreateByteVector(make([]byte, 32))
	funcNameOff := builder.CreateString("delete")

	var gasCoinVec flatbuffers.UOffsetT
	if gasCoin != nil {
		gasCoinVec = builder.CreateByteVector(gasCoin[:])
	}

	types.TransactionStart(builder)
	types.TransactionAddSender(builder, senderVec)
	types.TransactionAddPod(builder, podVec)
	types.TransactionAddFunctionName(builder, funcNameOff)
	types.TransactionAddMutableRefs(builder, mutVec)
	types.TransactionAddDeletedObjects(builder, deletedVec)

	if gasCoinVec != 0 {
		types.TransactionAddGasCoin(builder, gasCoinVec)
	}

	txOff := types.TransactionEnd(builder)
	builder.Finish(txOff)

	return types.GetRootAsTransaction(builder.FinishedBytes(), 0)
}

// buildDeletionATX builds an AttestedTransaction that declares the deletion of a
// single replicated object. The object is referenced mutably (at version 0) and
// listed in deleted_objects; the ATX carries the attested object copy so ownership
// resolves network-uniformly. Proofs are empty (synthetic test input).
func buildDeletionATX(sender, gasCoin Hash, maxGas uint64, delID Hash, delRep uint16, delOwner Hash) []byte {
	builder := flatbuffers.NewBuilder(1024)

	// Mutable ref to the object being deleted (at version 0).
	refIDVec := builder.CreateByteVector(delID[:])
	types.ObjectRefStart(builder)
	types.ObjectRefAddId(builder, refIDVec)
	types.ObjectRefAddVersion(builder, 0)
	refOff := types.ObjectRefEnd(builder)

	types.TransactionStartMutableRefsVector(builder, 1)
	builder.PrependUOffsetT(refOff)
	mutVec := builder.EndVector(1)

	deletedVec := builder.CreateByteVector(delID[:])
	hashVec := builder.CreateByteVector(make([]byte, 32))
	senderVec := builder.CreateByteVector(sender[:])
	podVec := builder.CreateByteVector(make([]byte, 32))
	funcNameOff := builder.CreateString("delete")
	gasCoinVec := builder.CreateByteVector(gasCoin[:])

	types.TransactionStart(builder)
	types.TransactionAddHash(builder, hashVec)
	types.TransactionAddSender(builder, senderVec)
	types.TransactionAddPod(builder, podVec)
	types.TransactionAddFunctionName(builder, funcNameOff)
	types.TransactionAddMaxGas(builder, maxGas)
	types.TransactionAddGasCoin(builder, gasCoinVec)
	types.TransactionAddMutableRefs(builder, mutVec)
	types.TransactionAddDeletedObjects(builder, deletedVec)
	txOff := types.TransactionEnd(builder)

	// Attested copy of the replicated object (owner used for the ownership check).
	objIDVec := builder.CreateByteVector(delID[:])
	objOwnerVec := builder.CreateByteVector(delOwner[:])
	objContentVec := builder.CreateByteVector([]byte("data"))
	types.ObjectStart(builder)
	types.ObjectAddId(builder, objIDVec)
	types.ObjectAddVersion(builder, 0)
	types.ObjectAddOwner(builder, objOwnerVec)
	types.ObjectAddReplication(builder, delRep)
	types.ObjectAddContent(builder, objContentVec)
	objOff := types.ObjectEnd(builder)

	types.AttestedTransactionStartObjectsVector(builder, 1)
	builder.PrependUOffsetT(objOff)
	objVec := builder.EndVector(1)

	types.AttestedTransactionStartProofsVector(builder, 0)
	prfVec := builder.EndVector(0)

	types.AttestedTransactionStart(builder)
	types.AttestedTransactionAddTransaction(builder, txOff)
	types.AttestedTransactionAddObjects(builder, objVec)
	types.AttestedTransactionAddProofs(builder, prfVec)
	atxOff := types.AttestedTransactionEnd(builder)

	builder.Finish(atxOff)

	return builder.FinishedBytes()
}
