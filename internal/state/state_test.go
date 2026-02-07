package state

import (
	"bytes"
	"encoding/binary"
	"os"
	"testing"

	flatbuffers "github.com/google/flatbuffers/go"
	"github.com/zeebo/blake3"

	"BluePods/internal/storage"
	"BluePods/internal/types"
)

// newTestStorage creates a temporary storage for testing.
func newTestStorage(t *testing.T) *storage.Storage {
	t.Helper()

	dir, err := os.MkdirTemp("", "state_test_*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}

	t.Cleanup(func() {
		os.RemoveAll(dir)
	})

	db, err := storage.New(dir)
	if err != nil {
		t.Fatalf("failed to create storage: %v", err)
	}

	t.Cleanup(func() {
		db.Close()
	})

	return db
}

func TestObjectStore(t *testing.T) {
	db := newTestStorage(t)
	store := newObjectStore(db)

	id := Hash{0x01, 0x02, 0x03}
	data := []byte("test object data")

	// Set and get
	store.set(id, data)
	got := store.get(id)

	if string(got) != string(data) {
		t.Errorf("expected %s, got %s", data, got)
	}

	// Get non-existent
	unknown := Hash{0xff}
	if store.get(unknown) != nil {
		t.Error("expected nil for unknown ID")
	}

	// Delete
	store.delete(id)
	if store.get(id) != nil {
		t.Error("expected nil after delete")
	}
}

func TestNewState(t *testing.T) {
	db := newTestStorage(t)
	state := New(db, nil)

	if state.objects == nil {
		t.Error("objects store not initialized")
	}
}

func TestStateSetGetObject(t *testing.T) {
	db := newTestStorage(t)
	state := New(db, nil)

	// Build a simple object
	obj := buildTestObject(Hash{0x01}, 1, []byte("content"))

	state.SetObject(obj)

	got := state.GetObject(Hash{0x01})
	if got == nil {
		t.Error("expected object, got nil")
	}
}

// --- computeObjectID ---

// TestComputeObjectID verifies deterministic ID generation: blake3(txHash || index_u32_LE).
func TestComputeObjectID(t *testing.T) {
	txHash := Hash{0xAA, 0xBB, 0xCC}

	id0 := computeObjectID(txHash, 0)
	id1 := computeObjectID(txHash, 1)

	// Different indices produce different IDs
	if id0 == id1 {
		t.Error("different indices should produce different IDs")
	}

	// Same inputs produce same ID (deterministic)
	id0Again := computeObjectID(txHash, 0)
	if id0 != id0Again {
		t.Error("same inputs should produce same ID")
	}

	// Verify against manual computation
	var buf [36]byte
	copy(buf[:32], txHash[:])
	binary.LittleEndian.PutUint32(buf[32:], 0)
	expected := blake3.Sum256(buf[:])

	if id0 != expected {
		t.Errorf("computeObjectID mismatch: got %x, want %x", id0, expected)
	}
}

// TestComputeObjectIDDifferentHash verifies different tx hashes yield different IDs.
func TestComputeObjectIDDifferentHash(t *testing.T) {
	hash1 := Hash{0x01}
	hash2 := Hash{0x02}

	id1 := computeObjectID(hash1, 0)
	id2 := computeObjectID(hash2, 0)

	if id1 == id2 {
		t.Error("different tx hashes should produce different IDs")
	}
}

// --- resolveMutableObjects ---

// TestResolveMutableObjects verifies that singletons missing from ATX are loaded from local state.
func TestResolveMutableObjects(t *testing.T) {
	db := newTestStorage(t)
	s := New(db, nil)

	// Store a singleton locally (simulating a pre-existing singleton)
	singletonID := Hash{0x10}
	singletonData := buildTestObjectWithReplication(singletonID, 5, []byte("singleton"), 0)
	s.SetObject(singletonData)

	// Build a transaction referencing the singleton in MutableObjects
	tx := buildTxWithMutableRefs([]objectRef{{id: singletonID, version: 5}})

	// No ATX objects (singleton excluded from ATX body)
	atxObjects := []*types.Object{}

	result := s.resolveMutableObjects(tx, atxObjects)

	// Should have resolved the singleton from local state
	if len(result) != 1 {
		t.Fatalf("expected 1 resolved object, got %d", len(result))
	}

	var resolvedID Hash
	copy(resolvedID[:], result[0].IdBytes())
	if resolvedID != singletonID {
		t.Errorf("resolved wrong object: got %x, want %x", resolvedID, singletonID)
	}
}

// TestResolveMutableObjectsAlreadyPresent verifies objects already in ATX are not duplicated.
func TestResolveMutableObjectsAlreadyPresent(t *testing.T) {
	db := newTestStorage(t)
	s := New(db, nil)

	objID := Hash{0x20}
	objData := buildTestObject(objID, 3, []byte("content"))
	s.SetObject(objData)

	// Object present in both ATX and MutableObjects ref
	tx := buildTxWithMutableRefs([]objectRef{{id: objID, version: 3}})

	existingObj := types.GetRootAsObject(objData, 0)
	atxObjects := []*types.Object{existingObj}

	result := s.resolveMutableObjects(tx, atxObjects)

	// Should still be just 1 (not duplicated)
	if len(result) != 1 {
		t.Fatalf("expected 1 object (no duplicate), got %d", len(result))
	}
}

// TestResolveMutableObjectsMissing verifies graceful handling when object is not in local state.
func TestResolveMutableObjectsMissing(t *testing.T) {
	db := newTestStorage(t)
	s := New(db, nil)

	// Reference an object we don't have locally
	missingID := Hash{0x30}
	tx := buildTxWithMutableRefs([]objectRef{{id: missingID, version: 1}})

	result := s.resolveMutableObjects(tx, []*types.Object{})

	// Should return empty (missing object silently skipped)
	if len(result) != 0 {
		t.Fatalf("expected 0 objects for missing ref, got %d", len(result))
	}
}

// --- ensureMutableVersions ---

// TestEnsureMutableVersions verifies version bump for objects not returned by pod.
func TestEnsureMutableVersions(t *testing.T) {
	db := newTestStorage(t)
	s := New(db, nil)

	objID := Hash{0x40}
	// Store object at version 5
	objData := buildTestObject(objID, 5, []byte("data"))
	s.SetObject(objData)

	// Transaction declares MutableObject at version 5 → should bump to 6
	tx := buildTxWithMutableRefs([]objectRef{{id: objID, version: 5}})

	s.ensureMutableVersions(tx)

	// Verify version was bumped to 6
	stored := s.GetObject(objID)
	if stored == nil {
		t.Fatal("object should still exist after version bump")
	}

	obj := types.GetRootAsObject(stored, 0)
	if obj.Version() != 6 {
		t.Errorf("expected version 6, got %d", obj.Version())
	}
}

// TestEnsureMutableVersionsAlreadyUpdated verifies no double-bump when pod already updated.
func TestEnsureMutableVersionsAlreadyUpdated(t *testing.T) {
	db := newTestStorage(t)
	s := New(db, nil)

	objID := Hash{0x41}
	// Object already at target version 6 (pod returned it in UpdatedObjects)
	objData := buildTestObject(objID, 6, []byte("data"))
	s.SetObject(objData)

	// Transaction declared version 5 → target is 6, object already at 6
	tx := buildTxWithMutableRefs([]objectRef{{id: objID, version: 5}})

	s.ensureMutableVersions(tx)

	stored := s.GetObject(objID)
	obj := types.GetRootAsObject(stored, 0)

	// Should still be 6 (not bumped to 7)
	if obj.Version() != 6 {
		t.Errorf("expected version 6 (no double bump), got %d", obj.Version())
	}
}

// TestEnsureMutableVersionsMultiple verifies version bump for multiple objects.
func TestEnsureMutableVersionsMultiple(t *testing.T) {
	db := newTestStorage(t)
	s := New(db, nil)

	id1 := Hash{0x42}
	id2 := Hash{0x43}

	s.SetObject(buildTestObject(id1, 1, []byte("obj1")))
	s.SetObject(buildTestObject(id2, 3, []byte("obj2")))

	tx := buildTxWithMutableRefs([]objectRef{
		{id: id1, version: 1},
		{id: id2, version: 3},
	})

	s.ensureMutableVersions(tx)

	obj1 := types.GetRootAsObject(s.GetObject(id1), 0)
	obj2 := types.GetRootAsObject(s.GetObject(id2), 0)

	if obj1.Version() != 2 {
		t.Errorf("obj1: expected version 2, got %d", obj1.Version())
	}

	if obj2.Version() != 4 {
		t.Errorf("obj2: expected version 4, got %d", obj2.Version())
	}
}

// --- applyCreatedObjects ---

// TestApplyCreatedObjectsDeterministicIDs verifies blake3(txHash||index) ID assignment.
func TestApplyCreatedObjectsDeterministicIDs(t *testing.T) {
	db := newTestStorage(t)
	s := New(db, nil)

	// No holder check → store everything
	s.SetIsHolder(func(objectID [32]byte, replication uint16) bool {
		return true
	})

	txHash := Hash{0xDE, 0xAD}
	output := buildPodOutputWithCreated(2, 10) // 2 objects, replication=10

	s.applyCreatedObjects(output, txHash)

	// Verify deterministic IDs
	for i := 0; i < 2; i++ {
		expectedID := computeObjectID(txHash, uint32(i))
		stored := s.GetObject(expectedID)

		if stored == nil {
			t.Errorf("object %d not stored under deterministic ID %x", i, expectedID[:4])
		}
	}
}

// TestApplyCreatedObjectsHolderSharding verifies only holder stores the object.
func TestApplyCreatedObjectsHolderSharding(t *testing.T) {
	db := newTestStorage(t)
	s := New(db, nil)

	// Only hold objects with ID starting with 0x00
	heldID := Hash{}
	s.SetIsHolder(func(objectID [32]byte, replication uint16) bool {
		return objectID == heldID
	})

	txHash := Hash{0x50}
	output := buildPodOutputWithCreated(1, 10)

	// Pre-compute the ID to know what isHolder will see
	expectedID := computeObjectID(txHash, 0)
	heldID = expectedID // Make isHolder return true for this ID

	s.applyCreatedObjects(output, txHash)

	stored := s.GetObject(expectedID)
	if stored == nil {
		t.Error("holder should store the object")
	}
}

// TestApplyCreatedObjectsNotHolder verifies non-holder skips storage.
func TestApplyCreatedObjectsNotHolder(t *testing.T) {
	db := newTestStorage(t)
	s := New(db, nil)

	// Never a holder
	s.SetIsHolder(func(objectID [32]byte, replication uint16) bool {
		return false
	})

	txHash := Hash{0x51}
	output := buildPodOutputWithCreated(1, 10)

	s.applyCreatedObjects(output, txHash)

	expectedID := computeObjectID(txHash, 0)
	if s.GetObject(expectedID) != nil {
		t.Error("non-holder should not store the object")
	}
}

// TestApplyCreatedObjectsNoHolderFunc verifies all objects stored when isHolder is nil.
func TestApplyCreatedObjectsNoHolderFunc(t *testing.T) {
	db := newTestStorage(t)
	s := New(db, nil)
	// isHolder is nil by default

	txHash := Hash{0x52}
	output := buildPodOutputWithCreated(1, 10)

	s.applyCreatedObjects(output, txHash)

	expectedID := computeObjectID(txHash, 0)
	if s.GetObject(expectedID) == nil {
		t.Error("should store object when isHolder is nil")
	}
}

// --- applyUpdatedObjects ---

// TestApplyUpdatedObjects verifies version increment on updated objects.
func TestApplyUpdatedObjects(t *testing.T) {
	db := newTestStorage(t)
	s := New(db, nil)

	objID := Hash{0x60}
	output := buildPodOutputWithUpdated(objID, 3, []byte("new content"))

	s.applyUpdatedObjects(output)

	stored := s.GetObject(objID)
	if stored == nil {
		t.Fatal("updated object should be stored")
	}

	obj := types.GetRootAsObject(stored, 0)

	// Version should be incremented by 1 (3 → 4)
	if obj.Version() != 4 {
		t.Errorf("expected version 4, got %d", obj.Version())
	}

	if !bytes.Equal(obj.ContentBytes(), []byte("new content")) {
		t.Errorf("content mismatch: got %s", obj.ContentBytes())
	}
}

// --- applyDeletedObjects ---

// TestApplyDeletedObjects verifies objects are removed from state.
func TestApplyDeletedObjects(t *testing.T) {
	db := newTestStorage(t)
	s := New(db, nil)

	objID := Hash{0x70}
	s.SetObject(buildTestObject(objID, 1, []byte("to delete")))

	// Verify it exists
	if s.GetObject(objID) == nil {
		t.Fatal("object should exist before deletion")
	}

	output := buildPodOutputWithDeleted(objID)

	s.applyDeletedObjects(output)

	if s.GetObject(objID) != nil {
		t.Error("object should be deleted")
	}
}

// --- extractObjectID ---

// TestExtractObjectID verifies object ID extraction from serialized bytes.
func TestExtractObjectID(t *testing.T) {
	id := Hash{0x80, 0x81, 0x82}
	data := buildTestObject(id, 1, []byte("content"))

	extracted := extractObjectID(data)
	if extracted != id {
		t.Errorf("expected %x, got %x", id, extracted)
	}
}

// --- rebuildObjectIncrementVersion ---

// TestRebuildObjectIncrementVersion verifies the rebuild preserves all fields.
func TestRebuildObjectIncrementVersion(t *testing.T) {
	id := Hash{0x90}
	owner := Hash{0xA0}
	content := []byte("rebuild test")

	original := buildTestObjectFull(id, 5, owner, 15, content)
	obj := types.GetRootAsObject(original, 0)

	rebuilt := rebuildObjectIncrementVersion(obj)
	result := types.GetRootAsObject(rebuilt, 0)

	// Version should be 6
	if result.Version() != 6 {
		t.Errorf("expected version 6, got %d", result.Version())
	}

	// Other fields preserved
	var resultID Hash
	copy(resultID[:], result.IdBytes())
	if resultID != id {
		t.Error("ID not preserved")
	}

	if result.Replication() != 15 {
		t.Errorf("expected replication 15, got %d", result.Replication())
	}

	if !bytes.Equal(result.ContentBytes(), content) {
		t.Error("content not preserved")
	}
}

// --- helpers ---

// objectRef represents a mutable object reference (ID + version).
type objectRef struct {
	id      Hash
	version uint64
}

// buildTestObject creates a serialized Object with replication=10 for testing.
func buildTestObject(id Hash, version uint64, content []byte) []byte {
	return buildTestObjectWithReplication(id, version, content, 10)
}

// buildTestObjectWithReplication creates a serialized Object with specified replication.
func buildTestObjectWithReplication(id Hash, version uint64, content []byte, replication uint16) []byte {
	return buildTestObjectFull(id, version, Hash{}, replication, content)
}

// buildTestObjectFull creates a serialized Object with all fields.
func buildTestObjectFull(id Hash, version uint64, owner Hash, replication uint16, content []byte) []byte {
	builder := flatbuffers.NewBuilder(256)

	idVec := builder.CreateByteVector(id[:])
	ownerVec := builder.CreateByteVector(owner[:])
	contentVec := builder.CreateByteVector(content)

	types.ObjectStart(builder)
	types.ObjectAddId(builder, idVec)
	types.ObjectAddVersion(builder, version)
	types.ObjectAddOwner(builder, ownerVec)
	types.ObjectAddReplication(builder, replication)
	types.ObjectAddContent(builder, contentVec)
	objOffset := types.ObjectEnd(builder)

	builder.Finish(objOffset)

	return builder.FinishedBytes()
}

// buildTxWithMutableRefs creates a Transaction with MutableObjects references.
func buildTxWithMutableRefs(refs []objectRef) *types.Transaction {
	builder := flatbuffers.NewBuilder(512)

	// Encode mutable refs: each is 32-byte ID + 8-byte version LE
	mutableData := make([]byte, len(refs)*40)
	for i, ref := range refs {
		copy(mutableData[i*40:i*40+32], ref.id[:])
		binary.LittleEndian.PutUint64(mutableData[i*40+32:i*40+40], ref.version)
	}

	mutVec := builder.CreateByteVector(mutableData)
	senderVec := builder.CreateByteVector(make([]byte, 32))
	podVec := builder.CreateByteVector(make([]byte, 32))
	funcName := builder.CreateString("test")
	argsVec := builder.CreateByteVector([]byte{})

	types.TransactionStart(builder)
	types.TransactionAddMutableObjects(builder, mutVec)
	types.TransactionAddSender(builder, senderVec)
	types.TransactionAddPod(builder, podVec)
	types.TransactionAddFunctionName(builder, funcName)
	types.TransactionAddArgs(builder, argsVec)
	txOffset := types.TransactionEnd(builder)

	builder.Finish(txOffset)

	return types.GetRootAsTransaction(builder.FinishedBytes(), 0)
}

// buildPodOutputWithCreated creates a PodExecuteOutput with created objects.
func buildPodOutputWithCreated(count int, replication uint16) *types.PodExecuteOutput {
	builder := flatbuffers.NewBuilder(1024)

	// Build created objects (without real IDs - they get assigned by applyCreatedObjects)
	offsets := make([]flatbuffers.UOffsetT, count)
	for i := count - 1; i >= 0; i-- {
		idVec := builder.CreateByteVector(make([]byte, 32))
		ownerVec := builder.CreateByteVector(make([]byte, 32))
		contentVec := builder.CreateByteVector([]byte("created"))

		types.ObjectStart(builder)
		types.ObjectAddId(builder, idVec)
		types.ObjectAddVersion(builder, 0)
		types.ObjectAddOwner(builder, ownerVec)
		types.ObjectAddReplication(builder, replication)
		types.ObjectAddContent(builder, contentVec)
		offsets[i] = types.ObjectEnd(builder)
	}

	types.PodExecuteOutputStartCreatedObjectsVector(builder, count)
	for i := count - 1; i >= 0; i-- {
		builder.PrependUOffsetT(offsets[i])
	}
	createdVec := builder.EndVector(count)

	// Empty updated and deleted
	types.PodExecuteOutputStartUpdatedObjectsVector(builder, 0)
	updatedVec := builder.EndVector(0)

	deletedVec := builder.CreateByteVector([]byte{})

	types.PodExecuteOutputStart(builder)
	types.PodExecuteOutputAddCreatedObjects(builder, createdVec)
	types.PodExecuteOutputAddUpdatedObjects(builder, updatedVec)
	types.PodExecuteOutputAddDeletedObjects(builder, deletedVec)
	outputOffset := types.PodExecuteOutputEnd(builder)

	builder.Finish(outputOffset)

	return types.GetRootAsPodExecuteOutput(builder.FinishedBytes(), 0)
}

// buildPodOutputWithUpdated creates a PodExecuteOutput with an updated object.
func buildPodOutputWithUpdated(id Hash, version uint64, content []byte) *types.PodExecuteOutput {
	builder := flatbuffers.NewBuilder(512)

	idVec := builder.CreateByteVector(id[:])
	ownerVec := builder.CreateByteVector(make([]byte, 32))
	contentVec := builder.CreateByteVector(content)

	types.ObjectStart(builder)
	types.ObjectAddId(builder, idVec)
	types.ObjectAddVersion(builder, version)
	types.ObjectAddOwner(builder, ownerVec)
	types.ObjectAddReplication(builder, 10)
	types.ObjectAddContent(builder, contentVec)
	objOffset := types.ObjectEnd(builder)

	types.PodExecuteOutputStartUpdatedObjectsVector(builder, 1)
	builder.PrependUOffsetT(objOffset)
	updatedVec := builder.EndVector(1)

	types.PodExecuteOutputStartCreatedObjectsVector(builder, 0)
	createdVec := builder.EndVector(0)

	deletedVec := builder.CreateByteVector([]byte{})

	types.PodExecuteOutputStart(builder)
	types.PodExecuteOutputAddUpdatedObjects(builder, updatedVec)
	types.PodExecuteOutputAddCreatedObjects(builder, createdVec)
	types.PodExecuteOutputAddDeletedObjects(builder, deletedVec)
	outputOffset := types.PodExecuteOutputEnd(builder)

	builder.Finish(outputOffset)

	return types.GetRootAsPodExecuteOutput(builder.FinishedBytes(), 0)
}

// buildPodOutputWithDeleted creates a PodExecuteOutput with a deleted object ID.
func buildPodOutputWithDeleted(id Hash) *types.PodExecuteOutput {
	builder := flatbuffers.NewBuilder(256)

	deletedVec := builder.CreateByteVector(id[:])

	types.PodExecuteOutputStartUpdatedObjectsVector(builder, 0)
	updatedVec := builder.EndVector(0)

	types.PodExecuteOutputStartCreatedObjectsVector(builder, 0)
	createdVec := builder.EndVector(0)

	types.PodExecuteOutputStart(builder)
	types.PodExecuteOutputAddDeletedObjects(builder, deletedVec)
	types.PodExecuteOutputAddUpdatedObjects(builder, updatedVec)
	types.PodExecuteOutputAddCreatedObjects(builder, createdVec)
	outputOffset := types.PodExecuteOutputEnd(builder)

	builder.Finish(outputOffset)

	return types.GetRootAsPodExecuteOutput(builder.FinishedBytes(), 0)
}
