package consensus

import "testing"

// The relaxed bootstrap regime resolves an undecided round through the first later
// certified anchor. Two view-dependent skips in that resolution forked the committed
// log under load, when a straggler delivered its backlog with rounds of skew: a node
// that received the backlog committed a round while a node that decided first skipped
// it. The tests below reproduce both forks at the anchor-decision seam — two relaxed
// observers that differ only by the straggler's backlog — and require that the node
// lacking it WAIT rather than reach the opposite terminal verdict (issue #8).

// relaxedDistinctRounds returns a four-validator committee whose rounds 3 and 4
// designate DISTINCT producers, so the queried round's producer and the resolving
// anchor's producer are different nodes. Keys are random, so it retries until the
// deterministic rotation yields two distinct designees.
func relaxedDistinctRounds(t *testing.T) []testValidator {
	t.Helper()

	for {
		vals, vs := newTestValidatorSet(4)
		dag := New(newTestStorage(t), vs, nil, testSystemPod, 0, vals[0].privKey, nil, WithMinValidators(4))
		setEqualStake(dag, vals, 25) // freeze the genesis committee so designation resolves
		p3 := designatedProducer(t, dag, vals, 3)
		p4 := designatedProducer(t, dag, vals, 4)
		dag.Close()

		if p3.pubKey != p4.pubKey {
			return vals
		}
	}
}

// newRelaxedPair builds two relaxed-regime observers of the same committee, each with
// the genesis committee frozen at equal stake and the strict latch unfired.
func newRelaxedPair(t *testing.T, vals []testValidator) (a, b *DAG) {
	t.Helper()

	build := func() *DAG {
		_, vs := reuseSet(vals)
		dag := New(newTestStorage(t), vs, nil, testSystemPod, 0, vals[0].privKey, nil, WithMinValidators(len(vals)))
		t.Cleanup(func() { dag.Close() })
		setEqualStake(dag, vals, 25)
		return dag
	}

	return build(), build()
}

// placer returns a helper that builds a vertex once and inserts it into each of the
// given DAGs, so a shared vertex is byte-identical across observers while a
// backlog-only vertex reaches just the observer that received it.
func placer(t *testing.T) func(dags []*DAG, v testValidator, round uint64, parents []Hash) Hash {
	t.Helper()

	return func(dags []*DAG, v testValidator, round uint64, parents []Hash) Hash {
		data, hash := buildRoundVertex(t, v, round, parents)
		for _, d := range dags {
			d.store.add(data, hash, round, v.pubKey)
		}
		return hash
	}
}

// memberExcept returns the first validator that is neither of the two excluded ones.
func memberExcept(vals []testValidator, a, b testValidator) testValidator {
	for _, v := range vals {
		if v.pubKey != a.pubKey && v.pubKey != b.pubKey {
			return v
		}
	}

	panic("no validator outside the two excluded")
}

// buildRelaxedAbsentCandidateFork builds two relaxed observers that differ only by the
// straggler's backlog: round 3's designated producer P's vertex vR and a round-4
// member citing it. The backlog reaches `received` — where the single supporter
// certifies round 3 and commits vR — but not `waiting`, which decides round 3 without
// P's vertex at all and resolves it through the round-4 certified anchor. The store
// stops far below the silence horizon, so P is not yet provably span-silent.
func buildRelaxedAbsentCandidateFork(t *testing.T, vals []testValidator) (received, waiting *DAG, vR Hash) {
	t.Helper()

	received, waiting = newRelaxedPair(t, vals)

	p3 := designatedProducer(t, received, vals, 3) // P: queried round's producer
	p4 := designatedProducer(t, received, vals, 4) // Q: resolving anchor's producer

	place := placer(t)
	both := []*DAG{received, waiting}
	backlog := []*DAG{received} // the straggler's backlog reaches `received` only

	// Round 3: P's vertex — backlog, delivered to `received` only.
	vR = place(backlog, p3, 3, nil)

	// A round-4 member cites vR (also backlog), so on `received` a single member
	// supporter certifies round 3 directly. It is neither the round-4 designated
	// producer nor P.
	citer := memberExcept(vals, p3, p4)
	place(backlog, citer, 4, []Hash{vR})

	// Round 4's designated vertex is the resolving anchor, on both, omitting vR.
	aQ := place(both, p4, 4, nil)

	// Round 5 certifies the round-4 anchor on both, so `waiting` reaches it as the
	// first later certified anchor while resolving round 3.
	for _, c := range others(vals, p4) {
		place(both, c, 5, []Hash{aQ})
	}

	return received, waiting, vR
}

