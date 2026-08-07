package index

// ancestorWalkLimit caps how many parent edges one proved ancestry answer
// walks. It is a DoS guard against a corrupted or adversarially constructed
// parent chain, not a functional limit on nesting depth, and mirrors the
// consensus-side cascade walk's own guard.
const ancestorWalkLimit = 256

// Roots are the four component roots the combined index root commits to,
// together with that combination. A proved answer carries all four because a
// proof folds to ONE of them: without the other three a verifier cannot
// recompute the combined root a quorum of vertex headers attests, and the
// proof would be tied to nothing.
type Roots struct {
	Domain    [32]byte // Domain is the domain tree's root
	Parent    [32]byte // Parent is the parent tree's root
	Children  [32]byte // Children is the children top tree's root
	Validator [32]byte // Validator is the validator tree's root
	Combined  [32]byte // Combined is CombinedRoot over the four above
}

// Anchor is the anchoring context every proved answer carries. Roots always
// describe the tree state the answer's proofs were taken against, so a proof
// always folds to one of them. Round names the committed frontier that state
// is anchored at and is meaningful only when Anchored: a client verifies by
// matching Roots.Combined against a quorum bundle, which exists only for a
// root some committed frontier recorded.
type Anchor struct {
	Roots    Roots  // Roots are the component roots the answer's proofs fold to
	Round    uint64 // Round is the committed frontier Roots.Combined is anchored at, when Anchored
	Anchored bool   // Anchored reports whether Roots.Combined is the root recorded at Round
}

// DomainAnswer is a proved domain resolution: the leaf exactly as the tree
// hashed it (empty when the name has none) and the proof binding it, or the
// name's absence, to Anchor.Roots.Domain. The value travels verbatim so a
// verifier folds the identical bytes rather than re-encoding a decoded copy
// and disagreeing over a field it dropped; DecodeDomainLeaf reads it.
type DomainAnswer struct {
	Anchor Anchor // Anchor is the state the proof was taken against
	Value  []byte // Value is the raw leaf value, nil when the name has no leaf
	Proof  Proof  // Proof authenticates Value, or the name's absence
}

// ChildrenAnswer is a proved enumeration: the parent's subtree root proven
// against the children top tree, plus the raw child-leaf stream. The stream
// itself is unauthenticated by design — the client rebuilds the subtree from
// it (ChildrenSubtreeRoot) and checks that root against the proven one, which
// is what makes a withheld or invented leaf detectable at every set size.
type ChildrenAnswer struct {
	Anchor      Anchor     // Anchor is the state the proof was taken against
	SubtreeRoot [32]byte   // SubtreeRoot is the proven root of the parent's children subtree, zero when Found is false
	Found       bool       // Found reports whether the parent currently has any children
	Proof       Proof      // Proof authenticates SubtreeRoot, or the parent's absence, against Anchor.Roots.Children
	Children    [][32]byte // Children are the raw child IDs, in no particular order
}

// AncestorEdge is one proved hop of an ancestry walk: the raw parent-tree leaf
// for ChildID (empty when the object has no edge at all) and the proof binding
// it to Anchor.Roots.Parent. Kind and parent live inside the leaf, so a client
// reads them through DecodeParentLeaf from bytes the proof authenticated,
// never from an unauthenticated field beside it.
type AncestorEdge struct {
	ChildID [32]byte // ChildID is the object this hop's edge belongs to
	Value   []byte   // Value is the raw parent leaf, nil when ChildID has no edge
	Proof   Proof    // Proof authenticates Value, or ChildID's absence
}

// AncestorAnswer is a proved ancestry walk, ordered from the queried object
// upward. It ends on the first edge that is a KeyRoot (the walk's terminus),
// on the first object with no edge at all (an absence proof, so a withheld
// edge is not confusable with a missing one), or at ancestorWalkLimit hops.
type AncestorAnswer struct {
	Anchor Anchor         // Anchor is the state every edge's proof was taken against
	Edges  []AncestorEdge // Edges are the walk's hops, the queried object first
}

