package consensus

import (
	"testing"
	"time"
)

// TestScanDetectsNewHoldings tests that the scanner identifies objects to fetch.
func TestScanDetectsNewHoldings(t *testing.T) {
	db := newTestStorage(t)
	validators, vs := newTestValidatorSet(4)

	dag := New(db, vs, nil, testSystemPod, 0, validators[0].privKey, nil,
		WithEpochLength(10),
	)
	defer dag.Close()

	// Track some objects
	obj1 := Hash{0x01}
	obj2 := Hash{0x02}
	dag.tracker.trackObject(obj1, 1, 3, 0)
	dag.tracker.trackObject(obj2, 1, 3, 0)

	// isHolder returns true for both, but hasLocal returns false for obj2
	isHolder := func(id [32]byte, rep uint16) bool {
		return true // this node holds everything
	}
	hasLocal := func(id [32]byte) bool {
		return id == obj1 // only have obj1 locally
	}

	result := dag.ScanObjects(isHolder, hasLocal)

	if len(result.NeedFetch) != 1 {
		t.Fatalf("expected 1 object to fetch, got %d", len(result.NeedFetch))
	}

	if result.NeedFetch[0] != obj2 {
		t.Errorf("expected obj2 in NeedFetch, got %x", result.NeedFetch[0][:4])
	}

	if len(result.CanDrop) != 0 {
		t.Errorf("expected 0 objects to drop, got %d", len(result.CanDrop))
	}
}

// TestScanDetectsDroppedHoldings tests that the scanner identifies objects to drop.
func TestScanDetectsDroppedHoldings(t *testing.T) {
	db := newTestStorage(t)
	validators, vs := newTestValidatorSet(4)

	dag := New(db, vs, nil, testSystemPod, 0, validators[0].privKey, nil,
		WithEpochLength(10),
	)
	defer dag.Close()

	obj1 := Hash{0x01}
	obj2 := Hash{0x02}
	dag.tracker.trackObject(obj1, 1, 3, 0)
	dag.tracker.trackObject(obj2, 1, 3, 0)

	// isHolder returns false for both (no longer responsible)
	isHolder := func(id [32]byte, rep uint16) bool {
		return false
	}
	// hasLocal returns true for both (still have them locally)
	hasLocal := func(id [32]byte) bool {
		return true
	}

	result := dag.ScanObjects(isHolder, hasLocal)

	if len(result.CanDrop) != 2 {
		t.Fatalf("expected 2 objects to drop, got %d", len(result.CanDrop))
	}

	if len(result.NeedFetch) != 0 {
		t.Errorf("expected 0 objects to fetch, got %d", len(result.NeedFetch))
	}
}

// TestScanNoChanges tests that no changes produces empty results.
func TestScanNoChanges(t *testing.T) {
	db := newTestStorage(t)
	validators, vs := newTestValidatorSet(4)

	dag := New(db, vs, nil, testSystemPod, 0, validators[0].privKey, nil,
		WithEpochLength(10),
	)
	defer dag.Close()

	obj1 := Hash{0x01}
	obj2 := Hash{0x02}
	dag.tracker.trackObject(obj1, 1, 3, 0)
	dag.tracker.trackObject(obj2, 1, 3, 0)

	// Perfect alignment: holder matches local
	isHolder := func(id [32]byte, rep uint16) bool {
		return true
	}
	hasLocal := func(id [32]byte) bool {
		return true
	}

	result := dag.ScanObjects(isHolder, hasLocal)

	if len(result.NeedFetch) != 0 {
		t.Errorf("expected 0 fetch, got %d", len(result.NeedFetch))
	}

	if len(result.CanDrop) != 0 {
		t.Errorf("expected 0 drop, got %d", len(result.CanDrop))
	}
}

// TestScanEmptyTracker tests scanning with no tracked objects.
func TestScanEmptyTracker(t *testing.T) {
	db := newTestStorage(t)
	validators, vs := newTestValidatorSet(4)

	dag := New(db, vs, nil, testSystemPod, 0, validators[0].privKey, nil,
		WithEpochLength(10),
	)
	defer dag.Close()

	isHolder := func(id [32]byte, rep uint16) bool { return true }
	hasLocal := func(id [32]byte) bool { return false }

	result := dag.ScanObjects(isHolder, hasLocal)

	if len(result.NeedFetch) != 0 {
		t.Errorf("expected 0 fetch on empty tracker, got %d", len(result.NeedFetch))
	}
	if len(result.CanDrop) != 0 {
		t.Errorf("expected 0 drop on empty tracker, got %d", len(result.CanDrop))
	}
}

