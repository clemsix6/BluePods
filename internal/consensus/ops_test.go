package consensus

import (
	"encoding/hex"
	"testing"

	flatbuffers "github.com/google/flatbuffers/go"

	"BluePods/internal/events"
	"BluePods/internal/genesis"
	"BluePods/internal/types"
)

// =============================================================================
// Reparent / transfer
// =============================================================================

// TestExecuteTx_DeclaredTransferMovesOnlyRoot commits a transfer (a reparent to
// a new KeyRoot) through the full commit path and asserts only the transferred
// object's tracker entry moves: its parent key and version change while its
// child's entry is untouched.
func TestExecuteTx_DeclaredTransferMovesOnlyRoot(t *testing.T) {
	db := newTestStorage(t)
	validators, vs := newTestValidatorSet(3)
	dag := New(db, vs, &mockBroadcaster{}, testSystemPod, 0, validators[0].privKey, nil)
	defer dag.Close()
	disableTxAuth(dag)

	sender := Hash{0xA1}   // current controlling key
	newOwner := Hash{0xB2} // transfer target key
	root := Hash{0x51}
	child := Hash{0x52}

	dag.tracker.trackObject(root, 0, 0, 0, keyRootKind, sender)
	dag.tracker.trackObject(child, 0, 0, 0, objectParentKind, root)

	ops := []genesis.DeclaredOp{{Kind: reparentOp, ObjectID: root[:], TargetKind: keyRootKind, Target: newOwner[:]}}
	atx := types.GetRootAsAttestedTransaction(
		buildOpsATX(t, sender, "", nil, []objectRef{{id: root, version: 0}}, ops, Hash{0x01}), 0)

	buf := captureEvents(t)
	dag.executeTx(atx, 1, validators[0].pubKey, nil, Hash{0xEE})

	if k, p, ok := dag.tracker.getParent(root); !ok || k != keyRootKind || p != newOwner {
		t.Fatalf("root parent = (kind=%d, %x, ok=%v), want (KeyRoot, %x)", k, p[:4], ok, newOwner[:4])
	}
	if v := dag.tracker.getVersion(root); v != 1 {
		t.Errorf("root version = %d, want 1", v)
	}
	if c := dag.tracker.childCount(root); c != 1 {
		t.Errorf("root child count = %d, want 1 (child kept)", c)
	}

	if k, p, ok := dag.tracker.getParent(child); !ok || k != objectParentKind || p != root {
		t.Errorf("child parent changed: (kind=%d, %x, ok=%v)", k, p[:4], ok)
	}
	if v := dag.tracker.getVersion(child); v != 0 {
		t.Errorf("child version = %d, want 0 (unchanged)", v)
	}

	assertSingleTxCommitted(t, buf, 1, true, "", Hash{0xEE})

	rep := eventsNamed(t, buf, events.EvObjectReparented)
	if len(rep) != 1 {
		t.Fatalf("want 1 %s event, got %d", events.EvObjectReparented, len(rep))
	}
	if rep[0]["object"] != hex.EncodeToString(root[:]) {
		t.Errorf("reparented object = %v, want %s", rep[0]["object"], hex.EncodeToString(root[:]))
	}
	if rep[0]["parent"] != hex.EncodeToString(newOwner[:]) {
		t.Errorf("reparented parent = %v, want %s", rep[0]["parent"], hex.EncodeToString(newOwner[:]))
	}
	if rep[0]["kind"] != float64(0) || rep[0]["version"] != float64(1) {
		t.Errorf("reparented event kind/version = %v/%v, want 0/1", rep[0]["kind"], rep[0]["version"])
	}
}

// TestHandleDeclaredOps_ReparentUnderForeignParentFails rejects a reparent whose
// ObjectParent target the sender does not control.
func TestHandleDeclaredOps_ReparentUnderForeignParentFails(t *testing.T) {
	dag := opsTestDAG(t)
	defer dag.Close()

	sender := Hash{0xA1}
	other := Hash{0xC3}
	obj := Hash{0x61}
	foreign := Hash{0x62}

	dag.tracker.trackObject(obj, 0, 0, 0, keyRootKind, sender)
	dag.tracker.trackObject(foreign, 0, 0, 0, keyRootKind, other)

	ops := []genesis.DeclaredOp{{Kind: reparentOp, ObjectID: obj[:], TargetKind: objectParentKind, Target: foreign[:]}}
	tx := opsTx(t, sender, "", nil, []objectRef{{id: obj, version: 0}}, ops, Hash{0x02})

	if dag.handleDeclaredOps(tx) {
		t.Fatal("reparent under a non-controlled ObjectParent must be rejected")
	}
	if k, p, _ := dag.tracker.getParent(obj); k != keyRootKind || p != sender {
		t.Errorf("object reparented on a rejected op: (kind=%d, %x)", k, p[:4])
	}
}

