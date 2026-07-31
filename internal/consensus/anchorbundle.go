package consensus

import (
	"crypto/ed25519"

	"BluePods/internal/types"
)

// bundleWindow bounds how many rounds behind this node's own committed
// frontier a quorum bundle may report. The window is anchored SOLELY at the
// serving node's own CommittedFrontier(), never at the highest frontier any
// stored header merely CLAIMS: a byzantine header naming an absurd future
// frontier (say 10^9) must never drag the window there, which would deny
// bundles to every client asking this node — the availability-DoS the spec
// closes by pinning the window to a value this node itself decided.
// TestIndexAnchorBundle_WindowAnchoredAtOwnFrontier is the regression.
const bundleWindow = 16

// AnchorBundle is the quorum-attested anchor a light client verifies without
// downloading the index: the highest frontier within this node's window for
// which the stored headers matching this node's own RootAt reach the
// capped-stake quorum, together with those headers. Epoch is re-derived from
// FrontierRound via commitEpochForRound, never read off any header — the 3.1
// epoch window bounds a header's own epoch field to only ±1 of the truth, so
// trusting it here would let a producer misname its own quorum tree.
type AnchorBundle struct {
	FrontierRound uint64   // FrontierRound is the committed round IndexRoot anchors
	IndexRoot     Hash     // IndexRoot is this node's own index root at FrontierRound
	Epoch         uint64   // Epoch names the validator tree the quorum is weighed against
	Headers       [][]byte // Headers are the quorum's records, each headerSize+ed25519.SignatureSize (184) bytes: header ‖ signature, one per distinct producer
}

// IndexAnchorBundle assembles the quorum bundle GetIndexAnchor serves: the
// highest frontier in [CommittedFrontier-15, CommittedFrontier] for which
// stored vertex headers agreeing with this node's own RootAt at that
// frontier carry the capped-stake quorum of the epoch the frontier commits
// under. ok is false when no indexer is wired or no frontier in the window
// reaches quorum.
//
// It reads only the indexer seam (CommittedFrontier, RootAt) and the vertex
// store, both already safe for concurrent access from any goroutine (see
// anchorLie and referenceableParents, which read them off the commit path
// for the identical reason) — never commitMu, so a client polling this
// never waits on a commit batch.
func (d *DAG) IndexAnchorBundle() (AnchorBundle, bool) {
	if d.indexer == nil {
		return AnchorBundle{}, false
	}

	committed, _ := d.indexer.CommittedFrontier()

	windowFloor := uint64(0)
	if committed >= bundleWindow-1 {
		windowFloor = committed - (bundleWindow - 1)
	}

	tallies := d.collectAnchorTallies(windowFloor, committed)

	for frontier := committed; ; frontier-- {
		if bundle, ok := d.bundleAtFrontier(frontier, tallies[frontier]); ok {
			return bundle, true
		}

		if frontier == windowFloor {
			return AnchorBundle{}, false
		}
	}
}

// collectAnchorTallies walks every vertex this node holds from windowFloor
// through its highest stored round — a vertex's own production round is
// always at or after the frontier it anchors, so this span covers every
// candidate in the window regardless of how far production has run ahead of
// commit — and groups the ones whose declared anchor lands in the window and
// matches this node's own RootAt, keyed by the frontier they claim, one
// record per producer.
func (d *DAG) collectAnchorTallies(windowFloor, committed uint64) map[uint64]map[Hash][]byte {
	tallies := make(map[uint64]map[Hash][]byte)

	for round := windowFloor; round <= d.store.highestRound(); round++ {
		for _, h := range d.store.getByRound(round) {
			d.tallyAnchorVertex(tallies, h, windowFloor, committed)
		}
	}

	return tallies
}

// tallyAnchorVertex adds one stored vertex's header record to its frontier's
// producer tally, when the vertex's claimed anchor falls inside the window
// and matches this node's own retained root at that frontier. A header whose
// claimed root does not match is excluded right here — a quarantined liar's
// header never matches its own frontier's true root, so no separate
// quarantine check is needed on top of this comparison. A producer that has
// already contributed a record for the same frontier is left alone: only one
// header per producer counts toward a quorum.
func (d *DAG) tallyAnchorVertex(tallies map[uint64]map[Hash][]byte, h Hash, windowFloor, committed uint64) {
	v := d.store.get(h)
	if v == nil {
		return
	}

	frontier := v.FrontierRound()
	if frontier < windowFloor || frontier > committed {
		return
	}

	localRoot, ok := d.indexer.RootAt(frontier)
	if !ok || hashFrom(v.IndexRootBytes()) != localRoot {
		return
	}

	record := headerRecord(v)
	if record == nil {
		return
	}

	group, exists := tallies[frontier]
	if !exists {
		group = make(map[Hash][]byte)
		tallies[frontier] = group
	}

	producer := hashFrom(v.ProducerBytes())
	if _, already := group[producer]; !already {
		group[producer] = record
	}
}

// bundleAtFrontier reports the bundle at one candidate frontier, if its
// tallied producers reach the capped-stake quorum of the holder snapshot the
// frontier commits under (epoch re-derived via commitEpochForRound, never
// trusted from a header) — reusing reachesStrictQuorum, the same test the
// anchor-decision path certifies rounds with.
func (d *DAG) bundleAtFrontier(frontier uint64, group map[Hash][]byte) (AnchorBundle, bool) {
	if len(group) == 0 {
		return AnchorBundle{}, false
	}

	epoch := d.commitEpochForRound(frontier)

	set, ok := d.HoldersForEpoch(epoch)
	if !ok {
		return AnchorBundle{}, false
	}

	producers := make(map[Hash]bool, len(group))
	for p := range group {
		producers[p] = true
	}

	if !d.reachesStrictQuorum(set, producers) {
		return AnchorBundle{}, false
	}

	root, ok := d.indexer.RootAt(frontier)
	if !ok {
		return AnchorBundle{}, false
	}

	headers := make([][]byte, 0, len(group))
	for _, record := range group {
		headers = append(headers, record)
	}

	return AnchorBundle{FrontierRound: frontier, IndexRoot: root, Epoch: epoch, Headers: headers}, true
}

// headerRecord returns the {120-byte normative header (headerSize) ‖ 64-byte
// Ed25519 signature} record a quorum bundle serves for one producer's
// vertex, or nil when the stored signature is not exactly
// ed25519.SignatureSize long.
func headerRecord(v *types.Vertex) []byte {
	sig := v.SignatureBytes()
	if len(sig) != ed25519.SignatureSize {
		return nil
	}

	out := make([]byte, 0, headerSize+ed25519.SignatureSize)
	out = append(out, headerBytes(v)...)
	out = append(out, sig...)

	return out
}
