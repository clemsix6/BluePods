package state

import (
	"testing"

	"BluePods/internal/types"
)

// TestDomainStore_SetGet verifies set stores and get retrieves the correct ObjectID.
func TestDomainStore_SetGet(t *testing.T) {
	db := newTestStorage(t)
	store := newDomainStore(db)

	objectID := Hash{0x01, 0x02, 0x03}
	store.set(DomainEntry{Name: "example.pod", ObjectID: objectID})

	got, found := store.get("example.pod")
	if !found {
		t.Fatal("expected domain to be found")
	}

	if got.ObjectID != objectID {
		t.Errorf("expected %x, got %x", objectID, got.ObjectID)
	}
}

// TestDomainStore_Exists verifies exists returns true for registered and false for unregistered.
func TestDomainStore_Exists(t *testing.T) {
	db := newTestStorage(t)
	store := newDomainStore(db)

	store.set(DomainEntry{Name: "registered.pod", ObjectID: Hash{0x10}})

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

	store.set(DomainEntry{Name: "to-delete.pod", ObjectID: Hash{0x20}})

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

	store.set(DomainEntry{Name: "overwrite.pod", ObjectID: first})
	store.set(DomainEntry{Name: "overwrite.pod", ObjectID: second})

	got, found := store.get("overwrite.pod")
	if !found {
		t.Fatal("expected domain to be found")
	}

	if got.ObjectID != second {
		t.Errorf("expected second value %x, got %x", second, got.ObjectID)
	}
}

// TestDomainStore_ExportImport verifies export then import into new store preserves data.
func TestDomainStore_ExportImport(t *testing.T) {
	db1 := newTestStorage(t)
	store1 := newDomainStore(db1)

	id1 := Hash{0xAA}
	id2 := Hash{0xBB}
	store1.set(DomainEntry{Name: "alpha.pod", ObjectID: id1})
	store1.set(DomainEntry{Name: "beta.pod", ObjectID: id2})

	entries := store1.export()
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(entries))
	}

	// Import into a fresh store
	db2 := newTestStorage(t)
	store2 := newDomainStore(db2)
	store2.importBatch(entries)

	got1, found1 := store2.get("alpha.pod")
	if !found1 || got1.ObjectID != id1 {
		t.Errorf("alpha.pod: expected %x found=%v, got %x found=%v", id1, true, got1.ObjectID, found1)
	}

	got2, found2 := store2.get("beta.pod")
	if !found2 || got2.ObjectID != id2 {
		t.Errorf("beta.pod: expected %x found=%v, got %x found=%v", id2, true, got2.ObjectID, found2)
	}
}

