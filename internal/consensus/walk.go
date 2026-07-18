package consensus

// walkDepthLimit caps the number of parent edges controllerOf and wouldCycle
// will traverse in a single walk. It is a DoS guard against a corrupted or
// adversarially constructed parent chain, not a functional limit — a
// legitimate cascade is expected to stay far shallower than this bound.
const walkDepthLimit = 256

// controllerOf walks parent edges from objectID up to the terminal KeyRoot
// and returns the controlling Ed25519 public key. It fails closed: a chain
// longer than walkDepthLimit edges, or a missing tracker entry anywhere
// along the way, resolves to (zero Hash, false). An object whose terminal
// KeyRoot is the zero key — a legacy pre-parent entry, or one explicitly set
// that way — resolves successfully to the zero key, meaning the object is
// frozen and nobody controls it; that is intended, not a failure.
func (ot *objectTracker) controllerOf(objectID Hash) (Hash, bool) {
	current := objectID

	for i := 0; i < walkDepthLimit; i++ {
		kind, parent, ok := ot.getParent(current)
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

// controls reports whether sender is objectID's controlling key, per
// controllerOf. It returns false whenever controllerOf cannot resolve a
// terminal KeyRoot within the depth guard — there is no ambiguous case where
// an unresolved chain is treated as controlled.
func (ot *objectTracker) controls(sender, objectID Hash) bool {
	controller, ok := ot.controllerOf(objectID)

	return ok && controller == sender
}

// wouldCycle reports whether attaching objectID under (newParentKind,
// newParent) would create a cycle in the parent hierarchy. Reparenting to a
// KeyRoot can never cycle — a key is a walk terminus, not a tracked object —
// so that case short-circuits to false. Otherwise it walks up from newParent
// looking for objectID: reaching it (including newParent == objectID itself)
// means objectID is already an ancestor of newParent, so the edge would
// close a loop. A missing tracker entry or a walk exceeding walkDepthLimit
// is treated conservatively as a cycle — an edge that cannot be proven safe
// is rejected rather than allowed.
func (ot *objectTracker) wouldCycle(objectID Hash, newParentKind byte, newParent Hash) bool {
	if newParentKind == keyRootKind {
		return false
	}

	current := newParent

	for i := 0; i < walkDepthLimit; i++ {
		if current == objectID {
			return true
		}

		kind, parent, ok := ot.getParent(current)
		if !ok {
			return true
		}

		if kind == keyRootKind {
			return false
		}

		current = parent
	}

	return true
}
