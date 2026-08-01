package consensus

import (
	"BluePods/internal/events"
	"BluePods/internal/index"
)

// indexer is the narrow surface the DAG feeds as objects are created,
// reparented, and deleted, and as committed rounds and epoch boundaries
// close. A nil indexer (the DAG's zero value) makes every feed point a
// no-op, so wiring the verifiable index is optional and additive over the
// existing commit path: cmd/node constructs a real *index.Manager and injects
// it through WithIndexer at construction, and any DAG built without that
// option runs exactly as it did before this package existed.
// The edge and object parameters below are typed [32]byte, not Hash: Hash is a
// named type over the same underlying array, so Hash values are directly
// assignable at every call site (Go's named/unnamed assignability rule), while
// [32]byte is what *index.Manager's methods actually take — internal/index
// stays free of any BluePods import, consensus included.
type indexer interface {
	// BuildFromState rebuilds every tree wholesale from the committed state
	// the DAG already holds. It runs once, inside New, before any goroutine
	// starts: the trees are derived state, so a node's index is only correct
	// from its first tick if it is rebuilt from the tracker, the domain
	// registry and the validator snapshot it boots with.
	BuildFromState(tracker []index.TrackerEntry, domains []index.DomainLeaf, validators []index.ValidatorLeaf)

	// ApplyEdge upserts child's parent-tree and children-tree edge, covering
	// both a newly created object's declared parent and a reparent's edge
	// move.
	ApplyEdge(child [32]byte, kind byte, parent [32]byte)

	// RemoveObject drops child from every tree it can appear in, on
	// deletion.
	RemoveObject(child [32]byte)

	// ApplyDomain upserts a name's domain-tree leaf. Every field of the leaf
	// is hashed into the tree, so registration, renewal, repoint and transfer
	// all feed through here: an expiry that moved in the registry without
	// moving in the tree would fork this node's anchored root.
	ApplyDomain(name string, objectID, owner [32]byte, expiryEpoch uint64)

	// RemoveDomain drops a name's leaf, on an owner's deletion or the expiry
	// sweep.
	RemoveDomain(name string)

	// RebuildValidators replaces the validator tree wholesale from a fresh
	// snapshot.
	RebuildValidators(entries []index.ValidatorLeaf)

	// SetFrontier records the committed round the current combined root
	// anchors.
	SetFrontier(round uint64)

	// CommittedFrontier returns the round and combined root of the most
	// recently recorded committed frontier, as one atomic pair — the read
	// side of SetFrontier. Vertex production calls it off the commit path's
	// own lock (see productionEpoch's liveEpoch mirror for the identical
	// reason), so the pair must come back atomic on the implementation's own
	// terms: a torn read across two separate calls could pair one commit's
	// round with a later commit's root, a false anchor stage-1 validation
	// rejects network-wide.
	CommittedFrontier() (round uint64, root [32]byte)

	// RootAt returns the combined root retained for a committed round, and
	// false when none is (the round is not decided yet, or it has fallen out
	// of the bounded retention window with no epoch checkpoint on it). It is
	// what ingress validation checks a received vertex's anchored pair
	// against, so like CommittedFrontier it is called off the commit path's
	// lock — from every gossip goroutine, concurrently with the commit loop's
	// own writes.
	RootAt(round uint64) (root [32]byte, ok bool)
}

// SetIndexer wires the verifiable-index manager after construction. It is a
// TEST-ONLY seam: package tests inject recording fakes and hand-built managers
// into a DAG whose loops they control, which no construction option can do.
// Production wires the index through WithIndexer instead — writing d.indexer
// once the commit and production goroutines are already running publishes it
// with no happens-before edge, and skips the construction backfill below, so
// every root the loop records until the wire lands is computed over empty
// trees.
//
// idx must never be a nil-typed concrete pointer (e.g. a nil *index.Manager)
// wrapped in the interface: `d.indexer != nil` is a check on the interface
// value, and an interface holding a nil concrete pointer is itself non-nil, so
// every nil-guarded feed site would call through to a nil receiver and panic.
// Only omitting the call at all leaves indexer correctly unset.
func (d *DAG) SetIndexer(idx indexer) {
	d.indexer = idx
}

