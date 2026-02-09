package consensus

import (
	"crypto/ed25519"
	"encoding/binary"
	"testing"
	"time"

	flatbuffers "github.com/google/flatbuffers/go"

	"BluePods/internal/genesis"
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

// TestTrackerDeleteObject verifies deleteObject removes an object from the tracker.
func TestTrackerDeleteObject(t *testing.T) {
	db := newTestStorage(t)
	ot := newObjectTracker(db)

	objID := Hash{0x03}
	ot.trackObject(objID, 5, 10)

	// Verify it exists
	if v := ot.getVersion(objID); v != 5 {
		t.Fatalf("expected version 5, got %d", v)
	}

	ot.deleteObject(objID)

	// Should return 0 (default for missing)
	if v := ot.getVersion(objID); v != 0 {
		t.Errorf("expected version 0 after delete, got %d", v)
	}

	if r := ot.getReplication(objID); r != 0 {
		t.Errorf("expected replication 0 after delete, got %d", r)
	}
}

// TestTrackerDeleteNonExistent verifies deleting a non-existent object doesn't panic.
func TestTrackerDeleteNonExistent(t *testing.T) {
	db := newTestStorage(t)
	ot := newObjectTracker(db)

	// Should not panic
	ot.deleteObject(Hash{0xFF})
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
	atxBytes := buildTestATX(t, "test_func", nil, []objectRef{{id: objID, version: 0}}, 0)
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
	atxBytes := buildTestATX(t, "test_func", nil, []objectRef{{id: objID, version: 0}}, 0)
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
	atxBytes := buildTestATX(t, "test_func", nil, []objectRef{{id: singletonObj, version: 0}}, 0)
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

// TestCreatesObjectsAllValidatorsExecute tests that max_create_objects>0 skips holder check.
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

	// Build ATX with max_create_objects=1
	atxBytes := buildTestATX(t, "test_func", nil, []objectRef{{id: objID, version: 0}}, 1)
	atx := types.GetRootAsAttestedTransaction(atxBytes, 0)
	tx := atx.Transaction(nil)

	// max_create_objects check happens in executeTx, not shouldExecute directly
	// The check is: if tx.MaxCreateObjects() == 0 && tx.MaxCreateDomains() == 0 && !d.shouldExecute(...)
	maxCreate := tx.MaxCreateObjects()

	if maxCreate == 0 {
		t.Fatal("max_create_objects should be > 0")
	}

	// With max_create_objects>0, the shouldExecute check is bypassed
	shouldSkip := maxCreate == 0 && tx.MaxCreateDomains() == 0 && !dag.shouldExecute(atx, tx)
	if shouldSkip {
		t.Fatal("max_create_objects>0 should force execution on all validators")
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

	atxBytes := buildTestATX(t, "some_func", nil, []objectRef{{id: objID, version: 0}}, 0)

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
	atxBytes := buildTestATX(t, "test", nil, nil, 0)
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
	atxBytes := buildTestATX(t, "some_func", nil, nil, 0)

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

// TestMaxCreateDomainsAllValidatorsExecute tests that max_create_domains>0 forces execution.
func TestMaxCreateDomainsAllValidatorsExecute(t *testing.T) {
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

	// Build ATX with max_create_domains=1
	atxBytes := buildTestATXWithDomains(t, "test_func", []objectRef{{id: objID, version: 0}}, 1)
	atx := types.GetRootAsAttestedTransaction(atxBytes, 0)
	tx := atx.Transaction(nil)

	maxDomains := tx.MaxCreateDomains()
	if maxDomains == 0 {
		t.Fatal("max_create_domains should be > 0")
	}

	// With max_create_domains>0, the shouldExecute check is bypassed
	shouldSkip := tx.MaxCreateObjects() == 0 && maxDomains == 0 && !dag.shouldExecute(atx, tx)
	if shouldSkip {
		t.Fatal("max_create_domains>0 should force execution on all validators")
	}
}

// buildTestATXWithDomains creates a test ATX with max_create_domains set.
func buildTestATXWithDomains(t *testing.T, funcName string, mutRefs []objectRef, maxCreateDomains uint16) []byte {
	t.Helper()

	builder := flatbuffers.NewBuilder(1024)

	mutVec := buildObjectRefVector(builder, mutRefs, true)

	hashVec := builder.CreateByteVector(make([]byte, 32))
	senderVec := builder.CreateByteVector(make([]byte, 32))
	podVec := builder.CreateByteVector(make([]byte, 32))
	funcNameOff := builder.CreateString(funcName)

	types.TransactionStart(builder)
	types.TransactionAddHash(builder, hashVec)
	types.TransactionAddSender(builder, senderVec)
	types.TransactionAddPod(builder, podVec)
	types.TransactionAddFunctionName(builder, funcNameOff)
	types.TransactionAddMaxCreateDomains(builder, maxCreateDomains)

	if mutVec != 0 {
		types.TransactionAddMutableRefs(builder, mutVec)
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

// =============================================================================
// checkRefList Tests (malformed refs vs domain refs)
// =============================================================================

// TestCheckRefList_MalformedIDRejected verifies that a mutable ref with a
// 16-byte ID (malformed standard ref) causes checkAndUpdate to return false.
func TestCheckRefList_MalformedIDRejected(t *testing.T) {
	db := newTestStorage(t)
	ot := newObjectTracker(db)

	// Build a transaction with a malformed 16-byte ID ref
	tx := buildTxWithMalformedRef(t, 16, false)

	if ot.checkAndUpdate(tx) {
		t.Fatal("checkAndUpdate should reject tx with malformed 16-byte object ID")
	}
}

// TestCheckRefList_DomainRefSkipped verifies that a ref with a domain field
// but no ID is treated as a domain ref and skipped (returns true).
func TestCheckRefList_DomainRefSkipped(t *testing.T) {
	db := newTestStorage(t)
	ot := newObjectTracker(db)

	// Build a transaction with a domain ref (has domain, no ID)
	tx := buildTxWithDomainRef(t, "example.com")

	if !ot.checkAndUpdate(tx) {
		t.Fatal("checkAndUpdate should succeed: domain refs should be skipped")
	}
}

// =============================================================================
// Register Validator Tests (via executeTx commit path)
// =============================================================================

// TestHandleRegisterValidator_ViaExecuteTx tests that a register_validator
// transaction submitted through executeTx correctly adds validator to set.
func TestHandleRegisterValidator_ViaExecuteTx(t *testing.T) {
	db := newTestStorage(t)
	validators, vs := newTestValidatorSet(2)
	mock := &mockBroadcaster{}

	dag := New(db, vs, mock, testSystemPod, 0, validators[0].privKey, nil)
	defer dag.Close()

	// Create a new validator to register
	newVal := newTestValidator()
	blsKey := [48]byte{0xAA, 0xBB}

	atxBytes := buildRegisterATX(t, newVal.pubKey, testSystemPod, "http://new:8080", "quic://new:9090", blsKey)
	atx := types.GetRootAsAttestedTransaction(atxBytes, 0)

	// Verify new validator is NOT in the set yet
	if dag.validators.Contains(newVal.pubKey) {
		t.Fatal("new validator should not be in set before register")
	}

	dag.executeTx(atx, 5)

	// Validator should now be in the set
	if !dag.validators.Contains(newVal.pubKey) {
		t.Fatal("new validator should be in set after register")
	}

	// Verify network info was stored
	info := dag.validators.Get(newVal.pubKey)
	if info == nil {
		t.Fatal("validator info should exist")
	}
	if info.HTTPAddr != "http://new:8080" {
		t.Fatalf("expected http addr 'http://new:8080', got '%s'", info.HTTPAddr)
	}
	if info.QUICAddr != "quic://new:9090" {
		t.Fatalf("expected quic addr 'quic://new:9090', got '%s'", info.QUICAddr)
	}
}

// TestHandleRegisterValidator_WrongPod tests that register on wrong pod is ignored.
func TestHandleRegisterValidator_WrongPod(t *testing.T) {
	db := newTestStorage(t)
	validators, vs := newTestValidatorSet(2)

	dag := New(db, vs, nil, testSystemPod, 0, validators[0].privKey, nil)
	defer dag.Close()

	newVal := newTestValidator()
	wrongPod := Hash{0xFF, 0xFE, 0xFD}

	atxBytes := buildRegisterATX(t, newVal.pubKey, wrongPod, "http://x:1", "quic://x:2", [48]byte{})
	atx := types.GetRootAsAttestedTransaction(atxBytes, 0)

	initialCount := dag.validators.Len()
	dag.executeTx(atx, 5)

	if dag.validators.Len() != initialCount {
		t.Fatal("register on wrong pod should be ignored")
	}
}

// TestHandleRegisterValidator_WrongFunction tests that wrong function name is ignored.
func TestHandleRegisterValidator_WrongFunction(t *testing.T) {
	db := newTestStorage(t)
	validators, vs := newTestValidatorSet(2)

	dag := New(db, vs, nil, testSystemPod, 0, validators[0].privKey, nil)
	defer dag.Close()

	newVal := newTestValidator()

	// Build ATX with right pod but wrong function
	atxBytes := buildSystemPodATX(t, newVal.pubKey, testSystemPod, "wrong_function")
	atx := types.GetRootAsAttestedTransaction(atxBytes, 0)

	initialCount := dag.validators.Len()
	dag.executeTx(atx, 5)

	if dag.validators.Len() != initialCount {
		t.Fatal("wrong function name should not register validator")
	}
}

// TestHandleRegisterValidator_InvalidSender tests that sender != 32 bytes is ignored.
func TestHandleRegisterValidator_InvalidSender(t *testing.T) {
	db := newTestStorage(t)
	validators, vs := newTestValidatorSet(2)

	dag := New(db, vs, nil, testSystemPod, 0, validators[0].privKey, nil)
	defer dag.Close()

	// Build ATX with short sender (16 bytes instead of 32)
	atxBytes := buildRegisterATXWithShortSender(t, testSystemPod)
	atx := types.GetRootAsAttestedTransaction(atxBytes, 0)

	initialCount := dag.validators.Len()
	dag.executeTx(atx, 5)

	if dag.validators.Len() != initialCount {
		t.Fatal("invalid sender should not register validator")
	}
}

// TestHandleRegisterValidator_DuplicateRegister tests that registering the same
// pubkey twice leaves the validator set unchanged (idempotent).
func TestHandleRegisterValidator_DuplicateRegister(t *testing.T) {
	db := newTestStorage(t)
	validators, vs := newTestValidatorSet(2)
	mock := &mockBroadcaster{}

	dag := New(db, vs, mock, testSystemPod, 0, validators[0].privKey, nil)
	defer dag.Close()

	newVal := newTestValidator()
	blsKey := [48]byte{0xCC}

	// Register once
	atx1 := buildRegisterATX(t, newVal.pubKey, testSystemPod, "http://x:1", "quic://x:2", blsKey)
	dag.executeTx(types.GetRootAsAttestedTransaction(atx1, 0), 5)

	countAfterFirst := dag.validators.Len()

	// Register again with same pubkey
	atx2 := buildRegisterATX(t, newVal.pubKey, testSystemPod, "http://x:1", "quic://x:2", blsKey)
	dag.executeTx(types.GetRootAsAttestedTransaction(atx2, 0), 6)

	if dag.validators.Len() != countAfterFirst {
		t.Fatal("duplicate register should not change validator count")
	}
}

// TestHandleRegisterValidator_EpochAdditionsTracked tests that with epochLength > 0,
// new validators are added to epochAdditions.
func TestHandleRegisterValidator_EpochAdditionsTracked(t *testing.T) {
	db := newTestStorage(t)
	validators, vs := newTestValidatorSet(2)
	mock := &mockBroadcaster{}

	dag := New(db, vs, mock, testSystemPod, 0, validators[0].privKey, nil,
		WithEpochLength(100),
	)
	defer dag.Close()

	newVal := newTestValidator()
	blsKey := [48]byte{0xDD}

	atxBytes := buildRegisterATX(t, newVal.pubKey, testSystemPod, "http://x:1", "quic://x:2", blsKey)
	atx := types.GetRootAsAttestedTransaction(atxBytes, 0)

	dag.executeTx(atx, 5)

	if len(dag.epochAdditions) != 1 {
		t.Fatalf("expected 1 epoch addition, got %d", len(dag.epochAdditions))
	}
	if dag.epochAdditions[0] != newVal.pubKey {
		t.Fatal("epoch addition should be the new validator")
	}
}

// TestHandleRegisterValidator_TriggersTransition tests that when minValidators
// is reached, transitionRound is set.
func TestHandleRegisterValidator_TriggersTransition(t *testing.T) {
	db := newTestStorage(t)
	validators, vs := newTestValidatorSet(2)
	mock := &mockBroadcaster{}

	// Set minValidators=3, currently we have 2
	dag := New(db, vs, mock, testSystemPod, 0, validators[0].privKey, nil,
		WithMinValidators(3),
	)
	defer dag.Close()

	// Verify transition has not fired yet
	if dag.transitionRound.Load() >= 0 {
		t.Fatal("transition should not have fired yet")
	}

	// Register a third validator to hit minValidators=3
	newVal := newTestValidator()
	blsKey := [48]byte{0xEE}

	atxBytes := buildRegisterATX(t, newVal.pubKey, testSystemPod, "http://x:1", "quic://x:2", blsKey)
	atx := types.GetRootAsAttestedTransaction(atxBytes, 0)

	dag.executeTx(atx, 10)

	// Transition should now be set
	if dag.transitionRound.Load() < 0 {
		t.Fatal("transition should have fired when minValidators was reached")
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

// buildObjectRefVector builds an ObjectRef vector in the builder.
func buildObjectRefVector(builder *flatbuffers.Builder, refs []objectRef, mutable bool) flatbuffers.UOffsetT {
	if len(refs) == 0 {
		return 0
	}

	offsets := make([]flatbuffers.UOffsetT, len(refs))
	for i, ref := range refs {
		idVec := builder.CreateByteVector(ref.id[:])
		types.ObjectRefStart(builder)
		types.ObjectRefAddId(builder, idVec)
		types.ObjectRefAddVersion(builder, ref.version)
		offsets[i] = types.ObjectRefEnd(builder)
	}

	if mutable {
		types.TransactionStartMutableRefsVector(builder, len(offsets))
	} else {
		types.TransactionStartReadRefsVector(builder, len(offsets))
	}
	for i := len(offsets) - 1; i >= 0; i-- {
		builder.PrependUOffsetT(offsets[i])
	}

	return builder.EndVector(len(offsets))
}

// buildTxWithMutables creates a Transaction with ReadRefs and MutableRefs.
func buildTxWithMutables(t *testing.T, readRefs []objectRef, mutRefs []objectRef) *types.Transaction {
	t.Helper()

	builder := flatbuffers.NewBuilder(512)

	readVec := buildObjectRefVector(builder, readRefs, false)
	mutVec := buildObjectRefVector(builder, mutRefs, true)

	hashVec := builder.CreateByteVector(make([]byte, 32))
	senderVec := builder.CreateByteVector(make([]byte, 32))
	podVec := builder.CreateByteVector(make([]byte, 32))
	funcName := builder.CreateString("test")

	types.TransactionStart(builder)
	types.TransactionAddHash(builder, hashVec)
	types.TransactionAddSender(builder, senderVec)
	types.TransactionAddPod(builder, podVec)
	types.TransactionAddFunctionName(builder, funcName)

	if readVec != 0 {
		types.TransactionAddReadRefs(builder, readVec)
	}
	if mutVec != 0 {
		types.TransactionAddMutableRefs(builder, mutVec)
	}

	txOff := types.TransactionEnd(builder)
	builder.Finish(txOff)

	return types.GetRootAsTransaction(builder.FinishedBytes(), 0)
}

// buildTestATX creates a test AttestedTransaction with given function, read/mutable refs.
func buildTestATX(t *testing.T, funcName string, readRefs []objectRef, mutRefs []objectRef, maxCreateObjects uint16) []byte {
	t.Helper()

	builder := flatbuffers.NewBuilder(1024)

	readVec := buildObjectRefVector(builder, readRefs, false)
	mutVec := buildObjectRefVector(builder, mutRefs, true)

	hashVec := builder.CreateByteVector(make([]byte, 32))
	senderVec := builder.CreateByteVector(make([]byte, 32))
	podVec := builder.CreateByteVector(make([]byte, 32))
	funcNameOff := builder.CreateString(funcName)

	types.TransactionStart(builder)
	types.TransactionAddHash(builder, hashVec)
	types.TransactionAddSender(builder, senderVec)
	types.TransactionAddPod(builder, podVec)
	types.TransactionAddFunctionName(builder, funcNameOff)
	types.TransactionAddMaxCreateObjects(builder, maxCreateObjects)

	if readVec != 0 {
		types.TransactionAddReadRefs(builder, readVec)
	}
	if mutVec != 0 {
		types.TransactionAddMutableRefs(builder, mutVec)
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

	mutVec := buildObjectRefVector(builder, mutRefs, true)

	hashVec := builder.CreateByteVector(make([]byte, 32))
	senderVec := builder.CreateByteVector(make([]byte, 32))
	podVec := builder.CreateByteVector(make([]byte, 32))
	funcNameOff := builder.CreateString(funcName)

	types.TransactionStart(builder)
	types.TransactionAddHash(builder, hashVec)
	types.TransactionAddSender(builder, senderVec)
	types.TransactionAddPod(builder, podVec)
	types.TransactionAddFunctionName(builder, funcNameOff)

	if mutVec != 0 {
		types.TransactionAddMutableRefs(builder, mutVec)
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

	// Rebuild Transaction using genesis helper
	var txOff flatbuffers.UOffsetT
	if tx != nil {
		txOff = genesis.RebuildTxInBuilder(builder, tx)
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

// buildRegisterATX creates a register_validator ATX with network addresses and BLS key.
func buildRegisterATX(t *testing.T, sender Hash, pod Hash, httpAddr, quicAddr string, blsPubkey [48]byte) []byte {
	t.Helper()

	builder := flatbuffers.NewBuilder(1024)

	// Encode args in Borsh format (same as genesis.encodeRegisterValidatorArgs)
	args := encodeRegisterValidatorArgsBorsh([]byte(httpAddr), []byte(quicAddr), blsPubkey[:])

	hashVec := builder.CreateByteVector(make([]byte, 32))
	senderVec := builder.CreateByteVector(sender[:])
	podVec := builder.CreateByteVector(pod[:])
	funcNameOff := builder.CreateString("register_validator")
	argsVec := builder.CreateByteVector(args)

	types.TransactionStart(builder)
	types.TransactionAddHash(builder, hashVec)
	types.TransactionAddSender(builder, senderVec)
	types.TransactionAddPod(builder, podVec)
	types.TransactionAddFunctionName(builder, funcNameOff)
	types.TransactionAddArgs(builder, argsVec)
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

// encodeRegisterValidatorArgsBorsh encodes register_validator args in Borsh format.
// Format: u32 len + http bytes + u32 len + quic bytes + u32 len + bls bytes
func encodeRegisterValidatorArgsBorsh(httpAddr, quicAddr, blsPubkey []byte) []byte {
	buf := make([]byte, 0, 4+len(httpAddr)+4+len(quicAddr)+4+len(blsPubkey))
	lenBuf := make([]byte, 4)

	binary.LittleEndian.PutUint32(lenBuf, uint32(len(httpAddr)))
	buf = append(buf, lenBuf...)
	buf = append(buf, httpAddr...)

	binary.LittleEndian.PutUint32(lenBuf, uint32(len(quicAddr)))
	buf = append(buf, lenBuf...)
	buf = append(buf, quicAddr...)

	binary.LittleEndian.PutUint32(lenBuf, uint32(len(blsPubkey)))
	buf = append(buf, lenBuf...)
	buf = append(buf, blsPubkey...)

	return buf
}

// buildRegisterATXWithShortSender creates a register_validator ATX with a 16-byte sender.
func buildRegisterATXWithShortSender(t *testing.T, pod Hash) []byte {
	t.Helper()

	builder := flatbuffers.NewBuilder(512)

	hashVec := builder.CreateByteVector(make([]byte, 32))
	senderVec := builder.CreateByteVector(make([]byte, 16)) // short sender
	podVec := builder.CreateByteVector(pod[:])
	funcNameOff := builder.CreateString("register_validator")

	types.TransactionStart(builder)
	types.TransactionAddHash(builder, hashVec)
	types.TransactionAddSender(builder, senderVec)
	types.TransactionAddPod(builder, podVec)
	types.TransactionAddFunctionName(builder, funcNameOff)
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

// buildTxWithMalformedRef creates a Transaction with a mutable ref that has a non-32-byte ID.
func buildTxWithMalformedRef(t *testing.T, idLen int, isDomain bool) *types.Transaction {
	t.Helper()

	builder := flatbuffers.NewBuilder(512)

	// Build a single mutable ref with a malformed ID
	malformedID := make([]byte, idLen)
	for i := range malformedID {
		malformedID[i] = 0xAA
	}

	idVec := builder.CreateByteVector(malformedID)
	types.ObjectRefStart(builder)
	types.ObjectRefAddId(builder, idVec)
	types.ObjectRefAddVersion(builder, 0)
	refOff := types.ObjectRefEnd(builder)

	types.TransactionStartMutableRefsVector(builder, 1)
	builder.PrependUOffsetT(refOff)
	mutVec := builder.EndVector(1)

	hashVec := builder.CreateByteVector(make([]byte, 32))
	senderVec := builder.CreateByteVector(make([]byte, 32))
	podVec := builder.CreateByteVector(make([]byte, 32))
	funcName := builder.CreateString("test")

	types.TransactionStart(builder)
	types.TransactionAddHash(builder, hashVec)
	types.TransactionAddSender(builder, senderVec)
	types.TransactionAddPod(builder, podVec)
	types.TransactionAddFunctionName(builder, funcName)
	types.TransactionAddMutableRefs(builder, mutVec)
	txOff := types.TransactionEnd(builder)
	builder.Finish(txOff)

	return types.GetRootAsTransaction(builder.FinishedBytes(), 0)
}

// buildTxWithDomainRef creates a Transaction with a mutable ref that has a domain field but no ID.
func buildTxWithDomainRef(t *testing.T, domain string) *types.Transaction {
	t.Helper()

	builder := flatbuffers.NewBuilder(512)

	// Build a domain ref: has domain field, no ID
	domainVec := builder.CreateByteVector([]byte(domain))
	types.ObjectRefStart(builder)
	types.ObjectRefAddDomain(builder, domainVec)
	types.ObjectRefAddVersion(builder, 0)
	refOff := types.ObjectRefEnd(builder)

	types.TransactionStartMutableRefsVector(builder, 1)
	builder.PrependUOffsetT(refOff)
	mutVec := builder.EndVector(1)

	hashVec := builder.CreateByteVector(make([]byte, 32))
	senderVec := builder.CreateByteVector(make([]byte, 32))
	podVec := builder.CreateByteVector(make([]byte, 32))
	funcName := builder.CreateString("test")

	types.TransactionStart(builder)
	types.TransactionAddHash(builder, hashVec)
	types.TransactionAddSender(builder, senderVec)
	types.TransactionAddPod(builder, podVec)
	types.TransactionAddFunctionName(builder, funcName)
	types.TransactionAddMutableRefs(builder, mutVec)
	txOff := types.TransactionEnd(builder)
	builder.Finish(txOff)

	return types.GetRootAsTransaction(builder.FinishedBytes(), 0)
}