// ResolveDomain answers a domain lookup with the leaf and its inclusion or
// absence proof, anchored as one atomic read of the trees and the frontier.
func (m *Manager) ResolveDomain(name string) DomainAnswer {
	m.treeMu.Lock()
	defer m.treeMu.Unlock()

	value, _ := m.domain.value(name)

	return DomainAnswer{
		Anchor: m.anchor(),
		Value:  value,
		Proof:  m.domain.Prove(name),
	}
}

// ListChildren answers an enumeration with the parent's proven subtree root
// and the raw leaves under it, anchored as one atomic read.
func (m *Manager) ListChildren(parent [32]byte) ChildrenAnswer {
	m.treeMu.Lock()
	defer m.treeMu.Unlock()

	subtreeRoot, found := m.hierarchy.Children.TopLeaf(parent)

	return ChildrenAnswer{
		Anchor:      m.anchor(),
		SubtreeRoot: subtreeRoot,
		Found:       found,
		Proof:       m.hierarchy.Children.Prove(parent),
		Children:    m.hierarchy.Children.Children(parent),
	}
}

// Ancestors answers an ancestry walk from objectID upward, one proof per edge,
// every edge anchored at the same atomically read frontier — a walk assembled
// from separate reads could mix two tree states and prove nothing coherent.
func (m *Manager) Ancestors(objectID [32]byte) AncestorAnswer {
	m.treeMu.Lock()
	defer m.treeMu.Unlock()

	return AncestorAnswer{Anchor: m.anchor(), Edges: m.walkEdges(objectID)}
}

// walkEdges collects the proved edges from child upward, stopping at the first
// terminal or absent one. Caller holds treeMu.
func (m *Manager) walkEdges(child [32]byte) []AncestorEdge {
	edges := make([]AncestorEdge, 0, 1)

	current := child
	for hop := 0; hop < ancestorWalkLimit; hop++ {
		value, found := m.hierarchy.Parent.value(current)
		edges = append(edges, AncestorEdge{
			ChildID: current,
			Value:   value,
			Proof:   m.hierarchy.Parent.Prove(current),
		})

		if !found {
			return edges
		}

		leaf, ok := DecodeParentLeaf(value)
		if !ok || leaf.ParentKind == KeyRootKind {
			return edges
		}

		current = leaf.Parent
	}

	return edges
}

// anchor reads the current component roots and reports whether their
// combination is the one recorded at the last committed frontier. It is false
// while the commit path sits between a tree mutation and the SetFrontier call
// that closes its batch: the trees are then ahead of every committed round, so
// no vertex header attests the root these proofs fold to and a client must
// retry rather than hunt for a bundle that cannot exist. Caller holds treeMu.
func (m *Manager) anchor() Anchor {
	roots := m.roots()

	m.frontierMu.RLock()
	defer m.frontierMu.RUnlock()

	if roots.Combined != m.frontierRoot {
		return Anchor{Roots: roots}
	}

	return Anchor{Roots: roots, Round: m.frontierRound, Anchored: true}
}

// ChildrenSubtreeRoot rebuilds a children subtree from a raw leaf stream and
// returns its root, the client side of the enumeration completeness check: it
// must equal the subtree root the top-tree proof commits to. It reports
// ok false, without computing a root, when children carries a duplicate ID:
// folding a duplicate into the set silently would let a server pad the stream
// with a repeated child and still match SubtreeRoot, so len(children) would no
// longer be an authenticated count. An honest server never repeats an ID, so
// this rejection never triggers on a genuine answer. Exported for verifiers
// outside this package; the tree's own writes go through the same functional
// recompute, so the two can never disagree over an identical, duplicate-free
// set.
func ChildrenSubtreeRoot(children [][32]byte) (root [32]byte, ok bool) {
	set := make(map[[32]byte]bool, len(children))
	for _, c := range children {
		if set[c] {
			return [32]byte{}, false
		}
		set[c] = true
	}

	return subtreeRoot(set), true
}
