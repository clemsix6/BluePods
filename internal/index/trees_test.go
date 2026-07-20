package index

import "testing"

// --- DomainTree ---

// TestDomainTree_SetGetRoundTrip verifies Set stores a leaf Get can decode back
// verbatim, including the name carried inside the leaf itself.
func TestDomainTree_SetGetRoundTrip(t *testing.T) {
	tree := NewDomainTree()
	leaf := DomainLeaf{Name: "example.pod", ObjectID: [32]byte{0x01}, Owner: [32]byte{0x02}, ExpiryEpoch: 42}
	tree.Set(leaf)

	got, ok := tree.Get("example.pod")
	if !ok {
		t.Fatal("expected leaf to be found")
	}
	if got != leaf {
		t.Errorf("got %+v, want %+v", got, leaf)
	}
}

// TestDomainTree_RemoveDropsLeafAndChangesRoot verifies Remove both makes the
// leaf disappear and changes the tree's root back toward empty.
func TestDomainTree_RemoveDropsLeafAndChangesRoot(t *testing.T) {
	tree := NewDomainTree()
	empty := tree.Root()

	tree.Set(DomainLeaf{Name: "a.pod", ObjectID: [32]byte{0x01}})
	if tree.Root() == empty {
		t.Fatal("root did not change after insert")
	}

	tree.Remove("a.pod")
	if _, ok := tree.Get("a.pod"); ok {
		t.Error("leaf still present after remove")
	}
	if tree.Root() != empty {
		t.Error("root did not return to empty after removing the only leaf")
	}
}

// TestDomainTree_ProveVerify verifies an inclusion proof round-trips through
// the shared SMT Verify function using the leaf's exact encoded bytes.
func TestDomainTree_ProveVerify(t *testing.T) {
	tree := NewDomainTree()
	leaf := DomainLeaf{Name: "verify.pod", ObjectID: [32]byte{0x09}, Owner: [32]byte{0x08}, ExpiryEpoch: 7}
	tree.Set(leaf)

	proof := tree.Prove("verify.pod")
	if !Verify(tree.Root(), []byte("verify.pod"), encodeDomainLeaf(leaf), proof) {
		t.Error("inclusion proof failed to verify")
	}
}

// --- ParentTree / ancestry walk ---

