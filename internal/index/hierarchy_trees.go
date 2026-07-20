package index

const (
	// KeyRootKind marks a parent edge whose target is an Ed25519 public key: the
	// object is a tree root and an ancestry walk over ParentTree leaves must
	// terminate here. Matches parent_kind=0 in types/object.fbs.
	KeyRootKind byte = 0

	// ObjectParentKind marks a parent edge whose target is another tracked
	// object's ID: the object is nested and the walk continues. Matches
	// parent_kind=1 in types/object.fbs.
	ObjectParentKind byte = 1

	// presentMarker is the children subtree's leaf value: membership is the
	// only fact a child leaf carries, so any fixed non-nil byte works.
	presentMarker byte = 1
)

// ParentLeaf is the parent tree's leaf record: an object's declared parent
// edge. ChildID rides in the leaf value (not just the key position) so an
// ancestry walk is self-describing — the client can verify each hop's kind and
// target from the leaf itself, including that the walk truly terminates at a
// KeyRoot rather than the server silently withholding a further edge.
type ParentLeaf struct {
	ChildID    [32]byte // ChildID is the object this edge belongs to
	ParentKind byte     // ParentKind is KeyRootKind or ObjectParentKind
	Parent     [32]byte // Parent is the parent reference: a key under KeyRoot, an object ID under ObjectParent
}

// ParentTree is a flat SMT keyed by blake3(childID), answering "what is X's
// parent" with one inclusion proof per edge.
type ParentTree struct {
	smt *SMT
}

// NewParentTree returns an empty parent tree.
func NewParentTree() *ParentTree {
	return &ParentTree{smt: New()}
}

// set inserts or overwrites child's parent edge. Unexported: callers go
// through HierarchyTrees.SetEdge so the parent tree and children tree, two
// views of the same edge set, never drift apart.
func (t *ParentTree) set(child [32]byte, kind byte, parent [32]byte) {
	t.smt.Insert(child[:], encodeParentLeaf(ParentLeaf{ChildID: child, ParentKind: kind, Parent: parent}))
}

// Remove deletes child's parent edge; removing an absent child is a no-op.
// Unexported-by-convention counterpart lives on HierarchyTrees.RemoveEdge.
func (t *ParentTree) Remove(child [32]byte) {
	t.smt.Delete(child[:])
}

// Get returns child's parent leaf and whether one exists.
func (t *ParentTree) Get(child [32]byte) (ParentLeaf, bool) {
	value, ok := t.smt.Get(child[:])
	if !ok {
		return ParentLeaf{}, false
	}

	return decodeParentLeaf(value)
}

// Root returns the tree's current Merkle root.
func (t *ParentTree) Root() [32]byte {
	return t.smt.Root()
}

// Prove returns an inclusion or absence proof for child against Root().
func (t *ParentTree) Prove(child [32]byte) Proof {
	return t.smt.Prove(child[:])
}

// encodeParentLeaf serializes a parent leaf as ChildID (32 bytes) || ParentKind
// (1 byte) || Parent (32 bytes), 65 bytes total.
func encodeParentLeaf(l ParentLeaf) []byte {
	buf := make([]byte, 0, 65)
	buf = append(buf, l.ChildID[:]...)
	buf = append(buf, l.ParentKind)
	buf = append(buf, l.Parent[:]...)
	return buf
}

// decodeParentLeaf reverses encodeParentLeaf, reporting ok=false on any length
// mismatch.
func decodeParentLeaf(data []byte) (ParentLeaf, bool) {
	if len(data) != 65 {
		return ParentLeaf{}, false
	}

	var l ParentLeaf
	copy(l.ChildID[:], data[:32])
	l.ParentKind = data[32]
	copy(l.Parent[:], data[33:65])

	return l, true
}

// ChildrenTree is a two-level SMT (a map of sets): a top tree keyed by
// blake3(parentID) whose leaf is the root of that parent's children subtree,
// and one subtree per parent keyed by childID. Enumerating a parent's children
// is a walk of its subtree, and completeness is provable because the subtree
// root the top tree carries commits to exactly that child set.
//
// The subtree contents are the source of truth (children); each subtree's
// root is recomputed functionally from that set on every change and pushed
// into the top tree, mirroring the SMT's own functional-recompute design so
// this stays immune to incremental sibling-reconstruction bugs.
type ChildrenTree struct {
	top      *SMT
	children map[[32]byte]map[[32]byte]bool
}

