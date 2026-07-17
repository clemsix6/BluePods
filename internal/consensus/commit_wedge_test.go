package consensus

import (
	"sync"
	"testing"

	"BluePods/internal/types"
)

// splitByMembership partitions members into those that should support (the
// designated producer first, then fill to wantSupport) and the rest as blamers.
func splitByMembership(members []testValidator, designated testValidator, wantSupport int) (supporters, blamers []testValidator) {
	supporters = append(supporters, designated)

	for _, v := range members {
		if v.pubKey == designated.pubKey {
			continue
		}
		if len(supporters) < wantSupport {
			supporters = append(supporters, v)
		} else {
			blamers = append(blamers, v)
		}
	}

	return supporters, blamers
}

// TestCommitWedge_SilentHoldersUnblockThinBlameSplit reproduces the crash form of
// the permanent commit wedge: two of ten validators die right after producing
// their last vertices, and a round whose next-round frontier SPLIT during the
// crash chaos (five of the eight survivors cite the designated vertex, three do
// not) blocks the forward scan forever. The stored blame (3/10 capped stake) is
// below one third, so achievable support charitably counting the two dead holders
// as potential supporters stays at or above the two-thirds quorum — yet those
// holders will never produce the frontier vertex, so the round is undecidable in
// truth and the cursor freezes with zero skips while production races on.
//
// The disarming evidence is SILENCE: the dead holders have no vertex anywhere in a
// deep stored span above the frontier. Once that span is observed, their stake
// must stop counting as potential support, the split round becomes provably
// certification-impossible, and the scan passes it to the certified anchor beyond.
//
// Layout: rounds 0-1 commit cleanly (all ten). Round 2 is the dead validators'
// last produced round; its round-3 frontier splits 4/4 among the survivors, so
// round 2 is undecided at the cursor. Round 3 is the BLOCKER: its round-4 frontier
// splits 5 support / 3 blame. Round 4 is certified by all eight survivors at round
// 5, and a survivor chain extends the store past the silence horizon.
func TestCommitWedge_SilentHoldersUnblockThinBlameSplit(t *testing.T) {
	vals, vs := newTestValidatorSet(10)
	dag := New(newTestStorage(t), vs, nil, testSystemPod, 0, vals[0].privKey, nil)
	t.Cleanup(func() { dag.Close() })

	// Equal stake: capped weight 25 each, capped total 250, quorum needs 7 of 10.
	setEqualStake(dag, vals, 25)
	disableTxAuth(dag)

	const wedge = 2 // the cursor round the scan must eventually pass

	// The designated producers of rounds 2-4. Rounds 3 and 4 are produced by
	// survivors only, so the dead pair is chosen outside all three designations.
	p2 := designatedProducer(t, dag, vals, 2)
	p3 := designatedProducer(t, dag, vals, 3)
	p4 := designatedProducer(t, dag, vals, 4)

	var dead, survivors []testValidator
	for _, v := range vals {
		notDesignated := v.pubKey != p2.pubKey && v.pubKey != p3.pubKey && v.pubKey != p4.pubKey
		if len(dead) < 2 && notDesignated {
			dead = append(dead, v)
		} else {
			survivors = append(survivors, v)
		}
	}

	// Rounds 0-1: fully connected, so each is directly certified and commits.
	r0 := connectedRound(t, dag, vals, 0, nil)
	r1 := connectedRound(t, dag, vals, 1, r0)

	// Round 2: ALL TEN produce (the dead validators' last vertices), fully citing r1.
	var v2 Hash
	r2all := make([]Hash, 0, len(vals))
	for _, v := range vals {
		h := addDagVertex(t, dag, v, 2, r1)
		r2all = append(r2all, h)
		if v.pubKey == p2.pubKey {
			v2 = h
		}
	}

	r2noV2 := withoutHash(r2all, v2)

	// Round 3: survivors only, split 4/4 on v2 → round 2 undecided at the cursor.
	sup3, blm3 := splitByMembership(survivors, p3, 4)
	var v3 Hash
	r3all := make([]Hash, 0, len(survivors))
	for _, v := range sup3 {
		h := addDagVertex(t, dag, v, 3, r2all)
		r3all = append(r3all, h)
		if v.pubKey == p3.pubKey {
			v3 = h
		}
	}
	for _, v := range blm3 {
		r3all = append(r3all, addDagVertex(t, dag, v, 3, r2noV2))
	}

	r3noV3 := withoutHash(r3all, v3)

	// Round 4: survivors only, split 5 support / 3 blame on v3 → round 3 undecided
	// with stored blame (75) below one third of the capped total (250). v4 cites all
	// of round 3, so its causal history carries v3 and (via round 3's supporters) v2.
	sup4, blm4 := splitByMembership(survivors, p4, 5)
	var v4 Hash
	for _, v := range sup4 {
		h := addDagVertex(t, dag, v, 4, r3all)
		if v.pubKey == p4.pubKey {
			v4 = h
		}
	}
	for _, v := range blm4 {
		addDagVertex(t, dag, v, 4, r3noV3)
	}

	// Diagnostics: the failure below must be the wedge, not a broken fixture.
	if verdict, _ := dag.directAnchorVerdict(wedge); verdict != verdictUndecided {
		t.Fatalf("setup: round %d must be undecided, got verdict=%d", wedge, verdict)
	}
	if verdict, _ := dag.directAnchorVerdict(wedge + 1); verdict != verdictUndecided {
		t.Fatalf("setup: blocking round %d must be undecided, got verdict=%d", wedge+1, verdict)
	}

	// Phase 1 — the silence span is not yet observable (store barely extends past the
	// frontier), so the split round must still read as possibly-certifiable and the
	// cursor must WAIT: this is the delayed-not-lost reading, and it guards the test
	// against declaring impossibility from a shallow store.
	dag.checkCommits()
	if got := dag.LastCommittedRound(); got != wedge {
		t.Fatalf("setup: expected the cursor to wait at round %d before the silence span is observable, got %d", wedge, got)
	}
	if dag.anchorCertImpossible(wedge + 1) {
		t.Fatalf("setup: round %d must not be certification-impossible before the silence span is observable", wedge+1)
	}

	// Round 5: all eight survivors certify v4 (200 of 250 capped stake).
	for _, v := range survivors {
		addDagVertex(t, dag, v, 5, []Hash{v4})
	}
	if dec := dag.anchorStatus(wedge + 2); dec.kind != anchorCommit || dec.anchor != v4 {
		t.Fatalf("setup: resolving round %d must be a certified anchor (v4), got kind=%d", wedge+2, dec.kind)
	}

	// Rounds 6..25: a survivor chain extends the store past the silence horizon for
	// round 3's frontier (rounds 4..24 observed, the dead absent from all of them).
	prev := []Hash{v4}
	for r := uint64(6); r <= 25; r++ {
		prev = []Hash{addDagVertex(t, dag, survivors[0], r, prev)}
	}

	// Phase 2 — the dead holders are provably silent across the whole span, so the
	// split round is certification-impossible, the scan passes it, and the certified
	// anchor at round 4 resolves rounds 2 and 3. On wedged code the cursor stays at 2.
	dag.checkCommits()
	if got := dag.LastCommittedRound(); got <= wedge {
		t.Fatalf("commit cursor wedged at round %d by a thin-blame split at round %d whose missing support is two dead validators: cursor=%d, scan=%d, certImpossible(%d)=%v",
			wedge, wedge+1, got, dag.anchorStatus(wedge).kind, wedge+1, dag.anchorCertImpossible(wedge+1))
	}
	if got := dag.LastCommittedRound(); got < 5 {
		t.Fatalf("expected rounds 2-4 all decided once the silence span is observable, cursor=%d", got)
	}
}

