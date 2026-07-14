package consensus

import (
	"sync"
	"testing"
)

// stubFetcher records the hash lists handed to it, so a test can assert whether
// and when the commit loop's stall trigger requested missing ancestry.
type stubFetcher struct {
	mu    sync.Mutex // mu guards calls
	calls [][]Hash   // calls is one entry per FetchVertices invocation
}

// FetchVertices records a copy of the requested hashes.
func (s *stubFetcher) FetchVertices(hashes []Hash) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.calls = append(s.calls, append([]Hash(nil), hashes...))
}

// count returns how many times FetchVertices was called.
func (s *stubFetcher) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	return len(s.calls)
}

// last returns the hashes of the most recent FetchVertices call.
func (s *stubFetcher) last() []Hash {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.calls[len(s.calls)-1]
}

// stallStore builds a store holding a round-1 vertex whose sole parent (a round-0
// vertex) is absent, so missingAncestors(anchor) is exactly that absent parent —
// the minimal shape of a decided anchor blocked on a missing ancestor.
func stallStore(t *testing.T) (st *store, anchor, missing Hash) {
	t.Helper()

	st = newStore(newTestStorage(t))

	parent := newTestValidator()
	_, missing = buildRoundVertex(t, parent, 0, nil) // built but never stored

	producer := newTestValidator()
	data, anchor := buildRoundVertex(t, producer, 1, []Hash{missing})
	st.add(data, anchor, 1, producer.pubKey)

	return st, anchor, missing
}

// waitFrontierStore builds a store shaped like a synced joiner wedged at an UNDECIDED
// round: an import floor at round 5, a forward vertex at round 7 whose round-6 parent
// is absent (a historical vertex the joiner missed on join), so no DECIDED anchor
// exists to seed missingAncestors but missingFrontierAbove still surfaces the gap.
func waitFrontierStore(t *testing.T) (st *store, wedgeRound uint64, missing Hash) {
	t.Helper()

	validators, _ := newTestValidatorSet(2)
	v0, v1 := validators[0], validators[1]

	// Import a committed chain rounds 1..5: the entries carry the committed flag, so
	// the walk stops at the committed round-5 parent, not at a round floor.
	r1d, r1 := buildRoundVertex(t, v0, 1, nil)
	r2d, r2 := buildRoundVertex(t, v0, 2, []Hash{r1})
	r3d, r3 := buildRoundVertex(t, v0, 3, []Hash{r2})
	r4d, r4 := buildRoundVertex(t, v0, 4, []Hash{r3})
	r5d, r5 := buildRoundVertex(t, v0, 5, []Hash{r4})

	st = newStore(newTestStorage(t))
	st.ImportVertices([]VertexEntry{
		{Round: 1, Data: r1d, Committed: true}, {Round: 2, Data: r2d, Committed: true},
		{Round: 3, Data: r3d, Committed: true}, {Round: 4, Data: r4d, Committed: true},
		{Round: 5, Data: r5d, Committed: true},
	})

	// The round-6 vertex the joiner is missing (built but never stored).
	_, missing = buildRoundVertex(t, v1, 6, []Hash{r5})

	// A round-7 vertex the joiner DOES hold, citing the absent round-6 vertex.
	r7d, r7 := buildRoundVertex(t, v1, 7, []Hash{missing})
	st.add(r7d, r7, 7, v1.pubKey)

	return st, 6, missing
}

// TestMissingFrontierAbove_SurfacesGapAboveFloor verifies the frontier walk, seeded
// from the whole forward frontier (not a single decided anchor), surfaces exactly the
// referenced-but-absent vertex above the floor, and returns nothing once it is present.
func TestMissingFrontierAbove_SurfacesGapAboveFloor(t *testing.T) {
	st, wedgeRound, missing := waitFrontierStore(t)

	got := st.missingFrontierAbove(wedgeRound)
	if len(got) != 1 || got[0] != missing {
		t.Fatalf("missingFrontierAbove = %v, want the single absent round-6 vertex %x", got, missing[:4])
	}

	// Deliver the missing vertex (as a successful fetch's AddVertex would): the frontier
	// is then empty because the walk stops at the committed round-5 parent (its flag set).
	validators, _ := newTestValidatorSet(2)
	_, r5 := buildRoundVertex(t, validators[0], 5, nil)
	_ = r5
	md, _ := buildRoundVertex(t, validators[1], 6, nil)
	st.add(md, missing, 6, validators[1].pubKey)

	if got := st.missingFrontierAbove(wedgeRound); len(got) != 0 {
		t.Fatalf("missingFrontierAbove = %v, want empty once the gap is delivered", got)
	}
}

