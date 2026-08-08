package main

import (
	"fmt"

	"BluePods/internal/consensus"
	"BluePods/internal/events"
	"BluePods/internal/logger"
	"BluePods/internal/sync"
)

// THE ROUTING RULE, and why it is not a hole in the join gate.
//
// A node handed --bootstrap-addr is not necessarily joining. It may be
// restarting over the data directory it has been committing into for weeks,
// with the same flags its supervisor has always passed. The two are different
// events and take different paths:
//
//   - EMPTY DIRECTORY: the node has nothing, so everything it will hold comes
//     from a peer. That is FOREIGN state, and adopting it is exactly what the
//     checkpoint gate exists to police (see checkpoint.go): the source must not
//     be allowed to supply both the state and the authority that judges it.
//     runValidator, performSync, verifySyncedState — unchanged.
//
//   - ADOPTED STATE (consensus.HasAdoptedState): the node already owns the
//     committed history in this directory, either because it originated it
//     (a genuine genesis start, cfg.Bootstrap — runBootstrap is ALSO Run's
//     general fallthrough for any node with no upstream, and marks the state
//     adopted only when that flag is set, never on the fallthrough alone) or
//     because it proved it before going live on it. It is its own witness: it
//     lived that history, so there is no foreign claim to judge and nothing to
//     fetch. It boots from local state (initConsensus, the same path a
//     bootstrap restart takes) and catches up the ordinary way, through gossip
//     and the vertex fetcher, with every vertex it ingests fully validated.
//
// Routing a restart through the join gate is not merely redundant, it is
// unsatisfiable: the gate requires a stake quorum of live validators to attest
// the state, and after a full-cluster extinction the FIRST node back can never
// see one — every peer is down, precisely because that is what extinction
// means. Requiring it would make recovery impossible by construction.
//
// --trust-checkpoint stays mandatory for any node with an upstream
// (Config.validateTrustAnchor). parseFlags runs before storage is open, so it
// cannot know which branch this start will take, and an anchor missing from a
// directory that turns out to be empty must fail before the node buffers a
// single vertex. A resuming node simply does not use it, and says so.

// resumesFromLocalState reports whether this start is a restart over the node's
// own committed state rather than a join. It is deliberately narrow: only a
// non-genesis node that was given an upstream can be in doubt at all, and only
// the adopted-state marker (never the presence of committed state alone)
// settles it — a join the gate refused leaves committed state behind too, and
// resuming that would hand a rejected snapshot the acceptance it was denied.
func (n *Node) resumesFromLocalState() bool {
	if n.cfg.Bootstrap || n.cfg.BootstrapAddr == "" {
		return false
	}

	return consensus.HasAdoptedState(n.storage)
}

// runResume runs a node that boots from the committed state it already owns:
// it syncs nothing, verifies nothing and overwrites nothing, it rejoins the
// mesh and catches up from where it stopped. The DAG is already built from the
// data directory by the time this runs (NewNode's initConsensus), commit
// cursor, epoch state, live validator set and index included.
func (n *Node) runResume() error {
	logger.Info("resuming from local committed state",
		"round", n.dag.LastCommittedRound(),
		"validators", n.dag.ValidatorSet().Len(),
		"upstream_unused", n.cfg.BootstrapAddr,
	)

	n.setupMessageHandlers()
	n.setupRequestHandlers()

	if err := n.network.Start(); err != nil {
		return fmt.Errorf("start network:\n%w", err)
	}

	// The mesh is rebuilt from the restored validator set's own addresses. No
	// registration is submitted: this node is already a committed member, and
	// re-registering would announce a membership change that never happened.
	n.connectToExistingValidators()

	n.snapManager = sync.NewSnapshotManager(n.storage, n.dag)
	n.snapManager.SetDomainExporter(n.state)
	n.snapManager.Start()

	go n.processCommitted()

	events.NodeResumed(n.dag.LastCommittedRound())
	events.NodeReady(n.dag.LastCommittedRound())

	return n.serve()
}
