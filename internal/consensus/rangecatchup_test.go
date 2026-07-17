package consensus

import (
	"sort"
	"testing"
)

// rangeServer models a mesh peer that holds the full chain and answers both the
// single-hash frontier fetch and the round-range catch-up fetch. It records what the
// wait-stall recovery asked for; the test then serves one response and counts the
// round-trip, so the two recovery shapes can be compared by how fast they close a gap.
type rangeServer struct {
	byHash    map[Hash]frozenVertex   // byHash indexes every chain vertex the peer holds
	byRound   map[uint64]frozenVertex // byRound indexes the single-producer chain by round
	reqHashes []Hash                  // reqHashes are the hashes requested since the last serve
	reqRange  *[2]uint64              // reqRange is the round span requested since the last serve, if any
}

// FetchVertices records the requested hashes (the layer-by-layer frontier path).
func (s *rangeServer) FetchVertices(hashes []Hash) {
	s.reqHashes = append(s.reqHashes, hashes...)
}

// FetchRange records the requested round span (the deep-gap catch-up path).
func (s *rangeServer) FetchRange(from, to uint64) {
	span := [2]uint64{from, to}
	s.reqRange = &span
}

// serve answers the recorded requests into dag's store, modeling the end state a
// fetched, validated vertex reaches. A range response returns the LOWEST absent rounds
// first, capped at chunk, so an advancing cursor marches up the gap over successive
// requests. It clears the recorded requests and returns how many vertices it delivered.
func (s *rangeServer) serve(dag *DAG, chunk int) int {
	delivered := 0

	for _, h := range s.reqHashes {
		if fv, ok := s.byHash[h]; ok && dag.store.get(fv.hash) == nil {
			dag.store.add(fv.data, fv.hash, fv.round, fv.producer)
			delivered++
		}
	}
	s.reqHashes = nil

	if s.reqRange != nil {
		delivered += s.serveRange(dag, s.reqRange[0], s.reqRange[1], chunk)
		s.reqRange = nil
	}

	return delivered
}

// serveRange delivers the absent rounds in [from, to] ascending, capped at chunk.
func (s *rangeServer) serveRange(dag *DAG, from, to uint64, chunk int) int {
	var rounds []uint64
	for r := from; r <= to; r++ {
		if fv, ok := s.byRound[r]; ok && dag.store.get(fv.hash) == nil {
			rounds = append(rounds, r)
		}
	}
	sort.Slice(rounds, func(i, j int) bool { return rounds[i] < rounds[j] })

	delivered := 0
	for _, r := range rounds {
		if delivered >= chunk {
			break
		}
		fv := s.byRound[r]
		dag.store.add(fv.data, fv.hash, fv.round, fv.producer)
		delivered++
	}

	return delivered
}

// storeHasRounds reports whether dag's store holds a vertex at every round in [lo, hi].
func storeHasRounds(dag *DAG, lo, hi uint64) bool {
	for r := lo; r <= hi; r++ {
		if len(dag.store.getByRound(r)) == 0 {
			return false
		}
	}
	return true
}

// TestWaitStall_DeepGapClosesInBoundedRoundTrips pins the deep-behind reintegration
// recovery. A validator's commit cursor sits at N+1 with rounds 0..N present; it
// received a distant frontier (N+50..N+80) by gossip and buffered it, but the whole
// block beneath (N+1..N+49) never arrived and forward gossip will never re-push it.
// The single-layer frontier walk surfaces exactly one missing round per stalled tick,
// so the 49-round gap would close one round per round-trip; the range catch-up fetches
// the whole span at once and closes it in a couple. The test drives the wait-stall
// handler tick by tick against a serving peer and fails if the gap is not bridged
// within a small bound — which it is not on the layer-by-layer path (red).
func TestWaitStall_DeepGapClosesInBoundedRoundTrips(t *testing.T) {
	const (
		committedTop = 20  // rounds 0..committedTop are present at the start
		cursor       = 21  // the commit cursor: the first undecided (absent) round
		holeHi       = 69  // the gap is rounds cursor..holeHi (49 rounds)
		bufLo        = 70  // the buffered far frontier is rounds bufLo..bufHi
		bufHi        = 100 //
		chunk        = 256 // a range response returns up to this many vertices
		bound        = 5   // the gap must bridge within this many round-trips
	)

	vals, vs := newTestValidatorSet(1)
	spine := buildLinkedSpine(t, vals, bufHi)

	server := &rangeServer{
		byHash:  make(map[Hash]frozenVertex),
		byRound: make(map[uint64]frozenVertex),
	}
	for _, fv := range spine {
		server.byHash[fv.hash] = fv
		server.byRound[fv.round] = fv
	}

	dag := New(newTestStorage(t), vs, nil, testSystemPod, 0, vals[0].privKey, nil)
	dag.Close() // stop the background loops; the test drives the wait-stall handler directly
	dag.vertexFetcher = server
	dag.lastCommitted = cursor

	// Rounds 0..committedTop are already present in the store.
	for r := uint64(0); r <= committedTop; r++ {
		fv := spine[r]
		dag.store.add(fv.data, fv.hash, fv.round, fv.producer)
	}

	// The far frontier arrived by gossip and sits in the pending buffer, blocked on the
	// absent block beneath it.
	for r := uint64(bufLo); r <= bufHi; r++ {
		fv := spine[r]
		dag.bufferPendingVertex(fv.hash, fv.data)
	}

	roundTrips := 0
	for roundTrips < bound {
		roundTrips++

		dag.commitMu.Lock()
		dag.onWaitStall(cursor)
		dag.commitMu.Unlock()

		server.serve(dag, chunk)

		if storeHasRounds(dag, cursor, holeHi) {
			return
		}
	}

	t.Fatalf("deep gap (rounds %d..%d) not bridged within %d round-trips: the wait-stall path surfaces one round per tick", cursor, holeHi, bound)
}
