package main

import (
	"BluePods/internal/consensus"
	"BluePods/internal/index"
	"BluePods/internal/state"
)

// initIndex constructs the verifiable-index manager, backfills it from the
// object tracker, the domain store and the validator set this node starts
// with, and wires it to the commit and domain-write paths so every subsequent
// mutation keeps it current.
//
// It runs unconditionally on all three construction paths, each of which has
// installed a complete committed state by the time it is called:
//
//   - fresh chain: seedGenesisState just seeded the genesis object;
//   - restart: the persisted tracker, domain store and live validator set come
//     back from the data directory;
//   - sync: WithImportData installed the snapshot's tracker, ImportDomains
//     wrote its domains, and buildValidatorSetFromSnapshot rebuilt its
//     validator set — all before the DAG was constructed.
//
// Either way the index is correct BEFORE this node produces or verifies a
// single vertex. A node that skipped this anchors an empty index's roots
// — (0, zero root) — in everything it produces, and every peer whose own
// retained history contradicts that pair rejects those vertices.
//
// SetIndexer lands here, during single-threaded boot, on every path: the DAG's
// indexer field has no happens-before guard of its own, so that ordering — the
// wire completing before the gossip handler is switched onto this DAG and
// before any buffered vertex is replayed — is what makes it safe to read from
// the commit and gossip goroutines. A dynamic re-wire later would need a real
// guard.
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
	//
	// The same arithmetic puts the sync path on the round its snapshot was cut
	// at, which is the point of reusing it: the source exports its LAST DECIDED
	// round (internal/sync's lastDecidedRound, cursor-1) and
	// WithLastCommittedRound reads it back as cursor = round+1, so cursor-1 is
	// exactly the snapshot's own committed frontier — the round the imported
	// tracker, domains and validator set describe, and the round a peer that
	// followed that history live holds the same root for in RootAt.
	if cursor := n.dag.LastCommittedRound(); cursor > 0 {
		mgr.SetFrontier(cursor - 1)
	}

	n.idxManager = mgr
	n.dag.SetIndexer(mgr)
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
// type. Owner and expiry ride along: the domain tree hashes both, so a rebuild
// that dropped them would compute a root no live node agrees with.
func domainLeaves(entries []state.DomainEntry) []index.DomainLeaf {
	out := make([]index.DomainLeaf, len(entries))
	for i, e := range entries {
		out[i] = index.DomainLeaf{
			Name:        e.Name,
			ObjectID:    e.ObjectID,
			Owner:       e.Owner,
			ExpiryEpoch: e.ExpiryEpoch,
		}
	}

	return out
}
