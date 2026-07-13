package consensus

import (
	"testing"
)

// setEqualStake gives every validator the same self-stake, so a 2/3 capped-stake
// quorum is a clean three-of-four majority under the default voting cap.
func setEqualStake(dag *DAG, vals []testValidator, stake uint64) {
	for _, v := range vals {
		dag.validators.SetSelfStake(v.pubKey, stake)
	}

	// Freeze the epoch-0 genesis committee now that stakes are final, so the anchor
	// path resolves epoch 0 from the frozen snapshot rather than the (removed) live-set
	// fallback.
	freezeGenesis(dag)
}

// addDagVertex builds a signed vertex for v at round with the given parents and
// inserts it directly into the DAG store, returning its hash.
func addDagVertex(t *testing.T, dag *DAG, v testValidator, round uint64, parents []Hash) Hash {
	t.Helper()

	data, hash := buildRoundVertex(t, v, round, parents)
	dag.store.add(data, hash, round, v.pubKey)

	return hash
}

// designatedProducer returns the validator designated as the anchor producer for
// the round, failing if the designation does not resolve to a known test validator.
func designatedProducer(t *testing.T, dag *DAG, vals []testValidator, round uint64) testValidator {
	t.Helper()

	pk, ok := dag.anchorProducerFor(round)
	if !ok {
		t.Fatalf("round %d: no anchor producer designated", round)
	}

	for _, v := range vals {
		if v.pubKey == pk {
			return v
		}
	}

	t.Fatalf("round %d: designated producer not among test validators", round)
	return testValidator{}
}

// others returns the validators other than the given one, preserving order.
func others(vals []testValidator, except testValidator) []testValidator {
	var out []testValidator
	for _, v := range vals {
		if v.pubKey != except.pubKey {
			out = append(out, v)
		}
	}

	return out
}

// newDecisionDAG builds a four-validator DAG with equal stake, the standard fixture
// for the anchor decision tests (quorum = three of four).
func newDecisionDAG(t *testing.T) ([]testValidator, *DAG) {
	t.Helper()

	vals, vs := newTestValidatorSet(4)
	dag := New(newTestStorage(t), vs, nil, testSystemPod, 0, vals[0].privKey, nil)
	t.Cleanup(func() { dag.Close() })
	setEqualStake(dag, vals, 25)

	return vals, dag
}

// TestAnchorStatusDirectCertify verifies that a designated producer's single
// vertex, cited by a round-N+1 stake quorum among its direct parents, is certified
// and committed as the round's anchor.
func TestAnchorStatusDirectCertify(t *testing.T) {
	vals, dag := newDecisionDAG(t)

	const round = 3
	producer := designatedProducer(t, dag, vals, round)
	v := addDagVertex(t, dag, producer, round, nil)

	// Three of four validators cite v at round+1 → certification quorum.
	for _, c := range others(vals, producer)[:3] {
		addDagVertex(t, dag, c, round+1, []Hash{v})
	}

	dec := dag.anchorStatus(round)
	if dec.kind != anchorCommit || dec.anchor != v {
		t.Fatalf("expected commit(%x), got kind=%d anchor=%x", v[:4], dec.kind, dec.anchor[:4])
	}
}

// TestAnchorStatusDirectBlame verifies that a round-N+1 stake quorum citing no
// vertex of the designated producer blames the round, which is skipped, even
// though the producer did produce a vertex.
func TestAnchorStatusDirectBlame(t *testing.T) {
	vals, dag := newDecisionDAG(t)

	const round = 3
	producer := designatedProducer(t, dag, vals, round)
	addDagVertex(t, dag, producer, round, nil) // present but ignored by the quorum

	// Three of four validators cite nothing of the producer at round+1 → blame quorum.
	for _, c := range others(vals, producer)[:3] {
		addDagVertex(t, dag, c, round+1, nil)
	}

	if dec := dag.anchorStatus(round); dec.kind != anchorSkip {
		t.Fatalf("expected skip, got kind=%d", dec.kind)
	}
}

