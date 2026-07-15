package sync

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"BluePods/internal/consensus"
	"BluePods/internal/events"
	"BluePods/internal/storage"
)

// mockSnapshotProvider implements SnapshotProvider for testing.
type mockSnapshotProvider struct {
	round uint64
	db    *storage.Storage // db backs the consistent-cut storage snapshot
}

func (m *mockSnapshotProvider) Round() uint64 {
	return m.round
}

// ExportConsistentCut returns a cut whose cursor and round are the mock round and
// whose storage snapshot is taken from the manager's backing db, so CreateSnapshot
// can collect (empty) object state from a real consistent view.
func (m *mockSnapshotProvider) ExportConsistentCut(historyRounds uint64) consensus.ConsistentCut {
	return consensus.ConsistentCut{
		Cursor:     m.round,
		Round:      m.round,
		DBSnapshot: m.db.Snapshot(),
	}
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

	provider := &mockSnapshotProvider{round: 10, db: db}
	manager := NewSnapshotManager(db, provider)

	manager.Start()
	defer manager.Stop()

	// Wait for initial snapshot (loop waits 2s before first snapshot)
	time.Sleep(3 * time.Second)

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

	provider := &mockSnapshotProvider{round: 5, db: db}

	// Use shorter interval for testing
	manager := NewSnapshotManager(db, provider)
	manager.interval = 50 * time.Millisecond

	manager.Start()
	defer manager.Stop()

	// Wait for initial snapshot (loop waits 2s before first snapshot)
	time.Sleep(3 * time.Second)

	_, round1 := manager.Latest()
	if round1 != 5 {
		t.Errorf("initial round = %d, want 5", round1)
	}

	// Update round
	provider.round = 20

	// Wait for next snapshot cycle
	time.Sleep(200 * time.Millisecond)

	_, round2 := manager.Latest()
	if round2 != 20 {
		t.Errorf("updated round = %d, want 20", round2)
	}
}

func TestSnapshotManager_SkipsIfNoNewCommits(t *testing.T) {
	db, cleanup := createTestStorageForManager(t)
	defer cleanup()

	provider := &mockSnapshotProvider{round: 10, db: db}

	manager := NewSnapshotManager(db, provider)
	manager.interval = 50 * time.Millisecond

	manager.Start()
	defer manager.Stop()

	// Wait for initial snapshot (loop waits 2s before first snapshot)
	time.Sleep(3 * time.Second)

	data1, _ := manager.Latest()
	if data1 == nil {
		t.Fatal("expected initial snapshot to be created")
	}

	// Wait for another cycle (should skip since round hasn't changed)
	time.Sleep(200 * time.Millisecond)

	data2, _ := manager.Latest()

	// Should be the same snapshot (pointer comparison)
	if &data1[0] != &data2[0] {
		t.Error("snapshot should not be recreated if round hasn't changed")
	}
}

func TestSnapshotManager_StopsCleanly(t *testing.T) {
	db, cleanup := createTestStorageForManager(t)
	defer cleanup()

	provider := &mockSnapshotProvider{round: 0, db: db}
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
	case <-time.After(5 * time.Second):
		t.Fatal("Stop() timed out")
	}
}

// captureEvents swaps the default slog logger for a JSON handler writing into a
// fresh buffer, restoring the previous default on test cleanup.
func captureEvents(t *testing.T) *bytes.Buffer {
	t.Helper()

	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })

	return &buf
}

// eventsNamed decodes one JSON object per captured line and returns those whose
// reserved events.Key attribute equals name, in emission order.
func eventsNamed(t *testing.T, buf *bytes.Buffer, name string) []map[string]any {
	t.Helper()

	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}

		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("captured line is not JSON: %v (line=%q)", err, line)
		}

		if rec[events.Key] == name {
			out = append(out, rec)
		}
	}

	return out
}

// TestSnapshotManager_EmitsSnapshotCreated verifies createSnapshot emits
// sync.snapshot.created carrying the round and the checksum embedded in the
// serialized snapshot.
func TestSnapshotManager_EmitsSnapshotCreated(t *testing.T) {
	db, cleanup := createTestStorageForManager(t)
	defer cleanup()

	provider := &mockSnapshotProvider{round: 7, db: db}
	manager := NewSnapshotManager(db, provider)

	buf := captureEvents(t)

	manager.createSnapshot()

	recs := eventsNamed(t, buf, events.EvSnapshotCreated)
	if len(recs) != 1 {
		t.Fatalf("want 1 %s event, got %d", events.EvSnapshotCreated, len(recs))
	}

	if recs[0]["round"] != float64(7) {
		t.Errorf("round = %v, want 7", recs[0]["round"])
	}

	checksum, ok := recs[0]["checksum"].(string)
	if !ok || len(checksum) != 64 {
		t.Errorf("checksum = %v, want 64-char hex string", recs[0]["checksum"])
	}
}

func TestSnapshotManager_LatestBeforeStart(t *testing.T) {
	db, cleanup := createTestStorageForManager(t)
	defer cleanup()

	provider := &mockSnapshotProvider{round: 0, db: db}
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
