package sync

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"BluePods/internal/storage"
)

// mockSnapshotProvider implements SnapshotProvider for testing.
type mockSnapshotProvider struct {
	round      uint64
	validators [][]byte
}

func (m *mockSnapshotProvider) LastCommittedRound() uint64 {
	return m.round
}

func (m *mockSnapshotProvider) Validators() [][]byte {
	return m.validators
}

func createTestStorageForManager(t *testing.T) (*storage.Storage, func()) {
	t.Helper()

	dir, err := os.MkdirTemp("", "manager_test_*")
	if err != nil {
		t.Fatalf("create temp dir: %v", err)
	}

	db, err := storage.New(filepath.Join(dir, "db"))
	if err != nil {
		os.RemoveAll(dir)
		t.Fatalf("create storage: %v", err)
	}

	cleanup := func() {
		db.Close()
		os.RemoveAll(dir)
	}

	return db, cleanup
}

func TestSnapshotManager_CreatesInitialSnapshot(t *testing.T) {
	db, cleanup := createTestStorageForManager(t)
	defer cleanup()

	provider := &mockSnapshotProvider{round: 10}
	manager := NewSnapshotManager(db, provider)

	manager.Start()
	defer manager.Stop()

	// Wait for initial snapshot
	time.Sleep(100 * time.Millisecond)

	data, round := manager.Latest()
	if data == nil {
		t.Fatal("expected initial snapshot to be created")
	}

	if round != 10 {
		t.Errorf("round = %d, want 10", round)
	}
}

func TestSnapshotManager_UpdatesOnNewRound(t *testing.T) {
	db, cleanup := createTestStorageForManager(t)
	defer cleanup()

	provider := &mockSnapshotProvider{round: 5}

	// Use shorter interval for testing
	manager := NewSnapshotManager(db, provider)
	manager.interval = 50 * time.Millisecond

	manager.Start()
	defer manager.Stop()

	// Wait for initial snapshot
	time.Sleep(100 * time.Millisecond)

	_, round1 := manager.Latest()
	if round1 != 5 {
		t.Errorf("initial round = %d, want 5", round1)
	}

	// Update round
	provider.round = 20

	// Wait for next snapshot cycle
	time.Sleep(100 * time.Millisecond)

	_, round2 := manager.Latest()
	if round2 != 20 {
		t.Errorf("updated round = %d, want 20", round2)
	}
}

func TestSnapshotManager_SkipsIfNoNewCommits(t *testing.T) {
	db, cleanup := createTestStorageForManager(t)
	defer cleanup()

	provider := &mockSnapshotProvider{round: 10}

	manager := NewSnapshotManager(db, provider)
	manager.interval = 50 * time.Millisecond

	manager.Start()
	defer manager.Stop()

	// Wait for initial snapshot
	time.Sleep(100 * time.Millisecond)

	data1, _ := manager.Latest()

	// Wait for another cycle (should skip since round hasn't changed)
	time.Sleep(100 * time.Millisecond)

	data2, _ := manager.Latest()

	// Should be the same snapshot (pointer comparison)
	if &data1[0] != &data2[0] {
		t.Error("snapshot should not be recreated if round hasn't changed")
	}
}

func TestSnapshotManager_StopsCleanly(t *testing.T) {
	db, cleanup := createTestStorageForManager(t)
	defer cleanup()

	provider := &mockSnapshotProvider{round: 0}
	manager := NewSnapshotManager(db, provider)

	manager.Start()

	// Stop should not hang
	done := make(chan struct{})
	go func() {
		manager.Stop()
		close(done)
	}()

	select {
	case <-done:
		// OK
	case <-time.After(time.Second):
		t.Fatal("Stop() timed out")
	}
}

func TestSnapshotManager_LatestBeforeStart(t *testing.T) {
	db, cleanup := createTestStorageForManager(t)
	defer cleanup()

	provider := &mockSnapshotProvider{round: 0}
	manager := NewSnapshotManager(db, provider)

	// Don't start the manager
	data, round := manager.Latest()

	if data != nil {
		t.Error("expected nil data before start")
	}

	if round != 0 {
		t.Error("expected round 0 before start")
	}
}