// TestAnchorStatusLoneSupporterDoesNotCertify verifies that a single small-stake
// supporter cannot certify, and that certification reads round-N+1 DIRECT parents
// only: a round-N+2 vertex transitively reaching the anchor must not count as
// support. With no later direct verdict the round is WAIT.
func TestAnchorStatusLoneSupporterDoesNotCertify(t *testing.T) {
	vals, dag := newDecisionDAG(t)

	const round = 3
	producer := designatedProducer(t, dag, vals, round)
	v := addDagVertex(t, dag, producer, round, nil)

	other := others(vals, producer)
	// Only ONE round-N+1 vertex cites v (25% stake, below the 2/3 quorum).
	supporter := addDagVertex(t, dag, other[0], round+1, []Hash{v})

	// A round-N+2 vertex transitively reaches v through the lone supporter. The rule
	// must read round-N+1 direct parents only, so this must not manufacture support.
	addDagVertex(t, dag, other[1], round+2, []Hash{supporter})

	verdict, _ := dag.directAnchorVerdict(round)
	if verdict == verdictCertified {
		t.Fatal("a lone 25% supporter must not certify; round+2 reachability must not count")
	}
	if verdict == verdictBlamed {
		t.Fatal("a present supporter means the round is not blamed")
	}

	if dec := dag.anchorStatus(round); dec.kind != anchorWait {
		t.Fatalf("expected wait with no later verdict, got kind=%d", dec.kind)
	}
}

// TestAnchorStatusC1ForkRegression is the anti-fork regression from the reverted
// first attempt: the designated producer equivocates E1/E2 and the round-N+1
// quorum cites E2. A node holding only E2 and a node holding E1+E2 must BOTH
// certify E2 — the quorum selects the anchor, never a local view. More evidence
// (adding E1 to the E2-only node) must never flip the formed verdict.
func TestAnchorStatusC1ForkRegression(t *testing.T) {
	vals := make([]testValidator, 4)
	pubkeys := make([]Hash, 4)
	for i := range vals {
		vals[i] = newTestValidator()
		pubkeys[i] = vals[i].pubKey
	}

	build := func() *DAG {
		dag := New(newTestStorage(t), NewValidatorSet(pubkeys), nil, testSystemPod, 0, vals[0].privKey, nil)
		t.Cleanup(func() { dag.Close() })
		setEqualStake(dag, vals, 25)
		return dag
	}

	const round = 3

	// Both instances share membership, so both designate the same producer.
	ref := build()
	producer := designatedProducer(t, ref, vals, round)

	// The producer equivocates: distinct parents give E1 and E2 distinct hashes.
	e1Data, e1 := buildRoundVertex(t, producer, round, []Hash{{0x01}})
	e2Data, e2 := buildRoundVertex(t, producer, round, []Hash{{0x02}})

	other := others(vals, producer) // three validators form the E2 quorum

	// Build the E2-citing quorum once so both instances ingest identical bytes.
	type built struct {
		data     []byte
		hash     Hash
		producer Hash
	}
	var quorum []built
	for _, c := range other[:3] {
		d, h := buildRoundVertex(t, c, round+1, []Hash{e2})
		quorum = append(quorum, built{d, h, c.pubKey})
	}
	// One round-N+1 vertex from the producer citing its own E1 (only the E1+E2 node
	// holds it); a lone 25% supporter of E1 that must not defeat the E2 quorum.
	e1CiteData, e1Cite := buildRoundVertex(t, producer, round+1, []Hash{e1})

	// Node X holds ONLY E2 plus the E2-citing quorum.
	dagX := build()
	dagX.store.add(e2Data, e2, round, producer.pubKey)
	for _, q := range quorum {
		dagX.store.add(q.data, q.hash, round+1, q.producer)
	}

	// Node Y holds E1 + E2, the E2 quorum, and the E1-citing vertex.
	dagY := build()
	dagY.store.add(e1Data, e1, round, producer.pubKey)
	dagY.store.add(e2Data, e2, round, producer.pubKey)
	for _, q := range quorum {
		dagY.store.add(q.data, q.hash, round+1, q.producer)
	}
	dagY.store.add(e1CiteData, e1Cite, round+1, producer.pubKey)

	decX := dagX.anchorStatus(round)
	decY := dagY.anchorStatus(round)

	if decX.kind != anchorCommit || decX.anchor != e2 {
		t.Fatalf("E2-only node must commit E2, got kind=%d anchor=%x", decX.kind, decX.anchor[:4])
	}
	if decY.kind != anchorCommit || decY.anchor != e2 {
		t.Fatalf("E1+E2 node must commit E2, got kind=%d anchor=%x", decY.kind, decY.anchor[:4])
	}

	// Durability: give the E2-only node the E1 evidence; the verdict must not flip.
	dagX.store.add(e1Data, e1, round, producer.pubKey)
	dagX.store.add(e1CiteData, e1Cite, round+1, producer.pubKey)
	if dec := dagX.anchorStatus(round); dec.kind != anchorCommit || dec.anchor != e2 {
		t.Fatalf("more evidence flipped a formed verdict: kind=%d anchor=%x", dec.kind, dec.anchor[:4])
	}
}