// TestOnWaitStall_FetchesFrontierOnSecondTick pins the WAIT-stall trigger: an
// UNDECIDED wedge does NOT fetch on the first stalled tick, fetches the missing
// frontier on the second, keeps retrying while wedged, and resets its count when the
// wedged round changes. This is the recovery the former I6 hole lacked.
func TestOnWaitStall_FetchesFrontierOnSecondTick(t *testing.T) {
	st, wedgeRound, missing := waitFrontierStore(t)
	fetcher := &stubFetcher{}
	dag := &DAG{store: st, vertexFetcher: fetcher}

	dag.onWaitStall(wedgeRound) // tick 1: no fetch yet
	if fetcher.count() != 0 {
		t.Fatalf("fetch fired on the first WAIT tick; want none until two consecutive stalls")
	}

	dag.onWaitStall(wedgeRound) // tick 2: fetch the missing frontier
	if fetcher.count() != 1 {
		t.Fatalf("want one fetch after two consecutive WAIT ticks, got %d", fetcher.count())
	}
	if got := fetcher.last(); len(got) != 1 || got[0] != missing {
		t.Fatalf("fetch requested %v, want the single missing vertex %x", got, missing[:4])
	}

	dag.onWaitStall(wedgeRound) // tick 3: still wedged -> retry (fetcher dedups in flight)
	if fetcher.count() != 2 {
		t.Fatalf("want a retry on the third WAIT tick, got %d fetches", fetcher.count())
	}

	dag.onWaitStall(wedgeRound + 1) // the cursor advanced to a new WAIT round: restart the count
	if fetcher.count() != 2 {
		t.Fatalf("a new wedged round must restart the count, got %d fetches", fetcher.count())
	}
}

// TestOnWaitStall_NilFetcherIsNoOp verifies the WAIT trigger is nil-safe: with no
// fetcher wired, a wedge never panics and never fetches.
func TestOnWaitStall_NilFetcherIsNoOp(t *testing.T) {
	st, wedgeRound, _ := waitFrontierStore(t)
	dag := &DAG{store: st} // vertexFetcher nil

	dag.onWaitStall(wedgeRound)
	dag.onWaitStall(wedgeRound) // second tick would fetch, but no fetcher is wired
}

// TestClearStall_ResetsWaitCount verifies an intervening cleared stall (cursor
// advanced) resets the WAIT count, so a later wedge again needs two consecutive ticks.
func TestClearStall_ResetsWaitCount(t *testing.T) {
	st, wedgeRound, _ := waitFrontierStore(t)
	fetcher := &stubFetcher{}
	dag := &DAG{store: st, vertexFetcher: fetcher}

	dag.onWaitStall(wedgeRound) // tick 1
	dag.clearStall()            // cursor advanced
	dag.onWaitStall(wedgeRound) // fresh first tick
	if fetcher.count() != 0 {
		t.Fatalf("fetch fired after a clear reset the WAIT count, got %d", fetcher.count())
	}

	dag.onWaitStall(wedgeRound) // second consecutive tick
	if fetcher.count() != 1 {
		t.Fatalf("want one fetch after two consecutive post-clear WAIT ticks, got %d", fetcher.count())
	}
}

// TestMissingAncestors_ReturnsAbsentFrontier verifies the walk surfaces exactly the
// referenced-but-absent vertices, and returns nothing when every ancestor is present.
func TestMissingAncestors_ReturnsAbsentFrontier(t *testing.T) {
	st, anchor, missing := stallStore(t)

	got := st.missingAncestors(anchor)
	if len(got) != 1 || got[0] != missing {
		t.Fatalf("missingAncestors = %v, want the single absent parent %x", got, missing[:4])
	}

	// The anchor itself is present, so once its parent is present too the frontier is
	// empty (nothing left to fetch). The payload is filed under the referenced hash
	// `missing`, NOT under pData's own hash: missingAncestors decides presence by store
	// key alone, so a byte-accurate payload is unnecessary — only the key matters.
	parent := newTestValidator()
	pData, _ := buildRoundVertex(t, parent, 0, nil)
	st.add(pData, missing, 0, parent.pubKey)

	if got := st.missingAncestors(anchor); len(got) != 0 {
		t.Fatalf("missingAncestors = %v, want empty once the ancestor is present", got)
	}
}

// TestOnCausalStall_FiresOnSecondConsecutiveTick pins the two-tick rule: a decided
// anchor blocked on missing ancestry does NOT fetch on the first stalled tick, fetches
// on the second (handing over the exact missing hash), keeps retrying while stalled,
// and resets its count when the stalling anchor changes.
func TestOnCausalStall_FiresOnSecondConsecutiveTick(t *testing.T) {
	st, anchor, missing := stallStore(t)
	fetcher := &stubFetcher{}
	dag := &DAG{store: st, vertexFetcher: fetcher}

	dag.onCausalStall(anchor) // tick 1: no fetch yet
	if fetcher.count() != 0 {
		t.Fatalf("fetch fired on the first stalled tick; want none until two consecutive stalls")
	}

	dag.onCausalStall(anchor) // tick 2: fetch the missing ancestry
	if fetcher.count() != 1 {
		t.Fatalf("want one fetch after two consecutive stalled ticks, got %d", fetcher.count())
	}
	if got := fetcher.last(); len(got) != 1 || got[0] != missing {
		t.Fatalf("fetch requested %v, want the single missing ancestor %x", got, missing[:4])
	}

	dag.onCausalStall(anchor) // tick 3: still stalled -> retry (in-flight dedup lives in the fetcher)
	if fetcher.count() != 2 {
		t.Fatalf("want a retry on the third stalled tick, got %d fetches", fetcher.count())
	}

	// A different stalling anchor restarts the two-tick count.
	other := Hash{0xAB, 0xCD}
	dag.onCausalStall(other)
	if fetcher.count() != 2 {
		t.Fatalf("a new stalling anchor must restart the count, got %d fetches", fetcher.count())
	}
}

