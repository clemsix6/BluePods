package consensus

import (
	"os"
	"path/filepath"
	"testing"

	"BluePods/internal/storage"
)

// newTrackerTestStorage creates a temporary storage for tracker tests.
func newTrackerTestStorage(t *testing.T) *storage.Storage {
	t.Helper()

	dir, err := os.MkdirTemp("", "tracker_test_*")
	if err != nil {
		t.Fatalf("create temp dir: %v", err)
	}

	t.Cleanup(func() { os.RemoveAll(dir) })

	db, err := storage.New(filepath.Join(dir, "db"))
	if err != nil {
		t.Fatalf("create storage: %v", err)
	}

	t.Cleanup(func() { db.Close() })

	return db
}

// TestNewObjectTracker tests creating an empty tracker.
func TestNewObjectTracker(t *testing.T) {
	db := newTrackerTestStorage(t)
	ot := newObjectTracker(db)

	entries := ot.Export()
	if len(entries) != 0 {
		t.Fatalf("expected empty tracker, got %d entries", len(entries))
	}
}

// TestCheckAndUpdate_Success tests matching versions pass.
func TestCheckAndUpdate_Success(t *testing.T) {
	db := newTrackerTestStorage(t)
	ot := newObjectTracker(db)

	objID := Hash{0x01}
	tx := buildTxWithMutables(t, nil, []objectRef{{id: objID, version: 0}})

	if !ot.checkAndUpdate(tx) {
		t.Fatal("expected success (version 0 matches default)")
	}
}

// TestCheckAndUpdate_VersionConflict tests mismatch fails.
func TestCheckAndUpdate_VersionConflict(t *testing.T) {
	db := newTrackerTestStorage(t)
	ot := newObjectTracker(db)

	objID := Hash{0x01}
	ot.trackObject(objID, 5, 0, 0)

	tx := buildTxWithMutables(t, nil, []objectRef{{id: objID, version: 0}})

	if ot.checkAndUpdate(tx) {
		t.Fatal("expected failure: version 0 != stored 5")
	}
}

// TestCheckAndUpdate_MutableVersionIncrement tests version bumped after success.
func TestCheckAndUpdate_MutableVersionIncrement(t *testing.T) {
	db := newTrackerTestStorage(t)
	ot := newObjectTracker(db)

	objID := Hash{0x01}
	tx := buildTxWithMutables(t, nil, []objectRef{{id: objID, version: 0}})

	ot.checkAndUpdate(tx)

	if v := ot.getVersion(objID); v != 1 {
		t.Fatalf("expected version 1, got %d", v)
	}
}

// TestCheckAndUpdate_ReadOnlyNotIncremented tests ReadObjects versions not bumped.
func TestCheckAndUpdate_ReadOnlyNotIncremented(t *testing.T) {
	db := newTrackerTestStorage(t)
	ot := newObjectTracker(db)

	readObj := Hash{0x01}
	mutObj := Hash{0x02}

	tx := buildTxWithMutables(t,
		[]objectRef{{id: readObj, version: 0}},
		[]objectRef{{id: mutObj, version: 0}},
	)

	ot.checkAndUpdate(tx)

	// Read object should still be at version 0
	if v := ot.getVersion(readObj); v != 0 {
		t.Fatalf("read object version: expected 0, got %d", v)
	}

	// Mutable object should be incremented
	if v := ot.getVersion(mutObj); v != 1 {
		t.Fatalf("mutable object version: expected 1, got %d", v)
	}
}

// TestCheckAndUpdate_UnknownObjectDefaultsZero tests missing key → version=0.
func TestCheckAndUpdate_UnknownObjectDefaultsZero(t *testing.T) {
	db := newTrackerTestStorage(t)
	ot := newObjectTracker(db)

	unknown := Hash{0xFF}

	if v := ot.getVersion(unknown); v != 0 {
		t.Fatalf("expected version 0 for unknown object, got %d", v)
	}
}

// TestTrackObject_StoresVersionAndReplication tests explicit creation tracking.
func TestTrackObject_StoresVersionAndReplication(t *testing.T) {
	db := newTrackerTestStorage(t)
	ot := newObjectTracker(db)

	objID := Hash{0x42}
	ot.trackObject(objID, 1, 10, 0)

	if v := ot.getVersion(objID); v != 1 {
		t.Fatalf("expected version 1, got %d", v)
	}

	if r := ot.getReplication(objID); r != 10 {
		t.Fatalf("expected replication 10, got %d", r)
	}
}