// TestAnchorStatusAbstainNeverBlames verifies that a round-N+1 vertex citing a
// different vertex of the designated producer abstains, never blames: with the
// producer equivocating and round-N+1 split two/two across E1 and E2, neither
// certifies and no blame quorum forms.
func TestAnchorStatusAbstainNeverBlames(t *testing.T) {
	vals, dag := newDecisionDAG(t)

	const round = 3
	producer := designatedProducer(t, dag, vals, round)
	e1 := addDagVertex(t, dag, producer, round, []Hash{{0x01}})
	e2 := addDagVertex(t, dag, producer, round, []Hash{{0x02}})

	// All four validators produce round-N+1 vertices: two cite E1, two cite E2.
	// Every one cites some vertex of the producer, so none is a blamer.
	cite := []Hash{e1, e1, e2, e2}
	for i, c := range vals {
		addDagVertex(t, dag, c, round+1, []Hash{cite[i]})
	}

	verdict, _ := dag.directAnchorVerdict(round)
	if verdict == verdictBlamed {
		t.Fatal("citing a different vertex of the producer must abstain, never blame")
	}
	if verdict == verdictCertified {
		t.Fatal("a two/two split must not certify")
	}
}

// TestAnchorStatusSubQuorumBlameUndecided verifies that blame requires a full
// stake quorum: with two of four citing nothing and two citing the producer's
// vertex, neither quorum forms, so the direct verdict is undecided.
func TestAnchorStatusSubQuorumBlameUndecided(t *testing.T) {
	vals, dag := newDecisionDAG(t)

	const round = 3
	producer := designatedProducer(t, dag, vals, round)
	v := addDagVertex(t, dag, producer, round, nil)

	other := others(vals, producer)
	addDagVertex(t, dag, other[0], round+1, []Hash{v}) // supporter
	addDagVertex(t, dag, other[1], round+1, []Hash{v}) // supporter
	addDagVertex(t, dag, other[2], round+1, nil)       // blamer
	addDagVertex(t, dag, producer, round+1, nil)       // blamer

	verdict, _ := dag.directAnchorVerdict(round)
	if verdict != verdictUndecided {
		t.Fatalf("expected undecided with sub-quorum blame and support, got verdict=%d", verdict)
	}
}

// buildSplitRound sets up an undecided round N whose designated producer has a
// single vertex V that two of four round-N+1 producers cite and two ignore, then
// designates the round-N+1 anchor. anchorCites controls whether that anchor cites
// V (so V enters its causal history) or nothing. It returns V and the anchor hash.
func buildSplitRound(t *testing.T, dag *DAG, vals []testValidator, round uint64, anchorCites bool) (v, anchor Hash) {
	t.Helper()

	producer := designatedProducer(t, dag, vals, round)
	v = addDagVertex(t, dag, producer, round, nil)

	anchorer := designatedProducer(t, dag, vals, round+1)
	rest := others(vals, anchorer) // three non-anchor validators

	// The anchor supports V (cites it) or blames (cites nothing). Either way the
	// round is a two/two split — undecided, never a direct verdict — so the outcome
	// is forced through the indirect rule. The rest fill in to keep supporters and
	// blamers at two each.
	anchorParents := []Hash(nil)
	restSupporters := 2 // anchor blames → need two rest supporters
	if anchorCites {
		anchorParents = []Hash{v}
		restSupporters = 1 // anchor supports → need one rest supporter
	}
	anchor = addDagVertex(t, dag, anchorer, round+1, anchorParents)

	for i, c := range rest {
		if i < restSupporters {
			addDagVertex(t, dag, c, round+1, []Hash{v})
		} else {
			addDagVertex(t, dag, c, round+1, nil)
		}
	}

	return v, anchor
}

