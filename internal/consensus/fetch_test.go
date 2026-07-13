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

// TestMissingAncestors_ReturnsAbsentFrontier verifies the walk surfaces exactly the
// referenced-but-absent vertices, and returns nothing when every ancestor is present.
func TestMissingAncestors_ReturnsAbsentFrontier(t *testing.T) {
	st, anchor, missing := stallStore(t)

	got := st.missingAncestors(anchor)
	if len(got) != 1 || got[0] != missing {
		t.Fatalf("missingAncestors = %v, want the single absent parent %x", got, missing[:4])
	}

	// The anchor itself is present, so once its parent is present too the frontier is
	// empty (nothing left to fetch).
	parent := newTestValidator()
	pData, pHash := buildRoundVertex(t, parent, 0, nil)
	_ = pHash
	st.add(pData, missing, 0, parent.pubKey) // store under the referenced (absent) hash

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

	// Deliver the missing ancestor, exactly as a successful fetch's AddVertex would land
	// it in the store, then let the commit loop pick it up.
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
