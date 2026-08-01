package consensus

import "BluePods/internal/index"

// indexer is the narrow surface the DAG feeds as objects are created,
// reparented, and deleted, and as committed rounds and epoch boundaries
// close. A nil indexer (the DAG's zero value) makes every feed point a
// no-op, so wiring the verifiable index is optional and additive over the
// existing commit path: cmd/node constructs a real *index.Manager and injects
// it through SetIndexer once it exists, and any DAG built without that call
// runs exactly as it did before this package existed.
// The edge and object parameters below are typed [32]byte, not Hash: Hash is a
// named type over the same underlying array, so Hash values are directly
// assignable at every call site (Go's named/unnamed assignability rule), while
// [32]byte is what *index.Manager's methods actually take — internal/index
// stays free of any BluePods import, consensus included.
type indexer interface {
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

// SetIndexer wires the verifiable-index manager so object creation, reparent,
// deletion, epoch validator snapshots, and committed frontiers all feed it.
// Left unset, every feed point is a no-op.
//
// idx must never be a nil-typed concrete pointer (e.g. a nil *index.Manager)
// wrapped in the interface: `d.indexer != nil` is a check on the interface
// value, and an interface holding a nil concrete pointer is itself non-nil, so
// every nil-guarded feed site would call through to a nil receiver and panic.
// Only omitting the call at all leaves indexer correctly unset.
func (d *DAG) SetIndexer(idx indexer) {
	d.indexer = idx
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
