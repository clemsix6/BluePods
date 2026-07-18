package state

import (
	"testing"

	"BluePods/internal/types"
)

// TestReparentObject_RewritesHeldBodyOwner asserts that after the reparent hook
// fires, a held body shows the new owner and parent kind through the same store
// read GetObject uses.
func TestReparentObject_RewritesHeldBodyOwner(t *testing.T) {
	db := newTestStorage(t)
	s := New(db, nil)

	id := Hash{0x01}
	oldOwner := Hash{0xA1}
	newParent := Hash{0xD3}

	s.SetObject(buildTestObjectFull(id, 7, oldOwner, 3, []byte("held content")))

	s.ReparentObject(id, 1, newParent) // ObjectParent

	data := s.GetObject(id)
	if data == nil {
		t.Fatal("held object vanished after reparent")
	}

	obj := types.GetRootAsObject(data, 0)
	if got := obj.OwnerBytes(); Hash(sliceTo32(got)) != newParent {
		t.Errorf("body owner = %x, want new parent %x", got[:8], newParent[:8])
	}
	if k := obj.ParentKind(); k != 1 {
		t.Errorf("body parent_kind = %d, want 1 (ObjectParent)", k)
	}

	// Other fields survive the rewrite.
	if obj.Version() != 7 {
		t.Errorf("version changed by reparent: %d, want 7", obj.Version())
	}
	if obj.Replication() != 3 {
		t.Errorf("replication changed by reparent: %d, want 3", obj.Replication())
	}
	if string(obj.ContentBytes()) != "held content" {
		t.Errorf("content changed by reparent: %q", obj.ContentBytes())
	}
}

// TestReparentObject_NonHolderNoOps asserts a node that never held the object
// (nothing stored) processes the reparent without creating a body.
func TestReparentObject_NonHolderNoOps(t *testing.T) {
	db := newTestStorage(t)
	s := New(db, nil)

	id := Hash{0x02}

	s.ReparentObject(id, 0, Hash{0xB2})

	if s.GetObject(id) != nil {
		t.Error("reparent created a body on a non-holder")
	}
}

// sliceTo32 copies a byte slice into a fixed 32-byte array for comparison.
func sliceTo32(b []byte) [32]byte {
	var out [32]byte
	copy(out[:], b)
	return out
}