// withoutHash returns hashes with the given hash removed.
func withoutHash(hashes []Hash, remove Hash) []Hash {
	out := make([]Hash, 0, len(hashes)-1)
	for _, h := range hashes {
		if h != remove {
			out = append(out, h)
		}
	}

	return out
}

// recordingFetcher is a VertexFetcher stub that records every requested hash so
// a test can assert exactly which vertices the recovery path asked for.
type recordingFetcher struct {
	mu     sync.Mutex    // mu guards requested
	ustore map[Hash]bool // ustore is the set of hashes requested so far
}

// FetchVertices records the requested hashes and returns immediately.
func (f *recordingFetcher) FetchVertices(hashes []Hash) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.ustore == nil {
		f.ustore = make(map[Hash]bool)
	}
	for _, h := range hashes {
		f.ustore[h] = true
	}
}

// FetchRange is unused by the causal-stall recovery this fetcher observes.
func (f *recordingFetcher) FetchRange(from, to uint64) {}

// requested reports whether the given hash was ever requested.
func (f *recordingFetcher) requested(h Hash) bool {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.ustore[h]
}

// ingestLinkedRound builds one signed vertex per producer at round, each citing
// the given parent links, and feeds them through the REAL AddVertex ingestion
// path (validation, buffering). It returns the produced links keyed for reuse as
// the next round's parents.
func ingestLinkedRound(t *testing.T, dag *DAG, producers []testValidator, round uint64, parents []parentLink) []parentLink {
	t.Helper()

	var links []parentLink
	for _, v := range producers {
		data := buildTestVertexWithParentLinks(t, v, round, 0, parents)
		dag.AddVertex(data)

		vertex := types.GetRootAsVertex(data, 0)
		var h Hash
		copy(h[:], vertex.HashBytes())
		links = append(links, parentLink{hash: h, producer: v.pubKey})
	}

	return links
}

