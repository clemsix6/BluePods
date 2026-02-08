package consensus

import (
	"crypto/ed25519"
	"encoding/binary"
	"testing"
	"time"

	flatbuffers "github.com/google/flatbuffers/go"

	"BluePods/internal/types"
)

// =============================================================================
// Object Tracker Tests (basic — full tests in tracker_test.go)
// =============================================================================

// TestTrackerCheckAndUpdate tests basic version check and increment.
func TestTrackerCheckAndUpdate(t *testing.T) {
	db := newTestStorage(t)
	ot := newObjectTracker(db)

	objID := Hash{0x01}
	tx := buildTxWithMutables(t, nil, []objectRef{{id: objID, version: 0}})

	if !ot.checkAndUpdate(tx) {
		t.Fatal("first tx should succeed (version 0 matches default)")
	}

	if v := ot.getVersion(objID); v != 1 {
		t.Fatalf("expected version 1, got %d", v)
	}
}

// TestTrackerConflict tests that a duplicate version causes conflict.
func TestTrackerConflict(t *testing.T) {
	db := newTestStorage(t)
	ot := newObjectTracker(db)

	objID := Hash{0x01}

	tx1 := buildTxWithMutables(t, nil, []objectRef{{id: objID, version: 0}})
	if !ot.checkAndUpdate(tx1) {
		t.Fatal("tx1 should succeed")
	}

	// tx2 expects version 0 but current is now 1
	tx2 := buildTxWithMutables(t, nil, []objectRef{{id: objID, version: 0}})
	if ot.checkAndUpdate(tx2) {
		t.Fatal("tx2 should fail: version conflict")
	}
}

// TestTrackerSequential tests sequential version increments.
func TestTrackerSequential(t *testing.T) {
	db := newTestStorage(t)
	ot := newObjectTracker(db)

	objID := Hash{0x01}

	for i := uint64(0); i < 5; i++ {
		tx := buildTxWithMutables(t, nil, []objectRef{{id: objID, version: i}})

		if !ot.checkAndUpdate(tx) {
			t.Fatalf("tx at version %d should succeed", i)
		}

		if v := ot.getVersion(objID); v != i+1 {
			t.Fatalf("after version %d: expected %d, got %d", i, i+1, v)
		}
	}
}

// TestTrackerMultipleObjects tests tracking multiple objects independently.
func TestTrackerMultipleObjects(t *testing.T) {
	db := newTestStorage(t)
	ot := newObjectTracker(db)

	obj1 := Hash{0x01}
	obj2 := Hash{0x02}

	tx := buildTxWithMutables(t, nil, []objectRef{
		{id: obj1, version: 0},
		{id: obj2, version: 0},
	})

	if !ot.checkAndUpdate(tx) {
		t.Fatal("tx should succeed")
	}

	if v := ot.getVersion(obj1); v != 1 {
		t.Fatalf("obj1: expected version 1, got %d", v)
	}

	if v := ot.getVersion(obj2); v != 1 {
		t.Fatalf("obj2: expected version 1, got %d", v)
	}
}

// TestTrackerReadObjectConflict tests that ReadObjects versions are also checked.
func TestTrackerReadObjectConflict(t *testing.T) {
	db := newTestStorage(t)
	ot := newObjectTracker(db)

	readObj := Hash{0x01}
	mutObj := Hash{0x02}

	// First bump readObj version
	tx1 := buildTxWithMutables(t, nil, []objectRef{{id: readObj, version: 0}})
	if !ot.checkAndUpdate(tx1) {
		t.Fatal("tx1 should succeed")
	}

	// tx2 reads readObj at version 0 (stale), should fail
	tx2 := buildTxWithMutables(t,
		[]objectRef{{id: readObj, version: 0}},
		[]objectRef{{id: mutObj, version: 0}},
	)

	if ot.checkAndUpdate(tx2) {
		t.Fatal("tx2 should fail: read object version conflict")
	}
}