// NewChildrenTree returns an empty children tree.
func NewChildrenTree() *ChildrenTree {
	return &ChildrenTree{top: New(), children: make(map[[32]byte]map[[32]byte]bool)}
}

// addChild adds child to parent's subtree and updates the top tree's leaf for
// parent to the recomputed subtree root. Adding an already-present child is a
// no-op past the set insert.
func (t *ChildrenTree) addChild(parent, child [32]byte) {
	set := t.children[parent]
	if set == nil {
		set = make(map[[32]byte]bool)
		t.children[parent] = set
	}

	set[child] = true
	t.syncTop(parent)
}

// removeChild drops child from parent's subtree and updates the top tree:
// when the subtree becomes empty, the top-tree leaf for parent is removed
// entirely rather than left pointing at the empty-subtree root, keeping the
// top tree's entries exactly the parents that currently have children.
func (t *ChildrenTree) removeChild(parent, child [32]byte) {
	set := t.children[parent]
	if set == nil {
		return
	}

	delete(set, child)
	if len(set) == 0 {
		delete(t.children, parent)
	}

	t.syncTop(parent)
}

// syncTop recomputes parent's subtree root from its current child set and
// writes (or removes) the corresponding top-tree leaf.
func (t *ChildrenTree) syncTop(parent [32]byte) {
	set := t.children[parent]
	if len(set) == 0 {
		t.top.Delete(parent[:])
		return
	}

	root := subtreeRoot(set)
	t.top.Insert(parent[:], root[:])
}

// subtreeRoot builds a fresh SMT over children (each keyed by its own ID, with
// the fixed presentMarker value) and returns its root. Building it fresh
// mirrors the primitive SMT's own functional determinism: the result depends
// only on the child set, never on insertion order or history.
func subtreeRoot(children map[[32]byte]bool) [32]byte {
	sub := New()
	for c := range children {
		sub.Insert(c[:], []byte{presentMarker})
	}

	return sub.Root()
}

// Children returns the raw child IDs currently tracked under parent, in no
// particular order. It backs enumeration (a client streams these and rebuilds
// the subtree to check completeness against TopLeaf) and is not itself an
// authenticated response — callers checking completeness recompute the root
// from this list and compare it to TopLeaf.
func (t *ChildrenTree) Children(parent [32]byte) [][32]byte {
	set := t.children[parent]
	out := make([][32]byte, 0, len(set))
	for c := range set {
		out = append(out, c)
	}

	return out
}

// TopLeaf returns the 32-byte subtree root stored in the top tree for parent,
// and whether an entry exists (false when parent currently has no children).
func (t *ChildrenTree) TopLeaf(parent [32]byte) ([32]byte, bool) {
	value, ok := t.top.Get(parent[:])
	if !ok || len(value) != 32 {
		return [32]byte{}, false
	}

	var root [32]byte
	copy(root[:], value)

	return root, true
}

// Root returns the top tree's current Merkle root.
func (t *ChildrenTree) Root() [32]byte {
	return t.top.Root()
}

// Prove returns an inclusion or absence proof for parent's top-tree leaf
// against Root().
func (t *ChildrenTree) Prove(parent [32]byte) Proof {
	return t.top.Prove(parent[:])
}

// HierarchyTrees couples the parent tree and children tree — two views of the
// same edge set — behind operations that keep them consistent: a caller never
// updates one without the other.
type HierarchyTrees struct {
	Parent   *ParentTree
	Children *ChildrenTree
}

// NewHierarchyTrees returns an empty parent tree and children tree pair.
func NewHierarchyTrees() *HierarchyTrees {
	return &HierarchyTrees{Parent: NewParentTree(), Children: NewChildrenTree()}
}

// SetEdge inserts child's parent edge, or moves it (a reparent): when child
// already has an edge, it is first removed from its old parent's children
// subtree before being added under the new one, so an edge move updates both
// trees in one step and neither tree can observe a stale edge the other has
// already replaced.
func (h *HierarchyTrees) SetEdge(child [32]byte, kind byte, parent [32]byte) {
	if old, ok := h.Parent.Get(child); ok {
		h.Children.removeChild(old.Parent, child)
	}

	h.Parent.set(child, kind, parent)
	h.Children.addChild(parent, child)
}

// RemoveEdge drops child from both trees entirely (used on object deletion).
func (h *HierarchyTrees) RemoveEdge(child [32]byte) {
	if old, ok := h.Parent.Get(child); ok {
		h.Children.removeChild(old.Parent, child)
	}

	h.Parent.Remove(child)
}
