package consensus

import (
	"fmt"

	"BluePods/internal/types"
)

// validateIndexAnchor is stage 1 of the anchor enforcement the spec splits over
// ingress, production and commit: the part a receiver can decide alone, on the
// pair a vertex header already carries.
//
// It verifies exactly what is locally verifiable and passes everything else. A
// vertex whose frontier this node has not committed yet, or has committed too
// long ago to still retain, is ACCEPTED as it stands: not buffered, not
// retried, not held. Blocking the door on an anchor the receiver cannot check
// would couple vertex acceptance to commit lag and stall the DAG under normal
// frontier skew, while costing nothing an attacker cares about — a wrong root
// is already excluded from committed history by the production-side check
// (honest producers never reference an unverified parent), and convicted after
// the fact by the commit-side recheck. Ingress is the cheap 32-byte filter, not
// the guarantee.
//
// A mismatch is a terminal rejection, like a bad fee summary. It is NOT fault
// evidence: attributing the lie is the commit path's job, over vertices that
// actually entered committed history.
func (d *DAG) validateIndexAnchor(v *types.Vertex) error {
	if d.indexer == nil {
		return nil // no index wired: this node can verify nothing
	}

	frontier := v.FrontierRound()

	// "At or below the receiver's own committed frontier" is read from the
	// index seam, NOT from the commit cursor: LastCommittedRound returns the
	// NEXT round to decide (advanceCommitCursor sets it to round+1), so the
	// obvious `frontier <= LastCommittedRound()` is off by one and would treat
	// the undecided cursor round as verifiable — the exact next-vs-last
	// confusion that produced batch 0's I4 bug and cmd/node initIndex's
	// cursor-1 seed. Reading the cursor also takes commitMu, which would put a
	// whole commit batch's execution on every gossip goroutine. The seam's
	// frontier is the last round SetFrontier recorded, which IS the last
	// decided round, and it comes back under the manager's own lock.
	committed, _ := d.indexer.CommittedFrontier()
	if frontier > committed {
		return nil // anchors a frontier this node has not decided yet
	}

	root, ok := d.indexer.RootAt(frontier)
	if !ok {
		return nil // decided, but no longer retained: unverifiable, not wrong
	}

	claimed := hashFrom(v.IndexRootBytes())
	if claimed == root {
		return nil
	}

	if d.toleratesZeroAnchor(v, claimed) {
		return nil
	}

	return fmt.Errorf("index_root mismatch at frontier %d: claimed %x, local %x:\n%w",
		frontier, claimed[:8], root[:8], errIndexRoot)
}

// toleratesZeroAnchor reports whether a zero index root is acceptable on this
// vertex: only during the genesis epoch, where a producer may legitimately have
// no index to anchor yet. From the first epoch boundary on, an empty anchor is
// a wrong anchor (spec §5).
//
// The epoch is derived from the VERTEX's own round, never from the receiver's
// currentEpoch. The receiver's epoch is not network-uniform at any instant: a
// node that crashed, joined late or stalled trails the live epoch by however
// far its commit cursor trails, and it is precisely the vertices from ahead of
// its own epoch that it must accept to catch up at all. Judging a genesis
// vertex by the receiver's clock would terminally reject it on any node that
// has already crossed the boundary — one more instance of the receiver-relative
// unsoundness the epoch window was reworked to remove.
func (d *DAG) toleratesZeroAnchor(v *types.Vertex, claimed Hash) bool {
	return claimed == (Hash{}) && d.commitEpochForRound(v.Round()) == 0
}
