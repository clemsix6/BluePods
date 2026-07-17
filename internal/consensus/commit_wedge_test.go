package consensus

import "testing"

// TestCommitWedge_LateCertifiedAnchorUnblocksUndecidedRun reproduces a permanent
// commit-cursor wedge at the DAG level, with no real network: an anchor round whose
// producer was unreachable when the round passed is never resolved, freezing
// lastCommitted while round production continues indefinitely.
//
// The mechanism: when a producer is UNREACHABLE around an anchor round, its vertex
// reaches only a sub-quorum of the next round. With the round-N+1 frontier also
// short by the unreachable stake, neither a certify quorum (2/3 cite the producer)
// nor a blame quorum (2/3 cite none of it) forms over the FULL holder set, so the
// round is UNDECIDED — and, because the round-N+1 citations are immutable once
// built, permanently so. A single undecided round would still be resolved by the
// indirect rule (the first later CERTIFIED anchor decides it by causal membership).
// The wedge arms when TWO such rounds land consecutively at the commit cursor:
// anchorStatus refuses to scan PAST a later still-undecided round (anchor_decision.go
// default -> anchorWait), so it never reaches the certified anchor that would
// resolve the first — even though that anchor is present and decided. The commit
// cursor freezes while production races ahead.
//
// The DAG below: rounds 0-1 commit cleanly; round 2 (the withheld anchor round) is
// undecided (its designated vertex reaches 2 of 4 at round 3); round 3 is likewise
// undecided (its designated vertex reaches 2 of 4 at round 4); round 4 hosts a
// CERTIFIED anchor (cited by 3 of 4 at round 5) whose causal history reaches round
// 2's vertex. Delivering rounds 4-5 LATE (the network moving on while the withheld
// producer was unreachable) makes the resolving anchor present, so the cursor must
// pass the withheld round. It does not: the still-undecided round 3 blocks the scan.
//
// FAILS on current code (cursor frozen at the withheld round 2). A fix that lets the
// commit loop resolve past a provably-decided (cert-impossible) later round — without
// weakening zero rollback — turns it green.
func TestCommitWedge_LateCertifiedAnchorUnblocksUndecidedRun(t *testing.T) {
	vals, vs := newTestValidatorSet(4)
	dag := New(newTestStorage(t), vs, nil, testSystemPod, 0, vals[0].privKey, nil)
	t.Cleanup(func() { dag.Close() })

	// Equal stake (quorum = 3 of 4) and a frozen genesis holder set; minValidators 0
	// keeps the strict BFT regime from round 1, so blame is reachable in principle.
	setEqualStake(dag, vals, 25)
	disableTxAuth(dag)

	// Production races ahead of the commit cursor, as in the wedged runs.
	dag.round.Store(20)

	const wedge = 2 // the withheld anchor round the cursor must eventually pass

	// --- Spine rounds 0-1: fully connected, so each is directly certified and commits. ---
	r0 := connectedRound(t, dag, vals, 0, nil)
	r1 := connectedRound(t, dag, vals, 1, r0)

	// --- Round 2 (the withheld anchor round). Its designated producer's vertex v2
	//     exists but will be cited by only a sub-quorum of round 3. ---
	p2 := designatedProducer(t, dag, vals, 2)
	v2 := addDagVertex(t, dag, p2, 2, r1)

	var r2others []Hash
	for _, v := range others(vals, p2) {
		r2others = append(r2others, addDagVertex(t, dag, v, 2, r1))
	}
	r2all := append([]Hash{v2}, r2others...)

	// --- Round 3: two producers cite v2, two do not, so round 2 is UNDECIDED (a
	//     sub-quorum split, neither certified nor blamed over the 4-member set).
	//     Round 3 is itself made undecided below via round 4. ---
	p3 := designatedProducer(t, dag, vals, 3)
	var v3 Hash
	r3all := make([]Hash, 0, len(vals))
	for i, v := range vals {
		parents := r2others // blamer of round 2: cites none of p2's vertices
		if i < 2 {
			parents = r2all // supporter of round 2: cites v2
		}
		h := addDagVertex(t, dag, v, 3, parents)
		r3all = append(r3all, h)
		if v.pubKey == p3.pubKey {
			v3 = h
		}
	}

	// Phase 1 — the withheld producer is still unreachable and no later certified
	// anchor exists yet. The cursor MUST wait at the wedge round: this is a correct
	// wait, not the bug, and asserting it guards the test against a setup artifact.
	dag.checkCommits()
	if got := dag.LastCommittedRound(); got != wedge {
		t.Fatalf("setup: expected the cursor to wait at the withheld round %d before the resolver arrives, got %d", wedge, got)
	}

	// --- Round 4 (delivered LATE): the resolving CERTIFIED anchor. Its designated
	//     vertex v4 cites all of round 3 (so its causal history reaches round 2's v2)
	//     and supports v3; one other round-4 vertex supports v3, two blame it, so
	//     round 3 is UNDECIDED (2/4 support, 2/4 blame). ---
	p4 := designatedProducer(t, dag, vals, 4)

	r3noV3 := make([]Hash, 0, len(r3all)-1)
	for _, h := range r3all {
		if h != v3 {
			r3noV3 = append(r3noV3, h)
		}
	}

	otherR4 := others(vals, p4)                 // three non-designated round-4 producers
	v4 := addDagVertex(t, dag, p4, 4, r3all)    // supporter of v3, reaches all of round 3
	addDagVertex(t, dag, otherR4[0], 4, r3all)  // supporter of v3
	addDagVertex(t, dag, otherR4[1], 4, r3noV3) // blamer of v3
	addDagVertex(t, dag, otherR4[2], 4, r3noV3) // blamer of v3

	// --- Round 5 (delivered LATE): certifies round 4's anchor, making round 4 the
	//     first CERTIFIED anchor after the wedge — the resolver the scan must reach. ---
	for _, c := range others(vals, p4)[:3] {
		addDagVertex(t, dag, c, 5, []Hash{v4})
	}

	// Diagnostics: the failure below must be the wedge, not a broken fixture.
	if verdict, _ := dag.directAnchorVerdict(wedge); verdict != verdictUndecided {
		t.Fatalf("setup: withheld round %d must be undecided, got verdict=%d", wedge, verdict)
	}
	if verdict, _ := dag.directAnchorVerdict(wedge + 1); verdict != verdictUndecided {
		t.Fatalf("setup: blocking round %d must be undecided, got verdict=%d", wedge+1, verdict)
	}
	if dec := dag.anchorStatus(wedge + 2); dec.kind != anchorCommit || dec.anchor != v4 {
		t.Fatalf("setup: resolving round %d must be a certified anchor (v4), got kind=%d", wedge+2, dec.kind)
	}

	// Phase 2 — the resolving certified anchor is now present. The cursor must pass
	// the withheld round. It does not on current code: the still-undecided round 3
	// blocks anchorStatus's forward scan from ever reaching round 4's certification.
	dag.checkCommits()
	if got := dag.LastCommittedRound(); got <= wedge {
		t.Fatalf("commit cursor wedged at the withheld round %d while a certified anchor at round %d would resolve it: cursor=%d", wedge, wedge+2, got)
	}
}
