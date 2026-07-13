package consensus

import (
	"bytes"
	"testing"

	"BluePods/internal/types"
)

// TestAnchorProducerDeterministicAcrossInstances verifies that two independent
// DAG instances with the same validator membership derive the SAME anchor
// producer for every round, even when the two sets were built in opposite
// insertion orders. The anchor must depend only on the membership and the round,
// never on how a node happened to accumulate its validators.
func TestAnchorProducerDeterministicAcrossInstances(t *testing.T) {
	validators, _ := newTestValidatorSet(5)

	pubkeys := make([]Hash, len(validators))
	for i, v := range validators {
		pubkeys[i] = v.pubKey
	}

	// Second set has the same members inserted in reverse order.
	reversed := make([]Hash, len(pubkeys))
	for i, pk := range pubkeys {
		reversed[len(pubkeys)-1-i] = pk
	}

	dagA := New(newTestStorage(t), NewValidatorSet(pubkeys), nil, testSystemPod, 0, validators[0].privKey, nil)
	defer dagA.Close()

	dagB := New(newTestStorage(t), NewValidatorSet(reversed), nil, testSystemPod, 0, validators[0].privKey, nil)
	defer dagB.Close()

	for round := uint64(0); round < 100; round++ {
		gotA, okA := dagA.anchorProducerFor(round)
		gotB, okB := dagB.anchorProducerFor(round)

		if !okA || !okB {
			t.Fatalf("round %d: anchor not designated (okA=%v okB=%v)", round, okA, okB)
		}

		if gotA != gotB {
			t.Fatalf("round %d: anchor differs across instances: A=%x B=%x", round, gotA[:4], gotB[:4])
		}
	}
}

// TestAnchorProducerSpreadAcrossRounds verifies that the anchor is not pinned to a
// single producer: across many rounds the designation spreads over more than one
// member of the validator set, and every designated producer is a real member.
func TestAnchorProducerSpreadAcrossRounds(t *testing.T) {
	validators, vs := newTestValidatorSet(5)
	dag := New(newTestStorage(t), vs, nil, testSystemPod, 0, validators[0].privKey, nil)
	defer dag.Close()

	members := make(map[Hash]bool, len(validators))
	for _, v := range validators {
		members[v.pubKey] = true
	}

	distinct := make(map[Hash]bool)
	for round := uint64(0); round < 100; round++ {
		producer, ok := dag.anchorProducerFor(round)
		if !ok {
			t.Fatalf("round %d: anchor not designated", round)
		}

		if !members[producer] {
			t.Fatalf("round %d: anchor %x is not a validator", round, producer[:4])
		}

		distinct[producer] = true
	}

	if len(distinct) < 2 {
		t.Fatalf("anchor pinned to %d producer(s) over 100 rounds, expected spread", len(distinct))
	}

	t.Logf("anchor spread over %d/%d producers across 100 rounds", len(distinct), len(validators))
}

// TestAnchorProducerNoHolders verifies that a DAG with an empty validator set
// designates no anchor.
func TestAnchorProducerNoHolders(t *testing.T) {
	v := newTestValidator()
	dag := New(newTestStorage(t), NewValidatorSet(nil), nil, testSystemPod, 0, v.privKey, nil)
	defer dag.Close()

	if _, ok := dag.anchorProducerFor(7); ok {
		t.Fatal("expected no anchor with an empty holder set")
	}
}

// TestAnchorGetByRoundProducerEquivocation verifies the equivocation contract of
// store.getByRoundProducer: when one producer holds two vertices in the same
// round, the lookup returns BOTH hashes in a stable order derived from the hashes
// alone. Two independent stores that ingested the vertices in opposite orders must
// return the identical slice — the store never picks a local winner, so no
// view-dependent choice can fork the committed log.
func TestAnchorGetByRoundProducerEquivocation(t *testing.T) {
	equivocator := newTestValidator()
	const round = 7

	// Two distinct vertices from the same producer at the same round: distinct
	// parents give them distinct hashes.
	dataX, hashX := buildRoundVertex(t, equivocator, round, []Hash{{0x01}})
	dataY, hashY := buildRoundVertex(t, equivocator, round, []Hash{{0x02}})

	if hashX == hashY {
		t.Fatal("setup: expected two distinct vertex hashes")
	}

	// Store A ingests X then Y; store B ingests Y then X.
	storeA := newStore(newTestStorage(t))
	storeA.add(dataX, hashX, round, equivocator.pubKey)
	storeA.add(dataY, hashY, round, equivocator.pubKey)

	storeB := newStore(newTestStorage(t))
	storeB.add(dataY, hashY, round, equivocator.pubKey)
	storeB.add(dataX, hashX, round, equivocator.pubKey)

	resA, okA := storeA.getByRoundProducer(round, equivocator.pubKey)
	resB, okB := storeB.getByRoundProducer(round, equivocator.pubKey)

	if !okA || !okB {
		t.Fatalf("expected both stores to hold the producer's vertices (okA=%v okB=%v)", okA, okB)
	}

	if len(resA) != 2 || len(resB) != 2 {
		t.Fatalf("expected 2 hashes per store, got A=%d B=%d", len(resA), len(resB))
	}

	// Both stores must return the identical slice despite opposite insertion order.
	for i := range resA {
		if resA[i] != resB[i] {
			t.Fatalf("stores disagree at index %d: A=%x B=%x", i, resA[i][:4], resB[i][:4])
		}
	}

	// The order is ascending by hash bytes, independent of arrival.
	if bytes.Compare(resA[0][:], resA[1][:]) >= 0 {
		t.Fatalf("hashes not in ascending order: %x then %x", resA[0][:4], resA[1][:4])
	}
}

// TestAnchorGetByRoundProducerFiltersAndMisses verifies that getByRoundProducer
// returns only the queried producer's vertices and reports absence for a producer
// with no vertex in the round.
func TestAnchorGetByRoundProducerFiltersAndMisses(t *testing.T) {
	alice := newTestValidator()
	bob := newTestValidator()
	const round = 3

	dataA, hashA := buildRoundVertex(t, alice, round, []Hash{{0x01}})
	dataB, hashB := buildRoundVertex(t, bob, round, []Hash{{0x02}})

	s := newStore(newTestStorage(t))
	s.add(dataA, hashA, round, alice.pubKey)
	s.add(dataB, hashB, round, bob.pubKey)

	got, ok := s.getByRoundProducer(round, alice.pubKey)
	if !ok || len(got) != 1 || got[0] != hashA {
		t.Fatalf("expected alice's single vertex, got ok=%v hashes=%v", ok, got)
	}

	// A producer with no vertex in the round is reported absent.
	stranger := newTestValidator()
	if got, ok := s.getByRoundProducer(round, stranger.pubKey); ok || len(got) != 0 {
		t.Fatalf("expected no vertices for a non-producer, got ok=%v hashes=%v", ok, got)
	}
}

// buildRoundVertex builds a signed vertex for the validator at the round with the
// given parents (which vary the hash) and returns the raw bytes and its hash.
func buildRoundVertex(t *testing.T, v testValidator, round uint64, parents []Hash) ([]byte, Hash) {
	t.Helper()

	data := buildTestVertex(t, v, round, parents, 1)
	vertex := types.GetRootAsVertex(data, 0)

	var hash Hash
	copy(hash[:], vertex.HashBytes())

	return data, hash
}