// TestHandleDeclaredOps_CycleRejected rejects a reparent that would make an
// object a descendant of itself.
func TestHandleDeclaredOps_CycleRejected(t *testing.T) {
	dag := opsTestDAG(t)
	defer dag.Close()

	sender := Hash{0xA1}
	a := Hash{0x71}
	b := Hash{0x72}

	dag.tracker.trackObject(a, 0, 0, 0, keyRootKind, sender)
	dag.tracker.trackObject(b, 0, 0, 0, objectParentKind, a) // b under a

	ops := []genesis.DeclaredOp{{Kind: reparentOp, ObjectID: a[:], TargetKind: objectParentKind, Target: b[:]}}
	tx := opsTx(t, sender, "", nil, []objectRef{{id: a, version: 0}}, ops, Hash{0x03})

	if dag.handleDeclaredOps(tx) {
		t.Fatal("a cycle-forming reparent must be rejected")
	}
	if k, p, _ := dag.tracker.getParent(a); k != keyRootKind || p != sender {
		t.Errorf("object reparented despite the cycle: (kind=%d, %x)", k, p[:4])
	}
}

// TestExecuteTx_DeclaredReparentBumpsVersionOncePerObject asserts an object
// touched by several ops in one transaction has its version bumped exactly once.
func TestExecuteTx_DeclaredReparentBumpsVersionOncePerObject(t *testing.T) {
	db := newTestStorage(t)
	validators, vs := newTestValidatorSet(3)
	dag := New(db, vs, &mockBroadcaster{}, testSystemPod, 0, validators[0].privKey, nil)
	defer dag.Close()
	disableTxAuth(dag)

	sender := Hash{0xA1}
	a := Hash{0x81}
	b := Hash{0x82}
	c := Hash{0x83}

	dag.tracker.trackObject(a, 0, 0, 0, keyRootKind, sender)
	dag.tracker.trackObject(b, 0, 0, 0, keyRootKind, sender)
	dag.tracker.trackObject(c, 0, 0, 0, keyRootKind, sender)

	ops := []genesis.DeclaredOp{
		{Kind: reparentOp, ObjectID: a[:], TargetKind: objectParentKind, Target: b[:]},
		{Kind: reparentOp, ObjectID: a[:], TargetKind: objectParentKind, Target: c[:]},
	}
	atx := types.GetRootAsAttestedTransaction(
		buildOpsATX(t, sender, "", nil, []objectRef{{id: a, version: 0}}, ops, Hash{0x04}), 0)

	dag.executeTx(atx, 1, validators[0].pubKey, nil, Hash{0xE4})

	if v := dag.tracker.getVersion(a); v != 1 {
		t.Errorf("A version = %d, want 1 (bumped once despite two ops)", v)
	}
	if k, p, _ := dag.tracker.getParent(a); k != objectParentKind || p != c {
		t.Errorf("A final parent = (kind=%d, %x), want (ObjectParent, %x)", k, p[:4], c[:4])
	}
}

// TestHandleDeclaredOps_DependentListAppliesNothing asserts a list whose second
// op depends on and is broken by the first (delete X, then reparent under X) is
// rejected wholesale with no effect.
func TestHandleDeclaredOps_DependentListAppliesNothing(t *testing.T) {
	dag := opsTestDAG(t)
	defer dag.Close()

	sender := Hash{0xA1}
	x := Hash{0x91}
	y := Hash{0x92}

	dag.tracker.trackObject(x, 0, 0, 0, keyRootKind, sender)
	dag.tracker.trackObject(y, 0, 0, 0, keyRootKind, sender)

	ops := []genesis.DeclaredOp{
		{Kind: deleteOp, ObjectID: x[:]},
		{Kind: reparentOp, ObjectID: y[:], TargetKind: objectParentKind, Target: x[:]},
	}
	tx := opsTx(t, sender, "", nil, []objectRef{{id: x, version: 0}, {id: y, version: 0}}, ops, Hash{0x05})

	if dag.handleDeclaredOps(tx) {
		t.Fatal("a list whose second op fails must be rejected wholesale")
	}
	if _, _, ok := dag.tracker.getParent(x); !ok {
		t.Error("X was deleted despite the list being rejected")
	}
	if k, p, _ := dag.tracker.getParent(y); k != keyRootKind || p != sender {
		t.Errorf("Y reparented despite the list being rejected: (kind=%d, %x)", k, p[:4])
	}
}