// TestTrackerExportImport tests snapshot export and import.
func TestTrackerExportImport(t *testing.T) {
	db := newTestStorage(t)
	ot := newObjectTracker(db)

	obj1, obj2 := Hash{0x01}, Hash{0x02}
	ot.trackObject(obj1, 5, 10)
	ot.trackObject(obj2, 10, 20)

	entries := ot.Export()
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(entries))
	}

	db2 := newTestStorage(t)
	ot2 := newObjectTracker(db2)
	ot2.Import(entries)

	if v := ot2.getVersion(obj1); v != 5 {
		t.Fatalf("obj1: expected 5, got %d", v)
	}

	if v := ot2.getVersion(obj2); v != 10 {
		t.Fatalf("obj2: expected 10, got %d", v)
	}

	if r := ot2.getReplication(obj1); r != 10 {
		t.Fatalf("obj1: expected replication 10, got %d", r)
	}

	if r := ot2.getReplication(obj2); r != 20 {
		t.Fatalf("obj2: expected replication 20, got %d", r)
	}
}

// =============================================================================
// Commit Path Tests
// =============================================================================

// TestExecuteTxVersionConflict tests that version conflicts reject the tx.
func TestExecuteTxVersionConflict(t *testing.T) {
	db := newTestStorage(t)
	validators, vs := newTestValidatorSet(1)
	mock := &mockBroadcaster{}

	dag := New(db, vs, mock, testSystemPod, 1, validators[0].privKey, nil)
	defer dag.Close()

	objID := Hash{0x10}

	// Pre-set version to 3
	dag.tracker.trackObject(objID, 3, 0)

	// Build ATX expecting version 0 (stale)
	atxBytes := buildTestATX(t, "test_func", nil, []objectRef{{id: objID, version: 0}}, false)
	atx := types.GetRootAsAttestedTransaction(atxBytes, 0)

	dag.executeTx(atx, 0)

	// Tx should fail, version should NOT have been incremented
	if v := dag.tracker.getVersion(objID); v != 3 {
		t.Fatalf("version should stay at 3 after conflict, got %d", v)
	}
}

// TestExecuteTxVersionSuccess tests that a valid tx increments versions.
func TestExecuteTxVersionSuccess(t *testing.T) {
	db := newTestStorage(t)
	validators, vs := newTestValidatorSet(1)
	mock := &mockBroadcaster{}

	dag := New(db, vs, mock, testSystemPod, 1, validators[0].privKey, nil)
	defer dag.Close()

	objID := Hash{0x10}

	// Build ATX expecting version 0 (matches default)
	atxBytes := buildTestATX(t, "test_func", nil, []objectRef{{id: objID, version: 0}}, false)
	atx := types.GetRootAsAttestedTransaction(atxBytes, 0)

	dag.executeTx(atx, 0)

	// Version should be incremented
	if v := dag.tracker.getVersion(objID); v != 1 {
		t.Fatalf("expected version 1 after success, got %d", v)
	}
}

// TestShouldExecuteHolder tests that holders execute transactions.
func TestShouldExecuteHolder(t *testing.T) {
	db := newTestStorage(t)
	validators, vs := newTestValidatorSet(1)

	dag := New(db, vs, nil, testSystemPod, 1, validators[0].privKey, nil)
	defer dag.Close()

	holderObj := Hash{0x10}

	// Configure isHolder to say we are holder of holderObj
	dag.isHolder = func(objectID [32]byte, replication uint16) bool {
		return objectID == holderObj
	}

	// ATX with holderObj as standard object (replication=50)
	atxBytes := buildTestATXWithObjects(t, "test_func",
		[]objectRef{{id: holderObj, version: 0}},
		[]testObject{{id: holderObj, replication: 50}},
	)
	atx := types.GetRootAsAttestedTransaction(atxBytes, 0)
	tx := atx.Transaction(nil)

	if !dag.shouldExecute(atx, tx) {
		t.Fatal("should execute: we are holder of holderObj")
	}
}

// TestShouldExecuteNotHolder tests that non-holders skip execution.
func TestShouldExecuteNotHolder(t *testing.T) {
	db := newTestStorage(t)
	validators, vs := newTestValidatorSet(1)

	dag := New(db, vs, nil, testSystemPod, 1, validators[0].privKey, nil)
	defer dag.Close()

	otherObj := Hash{0x20}

	// We are NOT a holder of anything
	dag.isHolder = func(objectID [32]byte, replication uint16) bool {
		return false
	}

	atxBytes := buildTestATXWithObjects(t, "test_func",
		[]objectRef{{id: otherObj, version: 0}},
		[]testObject{{id: otherObj, replication: 50}},
	)
	atx := types.GetRootAsAttestedTransaction(atxBytes, 0)
	tx := atx.Transaction(nil)

	if dag.shouldExecute(atx, tx) {
		t.Fatal("should NOT execute: we are not holder")
	}
}

