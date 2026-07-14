package consensus

import (
	"bytes"
	"os"
	"testing"

	"BluePods/internal/storage"
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
	freezeGenesis(dagA)

	dagB := New(newTestStorage(t), NewValidatorSet(reversed), nil, testSystemPod, 0, validators[0].privKey, nil)
	defer dagB.Close()
	freezeGenesis(dagB)

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
	freezeGenesis(dag)

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

// addRoundVertex builds and stores a vertex for the validator at the round with
// the given parents, returning its hash.
func addRoundVertex(t *testing.T, s *store, v testValidator, round uint64, parents []Hash) Hash {
	t.Helper()

	data, hash := buildRoundVertex(t, v, round, parents)
	s.add(data, hash, round, v.pubKey)

	return hash
}

// roundOf reads a stored vertex's round, failing the test if it is absent.
func roundOf(t *testing.T, s *store, hash Hash) uint64 {
	t.Helper()

	v := s.get(hash)
	if v == nil {
		t.Fatalf("vertex %x absent from store", hash[:4])
	}

	return v.Round()
}

// assertBatchOrder verifies the batch is sorted by round ascending, then by hash
// bytes ascending within a round.
func assertBatchOrder(t *testing.T, s *store, batch []Hash) {
	t.Helper()

	for i := 1; i < len(batch); i++ {
		prev := roundOf(t, s, batch[i-1])
		cur := roundOf(t, s, batch[i])

		if prev > cur {
			t.Fatalf("batch not round-ordered at %d: round %d then %d", i, prev, cur)
		}

		if prev == cur && bytes.Compare(batch[i-1][:], batch[i][:]) >= 0 {
			t.Fatalf("batch not hash-ordered within round %d at index %d", cur, i)
		}
	}
}

// TestCausalBatchDiamondSingleInclusionAndOrder verifies the causal walk over a
// diamond DAG: every ancestor is included exactly once (a vertex reached through
// two branches is not duplicated), a round-8 vertex reached through a single
// branch is still included, and the batch is ordered by (round, hash) with the
// round-8 vertices before the round-9 vertices before the round-10 anchor.
func TestCausalBatchDiamondSingleInclusionAndOrder(t *testing.T) {
	validators, _ := newTestValidatorSet(2)
	v0, v1 := validators[0], validators[1]
	s := newStore(newTestStorage(t))

	// Two round-8 leaves from distinct producers (so their hashes differ). g8 is
	// reached through one branch only (b9); h8 is the diamond apex reached through
	// both round-9 branches (b9 and c9).
	g8 := addRoundVertex(t, s, v0, 8, nil)
	h8 := addRoundVertex(t, s, v1, 8, nil)
	b9 := addRoundVertex(t, s, v0, 9, []Hash{g8, h8})
	c9 := addRoundVertex(t, s, v1, 9, []Hash{h8})
	d10 := addRoundVertex(t, s, v0, 10, []Hash{b9, c9})

	batch, ok := s.causalBatch(d10)
	if !ok {
		t.Fatal("expected ok: the anchor's causal history is fully present")
	}

	// Two round-8 leaves + two round-9 vertices + the anchor, each exactly once.
	if len(batch) != 5 {
		t.Fatalf("expected 5 vertices, got %d: %x", len(batch), batch)
	}

	counts := make(map[Hash]int)
	for _, h := range batch {
		counts[h]++
	}
	for _, h := range []Hash{g8, h8, b9, c9, d10} {
		if counts[h] != 1 {
			t.Fatalf("vertex %x included %d times, want exactly 1", h[:4], counts[h])
		}
	}

	assertBatchOrder(t, s, batch)

	if roundOf(t, s, batch[0]) != 8 || roundOf(t, s, batch[1]) != 8 {
		t.Fatal("round-8 vertices must sort first")
	}
	if batch[len(batch)-1] != d10 {
		t.Fatal("the round-10 anchor must sort last")
	}
}

// TestCausalBatchMissingAncestor verifies the walk reports ok=false when a
// referenced ancestor is absent from the local store, so the caller waits for it
// rather than committing a partial history.
func TestCausalBatchMissingAncestor(t *testing.T) {
	v := newTestValidator()
	s := newStore(newTestStorage(t))

	// The anchor references a parent that was never stored.
	anchor := addRoundVertex(t, s, v, 10, []Hash{{0xAB}})

	if _, ok := s.causalBatch(anchor); ok {
		t.Fatal("expected ok=false when an ancestor is missing")
	}
}

// TestCausalBatchExcludesCommitted verifies that already-committed vertices are
// excluded from the batch and bound the walk: the walk never traverses through a
// committed vertex, so a missing grandparent behind it does not trigger ok=false.
func TestCausalBatchExcludesCommitted(t *testing.T) {
	v := newTestValidator()
	s := newStore(newTestStorage(t))

	// a8 references a parent that is absent; if the walk crossed a8 it would fail.
	a8 := addRoundVertex(t, s, v, 8, []Hash{{0xEE}})
	b9 := addRoundVertex(t, s, v, 9, []Hash{a8})
	c10 := addRoundVertex(t, s, v, 10, []Hash{b9})

	s.markVertexCommitted(a8)

	batch, ok := s.causalBatch(c10)
	if !ok {
		t.Fatal("expected ok: a committed vertex must bound the walk")
	}

	if len(batch) != 2 {
		t.Fatalf("expected 2 vertices (anchor + its parent), got %d", len(batch))
	}
	for _, h := range batch {
		if h == a8 {
			t.Fatal("committed vertex a8 must be excluded from the batch")
		}
	}
}

// TestCausalBatchDeterministicAcrossFeedOrders verifies that two stores fed the
// same vertices in opposite orders return byte-identical batches: the walk's
// output depends only on the DAG content, never on arrival order.
func TestCausalBatchDeterministicAcrossFeedOrders(t *testing.T) {
	validators, _ := newTestValidatorSet(2)
	v0, v1 := validators[0], validators[1]

	// Build one diamond's raw vertices once, then feed two stores in opposite
	// orders. Each item carries the round used for storage.
	type item struct {
		data  []byte
		hash  Hash
		round uint64
	}

	g8d, g8 := buildRoundVertex(t, v0, 8, nil)
	h8d, h8 := buildRoundVertex(t, v1, 8, nil)
	b9d, b9 := buildRoundVertex(t, v0, 9, []Hash{g8, h8})
	c9d, c9 := buildRoundVertex(t, v1, 9, []Hash{h8})
	d10d, d10 := buildRoundVertex(t, v0, 10, []Hash{b9, c9})

	items := []item{{g8d, g8, 8}, {h8d, h8, 8}, {b9d, b9, 9}, {c9d, c9, 9}, {d10d, d10, 10}}
	producers := map[Hash]Hash{g8: v0.pubKey, h8: v1.pubKey, b9: v0.pubKey, c9: v1.pubKey, d10: v0.pubKey}

	storeA := newStore(newTestStorage(t))
	for i := 0; i < len(items); i++ {
		storeA.add(items[i].data, items[i].hash, items[i].round, producers[items[i].hash])
	}

	storeB := newStore(newTestStorage(t))
	for i := len(items) - 1; i >= 0; i-- {
		storeB.add(items[i].data, items[i].hash, items[i].round, producers[items[i].hash])
	}

	batchA, okA := storeA.causalBatch(d10)
	batchB, okB := storeB.causalBatch(d10)
	if !okA || !okB {
		t.Fatalf("expected both walks to succeed (okA=%v okB=%v)", okA, okB)
	}

	if len(batchA) != len(batchB) {
		t.Fatalf("batch lengths differ: A=%d B=%d", len(batchA), len(batchB))
	}
	for i := range batchA {
		if batchA[i] != batchB[i] {
			t.Fatalf("batches diverge at %d: A=%x B=%x", i, batchA[i][:4], batchB[i][:4])
		}
	}
}

// TestCausalBatchImportExcludesByCommittedFlag pins the C-1 semantics: after an
// import, exclusion from a later batch is by the per-vertex committed FLAG, never by
// a round threshold. A committed vertex is excluded; an uncommitted same-round
// sibling of a decided anchor is NOT dropped — it rides the next batch, exactly as it
// would on the source (the old round-floor wrongly dropped it, losing its effects).
func TestCausalBatchImportExcludesByCommittedFlag(t *testing.T) {
	validators, _ := newTestValidatorSet(2)
	v0, v1 := validators[0], validators[1]

	// Snapshot chain rounds 1..5, all folded into a commit batch on the source, so
	// every entry carries the committed flag.
	r1d, r1 := buildRoundVertex(t, v0, 1, nil)
	r2d, r2 := buildRoundVertex(t, v0, 2, []Hash{r1})
	r3d, r3 := buildRoundVertex(t, v0, 3, []Hash{r2})
	r4d, r4 := buildRoundVertex(t, v0, 4, []Hash{r3})
	r5d, r5 := buildRoundVertex(t, v0, 5, []Hash{r4})

	entries := []VertexEntry{
		{Round: 1, Data: r1d, Committed: true}, {Round: 2, Data: r2d, Committed: true},
		{Round: 3, Data: r3d, Committed: true}, {Round: 4, Data: r4d, Committed: true},
		{Round: 5, Data: r5d, Committed: true},
	}

	s := newStore(newTestStorage(t))
	s.ImportVertices(entries)

	if !s.isVertexCommitted(r5) {
		t.Fatal("an imported vertex carrying the committed flag must be marked committed")
	}

	// A round-5 sibling of the imported chain that the source had NOT yet committed
	// (a late arrival at the cut). It rode no batch on the source and must ride one here.
	lateData, late := buildRoundVertex(t, v1, 5, []Hash{r4})
	s.add(lateData, late, 5, v1.pubKey)

	// Anchor at round 6 references both the committed round-5 vertex and the sibling.
	anchor := addRoundVertex(t, s, v0, 6, []Hash{r5, late})

	batch, ok := s.causalBatch(anchor)
	if !ok {
		t.Fatal("expected ok: every referenced round-5 ancestor is present locally")
	}

	// The committed r5 is excluded; the uncommitted sibling and the anchor are kept.
	if len(batch) != 2 {
		t.Fatalf("expected the uncommitted sibling and the anchor, got %x", batch)
	}

	got := map[Hash]bool{}
	for _, h := range batch {
		got[h] = true
	}
	if got[r5] {
		t.Fatal("committed round-5 vertex must be excluded by its flag")
	}
	if !got[late] {
		t.Fatal("uncommitted round-5 sibling must NOT be dropped: it rides this batch")
	}
	if !got[anchor] {
		t.Fatal("the round-6 anchor must be in its own batch")
	}
}

// TestCausalBatchCommittedFlagSurvivesRestart verifies the committed flag is
// persisted: after reopening the store, a previously committed vertex is still
// committed and remains excluded from the batch, so a restart never re-applies it.
func TestCausalBatchCommittedFlagSurvivesRestart(t *testing.T) {
	dir, err := os.MkdirTemp("", "consensus_restart_*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(dir)

	v := newTestValidator()

	a8d, a8 := buildRoundVertex(t, v, 8, []Hash{{0xEE}})
	b9d, b9 := buildRoundVertex(t, v, 9, []Hash{a8})
	c10d, c10 := buildRoundVertex(t, v, 10, []Hash{b9})

	db1, err := storage.New(dir)
	if err != nil {
		t.Fatalf("failed to open storage: %v", err)
	}
	s1 := newStore(db1)
	s1.add(a8d, a8, 8, v.pubKey)
	s1.add(b9d, b9, 9, v.pubKey)
	s1.add(c10d, c10, 10, v.pubKey)
	s1.markVertexCommitted(a8)
	db1.Close()

	db2, err := storage.New(dir)
	if err != nil {
		t.Fatalf("failed to reopen storage: %v", err)
	}
	defer db2.Close()
	s2 := newStore(db2)

	if !s2.isVertexCommitted(a8) {
		t.Fatal("committed flag did not survive restart")
	}

	batch, ok := s2.causalBatch(c10)
	if !ok {
		t.Fatal("expected ok after restart: the committed vertex still bounds the walk")
	}
	for _, h := range batch {
		if h == a8 {
			t.Fatal("committed vertex must stay excluded after restart")
		}
	}
}

// TestCausalBatchEdgeCases covers a lone anchor with no ancestry, an
// already-committed anchor (empty batch), and a missing anchor (ok=false).
func TestCausalBatchEdgeCases(t *testing.T) {
	v := newTestValidator()
	s := newStore(newTestStorage(t))

	solo := addRoundVertex(t, s, v, 4, nil)
	batch, ok := s.causalBatch(solo)
	if !ok || len(batch) != 1 || batch[0] != solo {
		t.Fatalf("lone anchor: expected [solo], got ok=%v batch=%x", ok, batch)
	}

	s.markVertexCommitted(solo)
	batch, ok = s.causalBatch(solo)
	if !ok || len(batch) != 0 {
		t.Fatalf("committed anchor: expected empty batch, got ok=%v batch=%x", ok, batch)
	}

	if _, ok := s.causalBatch(Hash{0xDE, 0xAD}); ok {
		t.Fatal("missing anchor must return ok=false")
	}
}
