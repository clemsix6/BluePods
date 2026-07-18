package consensus

import "testing"

// deadPairWedge is the first of two consecutive rounds whose designated producers
// crashed; the commit cursor must eventually pass both to reach the certified anchor.
const deadPairWedge = 2

// TestCommitWedge_RelaxedDeadPairReachesCertifiedAnchor reproduces the relaxed-regime
// form of the permanent commit wedge: two consecutive anchor rounds are each designated
// to a validator that crashed and produced nothing. Each dead round is undecided (no
// candidate to certify, and the relaxed regime never directly blames), so the forward
// scan reaches the second dead round still undecided — and refuses to pass it, never
// reaching the live certified anchor one round beyond. The cursor freezes while
// production races on.
//
// The strict regime is the witness: there the same two rounds are directly BLAMED (a
// next-round quorum cites none of the absent producer), so each is skipped at the cursor
// and the anchor is reached without ever consulting the forward scan. The relaxed regime
// disarmed both blame escapes, so the same shape wedges — until the scan learns to pass a
// round whose designated producer left, and will deliver, no candidate to certify.
func TestCommitWedge_RelaxedDeadPairReachesCertifiedAnchor(t *testing.T) {
	vals := distinctDesignationVals(t)

	t.Run("relaxed", func(t *testing.T) {
		dag := buildDeadPairWedge(t, vals, true)

		dag.checkCommits()

		if got := dag.LastCommittedRound(); got <= deadPairWedge {
			t.Fatalf("relaxed commit cursor wedged at round %d by two consecutive rounds designated to crashed producers: cursor=%d, scan=%d, certImpossible(%d)=%v",
				deadPairWedge, got, dag.anchorStatus(deadPairWedge).kind, deadPairWedge+1, dag.anchorCertImpossible(deadPairWedge+1))
		}
		if got := dag.LastCommittedRound(); got < deadPairWedge+3 {
			t.Fatalf("expected both dead rounds passed and the certified anchor at round 4 committed (cursor %d), got %d", deadPairWedge+3, got)
		}
	})

	t.Run("strict", func(t *testing.T) {
		dag := buildDeadPairWedge(t, vals, false)

		dag.checkCommits()

		if got := dag.LastCommittedRound(); got < deadPairWedge+3 {
			t.Fatalf("strict witness: direct blame must skip both dead rounds and reach the certified anchor, cursor=%d", got)
		}
	})
}

// distinctDesignationVals generates a ten-validator committee whose rounds 2, 3 and 4
// designate three DISTINCT producers, so the two crashed producers of the wedge rounds
// and the live certifier of round 4 are different nodes. Keys are random, so it retries
// until the deterministic rotation yields three distinct designees.
func distinctDesignationVals(t *testing.T) []testValidator {
	t.Helper()

	for {
		vals, vs := newTestValidatorSet(10)
		dag := New(newTestStorage(t), vs, nil, testSystemPod, 0, vals[0].privKey, nil)
		setEqualStake(dag, vals, 25)

		p2 := designatedProducer(t, dag, vals, 2)
		p3 := designatedProducer(t, dag, vals, 3)
		p4 := designatedProducer(t, dag, vals, 4)
		dag.Close()

		if p2.pubKey != p3.pubKey && p2.pubKey != p4.pubKey && p3.pubKey != p4.pubKey {
			return vals
		}
	}
}

// buildDeadPairWedge builds the dead-pair wedge fixture. Rounds 0-1 commit cleanly with
// the whole committee; then the designated producers of rounds 2 and 3 crash — they
// produce nothing from round 2 on — while the other eight extend a fully connected DAG
// through round 25. Round 4's anchor is certified by the survivors, and the chain runs
// past the silence horizon so both crashed producers are provably span-silent above
// their frontiers. relaxed selects the unlatched bootstrap regime; otherwise the strict
// BFT regime governs.
func buildDeadPairWedge(t *testing.T, vals []testValidator, relaxed bool) *DAG {
	t.Helper()

	_, vs := reuseSet(vals)

	var opts []Option
	if relaxed {
		opts = append(opts, WithMinValidators(len(vals)))
	}

	dag := New(newTestStorage(t), vs, nil, testSystemPod, 0, vals[0].privKey, nil, opts...)
	t.Cleanup(func() { dag.Close() })

	setEqualStake(dag, vals, 25)
	disableTxAuth(dag)
	dag.round.Store(30) // production races ahead of the frozen commit cursor

	if got := dag.roundIsRelaxed(deadPairWedge + 1); got != relaxed {
		t.Fatalf("regime setup: roundIsRelaxed(%d)=%v, want %v", deadPairWedge+1, got, relaxed)
	}

	p2 := designatedProducer(t, dag, vals, 2)
	p3 := designatedProducer(t, dag, vals, 3)

	survivors := make([]testValidator, 0, len(vals)-2)
	for _, v := range vals {
		if v.pubKey != p2.pubKey && v.pubKey != p3.pubKey {
			survivors = append(survivors, v)
		}
	}

	// Rounds 0-1: the whole committee produces, so each round commits cleanly and the
	// cursor reaches the first wedge round.
	r0 := connectedRound(t, dag, vals, 0, nil)
	prev := connectedRound(t, dag, vals, 1, r0)

	// Rounds 2-25: only the survivors produce. p2 and p3 are absent from every round of
	// the silence span above both frontiers, so their rounds are dead — no designated
	// vertex, undecided in the relaxed regime, directly blamed in the strict one — while
	// round 4's designated survivor anchors a certified chain the store carries past the
	// silence horizon.
	for r := uint64(2); r <= 25; r++ {
		prev = connectedRound(t, dag, survivors, r, prev)
	}

	assertDeadPairFixture(t, dag, relaxed)

	return dag
}

// assertDeadPairFixture guards the wedge test against a broken fixture: both wedge rounds
// must be undecided under the relaxed regime (blamed under the strict one), and round 4
// must host the certified anchor the scan has to reach.
func assertDeadPairFixture(t *testing.T, dag *DAG, relaxed bool) {
	t.Helper()

	want := verdictBlamed
	if relaxed {
		want = verdictUndecided
	}

	for _, r := range []uint64{deadPairWedge, deadPairWedge + 1} {
		if verdict, _ := dag.directAnchorVerdict(r); verdict != want {
			t.Fatalf("setup: dead round %d verdict=%d, want %d (relaxed=%v)", r, verdict, want, relaxed)
		}
	}

	if dec := dag.anchorStatus(deadPairWedge + 2); dec.kind != anchorCommit {
		t.Fatalf("setup: round %d must host a certified anchor, got kind=%d", deadPairWedge+2, dec.kind)
	}
}