// TestScanMixedResults tests a mix of fetch and drop results.
func TestScanMixedResults(t *testing.T) {
	db := newTestStorage(t)
	validators, vs := newTestValidatorSet(4)

	dag := New(db, vs, nil, testSystemPod, 0, validators[0].privKey, nil,
		WithEpochLength(10),
	)
	defer dag.Close()

	objKeep := Hash{0x01}    // holder + has local = keep (no action)
	objFetch := Hash{0x02}   // holder + no local = fetch
	objDrop := Hash{0x03}    // not holder + has local = drop
	objIgnore := Hash{0x04}  // not holder + no local = ignore

	dag.tracker.trackObject(objKeep, 1, 3, 0)
	dag.tracker.trackObject(objFetch, 1, 3, 0)
	dag.tracker.trackObject(objDrop, 1, 3, 0)
	dag.tracker.trackObject(objIgnore, 1, 3, 0)

	isHolder := func(id [32]byte, rep uint16) bool {
		return id == objKeep || id == objFetch
	}
	hasLocal := func(id [32]byte) bool {
		return id == objKeep || id == objDrop
	}

	result := dag.ScanObjects(isHolder, hasLocal)

	if len(result.NeedFetch) != 1 {
		t.Fatalf("expected 1 fetch, got %d", len(result.NeedFetch))
	}
	if result.NeedFetch[0] != objFetch {
		t.Errorf("expected objFetch in NeedFetch")
	}

	if len(result.CanDrop) != 1 {
		t.Fatalf("expected 1 drop, got %d", len(result.CanDrop))
	}
	if result.CanDrop[0] != objDrop {
		t.Errorf("expected objDrop in CanDrop")
	}
}

// TestScanBackground_DoesNotBlockCommit tests that a slow scan goroutine
// does not block the main goroutine (simulating the commit path).
func TestScanBackground_DoesNotBlockCommit(t *testing.T) {
	db := newTestStorage(t)
	validators, vs := newTestValidatorSet(4)

	dag := New(db, vs, nil, testSystemPod, 0, validators[0].privKey, nil,
		WithEpochLength(10),
	)
	defer dag.Close()

	// Track 1000 objects
	for i := 0; i < 1000; i++ {
		var id Hash
		id[0] = byte(i >> 8)
		id[1] = byte(i)
		dag.tracker.trackObject(id, 1, 3, 0)
	}

	// Slow isHolder: sleeps 1ms per call to simulate expensive work
	slowIsHolder := func(id [32]byte, rep uint16) bool {
		time.Sleep(1 * time.Millisecond)
		return true
	}
	hasLocal := func(id [32]byte) bool { return false }

	// Launch scan in background goroutine
	done := make(chan ScanResult, 1)
	go func() {
		done <- dag.ScanObjects(slowIsHolder, hasLocal)
	}()

	// Main goroutine should NOT be blocked — verify with timeout
	mainDone := make(chan struct{})
	go func() {
		// Simulate commit-path work: access the DAG
		_ = dag.Round()
		_ = dag.Epoch()
		_ = dag.EpochHoldersCount()
		close(mainDone)
	}()

	select {
	case <-mainDone:
		// Main goroutine completed without blocking
	case <-time.After(100 * time.Millisecond):
		t.Fatal("main goroutine blocked by background scan")
	}

	// Wait for scan to complete
	select {
	case result := <-done:
		if len(result.NeedFetch) != 1000 {
			t.Errorf("expected 1000 NeedFetch, got %d", len(result.NeedFetch))
		}
	case <-time.After(10 * time.Second):
		t.Fatal("background scan did not complete within timeout")
	}
}

// TestScanLargeObjectCount tests scanning with many tracked objects.
func TestScanLargeObjectCount(t *testing.T) {
	db := newTestStorage(t)
	validators, vs := newTestValidatorSet(4)

	dag := New(db, vs, nil, testSystemPod, 0, validators[0].privKey, nil,
		WithEpochLength(10),
	)
	defer dag.Close()

	// Track 1000 objects
	for i := 0; i < 1000; i++ {
		var id Hash
		id[0] = byte(i >> 8)
		id[1] = byte(i)
		dag.tracker.trackObject(id, 1, 3, 0)
	}

	// Half need fetch, half can drop
	isHolder := func(id [32]byte, rep uint16) bool {
		return id[1]%2 == 0 // even objects
	}
	hasLocal := func(id [32]byte) bool {
		return id[1]%2 != 0 // odd objects
	}

	result := dag.ScanObjects(isHolder, hasLocal)

	// Should have results for both fetch and drop
	if len(result.NeedFetch) == 0 {
		t.Error("expected some objects to fetch")
	}
	if len(result.CanDrop) == 0 {
		t.Error("expected some objects to drop")
	}

	totalActions := len(result.NeedFetch) + len(result.CanDrop)
	if totalActions > 1000 {
		t.Errorf("total actions %d exceeds tracked objects 1000", totalActions)
	}
}
