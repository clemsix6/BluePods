package consensus

import (
	"encoding/binary"
	"testing"
)

// buildChain tracks a chain of n objects under the tracker, each an
// ObjectParent child of the previous one, with the first object (index 0)
// rooted under root via KeyRoot. It returns the leaf (index n-1), the object
// furthest from root. n must be at least 1.
func buildChain(t *testing.T, ot *objectTracker, n int, root Hash) Hash {
	t.Helper()

	prevKind := keyRootKind
	prevParent := root
	var leaf Hash

	for i := 0; i < n; i++ {
		leaf = chainHash(i)
		ot.trackObject(leaf, 0, 0, 0, prevKind, prevParent)
		prevKind = objectParentKind
		prevParent = leaf
	}

	return leaf
}

// chainHash derives a deterministic, collision-free Hash for chain index i,
// wide enough to stay unique past 256 entries.
func chainHash(i int) Hash {
	var h Hash
	binary.BigEndian.PutUint32(h[:4], uint32(i)+1)
	return h
}

// TestControllerOf_NestedChain tests that a multi-level ObjectParent chain
// resolves to the terminal KeyRoot's public key.
func TestControllerOf_NestedChain(t *testing.T) {
	db := newTrackerTestStorage(t)
	ot := newObjectTracker(db)

	root := Hash{0xAA}
	leaf := buildChain(t, ot, 3, root)

	controller, ok := ot.controllerOf(leaf)
	if !ok {
		t.Fatal("expected controllerOf to resolve")
	}
	if controller != root {
		t.Fatalf("expected controller %x, got %x", root, controller)
	}
}

// TestControllerOf_DepthBoundarySucceeds tests that a chain resolving in
// exactly walkDepthLimit lookups still succeeds.
func TestControllerOf_DepthBoundarySucceeds(t *testing.T) {
	db := newTrackerTestStorage(t)
	ot := newObjectTracker(db)

	root := Hash{0xBB}
	leaf := buildChain(t, ot, walkDepthLimit, root)

	controller, ok := ot.controllerOf(leaf)
	if !ok {
		t.Fatal("expected controllerOf to resolve at the depth boundary")
	}
	if controller != root {
		t.Fatalf("expected controller %x, got %x", root, controller)
	}
}

// TestControllerOf_DepthOverflowFailsClosed tests that a chain requiring one
// more lookup than walkDepthLimit fails closed rather than resolving.
func TestControllerOf_DepthOverflowFailsClosed(t *testing.T) {
	db := newTrackerTestStorage(t)
	ot := newObjectTracker(db)

	root := Hash{0xCC}
	leaf := buildChain(t, ot, walkDepthLimit+1, root)

	if _, ok := ot.controllerOf(leaf); ok {
		t.Fatal("expected controllerOf to fail closed past the depth guard")
	}
}

// TestControllerOf_MissingEntryMidChain tests that a chain referencing an
// untracked object fails closed instead of resolving.
func TestControllerOf_MissingEntryMidChain(t *testing.T) {
	db := newTrackerTestStorage(t)
	ot := newObjectTracker(db)

	missing := Hash{0xDE, 0xAD}
	leaf := Hash{0x01}
	ot.trackObject(leaf, 0, 0, 0, objectParentKind, missing)

	if _, ok := ot.controllerOf(leaf); ok {
		t.Fatal("expected controllerOf to fail closed on a missing tracker entry")
	}
}

// TestControllerOf_LegacyZeroParentResolvesToZeroKey tests that a legacy
// entry (KeyRoot + zero parent) resolves successfully to the zero key: the
// object is frozen, nobody controls it, but the walk itself does not fail.
func TestControllerOf_LegacyZeroParentResolvesToZeroKey(t *testing.T) {
	db := newTrackerTestStorage(t)
	ot := newObjectTracker(db)

	objID := Hash{0x01}
	ot.trackObject(objID, 0, 0, 0, keyRootKind, Hash{})

	controller, ok := ot.controllerOf(objID)
	if !ok {
		t.Fatal("expected controllerOf to resolve for a legacy zero parent")
	}
	if controller != (Hash{}) {
		t.Fatalf("expected zero key, got %x", controller)
	}
}

// TestControls_MatchesController tests that controls reports true only for
// the resolved controlling key.
func TestControls_MatchesController(t *testing.T) {
	db := newTrackerTestStorage(t)
	ot := newObjectTracker(db)

	root := Hash{0xAA}
	other := Hash{0xBB}
	leaf := buildChain(t, ot, 2, root)

	if !ot.controls(root, leaf) {
		t.Fatal("expected root to control leaf")
	}
	if ot.controls(other, leaf) {
		t.Fatal("expected other to not control leaf")
	}
}