// TestParentTree_SetGetRoundTrip verifies set stores a leaf Get decodes back.
func TestParentTree_SetGetRoundTrip(t *testing.T) {
	tree := NewParentTree()
	child := [32]byte{0x11}
	parent := [32]byte{0x22}
	tree.set(child, ObjectParentKind, parent)

	got, ok := tree.Get(child)
	if !ok {
		t.Fatal("expected leaf to be found")
	}
	want := ParentLeaf{ChildID: child, ParentKind: ObjectParentKind, Parent: parent}
	if got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

// TestAncestryWalk_TerminatesAtKeyRoot walks a three-level chain (leaf object
// under a middle object under a KeyRoot) purely over ParentTree leaves, and
// verifies the walk both reaches the root key and recognizes the KeyRoot kind
// as the terminus a client can trust without any further edge.
func TestAncestryWalk_TerminatesAtKeyRoot(t *testing.T) {
	tree := NewParentTree()
	rootKey := [32]byte{0xFF}
	middle := [32]byte{0x02}
	leafObj := [32]byte{0x03}

	tree.set(middle, KeyRootKind, rootKey)
	tree.set(leafObj, ObjectParentKind, middle)

	current := leafObj
	hops := 0
	for {
		leaf, ok := tree.Get(current)
		if !ok {
			t.Fatalf("walk hit a missing edge at %x before reaching a KeyRoot", current)
		}
		hops++
		if hops > 8 {
			t.Fatal("walk did not terminate within a small bound")
		}
		if leaf.ParentKind == KeyRootKind {
			if leaf.Parent != rootKey {
				t.Errorf("terminal key = %x, want %x", leaf.Parent, rootKey)
			}
			break
		}
		current = leaf.Parent
	}

	if hops != 2 {
		t.Errorf("walk took %d hops, want 2", hops)
	}
}

// --- ChildrenTree enumeration completeness ---

// TestChildrenTree_EnumerationCompleteness verifies that streaming every
// child currently tracked under a parent and recomputing the subtree root
// from that stream reproduces exactly the root the top tree carries for that
// parent — the completeness guarantee a client's enumeration proof relies on.
func TestChildrenTree_EnumerationCompleteness(t *testing.T) {
	tree := NewChildrenTree()
	parent := [32]byte{0xAA}
	kids := [][32]byte{{0x01}, {0x02}, {0x03}}

	for _, c := range kids {
		tree.addChild(parent, c)
	}

	topLeaf, ok := tree.TopLeaf(parent)
	if !ok {
		t.Fatal("expected a top-tree leaf for parent")
	}

	streamed := tree.Children(parent)
	if len(streamed) != len(kids) {
		t.Fatalf("streamed %d children, want %d", len(streamed), len(kids))
	}

	recomputed := subtreeRoot(toSet(streamed))
	if recomputed != topLeaf {
		t.Errorf("recomputed subtree root %x != top-tree leaf %x", recomputed, topLeaf)
	}
}

// TestChildrenTree_EmptyParentHasNoTopLeaf verifies a parent with no children
// carries no top-tree entry, and that removing every child restores that.
func TestChildrenTree_EmptyParentHasNoTopLeaf(t *testing.T) {
	tree := NewChildrenTree()
	parent := [32]byte{0xBB}
	child := [32]byte{0x01}

	if _, ok := tree.TopLeaf(parent); ok {
		t.Fatal("unexpected top-tree leaf before any child was added")
	}

	tree.addChild(parent, child)
	if _, ok := tree.TopLeaf(parent); !ok {
		t.Fatal("expected a top-tree leaf after adding a child")
	}

	tree.removeChild(parent, child)
	if _, ok := tree.TopLeaf(parent); ok {
		t.Error("top-tree leaf still present after removing the only child")
	}
}

// toSet converts a slice of IDs into the set shape subtreeRoot expects.
func toSet(ids [][32]byte) map[[32]byte]bool {
	set := make(map[[32]byte]bool, len(ids))
	for _, id := range ids {
		set[id] = true
	}
	return set
}

// --- HierarchyTrees: SetEdge / RemoveEdge consistency ---

// TestHierarchyTrees_SetEdgeCreatesBothViews verifies a fresh SetEdge is
// visible from both the parent tree (child -> parent) and the children tree
// (parent -> child), the two views of one edge set.
func TestHierarchyTrees_SetEdgeCreatesBothViews(t *testing.T) {
	h := NewHierarchyTrees()
	child := [32]byte{0x01}
	parent := [32]byte{0x02}

	h.SetEdge(child, ObjectParentKind, parent)

	leaf, ok := h.Parent.Get(child)
	if !ok || leaf.Parent != parent || leaf.ParentKind != ObjectParentKind {
		t.Errorf("parent tree edge = %+v, ok=%v, want parent=%x kind=%d", leaf, ok, parent, ObjectParentKind)
	}

	kids := h.Children.Children(parent)
	if len(kids) != 1 || kids[0] != child {
		t.Errorf("children tree under parent = %v, want [%x]", kids, child)
	}
}

// TestHierarchyTrees_ReparentMovesEdgeConsistently verifies that moving
// child's edge from parentA to parentB (a reparent) removes it from parentA's
// children subtree, adds it to parentB's, and updates the single parent-tree
// leaf — never leaving the two trees disagreeing about where child sits.
func TestHierarchyTrees_ReparentMovesEdgeConsistently(t *testing.T) {
	h := NewHierarchyTrees()
	child := [32]byte{0x01}
	parentA := [32]byte{0xA0}
	parentB := [32]byte{0xB0}

	h.SetEdge(child, ObjectParentKind, parentA)
	h.SetEdge(child, KeyRootKind, parentB)

	leaf, ok := h.Parent.Get(child)
	if !ok || leaf.Parent != parentB || leaf.ParentKind != KeyRootKind {
		t.Errorf("parent tree edge after reparent = %+v, ok=%v, want parent=%x kind=%d", leaf, ok, parentB, KeyRootKind)
	}

	if kids := h.Children.Children(parentA); len(kids) != 0 {
		t.Errorf("old parent still lists children after reparent: %v", kids)
	}
	if _, ok := h.Children.TopLeaf(parentA); ok {
		t.Error("old parent still has a top-tree leaf after losing its only child")
	}

	kids := h.Children.Children(parentB)
	if len(kids) != 1 || kids[0] != child {
		t.Errorf("new parent's children = %v, want [%x]", kids, child)
	}
}

// TestHierarchyTrees_RemoveEdgeClearsBothViews verifies RemoveEdge drops the
// child from the parent tree and from its parent's children subtree.
func TestHierarchyTrees_RemoveEdgeClearsBothViews(t *testing.T) {
	h := NewHierarchyTrees()
	child := [32]byte{0x01}
	parent := [32]byte{0x02}

	h.SetEdge(child, ObjectParentKind, parent)
	h.RemoveEdge(child)

	if _, ok := h.Parent.Get(child); ok {
		t.Error("parent tree still carries the edge after RemoveEdge")
	}
	if kids := h.Children.Children(parent); len(kids) != 0 {
		t.Errorf("children tree still lists the child after RemoveEdge: %v", kids)
	}
}
