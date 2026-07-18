package consensus

import (
	"testing"

	"BluePods/internal/types"
)

// linkedGossip records every gossiped vertex so the driver can hand it to the
// peer half on its own schedule, modeling the mesh carrying a vertex across a
// healed partition without re-entering the producing DAG under its own lock.
type linkedGossip struct {
	out [][]byte // out holds the vertices gossiped since the last take
}

// Gossip records a copy of the gossiped vertex.
func (g *linkedGossip) Gossip(data []byte, fanout int) error {
	cp := make([]byte, len(data))
	copy(cp, data)
	g.out = append(g.out, cp)
	return nil
}

// take returns the gossiped vertices and clears the buffer.
func (g *linkedGossip) take() [][]byte {
	out := g.out
	g.out = nil
	return out
}

// frozenVertex is a prebuilt vertex with its identity, ingested directly so the
// test controls exactly which frontier leaves each half holds.
type frozenVertex struct {
	data     []byte // data is the serialized vertex
	hash     Hash   // hash is the vertex hash
	round    uint64 // round is the vertex round
	producer Hash   // producer is the producing validator
}

// buildLinkedSpine builds a fully connected spine over rounds 0..maxRound: every
// validator produces one vertex per round citing every vertex of the previous
// round with proper producer links, so a cross-delivered leaf passes the receiving
// node's parent-quorum check. Vertices carry epoch 0. Returned in round-then-
// validator order, so round r's block starts at index r*len(vals).
func buildLinkedSpine(t *testing.T, vals []testValidator, maxRound uint64) []frozenVertex {
	t.Helper()

	var out []frozenVertex
	var prev []parentLink

	for r := uint64(0); r <= maxRound; r++ {
		var cur []parentLink
		for _, v := range vals {
			data := buildTestVertexWithParentLinks(t, v, r, 0, prev)
			vertex := types.GetRootAsVertex(data, 0)

			var h Hash
			copy(h[:], vertex.HashBytes())

			out = append(out, frozenVertex{data: data, hash: h, round: r, producer: v.pubKey})
			cur = append(cur, parentLink{hash: h, producer: v.pubKey})
		}
		prev = cur
	}

	return out
}

// newFrozenHalf builds the DAG of one node of a symmetrically frozen cluster. It
// holds the shared certified spine below the frontier round in full and only its
// own side's leaves at the frontier round, and is pinned at the frozen state
// (lastProducedRound = frontier, round = frontier+1). Its background loops are
// stopped so the test drives production tick by tick.
func newFrozenHalf(t *testing.T, vals []testValidator, vs *ValidatorSet, self testValidator, g Broadcaster, spine []frozenVertex, frontier uint64, ownLeaves []int) *DAG {
	t.Helper()

	dag := New(newTestStorage(t), vs, g, testSystemPod, 0, self.privKey, nil)
	dag.Close() // stop the background commit/liveness loops; the test drives production directly
	setEqualStake(dag, vals, 25)

	// Shared certified spine, rounds 0..frontier-1 in full.
	for i := 0; i < int(frontier)*len(vals); i++ {
		fv := spine[i]
		dag.store.add(fv.data, fv.hash, fv.round, fv.producer)
	}

	// Only this side's own leaves at the frozen frontier round.
	for _, i := range ownLeaves {
		fv := spine[i]
		dag.store.add(fv.data, fv.hash, fv.round, fv.producer)
	}

	dag.lastProducedRound.Store(frontier)
	dag.round.Store(frontier + 1)

	return dag
}

// TestSymmetricFreeze_RebroadcastReconverges pins the post-heal recovery of a
// symmetrically frozen cluster. Four equal-stake validators split 2|2: each side
// produced its own two frontier leaves but holds neither of the other side's, so
// each round frontier carries only two of four producers — below the three-of-four
// stake quorum — and neither side can produce the next round. Forward gossip fired
// once at production and never repeats, so once the sides reconnect the leaves the
// other needs are never re-announced. Re-broadcasting each side's own frozen leaf
// on the liveness tick is what carries the missing leaves across: once each side
// receives one leaf from the other it reaches quorum at the frontier round and
// produces frontier+1. Without the re-broadcast the cannot-produce branch is silent
// and both halves wedge forever (red).
func TestSymmetricFreeze_RebroadcastReconverges(t *testing.T) {
	const nvals = 4
	const frontier = 6

	vals, _ := newTestValidatorSet(nvals)
	spine := buildLinkedSpine(t, vals, frontier)

	base := int(frontier) * nvals
	gA := &linkedGossip{}
	gB := &linkedGossip{}

	_, vsA := reuseSet(vals)
	a := newFrozenHalf(t, vals, vsA, vals[0], gA, spine, frontier, []int{base + 0, base + 1})

	_, vsB := reuseSet(vals)
	b := newFrozenHalf(t, vals, vsB, vals[2], gB, spine, frontier, []int{base + 2, base + 3})

	converged := func(d *DAG) bool { return d.Round() >= frontier+2 }

	const maxTicks = 10
	for tick := 0; tick < maxTicks; tick++ {
		a.tryProduceVertex()
		b.tryProduceVertex()

		for _, data := range gA.take() {
			b.AddVertex(data)
		}
		for _, data := range gB.take() {
			a.AddVertex(data)
		}

		if converged(a) && converged(b) {
			break
		}
	}

	if !converged(a) || !converged(b) {
		t.Fatalf("frozen halves never reconverged: a.round=%d b.round=%d, want both >= %d", a.Round(), b.Round(), frontier+2)
	}
}