// TestHandleDeclaredOps_SequentialReparentSucceeds asserts each op sees the
// staged effect of the previous one: reparent A under B, then C under A.
func TestHandleDeclaredOps_SequentialReparentSucceeds(t *testing.T) {
	dag := opsTestDAG(t)
	defer dag.Close()

	sender := Hash{0xA1}
	a := Hash{0xA0}
	b := Hash{0xB0}
	c := Hash{0xC0}

	dag.tracker.trackObject(a, 0, 0, 0, keyRootKind, sender)
	dag.tracker.trackObject(b, 0, 0, 0, keyRootKind, sender)
	dag.tracker.trackObject(c, 0, 0, 0, keyRootKind, sender)

	ops := []genesis.DeclaredOp{
		{Kind: reparentOp, ObjectID: a[:], TargetKind: objectParentKind, Target: b[:]},
		{Kind: reparentOp, ObjectID: c[:], TargetKind: objectParentKind, Target: a[:]},
	}
	tx := opsTx(t, sender, "", nil, []objectRef{{id: a, version: 0}, {id: c, version: 0}}, ops, Hash{0x06})

	if !dag.handleDeclaredOps(tx) {
		t.Fatal("valid sequential reparents must apply")
	}
	if k, p, _ := dag.tracker.getParent(a); k != objectParentKind || p != b {
		t.Errorf("A parent = (kind=%d, %x), want (ObjectParent, %x)", k, p[:4], b[:4])
	}
	if k, p, _ := dag.tracker.getParent(c); k != objectParentKind || p != a {
		t.Errorf("C parent = (kind=%d, %x), want (ObjectParent, %x)", k, p[:4], a[:4])
	}
	if n := dag.tracker.childCount(a); n != 1 {
		t.Errorf("A child count = %d, want 1", n)
	}
	if n := dag.tracker.childCount(b); n != 1 {
		t.Errorf("B child count = %d, want 1", n)
	}
}

// TestExecuteTx_OpsWithPodCallRejected rejects a transaction that carries both
// declared operations and a pod call.
func TestExecuteTx_OpsWithPodCallRejected(t *testing.T) {
	db := newTestStorage(t)
	validators, vs := newTestValidatorSet(3)
	dag := New(db, vs, &mockBroadcaster{}, testSystemPod, 0, validators[0].privKey, nil)
	defer dag.Close()
	disableTxAuth(dag)

	sender := Hash{0xA1}
	obj := Hash{0xD1}
	newOwner := Hash{0xD2}

	dag.tracker.trackObject(obj, 0, 0, 0, keyRootKind, sender)

	ops := []genesis.DeclaredOp{{Kind: reparentOp, ObjectID: obj[:], TargetKind: keyRootKind, Target: newOwner[:]}}
	atx := types.GetRootAsAttestedTransaction(
		buildOpsATX(t, sender, "transfer", nil, []objectRef{{id: obj, version: 0}}, ops, Hash{0x07}), 0)

	buf := captureEvents(t)
	dag.executeTx(atx, 1, validators[0].pubKey, nil, Hash{0xE7})

	assertSingleTxCommitted(t, buf, 1, false, "declared_ops", Hash{0xE7})
	if k, p, _ := dag.tracker.getParent(obj); k != keyRootKind || p != sender {
		t.Errorf("object reparented despite the ops+pod mix being rejected: (kind=%d, %x)", k, p[:4])
	}
}

// =============================================================================
// Delete
// =============================================================================