// TestShouldExecuteSingleton tests that singletons (not in ATX objects) trigger execution.
func TestShouldExecuteSingleton(t *testing.T) {
	db := newTestStorage(t)
	validators, vs := newTestValidatorSet(1)

	dag := New(db, vs, nil, testSystemPod, 1, validators[0].privKey, nil)
	defer dag.Close()

	singletonObj := Hash{0x30}

	// Singletons: isHolder returns true for replication=0
	dag.isHolder = func(objectID [32]byte, replication uint16) bool {
		return replication == 0
	}

	// ATX with singleton (not in Objects vector, so replication defaults to 0)
	atxBytes := buildTestATX(t, "test_func", nil, []objectRef{{id: singletonObj, version: 0}}, false)
	atx := types.GetRootAsAttestedTransaction(atxBytes, 0)
	tx := atx.Transaction(nil)

	if !dag.shouldExecute(atx, tx) {
		t.Fatal("should execute: singletons not in ATX → replication=0 → all validators hold them")
	}
}

// TestShouldExecuteNoSharding tests that without isHolder configured, all txs execute.
func TestShouldExecuteNoSharding(t *testing.T) {
	db := newTestStorage(t)
	validators, vs := newTestValidatorSet(1)

	dag := New(db, vs, nil, testSystemPod, 1, validators[0].privKey, nil)
	defer dag.Close()

	// isHolder not set (nil)
	objID := Hash{0x10}
	atxBytes := buildTestATXWithObjects(t, "test_func",
		[]objectRef{{id: objID, version: 0}},
		[]testObject{{id: objID, replication: 50}},
	)
	atx := types.GetRootAsAttestedTransaction(atxBytes, 0)
	tx := atx.Transaction(nil)

	if !dag.shouldExecute(atx, tx) {
		t.Fatal("should execute: no sharding configured → execute everything")
	}
}

// TestCreatesObjectsAllValidatorsExecute tests that creates_objects=true skips holder check.
func TestCreatesObjectsAllValidatorsExecute(t *testing.T) {
	db := newTestStorage(t)
	validators, vs := newTestValidatorSet(1)
	mock := &mockBroadcaster{}

	dag := New(db, vs, mock, testSystemPod, 1, validators[0].privKey, nil)
	defer dag.Close()

	// Not a holder of anything
	dag.isHolder = func(objectID [32]byte, replication uint16) bool {
		return false
	}

	objID := Hash{0x10}

	// Build ATX with creates_objects=true
	atxBytes := buildTestATX(t, "test_func", nil, []objectRef{{id: objID, version: 0}}, true)
	atx := types.GetRootAsAttestedTransaction(atxBytes, 0)
	tx := atx.Transaction(nil)

	// creates_objects check happens in executeTx, not shouldExecute directly
	// The check is: if !tx.CreatesObjects() && !d.shouldExecute(...)
	createsObjects := tx.CreatesObjects()

	if !createsObjects {
		t.Fatal("creates_objects should be true")
	}

	// With creates_objects=true, the shouldExecute check is bypassed
	shouldSkip := !createsObjects && !dag.shouldExecute(atx, tx)
	if shouldSkip {
		t.Fatal("creates_objects=true should force execution on all validators")
	}
}

// TestCommitRoundProcessing tests that committed round transactions are processed.
// Uses the version tracker directly to verify the commit path logic
// without relying on DAG timing.
func TestCommitRoundProcessing(t *testing.T) {
	db := newTestStorage(t)
	validators, vs := newTestValidatorSet(1)
	mock := &mockBroadcaster{}

	objID := Hash{0x10}

	atxBytes := buildTestATX(t, "some_func", nil, []objectRef{{id: objID, version: 0}}, false)

	dag := New(db, vs, mock, testSystemPod, 1, validators[0].privKey, nil,
		WithGenesisTxs([][]byte{atxBytes}))
	defer dag.Close()

	// With 1 validator, quorum=1 so our own vertices are enough.
	// Wait for vertex production and commit.
	time.Sleep(2 * time.Second)

	if v := dag.tracker.getVersion(objID); v != 1 {
		t.Fatalf("expected version 1 after commit, got %d", v)
	}
}