// certifyAnchor makes the round-N+1 designated anchor certified by having three of
// four round-N+2 producers cite it.
func certifyAnchor(t *testing.T, dag *DAG, vals []testValidator, round uint64, anchor Hash) {
	t.Helper()

	anchorer := designatedProducer(t, dag, vals, round+1)
	for _, c := range others(vals, anchorer)[:3] {
		addDagVertex(t, dag, c, round+2, []Hash{anchor})
	}
}

// TestAnchorStatusIndirectCommit verifies the indirect rule: an undecided round is
// committed when its designated producer's vertex sits in the causal history of
// the first later certified anchor.
func TestAnchorStatusIndirectCommit(t *testing.T) {
	vals, dag := newDecisionDAG(t)

	const round = 3
	v, anchor := buildSplitRound(t, dag, vals, round, true) // anchor cites V
	certifyAnchor(t, dag, vals, round, anchor)

	dec := dag.anchorStatus(round)
	if dec.kind != anchorCommit || dec.anchor != v {
		t.Fatalf("expected indirect commit(%x), got kind=%d anchor=%x", v[:4], dec.kind, dec.anchor[:4])
	}
}

// TestAnchorStatusIndirectSkip verifies the indirect rule's other branch: an
// undecided round is skipped when no vertex of its designated producer sits in the
// first later certified anchor's causal history.
func TestAnchorStatusIndirectSkip(t *testing.T) {
	vals, dag := newDecisionDAG(t)

	const round = 3
	_, anchor := buildSplitRound(t, dag, vals, round, false) // anchor cites nothing
	certifyAnchor(t, dag, vals, round, anchor)

	if dec := dag.anchorStatus(round); dec.kind != anchorSkip {
		t.Fatalf("expected indirect skip, got kind=%d", dec.kind)
	}
}

// TestAnchorStatusWaitsUntilLaterAnchorDecided verifies that an undecided round
// returns WAIT while no later anchor has a direct verdict, and flips to a decision
// once a later anchor is certified.
func TestAnchorStatusWaitsUntilLaterAnchorDecided(t *testing.T) {
	vals, dag := newDecisionDAG(t)

	const round = 3
	v, anchor := buildSplitRound(t, dag, vals, round, true)

	if dec := dag.anchorStatus(round); dec.kind != anchorWait {
		t.Fatalf("expected wait before any later anchor is decided, got kind=%d", dec.kind)
	}

	certifyAnchor(t, dag, vals, round, anchor)

	dec := dag.anchorStatus(round)
	if dec.kind != anchorCommit || dec.anchor != v {
		t.Fatalf("expected commit(%x) once the later anchor is certified, got kind=%d", v[:4], dec.kind)
	}
}

// TestAnchorStatusWaitsPastLaterUndecided is the anti-fork safety case: an
// undecided round must NOT be resolved by skipping past a later still-undecided
// round to reach a further certified anchor. That later round could yet certify
// and become the true resolving anchor, so a node with fuller evidence could
// decide differently. The rule must return wait, even though a certified anchor
// exists two rounds ahead.
func TestAnchorStatusWaitsPastLaterUndecided(t *testing.T) {
	vals, dag := newDecisionDAG(t)

	const round = 3
	v, a1 := buildSplitRound(t, dag, vals, round, true) // round N undecided; anchor a1

	// Round N+1 is itself undecided: its round-N+2 citations split two/two, so
	// neither a certify nor a blame quorum forms for a1.
	anchorer2 := designatedProducer(t, dag, vals, round+2)
	rest2 := others(vals, anchorer2)
	a2 := addDagVertex(t, dag, anchorer2, round+2, []Hash{a1}) // supports N+1 (and carries a1→V)
	addDagVertex(t, dag, rest2[0], round+2, []Hash{a1})        // supports N+1
	addDagVertex(t, dag, rest2[1], round+2, nil)               // blames N+1
	addDagVertex(t, dag, rest2[2], round+2, nil)               // blames N+1

	// A certified anchor two rounds ahead that the buggy "skip past undecided" rule
	// would wrongly use to resolve round N.
	for _, c := range others(vals, anchorer2)[:3] {
		addDagVertex(t, dag, c, round+3, []Hash{a2})
	}

	_ = v
	if dec := dag.anchorStatus(round); dec.kind != anchorWait {
		t.Fatalf("must wait past a later undecided round, got kind=%d", dec.kind)
	}
}