// TestCommitWedge_BufferedCascadeFetchesWithheldParent reproduces the partition
// form of the permanent commit wedge at the INGESTION layer, where the two
// decision-layer fixes cannot help because the store itself is starved.
//
// The shape: a partition cut is not atomic, so an unreachable validator's last
// vertex (v3E) reaches a subset of its peers. A peer that HAS it (D) keeps citing
// it and its descendants; every node that missed it BUFFERS all of D's subsequent
// vertices (missing parent), transitively and forever, because gossip only pushes
// a vertex at production time and nothing ever re-requests v3E: the WAIT-stall
// recovery walk starts from STORED vertices, and a validated store is causally
// closed, so the walk finds nothing missing while the pending buffer — which
// knows exactly which parent it is blocked on — is never consulted. The node's
// visible frontier thins to 3 of 5 producers (60 percent, below the two-thirds
// quorum), no round can ever certify in its view, and the commit cursor freezes
// with zero skips while production races on. The decision layer WAITS correctly
// here: the defect is pure recovery.
//
// The test drives a five-validator view through real AddVertex ingestion: rounds
// 0-2 full; v3E withheld (the cut); D's vertices from round 4 on cite v3E's
// descendants and are buffered; A/B/C chain on alone. It asserts the cursor
// wedges, then that the WAIT-stall fetch REQUESTS v3E (fails on unfixed code),
// then that delivering v3E (the fetch response / the heal) flushes the buffer and
// unwedges the cursor.
func TestCommitWedge_BufferedCascadeFetchesWithheldParent(t *testing.T) {
	vals, vs := newTestValidatorSet(5)

	// The DAG under test observes with a NON-member key so ingestion never
	// triggers its own production into the fixture rounds.
	observer := newTestValidator()
	dag := New(newTestStorage(t), vs, nil, testSystemPod, 0, observer.privKey, nil)
	t.Cleanup(func() { dag.Close() })

	// Equal stake: capped weight 20 percent each, quorum needs 4 of 5.
	setEqualStake(dag, vals, 25)

	fetcher := &recordingFetcher{}
	dag.SetVertexFetcher(fetcher)

	abc, dd, ee := vals[:3], vals[3], vals[4]

	// Rounds 0-2: fully connected, all five producers.
	r0 := ingestLinkedRound(t, dag, vals, 0, nil)
	r1 := ingestLinkedRound(t, dag, vals, 1, r0)
	r2 := ingestLinkedRound(t, dag, vals, 2, r1)

	// The cut: E's round-3 vertex exists (D received it) but never reaches this
	// node — built and withheld.
	v3eData := buildTestVertexWithParentLinks(t, ee, 3, 0, r2)
	var v3e Hash
	copy(v3e[:], types.GetRootAsVertex(v3eData, 0).HashBytes())

	// Round 3: A, B, C, D produce normally (citing round 2); E's vertex is withheld.
	r3 := ingestLinkedRound(t, dag, abc, 3, r2)
	r3d := ingestLinkedRound(t, dag, []testValidator{dd}, 3, r2)
	r3all := append(append([]parentLink{}, r3...), r3d...)
	r3withE := append(append([]parentLink{}, r3all...), parentLink{hash: v3e, producer: ee.pubKey})

	// Rounds 4..30: A/B/C chain on among themselves; D cites v3E (round 4) and its
	// own chain after that, so every D vertex is buffered here, cascading.
	abcParents, dParents := r3all, r3withE
	for r := uint64(4); r <= 30; r++ {
		abcLinks := ingestLinkedRound(t, dag, abc, r, abcParents)
		dLinks := ingestLinkedRound(t, dag, []testValidator{dd}, r, dParents)

		abcParents = abcLinks
		dParents = append(append([]parentLink{}, abcLinks...), dLinks...)
	}

	// Phase 1 — the wedge: only A/B/C are visible from round 4 on (60 percent), so
	// no round can certify and the cursor must freeze after the fully-cited rounds.
	dag.checkCommits()
	wedgedAt := dag.LastCommittedRound()
	if wedgedAt > 4 {
		t.Fatalf("setup: expected the cursor to wedge near round 3 on a 60 percent frontier, got %d", wedgedAt)
	}

	// Phase 2 — recovery must ASK for the withheld parent: two consecutive stalled
	// ticks arm the WAIT-stall fetch, which must surface v3E from the pending
	// buffer's blocked parents. On unfixed code the stored-frontier walk finds
	// nothing (the store is causally closed) and v3E is never requested.
	dag.checkCommits()
	dag.checkCommits()
	if !fetcher.requested(v3e) {
		t.Fatalf("WAIT-stall recovery never requested the withheld parent %x the pending buffer is blocked on", v3e[:4])
	}

	// Phase 3 — the fetch response (the heal): delivering v3E flushes D's buffered
	// chain, the frontier thickens to 4 of 5, rounds certify, and the cursor moves.
	if !dag.AddVertex(v3eData) {
		t.Fatal("withheld vertex was not accepted on delivery")
	}
	dag.checkCommits()
	if got := dag.LastCommittedRound(); got <= wedgedAt {
		t.Fatalf("cursor still wedged at %d after the withheld parent was delivered", got)
	}
}

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
