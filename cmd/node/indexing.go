package main

import (
	"BluePods/internal/consensus"
	"BluePods/internal/index"
)

// committedStateOpts builds this node's verifiable-index manager and returns
// the construction options that install, before consensus.New starts a single
// goroutine, everything the DAG's committed decisions depend on: the domain
// registry leases are validated and swept against, the governed fee parameters
// that price and cap them, and the index seam itself.
//
// They come as one set because the index rebuild reads the other two. New's
// backfill (see consensus.backfillIndex) rebuilds the four trees from the
// tracker, the domain registry and the validator snapshot this node boots
// with, then anchors them at the last DECIDED round — so every subsequent
// mutation, arriving through the commit path's own feed points, keeps a
// correct index current. Domain writes are no exception: they arrive through
// that same commit path (applyDomainOp calling writeDomainLeaf), not a
// separate wire of their own.
//
// All three construction paths pass these options, and each has installed the
// state they name by the time it calls New:
//
//   - fresh chain: nothing to rebuild — the genesis object arrives moments
//     later through seedGenesisState's own tracker feed, and the founder's
//     stake through the committed-member refreeze;
//   - restart: the persisted tracker, domain registry, live validator set and
//     epoch holders all come back from the data directory inside New;
//   - sync: WithImportData carries the snapshot's tracker, ImportDomains wrote
//     its domains before initState reopened the handle passed here, and
//     buildValidatorSetFromSnapshot rebuilt its validator set.
//
// Either way the index is correct BEFORE this node produces or verifies a
// single vertex. A node that skipped this anchors an empty index's roots —
// (0, zero root) — in everything it produces, and every peer whose own
// retained history contradicts that pair rejects those vertices.
//
// Construction is also the only point at which the wire is sound. The commit
// loop and the gossip goroutines read the DAG's indexer field with no
// happens-before guard of their own, and every round the commit loop decides
// before a post-construction wire lands records a root computed over empty
// trees — a wrong RootAt entry for a round the rest of the network holds a
// real root for, permanent because SetFrontier keeps the first root recorded
// for a round. Passing the manager as an option closes both: the field and the
// trees behind it are written once, on the constructing goroutine, and only
// read by loops that start after New returns.
//
// What the rebuild does NOT reconstruct: quarantine. A vertex proven to anchor
// a wrong root is stored under a `vq/` mark on the node that proved it, and
// snapshot-imported vertices carry no such mark — the joiner has verified
// nothing about them, so it is in the "could not check" class, which is the
// correct place for it and the same place any node sits for history predating
// its own retention. The honest limit is that the quarantine SET is not
// reconstructible from a snapshot: a joiner cannot learn which vertices its
// source convicted, it will relay a lie its source refused to relay until it
// proves that lie itself, and the fault records that convict a producer live
// only on the nodes that were there. Quarantine is per-node evidence, not
// replicated state; stage 3's commit re-check is what makes the verdict
// converge, on every node, when the lie reaches a committed batch.
func (n *Node) committedStateOpts() []consensus.Option {
	n.idxManager = index.NewManager()

	return []consensus.Option{
		consensus.WithDomainStore(n.state),
		consensus.WithFeeParams(n.feeParams()),
		consensus.WithIndexer(n.idxManager),
	}
}

// feeParams returns this node's governed fee parameters, allocated once and
// shared by every consumer: the DAG prices and caps domain leases from them,
// the state layer stamps storage deposits from them, and both must read the
// same values or a created object's stamped deposit and the fee debited for it
// drift apart.
func (n *Node) feeParams() *consensus.FeeParams {
	if n.fees == nil {
		params := consensus.DefaultFeeParams()
		n.fees = &params
	}

	return n.fees
}
