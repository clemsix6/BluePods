package consensus

import "BluePods/internal/genesis"

// parentEdge is a staged parent reference for an object whose edge a pending
// operation would change.
type parentEdge struct {
	kind   byte // kind is keyRootKind or objectParentKind
	parent Hash // parent is the staged parent bytes (a key or an object ID)
}

// stagedView overlays the pending effects of a declared-operation list on top of
// the committed tracker, so each operation validates against the state the
// previous operations would produce — a reparented edge, a removed object, a
// changed child count — without mutating the tracker before the whole list is
// proven valid.
type stagedView struct {
	ot         *objectTracker      // ot is the committed tracker read through the overlay
	parents    map[Hash]parentEdge // parents overrides an object's staged parent edge
	deleted    map[Hash]bool       // deleted marks objects a prior operation removed
	childDelta map[Hash]int32      // childDelta accumulates staged child-count changes per ObjectParent
}

// newStagedView returns an empty overlay over the given tracker.
func newStagedView(ot *objectTracker) *stagedView {
	return &stagedView{
		ot:         ot,
		parents:    make(map[Hash]parentEdge),
		deleted:    make(map[Hash]bool),
		childDelta: make(map[Hash]int32),
	}
}

// validate checks and stages one operation against the view, returning false for
// any invalid or unknown-kind operation (the domain kinds land in a later batch).
func (s *stagedView) validate(sender Hash, op genesis.DeclaredOp, refs map[Hash]bool) bool {
	switch op.Kind {
	case reparentOp:
		return s.validateReparent(sender, op, refs)
	case deleteOp:
		return s.validateDelete(sender, op, refs)
	default:
		return false
	}
}

// validateReparent checks and stages a reparent (kind 0). The object must be a
// version-tracked mutable ref the sender controls; a KeyRoot target may be any
// key (that IS the transfer), while an ObjectParent target must also be
// sender-controlled and must not close a cycle.
func (s *stagedView) validateReparent(sender Hash, op genesis.DeclaredOp, refs map[Hash]bool) bool {
	objID, ok := hash32(op.ObjectID)
	if !ok || !refs[objID] || !s.controls(sender, objID) {
		return false
	}

	target, ok := hash32(op.Target)
	if !ok || (op.TargetKind != keyRootKind && op.TargetKind != objectParentKind) {
		return false
	}

	if op.TargetKind == objectParentKind && !s.controls(sender, target) {
		return false
	}

	if s.wouldCycle(objID, op.TargetKind, target) {
		return false
	}

	s.stageReparent(objID, op.TargetKind, target)

	return true
}

// validateDelete checks and stages a delete (kind 1). The object must be a
// version-tracked mutable ref the sender controls, with no remaining children
// under the staged view — a prior operation may have reparented its last child
// away.
func (s *stagedView) validateDelete(sender Hash, op genesis.DeclaredOp, refs map[Hash]bool) bool {
	objID, ok := hash32(op.ObjectID)
	if !ok || !refs[objID] || !s.controls(sender, objID) {
		return false
	}

	if s.childCount(objID) != 0 {
		return false
	}

	s.stageDelete(objID)

	return true
}

// stageReparent records an object's new parent edge and rebinds staged child
// counts on the old and new ObjectParent (KeyRoot parents receive no count).
func (s *stagedView) stageReparent(id Hash, kind byte, parent Hash) {
	if oldKind, oldParent, ok := s.getParent(id); ok && oldKind == objectParentKind {
		s.childDelta[oldParent]--
	}

	s.parents[id] = parentEdge{kind: kind, parent: parent}

	if kind == objectParentKind {
		s.childDelta[parent]++
	}
}

// stageDelete marks an object removed and decrements its ObjectParent parent's
// staged child count (a KeyRoot parent receives no count).
func (s *stagedView) stageDelete(id Hash) {
	if kind, parent, ok := s.getParent(id); ok && kind == objectParentKind {
		s.childDelta[parent]--
	}

	s.deleted[id] = true
}

// getParent returns an object's staged parent edge: not-ok when a prior
// operation deleted it, the staged override when one reparented it, else the
// committed tracker's edge.
func (s *stagedView) getParent(id Hash) (byte, Hash, bool) {
	if s.deleted[id] {
		return keyRootKind, Hash{}, false
	}

	if e, ok := s.parents[id]; ok {
		return e.kind, e.parent, true
	}

	return s.ot.getParent(id)
}

// controllerOf walks staged parent edges from id to the terminal KeyRoot and
// returns the controlling key. It fails closed on a missing edge or a walk
// longer than walkDepthLimit, mirroring the tracker's own controllerOf.
func (s *stagedView) controllerOf(id Hash) (Hash, bool) {
	current := id

	for i := 0; i < walkDepthLimit; i++ {
		kind, parent, ok := s.getParent(current)
		if !ok {
			return Hash{}, false
		}

		if kind == keyRootKind {
			return parent, true
		}

		current = parent
	}

	return Hash{}, false
}

// controls reports whether sender is id's controlling key under the staged view.
func (s *stagedView) controls(sender, id Hash) bool {
	controller, ok := s.controllerOf(id)

	return ok && controller == sender
}

// wouldCycle reports whether attaching id under (kind, parent) would close a
// cycle under the staged view. A KeyRoot target never cycles; otherwise it walks
// up from parent looking for id. A missing edge or an over-long walk is treated
// conservatively as a cycle.
func (s *stagedView) wouldCycle(id Hash, kind byte, parent Hash) bool {
	if kind == keyRootKind {
		return false
	}

	current := parent

	for i := 0; i < walkDepthLimit; i++ {
		if current == id {
			return true
		}

		k, p, ok := s.getParent(current)
		if !ok {
			return true
		}

		if k == keyRootKind {
			return false
		}

		current = p
	}

	return true
}

// childCount returns an object's staged child count: the committed count plus
// the accumulated staged delta, floored at zero.
func (s *stagedView) childCount(id Hash) uint32 {
	staged := int64(s.ot.childCount(id)) + int64(s.childDelta[id])
	if staged < 0 {
		return 0
	}

	return uint32(staged)
}