// TestTrackObject_GetReplication tests replication retrieval.
func TestTrackObject_GetReplication(t *testing.T) {
	db := newTrackerTestStorage(t)
	ot := newObjectTracker(db)

	// Singleton (replication=0)
	singleton := Hash{0x01}
	ot.trackObject(singleton, 1, 0, 0)

	if r := ot.getReplication(singleton); r != 0 {
		t.Fatalf("expected replication 0, got %d", r)
	}

	// Standard object
	standard := Hash{0x02}
	ot.trackObject(standard, 1, 50, 0)

	if r := ot.getReplication(standard); r != 50 {
		t.Fatalf("expected replication 50, got %d", r)
	}
}

// TestExportImport_Roundtrip tests export all, import to new tracker, verify.
func TestExportImport_Roundtrip(t *testing.T) {
	db1 := newTrackerTestStorage(t)
	ot1 := newObjectTracker(db1)

	obj1 := Hash{0x01}
	obj2 := Hash{0x02}
	obj3 := Hash{0x03}

	ot1.trackObject(obj1, 5, 10, 0)
	ot1.trackObject(obj2, 10, 20, 0)
	ot1.trackObject(obj3, 15, 0, 0)

	entries := ot1.Export()
	if len(entries) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(entries))
	}

	db2 := newTrackerTestStorage(t)
	ot2 := newObjectTracker(db2)
	ot2.Import(entries)

	for _, e := range entries {
		if v := ot2.getVersion(e.ID); v != e.Version {
			t.Fatalf("id %x: expected version %d, got %d", e.ID[:4], e.Version, v)
		}

		if r := ot2.getReplication(e.ID); r != e.Replication {
			t.Fatalf("id %x: expected replication %d, got %d", e.ID[:4], e.Replication, r)
		}
	}
}

// TestExportImport_WithReplication tests replication preserved in roundtrip.
func TestExportImport_WithReplication(t *testing.T) {
	db1 := newTrackerTestStorage(t)
	ot1 := newObjectTracker(db1)

	objID := Hash{0xAA}
	ot1.trackObject(objID, 3, 42, 0)

	entries := ot1.Export()

	db2 := newTrackerTestStorage(t)
	ot2 := newObjectTracker(db2)
	ot2.Import(entries)

	if r := ot2.getReplication(objID); r != 42 {
		t.Fatalf("expected replication 42, got %d", r)
	}
}

// TestMultipleTransactions_SequentialVersions tests v0→v1→v2 across txs.
func TestMultipleTransactions_SequentialVersions(t *testing.T) {
	db := newTrackerTestStorage(t)
	ot := newObjectTracker(db)

	objID := Hash{0x01}

	for i := uint64(0); i < 10; i++ {
		tx := buildTxWithMutables(t, nil, []objectRef{{id: objID, version: i}})

		if !ot.checkAndUpdate(tx) {
			t.Fatalf("tx at version %d should succeed", i)
		}

		if v := ot.getVersion(objID); v != i+1 {
			t.Fatalf("after version %d: expected %d, got %d", i, i+1, v)
		}
	}
}

// TestConflictAfterIncrement tests second tx with old version fails.
func TestConflictAfterIncrement(t *testing.T) {
	db := newTrackerTestStorage(t)
	ot := newObjectTracker(db)

	objID := Hash{0x01}

	tx1 := buildTxWithMutables(t, nil, []objectRef{{id: objID, version: 0}})
	if !ot.checkAndUpdate(tx1) {
		t.Fatal("tx1 should succeed")
	}

	// tx2 with stale version 0 should fail
	tx2 := buildTxWithMutables(t, nil, []objectRef{{id: objID, version: 0}})
	if ot.checkAndUpdate(tx2) {
		t.Fatal("tx2 should fail: version conflict")
	}

	// tx3 with correct version 1 should succeed
	tx3 := buildTxWithMutables(t, nil, []objectRef{{id: objID, version: 1}})
	if !ot.checkAndUpdate(tx3) {
		t.Fatal("tx3 should succeed with version 1")
	}
}

// TestMixedReadMutableObjects tests read + mutable in same tx.
func TestMixedReadMutableObjects(t *testing.T) {
	db := newTrackerTestStorage(t)
	ot := newObjectTracker(db)

	readObj := Hash{0x01}
	mutObj := Hash{0x02}

	// Both start at version 0
	tx := buildTxWithMutables(t,
		[]objectRef{{id: readObj, version: 0}},
		[]objectRef{{id: mutObj, version: 0}},
	)

	if !ot.checkAndUpdate(tx) {
		t.Fatal("tx should succeed")
	}

	// readObj stays at 0, mutObj bumped to 1
	if v := ot.getVersion(readObj); v != 0 {
		t.Fatalf("readObj: expected 0, got %d", v)
	}

	if v := ot.getVersion(mutObj); v != 1 {
		t.Fatalf("mutObj: expected 1, got %d", v)
	}
}

