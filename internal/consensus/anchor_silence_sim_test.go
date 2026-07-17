package consensus

import "testing"

// buildSilenceForkDAGs constructs two honest observers of the SAME validator set
// that differ in exactly one vertex: holder H's round-4 vertex. The well-informed
// observer holds it; the starved observer has it delivered late, past the silence
// span. Everything else — rounds 0-2, the whole of round 3, the certified round-4
// resolver, the round-5 certification, and a survivor chain out to round 25 — is
// byte-identical on both, so any verdict divergence is caused solely by the delayed
// delivery of H's round-4 vertex, never by a different DAG.
//
// Layout (4 validators, equal stake, quorum three of four, capped total 100):
//   - Round 2 is undecided: two round-3 vertices cite p2's vertex v2, two do not, so
//     neither the certify nor the blame quorum forms. v2 is the fork target.
//   - Round 3's designated vertex v3 OMITS v2 (it is one of the round-2 blamers), so
//     resolving round 2 through v3 SKIPS it.
//   - Round 3 certifies on the well-informed observer: H, X and p4 all cite v3 at
//     round 4 (three of four). With H's round-4 vertex missing, the starved observer
//     sees only two citers, so round 3 is undecided there.
//   - The round-4 anchor v4 (p4) cites every round-3 vertex, so its causal history
//     carries v2: resolving round 2 through v4 COMMITS it.
//   - A survivor chain (the three non-H holders) runs rounds 5-25, so H is absent
//     from the whole silence span above the round-4 frontier on the starved observer.
func buildSilenceForkDAGs(t *testing.T) (well, starved *DAG, v2 Hash) {
	t.Helper()

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

	well = build()
	starved = build()

	// Designations are a pure function of membership and round, so both observers
	// agree on them.
	p2 := designatedProducer(t, well, vals, 2)
	p3 := designatedProducer(t, well, vals, 3)
	p4 := designatedProducer(t, well, vals, 4)

	// The three non-p4 holders are the extra round-4 citers; H is the delayed one.
	rest4 := others(vals, p4)
	holderH, holderX, holderY := rest4[0], rest4[1], rest4[2]

	// A vertex is built once and fed to both observers, except H's round-4 vertex,
	// which reaches only the well-informed one.
	feed := func(dags []*DAG, v testValidator, round uint64, parents []Hash) Hash {
		data, hash := buildRoundVertex(t, v, round, parents)
		for _, d := range dags {
			d.store.add(data, hash, round, v.pubKey)
		}
		return hash
	}
	both := []*DAG{well, starved}

	// Rounds 0-2: fully connected, so round 2 is a clean fixture.
	r0 := feedRound(both, feed, vals, 0, nil)
	r1 := feedRound(both, feed, vals, 1, hashesOf(r0, vals))
	r2 := feedRound(both, feed, vals, 2, hashesOf(r1, vals))

	v2 = r2[p2.pubKey]
	r2all := hashesOf(r2, vals)
	r2noV2 := withoutHash(r2all, v2)

	// Round 3: v3 (p3) and one other holder omit v2 (round-2 blamers); the remaining
	// two cite it (supporters). Two-two split → round 2 undecided, and v3 omits v2.
	omitter3 := others(vals, p3)[0]
	r3 := make(map[Hash]Hash, len(vals))
	for _, v := range vals {
		parents := r2all // v2 supporter
		if v.pubKey == p3.pubKey || v.pubKey == omitter3.pubKey {
			parents = r2noV2 // v2 blamer
		}
		r3[v.pubKey] = feed(both, v, 3, parents)
	}
	v3 := r3[p3.pubKey]
	r3all := hashesOf(r3, vals)
	r3noV3 := withoutHash(r3all, v3)

	// Round 4: p4 cites all of round 3 (carries v2 and v3); X and H cite v3; Y blames
	// v3. v3 supporters = {p4, X, H} = three of four → round 3 certifies with H,
	// undecided without it. Every round-4 vertex cites three of four round-3 vertices,
	// meeting the two-thirds production quorum.
	v4 := feed(both, p4, 4, r3all)
	citeV3 := append([]Hash{v3}, r3noV3[:2]...)
	x4 := feed(both, holderX, 4, citeV3)
	y4 := feed(both, holderY, 4, r3noV3)  // blames v3
	feed([]*DAG{well}, holderH, 4, citeV3) // delayed: well-informed only

	// Round 5: the three non-H holders certify v4. Each cites the three round-4
	// vertices visible without H (v4, x4, y4) — the full two-thirds production quorum
	// on the starved observer — so v4 reaches the citation quorum.
	survivors := []testValidator{p4, holderX, holderY}
	r4visible := []Hash{v4, x4, y4}
	r5 := make(map[Hash]Hash, len(survivors))
	for _, v := range survivors {
		r5[v.pubKey] = feed(both, v, 5, r4visible)
	}

	// Rounds 6-25: the survivor chain extends the store past the silence horizon of
	// the round-4 frontier, with H absent from every round of the span.
	prev := hashesOf(r5, survivors)
	for r := uint64(6); r <= 25; r++ {
		next := make(map[Hash]Hash, len(survivors))
		for _, v := range survivors {
			next[v.pubKey] = feed(both, v, r, prev)
		}
		prev = hashesOf(next, survivors)
	}

	return well, starved, v2
}