// TestClearStall_RestartsTheTwoTickCount verifies that an intervening cleared stall
// (cursor advanced or round not decidable) resets the count, so a later stall must
// again see two consecutive ticks before fetching.
func TestClearStall_RestartsTheTwoTickCount(t *testing.T) {
	st, anchor, _ := stallStore(t)
	fetcher := &stubFetcher{}
	dag := &DAG{store: st, vertexFetcher: fetcher}

	dag.onCausalStall(anchor) // tick 1
	dag.clearStall()          // cursor advanced / round went to WAIT
	dag.onCausalStall(anchor) // counts as a fresh first tick
	if fetcher.count() != 0 {
		t.Fatalf("fetch fired after a clear reset the count, got %d", fetcher.count())
	}

	dag.onCausalStall(anchor) // now the second consecutive tick
	if fetcher.count() != 1 {
		t.Fatalf("want one fetch after two consecutive post-clear ticks, got %d", fetcher.count())
	}
}

// TestRequestMissingAncestors_NilFetcherIsNoOp verifies the trigger is nil-safe: with
// no fetcher wired, a stall never panics and never fetches (the node then relies on
// gossip and the pending buffer alone).
func TestRequestMissingAncestors_NilFetcherIsNoOp(t *testing.T) {
	st, anchor, _ := stallStore(t)
	dag := &DAG{store: st} // vertexFetcher nil

	dag.onCausalStall(anchor)
	dag.onCausalStall(anchor) // second tick would fetch, but no fetcher is wired
}

// TestFetch_DeliveredAncestorCommitsIdenticalBatch is the liveness-recovery guarantee:
// a node stalled on a decided anchor's missing ancestor, once that ancestor is
// delivered (as the fetcher's AddVertex would), commits the byte-identical ordered log
// a fully-fed twin commits. Missing-ancestor recovery must change only WHEN a batch
// commits, never WHAT it commits.
func TestFetch_DeliveredAncestorCommitsIdenticalBatch(t *testing.T) {
	vals, _ := newTestValidatorSet(4)
	const maxRound = 3
	spine := buildTaggedSpine(t, vals, maxRound)

	// Fully-fed twin: commits rounds 0,1 and the round-2 anchor (9 vertices).
	_, vsFull := reuseSet(vals)
	dagFull := newSpineDAG(t, vals, vsFull, maxRound)
	ingest(dagFull, spine, forwardOrder(len(spine)))
	dagFull.checkCommits()
	fullLog := drainCommitted(dagFull)

	// Starved node: omit one round-1 vertex the round-2 anchor cites (never the round-1
	// anchor, so rounds 0,1 still commit and the cursor reaches the stall at round 2).
	a1 := designatedProducer(t, dagFull, vals, 1)
	omit := round1OmitIndex(vals, a1)
	omitted := spine[omit]

	var order []int
	for i := range spine {
		if i != omit {
			order = append(order, i)
		}
	}

	_, vsStarved := reuseSet(vals)
	dagStarved := newSpineDAG(t, vals, vsStarved, maxRound)
	ingest(dagStarved, spine, order)
	dagStarved.checkCommits() // stalls at round 2 on the missing ancestor

	// Deliver the missing ancestor by inserting it straight into the store, modeling the
	// END STATE a fetched vertex reaches AFTER AddVertex validates it (store.add is not
	// AddVertex: it skips validation and processing). Then let the commit loop pick it up.
	dagStarved.store.add(omitted.data, omitted.hash, omitted.round, omitted.producer)
	dagStarved.checkCommits()
	recoveredLog := drainCommitted(dagStarved)

	if len(fullLog) == 0 {
		t.Fatal("fully-fed twin committed nothing; scenario did not exercise the commit path")
	}
	if !equalLog(fullLog, recoveredLog) {
		t.Fatalf("recovery changed the committed log:\n fed=%v\n recovered=%v", fullLog, recoveredLog)
	}
}

// round1OmitIndex returns the spine index of a round-1 vertex produced by a validator
// other than the round-1 anchor, in buildTaggedSpine's round-then-validator layout.
func round1OmitIndex(vals []testValidator, anchor1 testValidator) int {
	for j, v := range vals {
		if v.pubKey != anchor1.pubKey {
			return len(vals) + j // round 1 block starts at len(vals)
		}
	}

	return len(vals) // unreachable with >1 validator
}
