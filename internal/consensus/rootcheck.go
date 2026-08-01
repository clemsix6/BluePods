package consensus

import (
	"encoding/hex"
	"fmt"

	"BluePods/internal/logger"
	"BluePods/internal/types"
)

// anchorLie is the single verdict rule the three enforcement stages share: it
// reports whether a vertex's anchored pair is a PROVEN lie against this node's
// own retained history, and the two roots that prove it. Every stage asks the
// same question at a different moment, so the rule exists once — a second copy
// would let ingress, production and commit disagree about who is lying.
//
// It decides exactly what is locally decidable and calls everything else
// honest. A vertex whose frontier this node has not committed yet, or has
// committed too long ago to still retain, is NOT a liar: it is unverifiable,
// which is the normal state of a peer that commits faster than this node.
// Treating unverifiable as guilty would turn ordinary frontier skew into
// misbehaviour.
func (d *DAG) anchorLie(v *types.Vertex) (claimed, computed Hash, lying bool) {
	if d.indexer == nil {
		return Hash{}, Hash{}, false // no index wired: this node can verify nothing
	}

	frontier := v.FrontierRound()

	// "At or below this node's own committed frontier" is read from the index
	// seam, NOT from the commit cursor: LastCommittedRound returns the NEXT
	// round to decide (advanceCommitCursor sets it to round+1), so the obvious
	// `frontier <= LastCommittedRound()` is off by one and would treat the
	// undecided cursor round as verifiable — the exact next-vs-last confusion
	// that produced batch 0's I4 bug and the construction backfill's cursor-1
	// seed (backfillIndex).
	// Reading the cursor also takes commitMu, which would put a whole commit
	// batch's execution on every gossip goroutine. The seam's frontier is the
	// last round SetFrontier recorded, which IS the last decided round, and it
	// comes back under the manager's own lock.
	committed, _ := d.indexer.CommittedFrontier()
	if frontier > committed {
		return Hash{}, Hash{}, false // anchors a frontier this node has not decided yet
	}

	computed, ok := d.indexer.RootAt(frontier)
	if !ok {
		return Hash{}, Hash{}, false // decided, but no longer retained: unverifiable, not wrong
	}

	claimed = hashFrom(v.IndexRootBytes())
	if claimed == computed {
		return claimed, computed, false
	}

	if d.toleratesZeroAnchor(v, claimed) {
		return claimed, computed, false
	}

	return claimed, computed, true
}

// validateIndexAnchor is stage 1 of the anchor enforcement the spec splits over
// ingress, production and commit: the part a receiver can decide alone, on the
// pair a vertex header already carries.
//
// It verifies exactly what is locally verifiable and passes everything else. A
// vertex whose anchor this node cannot check is ACCEPTED as it stands: not
// buffered, not retried, not held. Blocking the door on an anchor the receiver
// cannot check would couple vertex acceptance to commit lag and stall the DAG
// under normal frontier skew, while costing nothing an attacker cares about —
// a wrong root is kept out of committed history by the production-side filter
// (stage 2), and convicted after the fact by the commit-side recheck (stage 3).
// Ingress is the cheap 32-byte filter, not the guarantee.
//
// A mismatch is a QUARANTINE, not a rejection: the vertex is stored and served
// on request, and withheld only from relay and from reference. Dropping it
// outright was a partition lever. A vertex some nodes refuse to store, smuggled
// into committed causal history through a reference from a node that could not
// check it, leaves the refusing nodes unable to complete that causal batch:
// their walk aborts on the vertex they lack, the fetcher re-requests it, ingress
// refuses it again, and the commit cursor — hence the root history retention
// that would ever let them re-decide — never moves again. Any node's causal
// batch must remain completable from its own store, so storage is not where a
// lie is punished. Relay and reference are.
//
// It is NOT fault evidence either: attributing the lie is the commit path's job,
// over vertices that actually entered committed history.
func (d *DAG) validateIndexAnchor(v *types.Vertex) error {
	claimed, computed, lying := d.anchorLie(v)
	if !lying {
		return nil
	}

	return fmt.Errorf("index_root mismatch at frontier %d: claimed %x, local %x:\n%w",
		v.FrontierRound(), claimed[:8], computed[:8], errIndexRoot)
}