// TestTrackAllObjects tests that ALL committed tx objects are tracked.
func TestTrackAllObjects(t *testing.T) {
	db := newTrackerTestStorage(t)
	ot := newObjectTracker(db)

	// Track 100 objects
	for i := 0; i < 100; i++ {
		var id Hash
		id[0] = byte(i)
		id[1] = byte(i >> 8)
		ot.trackObject(id, 1, uint16(i%50), 0)
	}

	entries := ot.Export()
	if len(entries) != 100 {
		t.Fatalf("expected 100 entries, got %d", len(entries))
	}
}

// TestPebblePersistence tests close storage, reopen, verify data survives.
func TestPebblePersistence(t *testing.T) {
	dir, err := os.MkdirTemp("", "tracker_persist_*")
	if err != nil {
		t.Fatalf("create temp dir: %v", err)
	}
	defer os.RemoveAll(dir)

	dbPath := filepath.Join(dir, "db")

	// Write data
	db1, err := storage.New(dbPath)
	if err != nil {
		t.Fatalf("create storage: %v", err)
	}

	ot1 := newObjectTracker(db1)
	objID := Hash{0x42}
	ot1.trackObject(objID, 7, 25, 0)
	db1.Close()

	// Reopen and verify
	db2, err := storage.New(dbPath)
	if err != nil {
		t.Fatalf("reopen storage: %v", err)
	}
	defer db2.Close()

	ot2 := newObjectTracker(db2)

	if v := ot2.getVersion(objID); v != 7 {
		t.Fatalf("expected version 7 after reopen, got %d", v)
	}

	if r := ot2.getReplication(objID); r != 25 {
		t.Fatalf("expected replication 25 after reopen, got %d", r)
	}
}

// TestBatchImport_LargeDataset tests import 10k entries efficiently.
func TestBatchImport_LargeDataset(t *testing.T) {
	db := newTrackerTestStorage(t)
	ot := newObjectTracker(db)

	entries := make([]ObjectTrackerEntry, 10000)
	for i := range entries {
		var id Hash
		id[0] = byte(i)
		id[1] = byte(i >> 8)
		id[2] = byte(i >> 16)

		entries[i] = ObjectTrackerEntry{
			ID:          id,
			Version:     uint64(i + 1),
			Replication: uint16(i % 100),
		}
	}

	ot.Import(entries)

	// Spot check a few
	exported := ot.Export()
	if len(exported) != 10000 {
		t.Fatalf("expected 10000 entries, got %d", len(exported))
	}

	// Verify first entry
	if v := ot.getVersion(entries[0].ID); v != 1 {
		t.Fatalf("first entry: expected version 1, got %d", v)
	}

	// Verify last entry
	if v := ot.getVersion(entries[9999].ID); v != 10000 {
		t.Fatalf("last entry: expected version 10000, got %d", v)
	}
}

// TestCheckAndUpdate_EmptyTransaction tests tx with no object refs.
func TestCheckAndUpdate_EmptyTransaction(t *testing.T) {
	db := newTrackerTestStorage(t)
	ot := newObjectTracker(db)

	tx := buildTxWithMutables(t, nil, nil)

	if !ot.checkAndUpdate(tx) {
		t.Fatal("empty tx should succeed")
	}
}

// TestDeleteObject tests removing an object from the tracker.
func TestDeleteObject(t *testing.T) {
	db := newTrackerTestStorage(t)
	ot := newObjectTracker(db)

	objID := Hash{0x01}
	ot.trackObject(objID, 5, 10, 0)

	if v := ot.getVersion(objID); v != 5 {
		t.Fatalf("expected version 5, got %d", v)
	}

	ot.deleteObject(objID)

	// After deletion, version should return 0 (default)
	if v := ot.getVersion(objID); v != 0 {
		t.Fatalf("expected version 0 after delete, got %d", v)
	}
}

// TestIncrementPreservesReplication tests that incrementing version preserves replication.
func TestIncrementPreservesReplication(t *testing.T) {
	db := newTrackerTestStorage(t)
	ot := newObjectTracker(db)

	objID := Hash{0x01}
	ot.trackObject(objID, 0, 42, 0)

	tx := buildTxWithMutables(t, nil, []objectRef{{id: objID, version: 0}})
	if !ot.checkAndUpdate(tx) {
		t.Fatal("tx should succeed")
	}

	// Version should be incremented, replication preserved
	if v := ot.getVersion(objID); v != 1 {
		t.Fatalf("expected version 1, got %d", v)
	}

	if r := ot.getReplication(objID); r != 42 {
		t.Fatalf("expected replication 42 (preserved), got %d", r)
	}
}
