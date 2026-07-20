package index

// historyWindow bounds how many distinct committed rounds SetFrontier retains
// for RootAt, beyond the epoch checkpoints retained indefinitely (see
// Manager's history fields).
const historyWindow = 1000

// TrackerEntry is the subset of a tracked object's metadata the index needs
// to rebuild the parent and children trees: the object's ID and its declared
// parent reference. It mirrors the object tracker's ID/ParentKind/Parent
// fields without importing the package that owns the tracker.
type TrackerEntry struct {
	ID         [32]byte
	ParentKind byte
	Parent     [32]byte
}

// Manager owns the four SMT-backed trees, combines their roots into one
// anchor value, and retains a bounded history of that combined root by
// committed round. It is derived state: BuildFromState rebuilds every tree
// from the tracker, domain store, and validator snapshot a caller already
// persists elsewhere, and nothing here is itself persisted.
type Manager struct {
	hierarchy *HierarchyTrees
	domain    *DomainTree
	validator *ValidatorTree

	// history retains the combined root for the last historyWindow committed
	// rounds, evicted oldest-first as SetFrontier advances. order is the FIFO
	// queue of rounds backing that eviction.
	history map[uint64][32]byte
	order   []uint64

	// epochCheckpoints retains one root per epoch boundary indefinitely,
	// outliving the history window. pendingCheckpoint is set by
	// RebuildValidators and consumed by the next SetFrontier call, so the
	// round that first anchors a freshly rebuilt validator tree is the one
	// checkpointed.
	epochCheckpoints  map[uint64][32]byte
	pendingCheckpoint bool
}

// NewManager returns an empty Manager: every tree starts empty, matching a
// fresh chain before genesis seeding.
func NewManager() *Manager {
	return &Manager{
		hierarchy:        NewHierarchyTrees(),
		domain:           NewDomainTree(),
		validator:        NewValidatorTree(),
		history:          make(map[uint64][32]byte),
		epochCheckpoints: make(map[uint64][32]byte),
	}
}

// ApplyEdge upserts child's parent-tree and children-tree edge, covering both
// a newly created object's declared parent and a reparent's edge move.
func (m *Manager) ApplyEdge(child [32]byte, kind byte, parent [32]byte) {
	m.hierarchy.SetEdge(child, kind, parent)
}

// RemoveObject drops child from every tree it can appear in (parent tree and
// its old parent's children subtree), on deletion.
func (m *Manager) RemoveObject(child [32]byte) {
	m.hierarchy.RemoveEdge(child)
}

// ApplyDomain upserts a domain tree leaf.
func (m *Manager) ApplyDomain(name string, objectID, owner [32]byte, expiryEpoch uint64) {
	m.domain.Set(DomainLeaf{Name: name, ObjectID: objectID, Owner: owner, ExpiryEpoch: expiryEpoch})
}

// RemoveDomain drops a domain tree leaf; removing an absent name is a no-op.
func (m *Manager) RemoveDomain(name string) {
	m.domain.Remove(name)
}

// RebuildValidators replaces the validator tree wholesale from entries — an
// epoch boundary's holder freeze, or a caller's live genesis-registration
// snapshot before the first boundary. The round of the next SetFrontier call
// is marked as an epoch checkpoint and retained indefinitely, past the
// bounded history window.
func (m *Manager) RebuildValidators(entries []ValidatorLeaf) {
	m.validator.Rebuild(entries)
	m.pendingCheckpoint = true
}

// Root returns the current combined index root over the four trees' current
// contents.
func (m *Manager) Root() [32]byte {
	return CombinedRoot(m.domain.Root(), m.hierarchy.Parent.Root(), m.hierarchy.Children.Root(), m.validator.Root())
}

// SetFrontier records the current combined root as the anchor for round,
// called once per round the commit loop decides. It bounds retained history
// to the last historyWindow rounds, except a round marked pending by a
// preceding RebuildValidators call, which is retained indefinitely.
func (m *Manager) SetFrontier(round uint64) {
	root := m.Root()

	m.history[round] = root
	m.order = append(m.order, round)

	if m.pendingCheckpoint {
		m.epochCheckpoints[round] = root
		m.pendingCheckpoint = false
	}

	m.evictOldRounds()
}

// evictOldRounds drops the oldest retained rounds past historyWindow from the
// bounded history map; epochCheckpoints is untouched.
func (m *Manager) evictOldRounds() {
	for len(m.order) > historyWindow {
		oldest := m.order[0]
		m.order = m.order[1:]
		delete(m.history, oldest)
	}
}

// RootAt returns the combined root anchored at round and whether one is
// retained: inside the bounded history window, at an epoch checkpoint, or
// false when neither holds.
func (m *Manager) RootAt(round uint64) ([32]byte, bool) {
	if root, ok := m.epochCheckpoints[round]; ok {
		return root, true
	}

	root, ok := m.history[round]
	return root, ok
}

// BuildFromState rebuilds every tree from scratch out of persisted mappings:
// tracker entries (parent and children edges), domain entries, and the
// current validator snapshot. Used at boot to backfill a restarted node's
// index — from persisted tracker, domain store, and epoch holders — before it
// produces or verifies any vertex, and by a later sync-side snapshot rebuild.
// It does not touch history or epoch checkpoints: those describe rounds
// already committed, which BuildFromState does not know about.
func (m *Manager) BuildFromState(trackerEntries []TrackerEntry, domainEntries []DomainLeaf, validatorEntries []ValidatorLeaf) {
	hierarchy := NewHierarchyTrees()
	for _, e := range trackerEntries {
		hierarchy.SetEdge(e.ID, e.ParentKind, e.Parent)
	}
	m.hierarchy = hierarchy

	domain := NewDomainTree()
	for _, e := range domainEntries {
		domain.Set(e)
	}
	m.domain = domain

	m.validator.Rebuild(validatorEntries)
}