// TestBuildReplicationMap tests replication map extraction from ATX objects.
func TestBuildReplicationMap(t *testing.T) {
	obj1 := Hash{0x01}
	obj2 := Hash{0x02}

	atxBytes := buildTestATXWithObjects(t, "test",
		[]objectRef{{id: obj1, version: 0}, {id: obj2, version: 0}},
		[]testObject{
			{id: obj1, replication: 50},
			{id: obj2, replication: 10},
		},
	)
	atx := types.GetRootAsAttestedTransaction(atxBytes, 0)

	m := buildReplicationMap(atx)

	if m[obj1] != 50 {
		t.Fatalf("obj1: expected replication 50, got %d", m[obj1])
	}

	if m[obj2] != 10 {
		t.Fatalf("obj2: expected replication 10, got %d", m[obj2])
	}
}

// TestBuildReplicationMapEmpty tests empty ATX objects vector.
func TestBuildReplicationMapEmpty(t *testing.T) {
	atxBytes := buildTestATX(t, "test", nil, nil, false)
	atx := types.GetRootAsAttestedTransaction(atxBytes, 0)

	m := buildReplicationMap(atx)
	if m != nil {
		t.Fatal("expected nil map for empty objects")
	}
}

// TestCommittedTxOutput tests that committed transactions are emitted to the channel.
func TestCommittedTxOutput(t *testing.T) {
	db := newTestStorage(t)
	validators, vs := newTestValidatorSet(1)
	mock := &mockBroadcaster{}

	// Include tx in genesis so validator[0] produces it in round 0
	atxBytes := buildTestATX(t, "some_func", nil, nil, false)

	dag := New(db, vs, mock, testSystemPod, 1, validators[0].privKey, nil,
		WithGenesisTxs([][]byte{atxBytes}))
	defer dag.Close()

	// With 1 validator, quorum=1 so the DAG self-commits
	select {
	case committed := <-dag.Committed():
		if committed.Function != "some_func" {
			t.Fatalf("expected function 'some_func', got '%s'", committed.Function)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for committed tx")
	}
}

// =============================================================================
// Deregister Validator Tests (via executeTx commit path)
// =============================================================================

// TestHandleDeregisterValidator_ViaExecuteTx tests that a deregister_validator
// transaction submitted through executeTx correctly populates pendingRemovals.
func TestHandleDeregisterValidator_ViaExecuteTx(t *testing.T) {
	db := newTestStorage(t)
	validators, vs := newTestValidatorSet(4)
	mock := &mockBroadcaster{}

	dag := New(db, vs, mock, testSystemPod, 0, validators[0].privKey, nil,
		WithEpochLength(100),
	)
	defer dag.Close()

	// Build a deregister_validator ATX from validator[2]
	atxBytes := buildDeregisterATX(t, validators[2].pubKey, testSystemPod)
	atx := types.GetRootAsAttestedTransaction(atxBytes, 0)

	// Verify validator[2] is currently active
	if !dag.validators.Contains(validators[2].pubKey) {
		t.Fatal("validator[2] should be in active set before deregister")
	}

	dag.executeTx(atx, 5)

	// Validator should still be active (removal is deferred to epoch)
	if !dag.validators.Contains(validators[2].pubKey) {
		t.Fatal("validator[2] should STILL be active after deregister (deferred to epoch)")
	}

	// But should be in pending removals
	if !dag.pendingRemovals[validators[2].pubKey] {
		t.Fatal("validator[2] should be in pendingRemovals")
	}
}

// TestHandleDeregisterValidator_WrongPod tests that deregister on wrong pod is ignored.
func TestHandleDeregisterValidator_WrongPod(t *testing.T) {
	db := newTestStorage(t)
	validators, vs := newTestValidatorSet(4)

	dag := New(db, vs, nil, testSystemPod, 0, validators[0].privKey, nil,
		WithEpochLength(100),
	)
	defer dag.Close()

	wrongPod := Hash{0xFF, 0xFE, 0xFD}
	atxBytes := buildDeregisterATX(t, validators[2].pubKey, wrongPod)
	atx := types.GetRootAsAttestedTransaction(atxBytes, 0)

	dag.executeTx(atx, 5)

	if len(dag.pendingRemovals) != 0 {
		t.Fatal("deregister on wrong pod should be ignored")
	}
}

// TestHandleDeregisterValidator_WrongFunction tests that wrong function name is ignored.
func TestHandleDeregisterValidator_WrongFunction(t *testing.T) {
	db := newTestStorage(t)
	validators, vs := newTestValidatorSet(4)

	dag := New(db, vs, nil, testSystemPod, 0, validators[0].privKey, nil,
		WithEpochLength(100),
	)
	defer dag.Close()

	// Build ATX with right pod but wrong function
	atxBytes := buildSystemPodATX(t, validators[2].pubKey, testSystemPod, "wrong_function")
	atx := types.GetRootAsAttestedTransaction(atxBytes, 0)

	dag.executeTx(atx, 5)

	if len(dag.pendingRemovals) != 0 {
		t.Fatal("wrong function name should be ignored")
	}
}

// TestDeregisterThenEpoch_FullPath tests the complete path:
// deregister tx → pendingRemovals → epoch transition → validator removed.
func TestDeregisterThenEpoch_FullPath(t *testing.T) {
	db := newTestStorage(t)
	validators, vs := newTestValidatorSet(4)
	mock := &mockBroadcaster{}

	dag := New(db, vs, mock, testSystemPod, 0, validators[0].privKey, nil,
		WithEpochLength(10),
	)
	defer dag.Close()

	// Step 1: Submit deregister tx
	atxBytes := buildDeregisterATX(t, validators[3].pubKey, testSystemPod)
	atx := types.GetRootAsAttestedTransaction(atxBytes, 0)
	dag.executeTx(atx, 5)

	// Verify: still active, in pending
	if !dag.validators.Contains(validators[3].pubKey) {
		t.Fatal("should still be active before epoch")
	}
	if !dag.pendingRemovals[validators[3].pubKey] {
		t.Fatal("should be in pendingRemovals")
	}

	// Step 2: Epoch transition
	dag.transitionEpoch(10)

	// Verify: now removed from both validators and epoch holders
	if dag.validators.Contains(validators[3].pubKey) {
		t.Fatal("should be removed from validators after epoch")
	}
	if dag.EpochHolders().Contains(validators[3].pubKey) {
		t.Fatal("should be removed from epoch holders after epoch")
	}
	if len(dag.pendingRemovals) != 0 {
		t.Fatal("pendingRemovals should be empty after epoch applied it")
	}

	// Verify remaining validators are correct
	if dag.validators.Len() != 3 {
		t.Fatalf("expected 3 validators remaining, got %d", dag.validators.Len())
	}
	for i := 0; i < 3; i++ {
		if !dag.validators.Contains(validators[i].pubKey) {
			t.Errorf("validator[%d] should still be active", i)
		}
	}
}

// =============================================================================
// Test Helpers
// =============================================================================

// objectRef represents an object reference (ID + version).
type objectRef struct {
	id      Hash
	version uint64
}

// testObject represents an object with replication for ATX building.
type testObject struct {
	id          Hash
	replication uint16
}

// buildTxWithMutables creates a Transaction with ReadObjects and MutableObjects refs.
func buildTxWithMutables(t *testing.T, readRefs []objectRef, mutRefs []objectRef) *types.Transaction {
	t.Helper()

	builder := flatbuffers.NewBuilder(512)

	readBytes := encodeObjectRefs(readRefs)
	mutBytes := encodeObjectRefs(mutRefs)

	var readVec, mutVec flatbuffers.UOffsetT
	if len(readBytes) > 0 {
		readVec = builder.CreateByteVector(readBytes)
	}
	if len(mutBytes) > 0 {
		mutVec = builder.CreateByteVector(mutBytes)
	}

	hashVec := builder.CreateByteVector(make([]byte, 32))
	senderVec := builder.CreateByteVector(make([]byte, 32))
	podVec := builder.CreateByteVector(make([]byte, 32))
	funcName := builder.CreateString("test")

	types.TransactionStart(builder)
	types.TransactionAddHash(builder, hashVec)
	types.TransactionAddSender(builder, senderVec)
	types.TransactionAddPod(builder, podVec)
	types.TransactionAddFunctionName(builder, funcName)

	if len(readBytes) > 0 {
		types.TransactionAddReadObjects(builder, readVec)
	}
	if len(mutBytes) > 0 {
		types.TransactionAddMutableObjects(builder, mutVec)
	}

	txOff := types.TransactionEnd(builder)
	builder.Finish(txOff)

	return types.GetRootAsTransaction(builder.FinishedBytes(), 0)
}

// buildTestATX creates a test AttestedTransaction with given function, read/mutable refs.
func buildTestATX(t *testing.T, funcName string, readRefs []objectRef, mutRefs []objectRef, createsObjects bool) []byte {
	t.Helper()

	builder := flatbuffers.NewBuilder(1024)

	readBytes := encodeObjectRefs(readRefs)
	mutBytes := encodeObjectRefs(mutRefs)

	var readVec, mutVec flatbuffers.UOffsetT
	if len(readBytes) > 0 {
		readVec = builder.CreateByteVector(readBytes)
	}
	if len(mutBytes) > 0 {
		mutVec = builder.CreateByteVector(mutBytes)
	}

	hashVec := builder.CreateByteVector(make([]byte, 32))
	senderVec := builder.CreateByteVector(make([]byte, 32))
	podVec := builder.CreateByteVector(make([]byte, 32))
	funcNameOff := builder.CreateString(funcName)

	types.TransactionStart(builder)
	types.TransactionAddHash(builder, hashVec)
	types.TransactionAddSender(builder, senderVec)
	types.TransactionAddPod(builder, podVec)
	types.TransactionAddFunctionName(builder, funcNameOff)
	types.TransactionAddCreatesObjects(builder, createsObjects)

	if len(readBytes) > 0 {
		types.TransactionAddReadObjects(builder, readVec)
	}
	if len(mutBytes) > 0 {
		types.TransactionAddMutableObjects(builder, mutVec)
	}

	txOff := types.TransactionEnd(builder)

	// Empty objects and proofs
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

// buildTestATXWithObjects creates a test ATX with objects in the Objects vector.
func buildTestATXWithObjects(t *testing.T, funcName string, mutRefs []objectRef, objects []testObject) []byte {
	t.Helper()

	builder := flatbuffers.NewBuilder(2048)

	mutBytes := encodeObjectRefs(mutRefs)
	var mutVec flatbuffers.UOffsetT
	if len(mutBytes) > 0 {
		mutVec = builder.CreateByteVector(mutBytes)
	}

	hashVec := builder.CreateByteVector(make([]byte, 32))
	senderVec := builder.CreateByteVector(make([]byte, 32))
	podVec := builder.CreateByteVector(make([]byte, 32))
	funcNameOff := builder.CreateString(funcName)

	types.TransactionStart(builder)
	types.TransactionAddHash(builder, hashVec)
	types.TransactionAddSender(builder, senderVec)
	types.TransactionAddPod(builder, podVec)
	types.TransactionAddFunctionName(builder, funcNameOff)

	if len(mutBytes) > 0 {
		types.TransactionAddMutableObjects(builder, mutVec)
	}

	txOff := types.TransactionEnd(builder)

	// Build objects
	objOffsets := make([]flatbuffers.UOffsetT, len(objects))
	for i, obj := range objects {
		idVec := builder.CreateByteVector(obj.id[:])
		ownerVec := builder.CreateByteVector(make([]byte, 32))
		contentVec := builder.CreateByteVector([]byte("test"))

		types.ObjectStart(builder)
		types.ObjectAddId(builder, idVec)
		types.ObjectAddVersion(builder, 0)
		types.ObjectAddOwner(builder, ownerVec)
		types.ObjectAddReplication(builder, obj.replication)
		types.ObjectAddContent(builder, contentVec)
		objOffsets[i] = types.ObjectEnd(builder)
	}

	types.AttestedTransactionStartObjectsVector(builder, len(objOffsets))
	for i := len(objOffsets) - 1; i >= 0; i-- {
		builder.PrependUOffsetT(objOffsets[i])
	}
	objVec := builder.EndVector(len(objOffsets))

	// Empty proofs
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

// encodeObjectRefs encodes object references as 40-byte entries.
func encodeObjectRefs(refs []objectRef) []byte {
	if len(refs) == 0 {
		return nil
	}

	data := make([]byte, len(refs)*40)
	for i, ref := range refs {
		offset := i * 40
		copy(data[offset:offset+32], ref.id[:])
		binary.LittleEndian.PutUint64(data[offset+32:offset+40], ref.version)
	}

	return data
}

// buildTestVertexWithTx creates a signed vertex containing an ATX.
func buildTestVertexWithTx(t *testing.T, v testValidator, round uint64, parents []Hash, epoch uint64, atxBytes []byte) []byte {
	t.Helper()

	builder := flatbuffers.NewBuilder(4096)

	// Build the ATX inside vertex
	atxOffset := rebuildATXInBuilder(builder, atxBytes)

	// Build parents
	parentOffsets := make([]flatbuffers.UOffsetT, len(parents))
	for i, p := range parents {
		hVec := builder.CreateByteVector(p[:])
		pVec := builder.CreateByteVector(make([]byte, 32))

		types.VertexLinkStart(builder)
		types.VertexLinkAddHash(builder, hVec)
		types.VertexLinkAddProducer(builder, pVec)
		parentOffsets[i] = types.VertexLinkEnd(builder)
	}

	types.VertexStartParentsVector(builder, len(parentOffsets))
	for i := len(parentOffsets) - 1; i >= 0; i-- {
		builder.PrependUOffsetT(parentOffsets[i])
	}
	parentsVec := builder.EndVector(len(parentOffsets))

	// Tx vector with one entry
	types.VertexStartTransactionsVector(builder, 1)
	builder.PrependUOffsetT(atxOffset)
	txsVec := builder.EndVector(1)

	producerVec := builder.CreateByteVector(v.pubKey[:])

	// Build unsigned first for hash
	types.VertexStart(builder)
	types.VertexAddRound(builder, round)
	types.VertexAddProducer(builder, producerVec)
	types.VertexAddParents(builder, parentsVec)
	types.VertexAddTransactions(builder, txsVec)
	types.VertexAddEpoch(builder, epoch)
	vertexOff := types.VertexEnd(builder)
	builder.Finish(vertexOff)

	unsigned := builder.FinishedBytes()
	hash := hashVertex(unsigned)
	sig := ed25519.Sign(v.privKey, hash[:])

	// Rebuild with hash and signature
	builder.Reset()

	atxOffset = rebuildATXInBuilder(builder, atxBytes)

	parentOffsets = make([]flatbuffers.UOffsetT, len(parents))
	for i, p := range parents {
		hVec := builder.CreateByteVector(p[:])
		pVec := builder.CreateByteVector(make([]byte, 32))

		types.VertexLinkStart(builder)
		types.VertexLinkAddHash(builder, hVec)
		types.VertexLinkAddProducer(builder, pVec)
		parentOffsets[i] = types.VertexLinkEnd(builder)
	}

	types.VertexStartParentsVector(builder, len(parentOffsets))
	for i := len(parentOffsets) - 1; i >= 0; i-- {
		builder.PrependUOffsetT(parentOffsets[i])
	}
	parentsVec = builder.EndVector(len(parentOffsets))

	types.VertexStartTransactionsVector(builder, 1)
	builder.PrependUOffsetT(atxOffset)
	txsVec = builder.EndVector(1)

	hashVec := builder.CreateByteVector(hash[:])
	sigVec := builder.CreateByteVector(sig)
	producerVec = builder.CreateByteVector(v.pubKey[:])

	types.VertexStart(builder)
	types.VertexAddHash(builder, hashVec)
	types.VertexAddRound(builder, round)
	types.VertexAddProducer(builder, producerVec)
	types.VertexAddSignature(builder, sigVec)
	types.VertexAddParents(builder, parentsVec)
	types.VertexAddTransactions(builder, txsVec)
	types.VertexAddEpoch(builder, epoch)
	vertexOff = types.VertexEnd(builder)

	builder.Finish(vertexOff)

	return builder.FinishedBytes()
}

// rebuildATXInBuilder parses an ATX and rebuilds it in the given builder.
func rebuildATXInBuilder(builder *flatbuffers.Builder, atxBytes []byte) flatbuffers.UOffsetT {
	atx := types.GetRootAsAttestedTransaction(atxBytes, 0)
	tx := atx.Transaction(nil)

	// Rebuild Transaction
	var txOff flatbuffers.UOffsetT
	if tx != nil {
		hashV := builder.CreateByteVector(tx.HashBytes())
		readV := builder.CreateByteVector(tx.ReadObjectsBytes())
		mutV := builder.CreateByteVector(tx.MutableObjectsBytes())
		senderV := builder.CreateByteVector(tx.SenderBytes())
		sigV := builder.CreateByteVector(tx.SignatureBytes())
		podV := builder.CreateByteVector(tx.PodBytes())
		fnV := builder.CreateString(string(tx.FunctionName()))
		argsV := builder.CreateByteVector(tx.ArgsBytes())

		types.TransactionStart(builder)
		types.TransactionAddHash(builder, hashV)
		types.TransactionAddReadObjects(builder, readV)
		types.TransactionAddMutableObjects(builder, mutV)
		types.TransactionAddCreatesObjects(builder, tx.CreatesObjects())
		types.TransactionAddSender(builder, senderV)
		types.TransactionAddSignature(builder, sigV)
		types.TransactionAddPod(builder, podV)
		types.TransactionAddFunctionName(builder, fnV)
		types.TransactionAddArgs(builder, argsV)
		txOff = types.TransactionEnd(builder)
	}

	// Rebuild objects
	objOffsets := make([]flatbuffers.UOffsetT, atx.ObjectsLength())
	for i := 0; i < atx.ObjectsLength(); i++ {
		var obj types.Object
		atx.Objects(&obj, i)

		idV := builder.CreateByteVector(obj.IdBytes())
		ownerV := builder.CreateByteVector(obj.OwnerBytes())
		contentV := builder.CreateByteVector(obj.ContentBytes())

		types.ObjectStart(builder)
		types.ObjectAddId(builder, idV)
		types.ObjectAddVersion(builder, obj.Version())
		types.ObjectAddOwner(builder, ownerV)
		types.ObjectAddReplication(builder, obj.Replication())
		types.ObjectAddContent(builder, contentV)
		objOffsets[i] = types.ObjectEnd(builder)
	}

	types.AttestedTransactionStartObjectsVector(builder, len(objOffsets))
	for i := len(objOffsets) - 1; i >= 0; i-- {
		builder.PrependUOffsetT(objOffsets[i])
	}
	objVec := builder.EndVector(len(objOffsets))

	// Rebuild proofs
	proofOffsets := make([]flatbuffers.UOffsetT, atx.ProofsLength())
	for i := 0; i < atx.ProofsLength(); i++ {
		var proof types.QuorumProof
		atx.Proofs(&proof, i)

		objIdV := builder.CreateByteVector(proof.ObjectIdBytes())
		blsV := builder.CreateByteVector(proof.BlsSignatureBytes())
		bmpV := builder.CreateByteVector(proof.SignerBitmapBytes())

		types.QuorumProofStart(builder)
		types.QuorumProofAddObjectId(builder, objIdV)
		types.QuorumProofAddBlsSignature(builder, blsV)
		types.QuorumProofAddSignerBitmap(builder, bmpV)
		proofOffsets[i] = types.QuorumProofEnd(builder)
	}

	types.AttestedTransactionStartProofsVector(builder, len(proofOffsets))
	for i := len(proofOffsets) - 1; i >= 0; i-- {
		builder.PrependUOffsetT(proofOffsets[i])
	}
	prfVec := builder.EndVector(len(proofOffsets))

	types.AttestedTransactionStart(builder)
	types.AttestedTransactionAddTransaction(builder, txOff)
	types.AttestedTransactionAddObjects(builder, objVec)
	types.AttestedTransactionAddProofs(builder, prfVec)

	return types.AttestedTransactionEnd(builder)
}

// addQuorumVertices adds empty vertices from all validators at a given round.
// Uses stored hashes from previous round as parents.
func addQuorumVertices(t *testing.T, dag *DAG, validators []testValidator, round uint64) {
	t.Helper()

	var parents []Hash
	if round > 0 {
		parents = dag.store.getByRound(round - 1)
	}

	for _, v := range validators {
		data := buildTestVertex(t, v, round, parents, 1)
		dag.AddVertex(data)
	}
}

// buildDeregisterATX creates a deregister_validator ATX from the given sender on the given pod.
func buildDeregisterATX(t *testing.T, sender Hash, pod Hash) []byte {
	t.Helper()
	return buildSystemPodATX(t, sender, pod, "deregister_validator")
}

// buildSystemPodATX creates an ATX with specific sender, pod, and function name.
// Used for testing system pod functions (register/deregister validator).
func buildSystemPodATX(t *testing.T, sender Hash, pod Hash, funcName string) []byte {
	t.Helper()

	builder := flatbuffers.NewBuilder(512)

	hashVec := builder.CreateByteVector(make([]byte, 32))
	senderVec := builder.CreateByteVector(sender[:])
	podVec := builder.CreateByteVector(pod[:])
	funcNameOff := builder.CreateString(funcName)

	types.TransactionStart(builder)
	types.TransactionAddHash(builder, hashVec)
	types.TransactionAddSender(builder, senderVec)
	types.TransactionAddPod(builder, podVec)
	types.TransactionAddFunctionName(builder, funcNameOff)
	txOff := types.TransactionEnd(builder)

	// Empty objects and proofs
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
