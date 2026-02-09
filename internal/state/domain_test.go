package state

import (
	"testing"
)

// TestDomainStore_SetGet verifies set stores and get retrieves the correct ObjectID.
func TestDomainStore_SetGet(t *testing.T) {
	db := newTestStorage(t)
	store := newDomainStore(db)

	objectID := Hash{0x01, 0x02, 0x03}
	store.set("example.pod", objectID)

	got, found := store.get("example.pod")
	if !found {
		t.Fatal("expected domain to be found")
	}

	if got != objectID {
		t.Errorf("expected %x, got %x", objectID, got)
	}
}

// TestDomainStore_Exists verifies exists returns true for registered and false for unregistered.
func TestDomainStore_Exists(t *testing.T) {
	db := newTestStorage(t)
	store := newDomainStore(db)

	store.set("registered.pod", Hash{0x10})

	if !store.exists("registered.pod") {
		t.Error("expected exists=true for registered domain")
	}

	if store.exists("unregistered.pod") {
		t.Error("expected exists=false for unregistered domain")
	}
}

// TestDomainStore_Delete verifies delete removes the domain.
func TestDomainStore_Delete(t *testing.T) {
	db := newTestStorage(t)
	store := newDomainStore(db)

	store.set("to-delete.pod", Hash{0x20})

	if !store.exists("to-delete.pod") {
		t.Fatal("domain should exist before delete")
	}

	store.delete("to-delete.pod")

	if store.exists("to-delete.pod") {
		t.Error("domain should not exist after delete")
	}
}

// TestDomainStore_Overwrite verifies set same name twice uses second value.
func TestDomainStore_Overwrite(t *testing.T) {
	db := newTestStorage(t)
	store := newDomainStore(db)

	first := Hash{0x01}
	second := Hash{0x02}

	store.set("overwrite.pod", first)
	store.set("overwrite.pod", second)

	got, found := store.get("overwrite.pod")
	if !found {
		t.Fatal("expected domain to be found")
	}

	if got != second {
		t.Errorf("expected second value %x, got %x", second, got)
	}
}

// TestDomainStore_ExportImport verifies export then import into new store preserves data.
func TestDomainStore_ExportImport(t *testing.T) {
	db1 := newTestStorage(t)
	store1 := newDomainStore(db1)

	id1 := Hash{0xAA}
	id2 := Hash{0xBB}
	store1.set("alpha.pod", id1)
	store1.set("beta.pod", id2)

	entries := store1.export()
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(entries))
	}

	// Import into a fresh store
	db2 := newTestStorage(t)
	store2 := newDomainStore(db2)
	store2.importBatch(entries)

	got1, found1 := store2.get("alpha.pod")
	if !found1 || got1 != id1 {
		t.Errorf("alpha.pod: expected %x found=%v, got %x found=%v", id1, true, got1, found1)
	}

	got2, found2 := store2.get("beta.pod")
	if !found2 || got2 != id2 {
		t.Errorf("beta.pod: expected %x found=%v, got %x found=%v", id2, true, got2, found2)
	}
}

// TestDomainStore_GetMissing verifies get returns false for non-existent domain.
func TestDomainStore_GetMissing(t *testing.T) {
	db := newTestStorage(t)
	store := newDomainStore(db)

	_, found := store.get("nonexistent.pod")
	if found {
		t.Error("expected found=false for non-existent domain")
	}
}