// TestControls_FailsClosedWhenControllerOfFails tests that controls returns
// false, never panics or defaults true, when controllerOf cannot resolve.
func TestControls_FailsClosedWhenControllerOfFails(t *testing.T) {
	db := newTrackerTestStorage(t)
	ot := newObjectTracker(db)

	missing := Hash{0xDE, 0xAD}
	leaf := Hash{0x01}
	ot.trackObject(leaf, 0, 0, 0, objectParentKind, missing)

	if ot.controls(Hash{}, leaf) {
		t.Fatal("expected controls to fail closed on unresolved controllerOf")
	}
}

// TestWouldCycle_ReparentAncestorUnderDescendant tests that attaching an
// ancestor under its own descendant is detected as a cycle.
func TestWouldCycle_ReparentAncestorUnderDescendant(t *testing.T) {
	db := newTrackerTestStorage(t)
	ot := newObjectTracker(db)

	root := Hash{0xAA}
	a := chainHash(0)
	ot.trackObject(a, 0, 0, 0, keyRootKind, root)

	b := chainHash(1)
	ot.trackObject(b, 0, 0, 0, objectParentKind, a)

	c := chainHash(2)
	ot.trackObject(c, 0, 0, 0, objectParentKind, b)

	// a is an ancestor of c; reparenting a under c must be rejected.
	if !ot.wouldCycle(a, objectParentKind, c) {
		t.Fatal("expected reparenting an ancestor under its descendant to cycle")
	}
}

// TestWouldCycle_UnrelatedReparentDoesNotCycle tests that attaching an object
// under an unrelated ObjectParent does not cycle.
func TestWouldCycle_UnrelatedReparentDoesNotCycle(t *testing.T) {
	db := newTrackerTestStorage(t)
	ot := newObjectTracker(db)

	root := Hash{0xAA}
	a := chainHash(0)
	ot.trackObject(a, 0, 0, 0, keyRootKind, root)

	unrelated := chainHash(1)
	ot.trackObject(unrelated, 0, 0, 0, keyRootKind, root)

	target := chainHash(2)
	ot.trackObject(target, 0, 0, 0, keyRootKind, root)

	if ot.wouldCycle(target, objectParentKind, unrelated) {
		t.Fatal("expected an unrelated reparent to not cycle")
	}
}

// TestWouldCycle_ReparentToKeyRootNeverCycles tests that attaching an object
// directly under a KeyRoot never cycles, regardless of existing relationships.
func TestWouldCycle_ReparentToKeyRootNeverCycles(t *testing.T) {
	db := newTrackerTestStorage(t)
	ot := newObjectTracker(db)

	root := Hash{0xAA}
	leaf := buildChain(t, ot, 3, root)

	if ot.wouldCycle(leaf, keyRootKind, root) {
		t.Fatal("expected reparenting to a KeyRoot to never cycle")
	}
}

// TestWouldCycle_SelfParentCycles tests that attaching an object under
// itself is detected as a cycle without requiring any tracker entry.
func TestWouldCycle_SelfParentCycles(t *testing.T) {
	db := newTrackerTestStorage(t)
	ot := newObjectTracker(db)

	self := Hash{0x01}

	if !ot.wouldCycle(self, objectParentKind, self) {
		t.Fatal("expected self-parenting to cycle")
	}
}

// TestWouldCycle_MissingEntryMidWalkFailsClosed tests that a missing tracker
// entry encountered while walking up from newParent is treated conservatively
// as a cycle, even though the objectID never actually appears in the chain.
func TestWouldCycle_MissingEntryMidWalkFailsClosed(t *testing.T) {
	db := newTrackerTestStorage(t)
	ot := newObjectTracker(db)

	missing := Hash{0xDE, 0xAD}
	mid := Hash{0x01}
	ot.trackObject(mid, 0, 0, 0, objectParentKind, missing)

	target := Hash{0x02}

	if !ot.wouldCycle(target, objectParentKind, mid) {
		t.Fatal("expected a missing entry mid-walk to fail closed as a cycle")
	}
}

// TestWouldCycle_DepthOverflowFailsClosed tests that a walk exceeding
// walkDepthLimit is treated conservatively as a cycle.
func TestWouldCycle_DepthOverflowFailsClosed(t *testing.T) {
	db := newTrackerTestStorage(t)
	ot := newObjectTracker(db)

	root := Hash{0xCC}
	newParent := buildChain(t, ot, walkDepthLimit+1, root)

	target := Hash{0xFF, 0xFF, 0xFF, 0xFF}

	if !ot.wouldCycle(target, objectParentKind, newParent) {
		t.Fatal("expected wouldCycle to fail closed past the depth guard")
	}
}