// feedRound builds one fully-connected vertex per validator at round, citing every
// given parent, feeds them to the observers, and returns the per-producer hashes.
func feedRound(dags []*DAG, feed func([]*DAG, testValidator, uint64, []Hash) Hash, vals []testValidator, round uint64, parents []Hash) map[Hash]Hash {
	out := make(map[Hash]Hash, len(vals))
	for _, v := range vals {
		out[v.pubKey] = feed(dags, v, round, parents)
	}
	return out
}

// hashesOf collects the vertices produced by the given validators, in validator
// order, from a per-producer hash map.
func hashesOf(byProducer map[Hash]Hash, vals []testValidator) []Hash {
	out := make([]Hash, 0, len(vals))
	for _, v := range vals {
		if h, ok := byProducer[v.pubKey]; ok {
			out = append(out, h)
		}
	}
	return out
}

// TestAnchorSilenceDelayedHolderForksVerdict drives the anchor decision on both
// observers built by buildSilenceForkDAGs and asserts they agree on round 2. The
// only difference between them is the delivery time of one honest holder's vertex,
// so a divergence is a safety fork: the starved observer, seeing more silence,
// declares round 3 certification-impossible and resolves round 2 through the
// round-4 anchor, while the well-informed observer certifies round 3 and resolves
// round 2 through v3. With the silence rule anchored on evidence rather than the
// local delivery clock, the starved observer instead waits for the missing vertex,
// so both reach the same verdict (or one waits while the other could still act).
func TestAnchorSilenceDelayedHolderForksVerdict(t *testing.T) {
	well, starved, v2 := buildSilenceForkDAGs(t)

	// Both observers hold identical round-2 and round-3 vertices, so round 2 is
	// undecided on both: this is a genuine indirect-resolution fixture.
	if verdict, _ := well.directAnchorVerdict(2); verdict != verdictUndecided {
		t.Fatalf("setup: round 2 must be undecided on the well-informed observer, got %d", verdict)
	}
	if verdict, _ := starved.directAnchorVerdict(2); verdict != verdictUndecided {
		t.Fatalf("setup: round 2 must be undecided on the starved observer, got %d", verdict)
	}

	// Round 3 splits on the one delayed vertex: certified with it, undecided without.
	if verdict, _ := well.directAnchorVerdict(3); verdict != verdictCertified {
		t.Fatalf("setup: round 3 must certify on the well-informed observer, got %d", verdict)
	}
	if verdict, _ := starved.directAnchorVerdict(3); verdict != verdictUndecided {
		t.Fatalf("setup: round 3 must be undecided on the starved observer, got %d", verdict)
	}

	// The starved observer sees round 3 as silence-certification-impossible: the
	// delayed holder is absent from the whole span above the frontier, while a stored
	// blamer alone would not rule certification out. This is the reversible verdict the
	// scan must not act on past the adjacent round.
	impossible, byBlame := starved.certImpossibility(3)
	if !impossible || byBlame {
		t.Fatalf("setup: round 3 must be silence-certification-impossible on the starved observer, got impossible=%v byBlame=%v", impossible, byBlame)
	}

	decWell := well.anchorStatus(2)
	decStarved := starved.anchorStatus(2)

	// The safety property: two honest observers of the same vertices must never
	// commit round 2 to different verdicts. A terminal disagreement (one commits,
	// the other skips) is a zero-rollback fork. A wait on either side is acceptable —
	// it means that observer has not acted while the other still could.
	forked := isTerminal(decWell) && isTerminal(decStarved) && !sameVerdict(decWell, decStarved)
	if forked {
		t.Fatalf("SAFETY FORK on round 2 from a delayed honest holder: well-informed=%s starved=%s (v2=%x)",
			describeDecision(decWell, v2), describeDecision(decStarved, v2), v2[:4])
	}

	// The well-informed observer resolves round 2 through the certified round-3 anchor,
	// which omits v2, so it SKIPS. The starved observer, lacking the vertex that would
	// certify round 3, must WAIT for it rather than resolve round 2 through the round-4
	// anchor that carries v2 — the wait is what averts the fork.
	if decWell.kind != anchorSkip {
		t.Fatalf("well-informed observer must skip round 2 through the certified round-3 anchor, got %s", describeDecision(decWell, v2))
	}
	if decStarved.kind != anchorWait {
		t.Fatalf("starved observer must wait on the missing vertex, not act on reversible silence, got %s", describeDecision(decStarved, v2))
	}
}

// isTerminal reports whether an anchor decision is a final verdict (commit or skip)
// rather than a wait.
func isTerminal(dec anchorDecision) bool {
	return dec.kind == anchorCommit || dec.kind == anchorSkip
}

// sameVerdict reports whether two anchor decisions are the identical verdict,
// including the resolved anchor for commits.
func sameVerdict(a, b anchorDecision) bool {
	if a.kind != b.kind {
		return false
	}
	if a.kind == anchorCommit {
		return a.anchor == b.anchor
	}
	return true
}

// describeDecision renders an anchor decision for a failure message, naming the
// committed anchor relative to the fork target v2.
func describeDecision(dec anchorDecision, v2 Hash) string {
	switch dec.kind {
	case anchorCommit:
		if dec.anchor == v2 {
			return "commit(v2)"
		}
		return "commit(other)"
	case anchorSkip:
		return "skip"
	default:
		return "wait"
	}
}