// TestAnchorStatusSkipsPastLaterBlame verifies the complementary liveness case: an
// undecided round scans PAST a later directly-blamed (decided-skip) round to reach
// the first later certified anchor, and commits when its producer sits in that
// anchor's causal history.
func TestAnchorStatusSkipsPastLaterBlame(t *testing.T) {
	vals, dag := newDecisionDAG(t)

	const round = 3
	v, _ := buildSplitRound(t, dag, vals, round, true) // round N undecided

	// Round N+1 is directly blamed: a round-N+2 quorum cites no vertex of its anchor.
	// The round-N+2 anchor cites V, so V enters its causal history, and by citing a
	// round-N vertex rather than the round-N+1 anchor it is itself a blamer of N+1.
	anchorer2 := designatedProducer(t, dag, vals, round+2)
	rest2 := others(vals, anchorer2)
	a2 := addDagVertex(t, dag, anchorer2, round+2, []Hash{v}) // cites V, blames N+1
	addDagVertex(t, dag, rest2[0], round+2, nil)              // blames N+1
	addDagVertex(t, dag, rest2[1], round+2, nil)              // blames N+1

	for _, c := range others(vals, anchorer2)[:3] {
		addDagVertex(t, dag, c, round+3, []Hash{a2}) // certify the round-N+2 anchor
	}

	dec := dag.anchorStatus(round)
	if dec.kind != anchorCommit || dec.anchor != v {
		t.Fatalf("must scan past a later blamed round to a certified anchor and commit(%x), got kind=%d anchor=%x", v[:4], dec.kind, dec.anchor[:4])
	}
}

// TestAnchorStatusEpochTailResolves verifies I2: a split at the last round of an
// epoch resolves via a next-epoch anchor. The forward scan crosses the boundary
// only because the one-epoch-ahead holder snapshot is frozen; without it the tail
// wedges on WAIT.
func TestAnchorStatusEpochTailResolves(t *testing.T) {
	vals, vs := newTestValidatorSet(4)
	dag := New(newTestStorage(t), vs, nil, testSystemPod, 0, vals[0].privKey, nil, WithEpochLength(4))
	defer dag.Close()
	setEqualStake(dag, vals, 25)

	// Simulate the state after the first epoch transition: epoch-1 holders frozen and
	// a one-epoch-ahead proxy frozen, so round-9 producers (epoch 2) can be weighed.
	dag.currentEpoch = 1
	dag.epochHolders = snapshotOf(dag.validators)
	dag.nextEpochHolders = snapshotOf(dag.validators)

	const round = 8 // last round of epoch 1 (2*epochLength); round+1 = 9 is epoch 2
	v, anchor := buildSplitRound(t, dag, vals, round, true)
	certifyAnchor(t, dag, vals, round, anchor)

	// Without the frozen next-epoch snapshot the round-9 stake set does not resolve,
	// so the tail can only WAIT.
	saved := dag.nextEpochHolders
	dag.nextEpochHolders = nil
	if dec := dag.anchorStatus(round); dec.kind != anchorWait {
		t.Fatalf("expected wait when the next-epoch snapshot is absent, got kind=%d", dec.kind)
	}

	// With it frozen, the split resolves across the boundary via the next-epoch anchor.
	dag.nextEpochHolders = saved
	dec := dag.anchorStatus(round)
	if dec.kind != anchorCommit || dec.anchor != v {
		t.Fatalf("expected epoch-tail commit(%x) via a next-epoch anchor, got kind=%d anchor=%x", v[:4], dec.kind, dec.anchor[:4])
	}
}