// TestHandleDeclaredOps_DeleteLeafRefundsAndBurns deletes a controlled leaf and
// asserts the 95/5 refund/burn, the tracker release, the body-drop hook, and
// the state.object.deleted event.
func TestHandleDeclaredOps_DeleteLeafRefundsAndBurns(t *testing.T) {
	const (
		deposit     = 10000
		refundBPS   = 9500 // DefaultFeeParams.StorageRefundBPS
		startSupply = 100_000_000
		initialBal  = 5_000_000
	)
	expRefund := uint64(deposit) * refundBPS / 10000
	expBurn := uint64(deposit) - expRefund

	dag := opsTestDAG(t)
	defer dag.Close()

	coinStore := newMockCoinStore()
	coinStore.SetTotalSupply(startSupply)
	coinStore.SetCoinsTotal(initialBal)
	params := DefaultFeeParams()
	dag.SetFeeSystem(coinStore, &params, func([32]byte, int) []Hash { return nil })

	sender := Hash{0xA1}
	leaf := Hash{0xF1}
	gasCoin := Hash{0xEE}

	coinStore.SetObject(buildTestCoinObject(gasCoin, initialBal, sender, 0))
	dag.tracker.trackObject(leaf, 0, 0, deposit, keyRootKind, sender)

	var dropped []Hash
	dag.SetOnObjectDeleted(func(id [32]byte) { dropped = append(dropped, id) })

	ops := []genesis.DeclaredOp{{Kind: deleteOp, ObjectID: leaf[:]}}
	tx := opsTx(t, sender, "", &gasCoin, []objectRef{{id: leaf, version: 0}}, ops, Hash{0x08})

	buf := captureEvents(t)
	supplyBefore := coinStore.TotalSupply()
	if !dag.handleDeclaredOps(tx) {
		t.Fatal("deleting a controlled leaf must succeed")
	}

	if _, _, ok := dag.tracker.getParent(leaf); ok {
		t.Error("leaf not removed from the tracker")
	}
	if got := supplyBefore - coinStore.TotalSupply(); got != expBurn {
		t.Errorf("supply burn = %d, want %d", got, expBurn)
	}
	if bal, _ := readCoinBalance(coinStore.GetObject(gasCoin)); bal != uint64(initialBal)+expRefund {
		t.Errorf("gas coin = %d, want %d (refund credited)", bal, uint64(initialBal)+expRefund)
	}
	if len(dropped) != 1 || dropped[0] != leaf {
		t.Errorf("body-drop hook fired %v, want [leaf]", dropped)
	}
	del := eventsNamed(t, buf, events.EvObjectDeleted)
	if len(del) != 1 || del[0]["refund"] != float64(expRefund) {
		t.Errorf("object.deleted = %v, want one carrying refund %d", del, expRefund)
	}
}

// TestHandleDeclaredOps_DeleteParentWithChildrenRejected rejects a declared
// delete of an object that still has tracked children.
func TestHandleDeclaredOps_DeleteParentWithChildrenRejected(t *testing.T) {
	dag := opsTestDAG(t)
	defer dag.Close()

	sender := Hash{0xA1}
	parent := Hash{0x11}
	child := Hash{0x12}

	dag.tracker.trackObject(parent, 0, 0, 0, keyRootKind, sender)
	dag.tracker.trackObject(child, 0, 0, 0, objectParentKind, parent)

	ops := []genesis.DeclaredOp{{Kind: deleteOp, ObjectID: parent[:]}}
	tx := opsTx(t, sender, "", nil, []objectRef{{id: parent, version: 0}}, ops, Hash{0x09})

	if dag.handleDeclaredOps(tx) {
		t.Fatal("deleting a parent with children must be rejected")
	}
	if _, _, ok := dag.tracker.getParent(parent); !ok {
		t.Error("parent removed despite having children")
	}
}

// TestSettleDeclaredDeletions_ParentWithChildrenRejected rejects the pod
// carve-out channel's deletion of a parented object, settling nothing.
func TestSettleDeclaredDeletions_ParentWithChildrenRejected(t *testing.T) {
	dag := opsTestDAG(t)
	defer dag.Close()

	coinStore := newMockCoinStore()
	coinStore.SetTotalSupply(10000)
	params := DefaultFeeParams()
	dag.SetFeeSystem(coinStore, &params, nil)

	sender := Hash{0xA1}
	parent := Hash{0x21}
	child := Hash{0x22}

	dag.tracker.trackObject(parent, 0, 3, 1000, keyRootKind, sender)
	dag.tracker.trackObject(child, 0, 0, 0, objectParentKind, parent)

	tx := buildDeletionTx(sender, nil, parent)

	if dag.settleDeclaredDeletions(tx, Hash{0x0A}) {
		t.Fatal("pod carve-out deletion of a parented object must be rejected")
	}
	if _, _, ok := dag.tracker.getParent(parent); !ok {
		t.Error("parent settled despite having children")
	}
	if s := coinStore.TotalSupply(); s != 10000 {
		t.Errorf("supply changed on a rejected deletion: %d", s)
	}
}

