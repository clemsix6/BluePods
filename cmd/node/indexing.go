package main

import (
	"BluePods/internal/consensus"
	"BluePods/internal/index"
	"BluePods/internal/state"
)

// initIndex constructs the verifiable-index manager, backfills it from
// whatever this node's data directory already holds — the object tracker,
// the domain store, and the live validator set — and wires it to the commit
// and domain-write paths so every subsequent mutation keeps it current.
//
// The backfill runs unconditionally, on both a fresh chain and a restart:
// called right after seedGenesisState, it sees a freshly seeded genesis
// object on first boot and the full persisted tracker/domain/validator state
// on a restart over an existing data directory, either way rebuilding a
// correct index BEFORE this node produces or verifies a single vertex. A
// restarted node that skipped this would anchor an empty index's roots and
// be silently excluded by peers the moment anchoring lands.
func (n *Node) initIndex() {
	mgr := index.NewManager()

	mgr.BuildFromState(
		trackerEntries(n.dag.ExportTrackerEntries()),
		domainLeaves(n.state.ExportDomains()),
		n.dag.ValidatorLeaves(n.dag.EpochHolders().All()),
	)

	// Seed the boot frontier at the last DECIDED round, the round whose
	// committed state the backfill above just rebuilt. The commit cursor is
	// the NEXT round to decide, NOT the last committed (advanceCommitCursor
	// sets it to round+1; this exact next-vs-last confusion caused batch 0's
	// I4 bug), so the seed round is cursor-1. Seeding at the cursor itself
	// would record a pre-batch root under the cursor round's key, and
	// SetFrontier's non-advancing guard would then drop that round's real
	// root when the resumed commit loop decides it — forking RootAt against
	// a never-restarted twin. A fresh chain (cursor 0) has decided nothing,
	// so there is no round to seed: the commit loop is the sole frontier
	// writer from round 0 on.
	if cursor := n.dag.LastCommittedRound(); cursor > 0 {
		mgr.SetFrontier(cursor - 1)
	}

	n.idxManager = mgr
	n.dag.SetIndexer(mgr)

	// Domain writes still land through the pod output path (batch 4 replaces
	// this with a declared operation); applyRegisteredDomains is where they
	// are written today, so its callback is the index's only domain feed.
	n.state.SetOnDomainRegistered(func(name string, objectID [32]byte) {
		mgr.ApplyDomain(name, objectID, [32]byte{}, 0)
	})
}

// trackerEntries converts consensus tracker entries into the index package's
// self-contained entry type, dropping the fields (version, replication,
// fees, child count) the index does not need.
func trackerEntries(entries []consensus.ObjectTrackerEntry) []index.TrackerEntry {
	out := make([]index.TrackerEntry, len(entries))
	for i, e := range entries {
		out[i] = index.TrackerEntry{ID: e.ID, ParentKind: e.ParentKind, Parent: e.Parent}
	}

	return out
}

// domainLeaves converts state's domain entries into the index package's leaf
// type. The domain store carries no owner or expiry yet (that lands with
// rental economics), so both fields are zero until then.
func domainLeaves(entries []state.DomainEntry) []index.DomainLeaf {
	out := make([]index.DomainLeaf, len(entries))
	for i, e := range entries {
		out[i] = index.DomainLeaf{Name: e.Name, ObjectID: e.ObjectID}
	}

	return out
}
