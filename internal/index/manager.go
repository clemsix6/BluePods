package index

import "sync"

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

	// frontierMu guards EVERY field below it: the retained root history and
	// the cached committed pair. All of them are written by the commit path
	// alone, which serializes its own calls (the DAG's commitMu) — but none
	// of their readers sit on it. Vertex production calls CommittedFrontier
	// and ingress vertex validation calls RootAt, both from goroutines that
	// must NOT take commitMu: it would put a whole commit batch's execution
	// on transaction submission and on gossip ingestion respectively. Reading
	// history or epochCheckpoints unguarded while the commit loop writes them
	// is not a benign race but a fatal concurrent map access, so the reads go
	// through this lock instead.
	frontierMu sync.RWMutex

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

	// frontierRound and frontierRoot cache the pair written by the most
	// recent SetFrontier call, so production reads it as one atomic pair:
	// reading round and root as two separate calls could observe a
	// SetFrontier landing between them and pair one call's round with
	// another's root, a torn anchor that stage-1 validation later rejects
	// network-wide.
	frontierRound uint64
	frontierRoot  [32]byte
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

	m.frontierMu.Lock()
	m.pendingCheckpoint = true
	m.frontierMu.Unlock()
}

// Root returns the current combined index root over the four trees' current,
// possibly uncommitted, contents — it can differ from the root at the last
// committed frontier whenever a tree has been mutated since the most recent
// SetFrontier call. Never anchor this value into a vertex, and never compare
// it against a received vertex's index_root: use CommittedFrontier for both,
// which returns the root AT the committed frontier, not the live one.
func (m *Manager) Root() [32]byte {
	return CombinedRoot(m.domain.Root(), m.hierarchy.Parent.Root(), m.hierarchy.Children.Root(), m.validator.Root())
}

// SetFrontier records the current combined root as the anchor for round,
// called once per round the commit loop decides. A round at or before the
// last recorded round is ignored, keeping the FIFO order strictly monotonic
// (evictOldRounds' eviction assumption) and the FIRST root recorded for a
// round authoritative. The flip side: a boot-time seed must only ever target
// rounds already decided — strictly below the commit cursor, which is the
// NEXT round to decide — or it would steal the cursor round's key from the
// commit loop's own later call (see cmd/node initIndex). It bounds retained
// history to the last historyWindow rounds, except a round marked pending by
// a preceding RebuildValidators call, which is retained indefinitely.
//
// The whole body runs under frontierMu's write lock, so a concurrent RootAt
// or CommittedFrontier reader never observes a half-recorded round.
func (m *Manager) SetFrontier(round uint64) {
	m.frontierMu.Lock()
	defer m.frontierMu.Unlock()

	if len(m.order) > 0 && round <= m.order[len(m.order)-1] {
		return
	}

	// Root walks the trees, which the commit path owns exclusively and no
	// reader of this lock touches, so taking it here costs the readers only
	// the few rehashes the last batch dirtied.
	root := m.Root()

	m.history[round] = root
	m.order = append(m.order, round)

	if m.pendingCheckpoint {
		m.epochCheckpoints[round] = root
		m.pendingCheckpoint = false
	}

	m.frontierRound = round
	m.frontierRoot = root

	m.evictOldRounds()
}

// CommittedFrontier returns the round and combined root of the most recently
// recorded committed frontier, as one atomic pair. Nothing here is node-local:
// two managers fed the identical edit stream and SetFrontier round return the
// identical pair, which is what lets two nodes anchor byte-identical vertices
// at the same committed frontier. Zero values before the first SetFrontier
// call, matching a fresh chain that has not committed anything yet.
func (m *Manager) CommittedFrontier() (round uint64, root [32]byte) {
	m.frontierMu.RLock()
	defer m.frontierMu.RUnlock()

	return m.frontierRound, m.frontierRoot
}

// evictOldRounds drops the oldest retained rounds past historyWindow from the
// bounded history map; epochCheckpoints is untouched. Caller holds
// frontierMu's write lock.
func (m *Manager) evictOldRounds() {
	for len(m.order) > historyWindow {
		oldest := m.order[0]
		m.order = m.order[1:]
		delete(m.history, oldest)
	}
}

// RootAt returns the combined root anchored at round and whether one is
// retained: inside the bounded history window, at an epoch checkpoint, or
// false when neither holds. Ingress vertex validation calls it from the
// gossip goroutines while the commit loop writes, hence the read lock (see
// frontierMu).
//
// A retained round's root never changes — SetFrontier's non-advancing guard
// makes the first root recorded for a round authoritative and only eviction
// ever removes it — so a caller that reads the frontier and then this may
// interleave with a commit without observing an inconsistent pair.
func (m *Manager) RootAt(round uint64) ([32]byte, bool) {
	m.frontierMu.RLock()
	defer m.frontierMu.RUnlock()

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