// referenceableParents is stage 2, the teeth: the candidate parents a producer
// may reference, which is every candidate MINUS the ones it has proved are
// lying about their anchor. A proven liar is never referenced by an honest
// producer, so under the deterministic commit rule (a batch is the anchor's
// causal history) it does not enter committed history through this node — the
// network-wide exclusion spec §5 asks for.
//
// The filter is a denylist of proven liars, NOT an allowlist of verified
// anchors, and the difference is liveness. A producer references round N-1
// vertices whose anchors trail its own commit by the structural lag, so it
// usually can verify them — but "usually" is not "always": any peer that
// commits a round before this node does anchors a frontier ABOVE this node's
// own, which makes its perfectly honest vertex unverifiable HERE until this
// node's own commit catches up. An allowlist drops exactly those, and a
// producer that is one round behind its peers is left referencing nothing: the
// vertex it builds has no parents, its own validateParents rejects it, and it
// stops producing because its peers are ahead. Ordinary frontier skew would
// become a production wedge, and the node whose index lags would be the one
// silenced.
//
// Excluding proven liars only keeps both halves of the spec's promise: a
// vertex this node has PROVED wrong is never referenced (safety, the property
// stage 2 exists for), and an honest peer this node cannot check yet is
// referenced normally (liveness). What that leaves is a wrong-root vertex a
// lagging producer may still reference before it could verify it, which is
// precisely the residue stage 3 exists to record: the commit path re-checks
// every committed vertex once the frontier IS local, and convicts the liar with
// its own signature.
//
// This is a production-side choice, local to the vertex this node builds; it
// never decides the committed log, so nodes filtering different sets cannot
// fork it. A candidate whose vertex this node cannot read is dropped for the
// same reason: a parent link it cannot resolve carries no producer, so it is
// worth nothing to the peers that receive it.
func (d *DAG) referenceableParents(candidates []Hash) []Hash {
	kept := make([]Hash, 0, len(candidates))

	for _, h := range candidates {
		v := d.store.get(h)
		if v == nil {
			continue
		}

		if d.provenLiar(h, v) {
			logger.Warn("excluding wrong-root parent from production",
				"parent", hex.EncodeToString(h[:8]), "frontier", v.FrontierRound())
			continue
		}

		kept = append(kept, h)
	}

	return kept
}

// provenLiar reports whether this node has proved the candidate wrong about its
// anchor, by either route: the quarantine mark ingress persisted when the vertex
// arrived, or a fresh verdict from the current history.
//
// The mark is consulted FIRST, and outside the indexer guard, because it is the
// only one of the two that survives. The root history behind anchorLie is a
// bounded window and a restart rebuilds the index without it, so a node that
// asked only the live verdict would start referencing, after a reboot or a long
// enough run, exactly the vertex it had proved wrong. A verdict reached once is
// not un-reached by forgetting the evidence.
func (d *DAG) provenLiar(h Hash, v *types.Vertex) bool {
	if d.store.isQuarantined(h) {
		return true
	}

	if d.indexer == nil {
		return false
	}

	_, _, lying := d.anchorLie(v)

	return lying
}

// recheckCommittedAnchor is stage 3, the record: as the commit cursor passes a
// vertex, its anchor is checked once more against a history that has grown
// since the vertex arrived. A frontier that was above this node's own at
// ingress is below it by the time the vertex commits, so a lie stage 1 could
// not see and stage 2 could not exclude is caught here — and it is caught on
// every node, since committed history is identical everywhere.
//
// It convicts, it does not correct: the vertex is committed and stays
// committed. Rolling it back would make the committed log depend on each node's
// index retention, which is exactly the divergence the anchor exists to detect.
// What comes out is attributable evidence, kept node-locally (see prefixFault),
// and an event.
//
// Runs on the commit path under commitMu.
func (d *DAG) recheckCommittedAnchor(v *types.Vertex) {
	claimed, computed, lying := d.anchorLie(v)
	if !lying {
		return
	}

	d.recordAnchorFault(v, claimed, computed)
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