// backfillIndex rebuilds the wired index from this DAG's committed state and
// anchors it at the round that state describes. New calls it after every
// option has been applied and BEFORE the commit and production goroutines
// start, which is what makes this call itself safe with no lock: it is the
// index's only writer at a point where no other goroutine exists yet to race
// it. It is not the trees' only out-of-loop writer overall, though: on a
// fresh chain, SeedGenesisLedger (dag.go) feeds the reserve coin's edge into
// the same trees while the commit loop is already running, and is ordered
// against it not by construction timing but by holding d.commitMu for that
// write — the same lock the commit loop's own writes serialize under (see
// Manager's field comment in manager.go for both out-of-loop writers).
//
// It covers all three boot paths uniformly, because each one installs its
// committed state through options that ran above: a restart resumes the
// tracker, the domain registry and the persisted epoch holders from Pebble; a
// sync joiner has WithImportData's tracker entries, the snapshot's domains
// behind the domain store, and the validator set rebuilt from the snapshot; a
// fresh chain has nothing to rebuild and takes the genesis seed through the
// live feed points instead.
//
// The frontier seed is the LAST DECIDED round, which is the commit cursor
// minus one (advanceCommitCursor sets the cursor to round+1, so it names the
// NEXT round to decide — the next-vs-last confusion that caused batch 0's I4
// bug). Seeding at the cursor itself would record a pre-batch root under the
// cursor round's key, and SetFrontier's non-advancing guard would then drop
// that round's real root when the commit loop decides it, forking RootAt
// against a never-restarted twin. A fresh chain (cursor 0) has decided
// nothing, so nothing is seeded: the commit loop is the sole frontier writer
// from round 0 on.
func (d *DAG) backfillIndex() {
	if d.indexer == nil {
		return
	}

	leaves := d.ValidatorLeaves(d.EpochHolders().All())

	d.indexer.BuildFromState(
		d.indexTrackerEntries(),
		d.indexDomainLeaves(),
		leaves,
	)

	// Publish the epoch's validator-set root from the boot rebuild too, not
	// only from the freezes that follow: a node that just booted has frozen
	// nothing yet, and an operator (or the scenario harness) reading a
	// checkpoint off it would otherwise find no root at all until the next
	// boundary.
	events.ValidatorsFrozen(d.currentEpoch, index.ValidatorRootOf(leaves), len(leaves))

	if d.lastCommitted > 0 {
		d.indexer.SetFrontier(lastDecidedRound(d.lastCommitted))
	}
}

// rebuildIndexValidators replaces the index's validator tree with the leaves
// of a freshly frozen holder snapshot and publishes that epoch's validator-set
// root. epoch is passed explicitly because the boundary's freeze runs BEFORE
// the epoch counter is incremented: the set being frozen belongs to the epoch
// the boundary opens, not the one it closes, and an event naming the wrong one
// would hand a joiner a checkpoint it can never match. No-op when no index is
// wired.
func (d *DAG) rebuildIndexValidators(epoch uint64, holders []*ValidatorInfo) {
	if d.indexer == nil {
		return
	}

	leaves := d.ValidatorLeaves(holders)
	d.indexer.RebuildValidators(leaves)

	events.ValidatorsFrozen(epoch, index.ValidatorRootOf(leaves), len(leaves))
}

// indexTrackerEntries converts the tracked objects into the index package's
// self-contained entry type, dropping the fields (version, replication, fees,
// child count) the hierarchy trees do not hash.
func (d *DAG) indexTrackerEntries() []index.TrackerEntry {
	entries := d.tracker.Export()

	out := make([]index.TrackerEntry, len(entries))
	for i, e := range entries {
		out[i] = index.TrackerEntry{ID: e.ID, ParentKind: e.ParentKind, Parent: e.Parent}
	}

	return out
}

// indexDomainLeaves converts the committed domain registry into the index
// package's leaf type. Owner and expiry ride along: the domain tree hashes
// both, so a rebuild that dropped them would compute a root no live node
// agrees with. A DAG with no domain store wired rebuilds an empty domain
// tree, the same no-op every other domain feed point takes when unset.
func (d *DAG) indexDomainLeaves() []index.DomainLeaf {
	if d.domains == nil {
		return nil
	}

	entries := d.domains.ExportDomains()

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

// ValidatorLeaves converts a validator snapshot into the index package's
// self-contained leaf type, computing each validator's capped voting weight
// with the SAME formula (cappedWeight) the quorum path uses, over entries'
// own total and size — matching cappedStakeOf so the index reports the same
// capped weight a quorum check would see for this exact set. Exported so
// cmd/node can build the genesis-time snapshot without duplicating the
// capping formula.
func (d *DAG) ValidatorLeaves(entries []*ValidatorInfo) []index.ValidatorLeaf {
	var rawTotal uint64
	for _, v := range entries {
		rawTotal = safeAdd(rawTotal, EffectiveStake(v))
	}

	leaves := make([]index.ValidatorLeaf, 0, len(entries))
	for _, v := range entries {
		status := index.ValidatorActive
		if v.Jailed {
			status = index.ValidatorJailed
		}

		leaves = append(leaves, index.ValidatorLeaf{
			Pubkey:      v.Pubkey,
			CappedStake: cappedWeight(EffectiveStake(v), rawTotal, d.votingCapMille, len(entries)),
			BLSKey:      v.BLSPubkey,
			Status:      status,
		})
	}

	return leaves
}