// buildRelaxedOmittedCandidateFork builds two relaxed observers that both hold round
// 3's designated vertex vR but differ by the round-4 citer of it. The resolving
// round-4 anchor omits vR, so resolving through it SKIPS. On `received` the backlog's
// citer certifies round 3 and commits vR; on `waiting` that citer is absent, so round
// 3 is undecided and could still certify if the citer's vertex arrives — a single
// member supporter is all the relaxed regime needs.
func buildRelaxedOmittedCandidateFork(t *testing.T, vals []testValidator) (received, waiting *DAG, vR Hash) {
	t.Helper()

	received, waiting = newRelaxedPair(t, vals)

	p3 := designatedProducer(t, received, vals, 3) // P
	p4 := designatedProducer(t, received, vals, 4) // Q: resolving anchor, omits vR

	place := placer(t)
	both := []*DAG{received, waiting}
	backlog := []*DAG{received}

	// Round 3: P's vertex, on BOTH — the backlog carries a citer of it, not the vertex.
	vR = place(both, p3, 3, nil)

	// Round 4's designated anchor omits vR (a round-3 blamer): the resolving anchor.
	aQ := place(both, p4, 4, nil)

	// A member h cites vR at round 4 — backlog, `received` only. On `received` h
	// certifies round 3 directly; on `waiting` h is absent and the anchor omits vR.
	h := memberExcept(vals, p3, p4)
	place(backlog, h, 4, []Hash{vR})

	// Round 5 certifies the round-4 anchor on both.
	for _, c := range others(vals, p4) {
		place(both, c, 5, []Hash{aQ})
	}

	return received, waiting, vR
}

// TestRelaxedResolveWaitsForAbsentCandidate is the R14 resolver regression, case (a):
// resolving an undecided relaxed round through a later certified anchor must WAIT for
// the designated producer's vertex to arrive rather than SKIP the instant it is
// absent locally. A node that received the straggler's backlog certifies and commits
// the round; a node that decided first, lacking that vertex, must not skip it. The fix
// skips only once the producer is provably span-silent, so the two never fork.
func TestRelaxedResolveWaitsForAbsentCandidate(t *testing.T) {
	vals := relaxedDistinctRounds(t)
	received, waiting, vR := buildRelaxedAbsentCandidateFork(t, vals)

	const round = 3

	decReceived := received.anchorStatus(round)
	decWaiting := waiting.anchorStatus(round)

	if decReceived.kind != anchorCommit || decReceived.anchor != vR {
		t.Fatalf("received node must commit the producer's vertex, got %s", describeDecision(decReceived, vR))
	}

	forked := isTerminal(decReceived) && isTerminal(decWaiting) && !sameVerdict(decReceived, decWaiting)
	if forked {
		t.Fatalf("SAFETY FORK on round %d from a delayed candidate: received=%s waiting=%s (vR=%x)",
			round, describeDecision(decReceived, vR), describeDecision(decWaiting, vR), vR[:4])
	}

	if decWaiting.kind != anchorWait {
		t.Fatalf("waiting node must WAIT for the absent candidate, got %s", describeDecision(decWaiting, vR))
	}
}

// TestRelaxedResolveWaitsWhileSupporterPossible is the R14 resolver regression, case
// (b): when the resolving anchor omits the candidate, the relaxed round must not be
// skipped while a single member supporter could still certify it directly — a holder
// that is neither a stored blamer nor span-silent. A node that received the backlog's
// citer commits the round; a node without it must WAIT rather than skip, or the two
// fork.
func TestRelaxedResolveWaitsWhileSupporterPossible(t *testing.T) {
	vals := relaxedDistinctRounds(t)
	received, waiting, vR := buildRelaxedOmittedCandidateFork(t, vals)

	const round = 3

	decReceived := received.anchorStatus(round)
	decWaiting := waiting.anchorStatus(round)

	if decReceived.kind != anchorCommit || decReceived.anchor != vR {
		t.Fatalf("received node must commit the producer's vertex, got %s", describeDecision(decReceived, vR))
	}

	forked := isTerminal(decReceived) && isTerminal(decWaiting) && !sameVerdict(decReceived, decWaiting)
	if forked {
		t.Fatalf("SAFETY FORK on round %d from an omitted-but-possible candidate: received=%s waiting=%s (vR=%x)",
			round, describeDecision(decReceived, vR), describeDecision(decWaiting, vR), vR[:4])
	}

	if decWaiting.kind != anchorWait {
		t.Fatalf("waiting node must WAIT while a supporter is still possible, got %s", describeDecision(decWaiting, vR))
	}
}
