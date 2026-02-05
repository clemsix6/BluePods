package state

import (
	"os"
	"testing"

	flatbuffers "github.com/google/flatbuffers/go"

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

// buildTestObject creates a serialized Object for testing.
func buildTestObject(id Hash, version uint64, content []byte) []byte {
	builder := flatbuffers.NewBuilder(256)

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

	builder.Finish(objOffset)

	return builder.FinishedBytes()
}