// TestHandleDeclaredOps_DeleteWithoutHolderHookIsClean asserts a non-holder
// (no body-drop hook wired, nothing stored) settles the same declared deletion
// without panicking.
func TestHandleDeclaredOps_DeleteWithoutHolderHookIsClean(t *testing.T) {
	dag := opsTestDAG(t)
	defer dag.Close()

	coinStore := newMockCoinStore()
	coinStore.SetTotalSupply(100_000)
	params := DefaultFeeParams()
	dag.SetFeeSystem(coinStore, &params, func([32]byte, int) []Hash { return nil })

	sender := Hash{0xA1}
	leaf := Hash{0x31}
	gasCoin := Hash{0xEE}

	coinStore.SetObject(buildTestCoinObject(gasCoin, 5000, sender, 0))
	dag.tracker.trackObject(leaf, 0, 3, 2000, keyRootKind, sender)

	ops := []genesis.DeclaredOp{{Kind: deleteOp, ObjectID: leaf[:]}}
	tx := opsTx(t, sender, "", &gasCoin, []objectRef{{id: leaf, version: 0}}, ops, Hash{0x0B})

	if !dag.handleDeclaredOps(tx) {
		t.Fatal("a non-holder must settle the declared deletion too")
	}
	if _, _, ok := dag.tracker.getParent(leaf); ok {
		t.Error("leaf not removed on the non-holder")
	}
	if f := dag.tracker.getFees(leaf); f != 0 {
		t.Errorf("deposit not released: %d", f)
	}
}

// =============================================================================
// Defensive rejections
// =============================================================================

// TestHandleDeclaredOps_ZeroSenderRejected rejects a declared-op list whose
// sender is the all-zero key, so it can never reach controls() and seize a
// frozen object.
func TestHandleDeclaredOps_ZeroSenderRejected(t *testing.T) {
	dag := opsTestDAG(t)
	defer dag.Close()

	obj := Hash{0x41}
	newOwner := Hash{0x99}

	dag.tracker.trackObject(obj, 0, 0, 0, keyRootKind, Hash{}) // frozen under the zero key

	ops := []genesis.DeclaredOp{{Kind: reparentOp, ObjectID: obj[:], TargetKind: keyRootKind, Target: newOwner[:]}}
	tx := opsTx(t, Hash{}, "", nil, []objectRef{{id: obj, version: 0}}, ops, Hash{0x0C})

	if dag.handleDeclaredOps(tx) {
		t.Fatal("an all-zero sender must never reach controls()")
	}
	if k, p, _ := dag.tracker.getParent(obj); k != keyRootKind || p != (Hash{}) {
		t.Errorf("frozen object reparented by the zero sender: (kind=%d, %x)", k, p[:4])
	}
}

// TestHandleDeclaredOps_UnknownKindRejected rejects an op of an
// unknown/unsupported kind (the domain kinds land in a later batch).
func TestHandleDeclaredOps_UnknownKindRejected(t *testing.T) {
	dag := opsTestDAG(t)
	defer dag.Close()

	sender := Hash{0xA1}
	obj := Hash{0x51}

	dag.tracker.trackObject(obj, 0, 0, 0, keyRootKind, sender)

	for _, kind := range []byte{2, 3, 6, 7} {
		ops := []genesis.DeclaredOp{{Kind: kind, ObjectID: obj[:]}}
		tx := opsTx(t, sender, "", nil, []objectRef{{id: obj, version: 0}}, ops, Hash{0x0D, kind})
		if dag.handleDeclaredOps(tx) {
			t.Fatalf("kind %d must be rejected as unknown/unsupported", kind)
		}
	}
	if _, _, ok := dag.tracker.getParent(obj); !ok {
		t.Error("object mutated by an unknown-kind op")
	}
}

// =============================================================================
// Test helpers
// =============================================================================

