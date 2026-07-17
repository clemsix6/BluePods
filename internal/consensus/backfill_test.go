package consensus

import (
	"testing"

	"BluePods/internal/types"
)

// deliveringFetcher stands in for a mesh peer that holds every chain vertex: each
// requested hash it has is fed back through the real AddVertex path, modeling a fetch
// response. Delivery is synchronous so the backfill-iteration count is deterministic.
type deliveringFetcher struct {
	dag    *DAG            // dag is the node under test the delivered vertices re-enter
	source map[Hash][]byte // source maps every chain vertex hash to its serialized bytes
}

// FetchVertices delivers every requested hash the source holds through AddVertex — the
// same path a real fetch response takes — so a delivered root triggers the pending
// cascade exactly as it would in production.
func (f *deliveringFetcher) FetchVertices(hashes []Hash) {
	for _, h := range hashes {
		if data, ok := f.source[h]; ok {
			f.dag.AddVertex(data)
		}
	}
}

// FetchRange is unused by the causal-stall backfill this fetcher exercises.
func (f *deliveringFetcher) FetchRange(from, to uint64) {}

// deepBufferedGap builds an observer DAG whose commit cursor is blocked on a decided
// anchor sitting above a DEEP causal gap: the anchor is stored, a long single-producer
// chain below it is entirely BUFFERED (the descendants arrived out of order and cannot
// yet validate), and only the chain's true root is absent everywhere. It models a node
// rejoining after a partition, where gossip has re-delivered the descendants but never
// the historical root the whole cascade is blocked on. The background commit loop is
// stopped so only the causal-stall recovery under test runs (the WAIT/frontier path is
// not exercised here). It returns the DAG, the stored anchor, the absent root, a source
// of every chain vertex keyed by hash (a peer that holds them all), and the gap depth.
func deepBufferedGap(t *testing.T) (dag *DAG, anchor, root Hash, source map[Hash][]byte, depth int) {
	t.Helper()

	vals, vs := newTestValidatorSet(4)
	producer := vals[0]

	// A non-member key so ingestion never triggers the observer's own production, and
	// Close() so the background commit/liveness loops do not race the direct calls.
	observer := newTestValidator()
	dag = New(newTestStorage(t), vs, nil, testSystemPod, 0, observer.privKey, nil)
	dag.Close()

	const gapDepth = 40
	depth = gapDepth

	// A linked single-producer chain rounds 0..depth. Every vertex is a valid, signed
	// vertex a peer could serve; the map is that peer.
	source = make(map[Hash][]byte)
	chain := make([]Hash, depth+1)

	var parents []parentLink
	for round := 0; round <= depth; round++ {
		data := buildTestVertexWithParentLinks(t, producer, uint64(round), 0, parents)

		var h Hash
		copy(h[:], types.GetRootAsVertex(data, 0).HashBytes())

		source[h] = data
		chain[round] = h
		parents = []parentLink{{hash: h, producer: producer.pubKey}}
	}

	anchor = chain[depth]
	root = chain[0]

	// The anchor is DECIDED but its ancestry is not local: place it straight in the
	// store (as the decision layer's commit attempt leaves it), skipping validation.
	dag.store.add(source[anchor], anchor, uint64(depth), producer.pubKey)

	// Rounds 1..depth-1 arrive but cannot validate (their parents are absent), so they
	// pile into the pending buffer — the buffered cascade blocked on the absent root.
	for round := 1; round < depth; round++ {
		dag.AddVertex(source[chain[round]])
	}

	return dag, anchor, root, source, depth
}

// TestBackfill_DeepBufferedGapFillsInBoundedIterations is the backfill-speed guarantee:
// a node blocked on a decided anchor above a DEEP buffered gap must fetch the whole
// known depth of that gap in a bounded number of round-trips, not one frontier layer
// per stalled tick. A reintegrating node facing dozens or hundreds of missing rounds
// otherwise crawls the gap one layer at a time over tens of seconds, overrunning the
// bounded catch-up windows the scenarios enforce.
//
// The delivering fetcher stands in for a peer: each requested hash it holds re-enters
// through the real AddVertex path, exactly as a network fetch response would. The loop
// counts how many fetch passes it takes to make the anchor's causal batch whole. On the
// one-layer-per-tick path the causal recovery asks only for the anchor's immediate
// parent — which is already buffered — so a delivery makes no progress and the gap
// never closes. The fix surfaces the single root below the cascade; one delivery flushes
// the whole buffer, closing the gap in a single pass.
func TestBackfill_DeepBufferedGapFillsInBoundedIterations(t *testing.T) {
	dag, anchor, _, source, depth := deepBufferedGap(t)

	fetcher := &deliveringFetcher{dag: dag, source: source}
	dag.SetVertexFetcher(fetcher)

	const maxIterations = 3
	const safetyBound = 12 // hard stop so a non-converging path cannot hang the test

	iterations := 0
	filled := false
	for iterations < safetyBound {
		if _, ok := dag.store.causalBatch(anchor); ok {
			filled = true
			break
		}

		dag.requestMissingAncestors(anchor)
		iterations++
	}

	if !filled {
		t.Fatalf("a %d-layer buffered gap never backfilled in %d fetch passes: the causal recovery fetches only the anchor's immediate buffered parent, never the true root below the cascade", depth, safetyBound)
	}

	if iterations > maxIterations {
		t.Fatalf("a %d-layer buffered gap took %d fetch passes to backfill; want <= %d (the whole known depth is surfaced per pass, not one layer per pass)", depth, iterations, maxIterations)
	}
}

// TestOnCausalStall_FetchesImmediatelyWhenGapSitsBehindBufferedCascade pins the deep-gap
// trigger: when the missing ancestry sits behind a buffered cascade, recovery must fetch
// on the FIRST stalled tick rather than spending the two-tick gossip grace. Forward
// gossip never re-pushes an old vertex, so the grace there is pure catch-up latency —
// only a targeted fetch of the roots below the buffer can unblock the cursor. The shallow
// case still waits two ticks (TestOnCausalStall_FiresOnSecondConsecutiveTick).
func TestOnCausalStall_FetchesImmediatelyWhenGapSitsBehindBufferedCascade(t *testing.T) {
	dag, anchor, root, _, _ := deepBufferedGap(t)

	fetcher := &recordingFetcher{}
	dag.SetVertexFetcher(fetcher)

	dag.onCausalStall(anchor) // first stalled tick

	if !fetcher.requested(root) {
		t.Fatalf("first causal-stall tick did not fetch the root %x below the buffered cascade; a deep gap must not wait out the two-tick gossip grace", root[:4])
	}
}