// TestSetOnDomainRegistered_FiresOnRegisterAndUpdate verifies the callback
// wired through SetOnDomainRegistered fires once per name applied by
// applyRegisteredDomains, both for a first-time binding and a rebind — the
// same two cases that emit DomainRegistered and DomainUpdated — carrying the
// name and the resolved object ID. This is the domain store's only writer
// today, so it is the sole feed point a derived domain index has until
// domain writes become a declared operation.
func TestSetOnDomainRegistered_FiresOnRegisterAndUpdate(t *testing.T) {
	db := newTestStorage(t)
	s := New(db, nil)

	type call struct {
		name     string
		objectID Hash
	}
	var calls []call
	s.SetOnDomainRegistered(func(name string, objectID [32]byte) {
		calls = append(calls, call{name: name, objectID: objectID})
	})

	firstID := Hash{0x01}
	first := buildPodOutputWithDomainsRaw(0, 10, []testDomain{{name: "cb.pod", objectID: firstID}})
	s.applyRegisteredDomains(types.GetRootAsPodExecuteOutput(first, 0), Hash{0xAA})

	secondID := Hash{0x02}
	second := buildPodOutputWithDomainsRaw(0, 10, []testDomain{{name: "cb.pod", objectID: secondID}})
	s.applyRegisteredDomains(types.GetRootAsPodExecuteOutput(second, 0), Hash{0xBB})

	if len(calls) != 2 {
		t.Fatalf("callback fired %d times, want 2", len(calls))
	}
	if calls[0].name != "cb.pod" || calls[0].objectID != firstID {
		t.Errorf("first call = %+v, want name=cb.pod objectID=%x", calls[0], firstID)
	}
	if calls[1].name != "cb.pod" || calls[1].objectID != secondID {
		t.Errorf("second call = %+v, want name=cb.pod objectID=%x", calls[1], secondID)
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

// TestDomainStore_LeafRoundTrip verifies the leaf carries the object, the owner
// and the expiry epoch through a store/load cycle.
func TestDomainStore_LeafRoundTrip(t *testing.T) {
	db := newTestStorage(t)
	store := newDomainStore(db)

	entry := DomainEntry{Name: "lease.pod", ObjectID: Hash{0x11}, Owner: Hash{0x22}, ExpiryEpoch: 42}
	store.set(entry)

	got, found := store.get("lease.pod")
	if !found {
		t.Fatal("expected the leaf to be found")
	}
	if got != entry {
		t.Errorf("leaf = %+v, want %+v", got, entry)
	}
}

// TestDomainStore_LegacyValueDecodes verifies a stored bare 32-byte objectID —
// the pre-rental leaf shape — still decodes, as an unowned lease expiring at
// epoch 0 rather than a lost entry.
func TestDomainStore_LegacyValueDecodes(t *testing.T) {
	db := newTestStorage(t)
	store := newDomainStore(db)

	legacy := Hash{0x77}
	if err := db.Set(store.makeKey("legacy.pod"), legacy[:]); err != nil {
		t.Fatalf("seed legacy value: %v", err)
	}

	got, found := store.get("legacy.pod")
	if !found {
		t.Fatal("expected the legacy leaf to decode")
	}
	if got.ObjectID != legacy || got.Owner != (Hash{}) || got.ExpiryEpoch != 0 {
		t.Errorf("legacy leaf = %+v, want objectID %x with zero owner and expiry", got, legacy)
	}
}

// TestDomainStore_ExportImportCarriesLeaf verifies export/import preserves the
// owner and expiry, not just the object binding: the index root is rebuilt from
// these entries, so a dropped field forks the domain tree.
func TestDomainStore_ExportImportCarriesLeaf(t *testing.T) {
	src := newDomainStore(newTestStorage(t))
	src.set(DomainEntry{Name: "carry.pod", ObjectID: Hash{0xA1}, Owner: Hash{0xB2}, ExpiryEpoch: 9})

	dst := newDomainStore(newTestStorage(t))
	dst.importBatch(src.export())

	got, found := dst.get("carry.pod")
	if !found {
		t.Fatal("expected the imported leaf to be found")
	}
	if got.Owner != (Hash{0xB2}) || got.ExpiryEpoch != 9 {
		t.Errorf("imported leaf = %+v, want owner b2 expiry 9", got)
	}
}

// TestResolveDomain_PastExpiryIsAbsent verifies resolution treats a name past
// its expiry epoch as absent — during the grace window too, which reserves only
// the owner's right to renew, never continued resolution.
func TestResolveDomain_PastExpiryIsAbsent(t *testing.T) {
	db := newTestStorage(t)
	s := New(db, nil)

	epoch := uint64(0)
	s.SetEpochSource(func() uint64 { return epoch })
	s.SetDomainLeaf("lease.pod", Hash{0x33}, Hash{0x44}, 3)

	if id, ok := s.ResolveDomain("lease.pod"); !ok || id != (Hash{0x33}) {
		t.Fatalf("current lease resolved to (%x, %v), want the bound object", id, ok)
	}

	epoch = 3
	if _, ok := s.ResolveDomain("lease.pod"); !ok {
		t.Error("a lease expiring at the current epoch must still resolve")
	}

	epoch = 4
	if _, ok := s.ResolveDomain("lease.pod"); ok {
		t.Error("a lease past its expiry epoch must not resolve")
	}

	if _, _, expiry, ok := s.DomainLeaf("lease.pod"); !ok || expiry != 3 {
		t.Errorf("DomainLeaf = (expiry %d, ok %v), want the stored leaf unchanged", expiry, ok)
	}
}

// TestDomainLeaf_ReportsOwnerAndExpiry verifies the narrow accessor the commit
// path reads ownership and lease state through.
func TestDomainLeaf_ReportsOwnerAndExpiry(t *testing.T) {
	s := New(newTestStorage(t), nil)

	s.SetDomainLeaf("owned.pod", Hash{0x01}, Hash{0x02}, 7)

	objectID, owner, expiry, ok := s.DomainLeaf("owned.pod")
	if !ok || objectID != (Hash{0x01}) || owner != (Hash{0x02}) || expiry != 7 {
		t.Fatalf("DomainLeaf = (%x, %x, %d, %v), want the stored leaf", objectID, owner, expiry, ok)
	}

	s.DeleteDomainLeaf("owned.pod")

	if _, _, _, ok := s.DomainLeaf("owned.pod"); ok {
		t.Error("a deleted name must report absent")
	}
}