// opsTestDAG builds a DAG with commit-time authenticity disabled, for tests that
// drive handleDeclaredOps/settleDeclaredDeletions with synthetic transactions.
func opsTestDAG(t *testing.T) *DAG {
	t.Helper()

	db := newTestStorage(t)
	validators, vs := newTestValidatorSet(3)
	dag := New(db, vs, &mockBroadcaster{}, testSystemPod, 0, validators[0].privKey, nil)
	disableTxAuth(dag)

	return dag
}

// opsTx builds a declared-operation transaction and returns its inner
// Transaction, for direct handleDeclaredOps calls.
func opsTx(t *testing.T, sender Hash, funcName string, gasCoin *Hash, mutRefs []objectRef, ops []genesis.DeclaredOp, hash Hash) *types.Transaction {
	t.Helper()

	atx := types.GetRootAsAttestedTransaction(buildOpsATX(t, sender, funcName, gasCoin, mutRefs, ops, hash), 0)

	return atx.Transaction(nil)
}

// buildOpsATX builds an AttestedTransaction carrying declared operations, a
// sender, mutable refs, an optional function name (a pod-call signal), an
// optional gas coin, and a fixed hash. It carries no attested objects or proofs.
func buildOpsATX(t *testing.T, sender Hash, funcName string, gasCoin *Hash, mutRefs []objectRef, ops []genesis.DeclaredOp, hash Hash) []byte {
	t.Helper()

	builder := flatbuffers.NewBuilder(1024)

	mutVec := buildObjectRefVector(builder, mutRefs, true)
	opsVec := buildDeclaredOpsVector(builder, ops)
	hashVec := builder.CreateByteVector(hash[:])
	senderVec := builder.CreateByteVector(sender[:])
	podVec := builder.CreateByteVector(make([]byte, 32))

	var funcOff flatbuffers.UOffsetT
	if funcName != "" {
		funcOff = builder.CreateString(funcName)
	}

	var gasVec flatbuffers.UOffsetT
	if gasCoin != nil {
		gasVec = builder.CreateByteVector(gasCoin[:])
	}

	types.TransactionStart(builder)
	types.TransactionAddHash(builder, hashVec)
	types.TransactionAddSender(builder, senderVec)
	types.TransactionAddPod(builder, podVec)
	if funcOff != 0 {
		types.TransactionAddFunctionName(builder, funcOff)
	}
	if mutVec != 0 {
		types.TransactionAddMutableRefs(builder, mutVec)
	}
	types.TransactionAddOperations(builder, opsVec)
	if gasVec != 0 {
		types.TransactionAddGasCoin(builder, gasVec)
	}
	txOff := types.TransactionEnd(builder)

	types.AttestedTransactionStartObjectsVector(builder, 0)
	objVec := builder.EndVector(0)
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

// buildDeclaredOpsVector builds a DeclaredOp vector in the builder.
func buildDeclaredOpsVector(builder *flatbuffers.Builder, ops []genesis.DeclaredOp) flatbuffers.UOffsetT {
	offsets := make([]flatbuffers.UOffsetT, len(ops))

	for i, op := range ops {
		var objVec, tgtVec, nameOff flatbuffers.UOffsetT
		if len(op.ObjectID) > 0 {
			objVec = builder.CreateByteVector(op.ObjectID)
		}
		if len(op.Target) > 0 {
			tgtVec = builder.CreateByteVector(op.Target)
		}
		if op.Name != "" {
			nameOff = builder.CreateString(op.Name)
		}

		types.DeclaredOpStart(builder)
		types.DeclaredOpAddKind(builder, op.Kind)
		if objVec != 0 {
			types.DeclaredOpAddObjectId(builder, objVec)
		}
		types.DeclaredOpAddTargetKind(builder, op.TargetKind)
		if tgtVec != 0 {
			types.DeclaredOpAddTarget(builder, tgtVec)
		}
		if nameOff != 0 {
			types.DeclaredOpAddName(builder, nameOff)
		}
		types.DeclaredOpAddTermEpochs(builder, op.TermEpochs)
		offsets[i] = types.DeclaredOpEnd(builder)
	}

	types.TransactionStartOperationsVector(builder, len(offsets))
	for i := len(offsets) - 1; i >= 0; i-- {
		builder.PrependUOffsetT(offsets[i])
	}

	return builder.EndVector(len(offsets))
}
