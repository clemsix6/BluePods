package consensus

import (
	"encoding/binary"
	"fmt"
	"testing"

	flatbuffers "github.com/google/flatbuffers/go"

	"BluePods/internal/types"
)

// scenarioVertex is one prebuilt tagged vertex: identical bytes are ingested by
// every node so hashes match, and the tag lets a test observe commit order.
type scenarioVertex struct {
	data     []byte // data is the serialized vertex
	hash     Hash   // hash is the vertex hash
	round    uint64 // round is the vertex round
	producer Hash   // producer is the producing validator
	tag      string // tag is the vertex's transaction function name (its commit-order label)
}

// taggedATX builds an AttestedTransaction whose inner transaction carries a
// unique nonzero hash and a unique function name derived from idx. The unique
// hash keeps the commit-once guard from de-duplicating distinct tagged txs, and
// the function name surfaces on the committed channel as the vertex's label.
func taggedATX(t *testing.T, idx int) []byte {
	t.Helper()

	builder := flatbuffers.NewBuilder(1024)

	var h [32]byte
	binary.LittleEndian.PutUint64(h[:], uint64(idx)+1)

	hashVec := builder.CreateByteVector(h[:])
	senderVec := builder.CreateByteVector(make([]byte, 32))
	podVec := builder.CreateByteVector(make([]byte, 32))
	funcNameOff := builder.CreateString(fmt.Sprintf("t%d", idx))

	types.TransactionStart(builder)
	types.TransactionAddHash(builder, hashVec)
	types.TransactionAddSender(builder, senderVec)
	types.TransactionAddPod(builder, podVec)
	types.TransactionAddFunctionName(builder, funcNameOff)
	txOff := types.TransactionEnd(builder)

	types.AttestedTransactionStartObjectsVector(builder, 0)
	objVec := builder.EndVector(0)
	types.AttestedTransactionStartProofsVector(builder, 0)
	prfVec := builder.EndVector(0)

	types.AttestedTransactionStart(builder)
	types.AttestedTransactionAddTransaction(builder, txOff)
	types.AttestedTransactionAddObjects(builder, objVec)
	types.AttestedTransactionAddProofs(builder, prfVec)
	atxOff := types.AttestedTransactionEnd(builder)

	builder.Finish(atxOff)

	return builder.FinishedBytes()
}

// taggedVertex builds a signed vertex for v at round with the given parents,
// carrying a single tagged transaction, and returns its bytes and hash.
func taggedVertex(t *testing.T, v testValidator, round uint64, parents []Hash, idx int) ([]byte, Hash) {
	t.Helper()

	data := buildTestVertexWithTx(t, v, round, parents, 1, taggedATX(t, idx))
	vertex := types.GetRootAsVertex(data, 0)

	var hash Hash
	copy(hash[:], vertex.HashBytes())

	return data, hash
}

// buildTaggedSpine builds a fully connected DAG over rounds 0..maxRound: every
// validator produces one tagged vertex per round citing all vertices of the
// previous round. Every round below maxRound is therefore directly certified by
// the next round's quorum. Vertices are returned in round-then-validator order.
func buildTaggedSpine(t *testing.T, vals []testValidator, maxRound uint64) []scenarioVertex {
	t.Helper()

	var out []scenarioVertex
	var prev []Hash
	idx := 0

	for r := uint64(0); r <= maxRound; r++ {
		var cur []Hash
		for _, v := range vals {
			data, hash := taggedVertex(t, v, r, prev, idx)
			out = append(out, scenarioVertex{data, hash, r, v.pubKey, fmt.Sprintf("t%d", idx)})
			cur = append(cur, hash)
			idx++
		}
		prev = cur
	}

	return out
}

// ingest inserts the scenario vertices into the DAG store in the given order,
// bypassing validation so the test controls arrival order precisely.
func ingest(dag *DAG, spine []scenarioVertex, order []int) {
	for _, i := range order {
		sv := spine[i]
		dag.store.add(sv.data, sv.hash, sv.round, sv.producer)
	}
}

// forwardOrder returns indices 0..n-1.
func forwardOrder(n int) []int {
	order := make([]int, n)
	for i := range order {
		order[i] = i
	}
	return order
}

// reverseOrder returns indices n-1..0.
func reverseOrder(n int) []int {
	order := make([]int, n)
	for i := range order {
		order[i] = n - 1 - i
	}
	return order
}

// drainCommitted returns the function names of every CommittedTx currently
// queued on the DAG's committed channel, in emission (commit) order.
func drainCommitted(dag *DAG) []string {
	var fns []string
	for {
		select {
		case c := <-dag.committed:
			fns = append(fns, c.Function)
		default:
			return fns
		}
	}
}

// newSpineDAG builds a four-validator, equal-stake DAG for the deterministic
// commit tests and sets the production round high enough that the legacy
// arrival-order path would also run (so the pre-change test is a true RED).
func newSpineDAG(t *testing.T, vals []testValidator, vs *ValidatorSet, topRound uint64) *DAG {
	t.Helper()

	dag := New(newTestStorage(t), vs, nil, testSystemPod, 0, vals[0].privKey, nil)
	t.Cleanup(func() { dag.Close() })
	setEqualStake(dag, vals, 25)
	disableTxAuth(dag)
	dag.round.Store(topRound + 2)

	return dag
}

// equalLog reports whether two commit logs are byte-identical in order.
func equalLog(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// contains reports whether tag appears in log.
func contains(log []string, tag string) bool {
	for _, s := range log {
		if s == tag {
			return true
		}
	}
	return false
}

// countTag returns how many times tag appears in log.
func countTag(log []string, tag string) int {
	n := 0
	for _, s := range log {
		if s == tag {
			n++
		}
	}
	return n
}

// TestCheckCommits_IdenticalLogsAcrossArrivalOrders is the core determinism
// guarantee: two nodes fed the identical vertex set in opposite arrival orders
// must produce byte-identical committed logs. The legacy arrival-order path
// applies each round's vertices in insertion order, so it diverges (RED); the
// anchor batch path orders every batch by round-then-hash, so it agrees (GREEN).
func TestCheckCommits_IdenticalLogsAcrossArrivalOrders(t *testing.T) {
	vals, _ := newTestValidatorSet(4)
	const maxRound = 3

	spine := buildTaggedSpine(t, vals, maxRound)
	n := len(spine)

	_, vsA := reuseSet(vals)
	dagA := newSpineDAG(t, vals, vsA, maxRound)
	ingest(dagA, spine, forwardOrder(n))
	dagA.checkCommits()
	logForward := drainCommitted(dagA)

	_, vsB := reuseSet(vals)
	dagB := newSpineDAG(t, vals, vsB, maxRound)
	ingest(dagB, spine, reverseOrder(n))
	dagB.checkCommits()
	logReverse := drainCommitted(dagB)

	if len(logForward) == 0 {
		t.Fatal("forward node committed nothing")
	}
	if !equalLog(logForward, logReverse) {
		t.Fatalf("commit logs diverge across arrival orders:\n forward=%v\n reverse=%v", logForward, logReverse)
	}

	// Rounds 0 and 1 are fully swept (4 each); round 2 contributes only its
	// designated anchor; round 3 waits (no round 4 to certify it).
	if len(logForward) != 9 {
		t.Fatalf("expected 9 committed vertices (4+4+1), got %d: %v", len(logForward), logForward)
	}
}

// TestCheckCommits_NeverReferencedVertexNeverCommitted verifies that a vertex no
// committed anchor reaches through its causal history is applied on no node. The
// legacy path commits every vertex of a committed round regardless of citation,
// so the orphan leaks in (RED); the anchor path only applies causal ancestors.
func TestCheckCommits_NeverReferencedVertexNeverCommitted(t *testing.T) {
	vals, vs := newTestValidatorSet(4)
	const maxRound = 3

	spine := buildTaggedSpine(t, vals, maxRound)

	// An extra round-1 vertex from vals[0] (distinct parents → distinct hash) that
	// no round-2 vertex cites: it is an ancestor of nothing, so it can never ride a
	// commit batch.
	orphanData, orphanHash := taggedVertex(t, vals[0], 1, []Hash{{0xAB}}, 9001)

	dag := newSpineDAG(t, vals, vs, maxRound)
	ingest(dag, spine, forwardOrder(len(spine)))
	dag.store.add(orphanData, orphanHash, 1, vals[0].pubKey)

	dag.checkCommits()
	log := drainCommitted(dag)

	if contains(log, "t9001") {
		t.Fatalf("never-referenced orphan was committed: %v", log)
	}
	if len(log) == 0 {
		t.Fatal("nothing committed; scenario did not exercise the commit path")
	}
}

// TestCheckCommits_LateVertexCommittedExactlyOnce delivers a whole later round
// after earlier rounds have already been decided, and verifies the phased node
// converges on the identical log the all-at-once node produces, with every vertex
// applied exactly once. The late round-3 vertices are what certify round 2, so
// round 2 commits only after they arrive; the phased delivery respects the
// parents-before-child invariant (a vertex's parents are always delivered first),
// unlike withholding an anchor a present quorum already cites.
func TestCheckCommits_LateVertexCommittedExactlyOnce(t *testing.T) {
	vals, _ := newTestValidatorSet(4)
	const maxRound = 3

	spine := buildTaggedSpine(t, vals, maxRound)

	// Split the spine into rounds 0..2 (early) and round 3 (late).
	var early, late []int
	for i, sv := range spine {
		if sv.round < 3 {
			early = append(early, i)
		} else {
			late = append(late, i)
		}
	}

	_, vsA := reuseSet(vals)
	dagA := newSpineDAG(t, vals, vsA, maxRound)
	ingest(dagA, spine, forwardOrder(len(spine)))
	dagA.checkCommits()
	full := drainCommitted(dagA)

	_, vsB := reuseSet(vals)
	dagB := newSpineDAG(t, vals, vsB, maxRound)
	ingest(dagB, spine, early)
	dagB.checkCommits() // decides rounds 0 and 1; round 2 waits for round 3
	ingest(dagB, spine, late)
	dagB.checkCommits() // round 3 arrives late; round 2 now commits
	phased := drainCommitted(dagB)

	if !equalLog(full, phased) {
		t.Fatalf("late delivery changed the log:\n full=%v\n phased=%v", full, phased)
	}
	for _, sv := range spine {
		if got := countTag(phased, sv.tag); got > 1 {
			t.Fatalf("vertex %s applied %d times, want at most 1", sv.tag, got)
		}
	}
	if len(phased) == 0 {
		t.Fatal("nothing committed; scenario did not exercise the commit path")
	}
}

// reuseSet returns a fresh ValidatorSet over the same validator pubkeys, so two
// nodes designate the same anchors while keeping independent stores.
func reuseSet(vals []testValidator) ([]testValidator, *ValidatorSet) {
	pubkeys := make([]Hash, len(vals))
	for i, v := range vals {
		pubkeys[i] = v.pubKey
	}
	return vals, NewValidatorSet(pubkeys)
}

// connectedRound adds one empty-transaction vertex per validator at round, each
// citing the given parents, and returns their hashes in validator order.
func connectedRound(t *testing.T, dag *DAG, vals []testValidator, round uint64, parents []Hash) []Hash {
	t.Helper()

	var hs []Hash
	for _, v := range vals {
		hs = append(hs, addDagVertex(t, dag, v, round, parents))
	}
	return hs
}

// TestCheckCommits_SkipAdvancesStragglersRideNext verifies that a SKIP round
// advances the cursor without applying its designated (blamed) vertex, while its
// non-designated vertices — cited by the next round — ride the following commit
// batch. Rounds 0 and 1 commit normally; round 2's designated vertex is blamed by
// round 3 (which cites the round-2 stragglers instead), so round 2 is skipped and
// only its stragglers, as ancestors of round 3's certified anchor, are applied.
func TestCheckCommits_SkipAdvancesStragglersRideNext(t *testing.T) {
	vals, _ := newTestValidatorSet(4)
	_, vs := reuseSet(vals)
	dag := New(newTestStorage(t), vs, nil, testSystemPod, 0, vals[0].privKey, nil)
	t.Cleanup(func() { dag.Close() })
	setEqualStake(dag, vals, 25)

	r0 := connectedRound(t, dag, vals, 0, nil)
	r1 := connectedRound(t, dag, vals, 1, r0)

	// Round 2: the designated producer's vertex (blamed) plus three stragglers.
	p2 := designatedProducer(t, dag, vals, 2)
	blamed := addDagVertex(t, dag, p2, 2, r1)
	var stragglers []Hash
	for _, v := range others(vals, p2) {
		stragglers = append(stragglers, addDagVertex(t, dag, v, 2, r1))
	}

	// Round 3: every producer cites the round-2 stragglers, none cites the blamed
	// vertex, so a round-3 quorum blames round 2.
	p3 := designatedProducer(t, dag, vals, 3)
	var anchor3 Hash
	for _, v := range vals {
		h := addDagVertex(t, dag, v, 3, stragglers)
		if v.pubKey == p3.pubKey {
			anchor3 = h
		}
	}

	// Round 4 certifies the round-3 anchor so round 3 commits and sweeps its history.
	for _, c := range others(vals, p3)[:3] {
		addDagVertex(t, dag, c, 4, []Hash{anchor3})
	}

	if dec := dag.anchorStatus(2); dec.kind != anchorSkip {
		t.Fatalf("round 2 must be skipped (blamed), got kind=%d", dec.kind)
	}

	dag.checkCommits()

	if dag.LastCommittedRound() <= 2 {
		t.Fatalf("SKIP did not advance the cursor past round 2: cursor=%d", dag.LastCommittedRound())
	}
	if dag.store.isVertexCommitted(blamed) {
		t.Fatal("blamed designated vertex was applied; a SKIP must apply nothing of its own")
	}
	for _, s := range stragglers {
		if !dag.store.isVertexCommitted(s) {
			t.Fatalf("round-2 straggler %x did not ride the next commit batch", s[:4])
		}
	}
}

// TestCheckCommits_FullQuorumLatchesAfterDirectCertification is the C3 regression:
// the anchor commit loop is the sole caller of markFullQuorumAchieved. After the
// transition+buffer window, the first round certified DIRECTLY by a strict round-N+1
// quorum must latch fullQuorumAchieved — otherwise production quorum stays relaxed
// and validateVertex keeps skipping parent validation forever.
func TestCheckCommits_FullQuorumLatchesAfterDirectCertification(t *testing.T) {
	vals, _ := newTestValidatorSet(4)
	_, vs := reuseSet(vals)
	dag := New(newTestStorage(t), vs, nil, testSystemPod, 0, vals[0].privKey, nil)
	t.Cleanup(func() { dag.Close() })
	setEqualStake(dag, vals, 25)
	disableTxAuth(dag)

	// Enter the transition, then move the production round well past the
	// transition+buffer window so full quorum is allowed to latch on commit.
	dag.enterTransition(0)
	dag.round.Store(10000)

	if dag.FullQuorumAchieved() {
		t.Fatal("full quorum latched before any strict certification")
	}

	spine := buildTaggedSpine(t, vals, 3) // rounds 0-2 commit via direct certification
	ingest(dag, spine, forwardOrder(len(spine)))

	dag.checkCommits()

	if !dag.FullQuorumAchieved() {
		t.Fatal("full quorum did not latch after a strict direct certification")
	}
}

// TestCheckCommits_PersistsCommitCursorAcrossRestart verifies ONLY that the commit
// cursor survives a restart: a node re-opened over the same storage loads the
// persisted cursor instead of resetting to zero and re-deriving decided rounds. It
// deliberately does not drive a post-restart commit — that resume is proven, within
// epoch 0, by TestCheckCommits_ResumesDecidingWithinEpochZeroAfterRestart.
//
// Scope: this test covers only cursor persistence within epoch 0. The epoch state
// (currentEpoch and the epochHolders / prevEpochHolders / nextEpochHolders snapshots)
// is now restored on reopen too (Task 0.5a); a restart past the first epoch boundary
// resuming without wedge or genesis fallback is covered by
// TestRestartResumesPastEpochBoundary.
func TestCheckCommits_PersistsCommitCursorAcrossRestart(t *testing.T) {
	db := newTestStorage(t)
	vals, _ := newTestValidatorSet(4)
	_, vs := reuseSet(vals)

	dag := New(db, vs, nil, testSystemPod, 0, vals[0].privKey, nil)
	setEqualStake(dag, vals, 25)
	disableTxAuth(dag)

	spine := buildTaggedSpine(t, vals, 3)
	ingest(dag, spine, forwardOrder(len(spine)))
	dag.checkCommits()

	cursor := dag.LastCommittedRound()
	if cursor == 0 {
		t.Fatal("nothing decided; cannot test cursor persistence")
	}
	dag.Close()

	_, vs2 := reuseSet(vals)
	dag2 := New(db, vs2, nil, testSystemPod, 0, vals[0].privKey, nil)
	t.Cleanup(func() { dag2.Close() })

	if got := dag2.LastCommittedRound(); got != cursor {
		t.Fatalf("restart re-derived the cursor: got %d, want persisted %d", got, cursor)
	}
}

// TestCheckCommits_ResumesDecidingWithinEpochZeroAfterRestart proves the positive
// half of the restart story that holds today: a node re-opened over the same storage
// with a persisted cursor still inside epoch 0 (before any boundary) is not wedged.
// Fed a fresh continuation of the DAG it advances the cursor and applies the new
// rounds — the reopen resumes deciding, not merely loads the cursor. The pre-restart
// node stops with rounds 3..4 undelivered; after reopen those rounds arrive and the
// commit loop moves the cursor past where it stopped.
//
// It proves resume within epoch 0. A restart PAST the first epoch boundary — where
// currentEpoch and the holder snapshots must be restored so the node does not wedge
// or fall back to the genesis live set — is covered by
// TestRestartResumesPastEpochBoundary (Task 0.5a).
func TestCheckCommits_ResumesDecidingWithinEpochZeroAfterRestart(t *testing.T) {
	db := newTestStorage(t)
	vals, _ := newTestValidatorSet(4)
	_, vs := reuseSet(vals)

	dag := New(db, vs, nil, testSystemPod, 0, vals[0].privKey, nil)
	setEqualStake(dag, vals, 25)
	disableTxAuth(dag)

	// Deliver only rounds 0..2 before the crash: the node commits through its cursor
	// and stops, leaving rounds 3..4 for after the restart.
	spine := buildTaggedSpine(t, vals, 4)
	cut := 3 * len(vals) // rounds 0..2 arrive before the crash; rounds 3..4 after.
	ingest(dag, spine[:cut], forwardOrder(cut))
	dag.checkCommits()

	cursor := dag.LastCommittedRound()
	if cursor == 0 {
		t.Fatal("nothing decided before the restart; cannot test resume")
	}
	dag.Close()

	// Re-open over the same storage: the cursor is restored, the in-memory graph is
	// not, so the reopened node must decide from freshly delivered rounds.
	_, vs2 := reuseSet(vals)
	dag2 := New(db, vs2, nil, testSystemPod, 0, vals[0].privKey, nil)
	t.Cleanup(func() { dag2.Close() })
	setEqualStake(dag2, vals, 25)
	disableTxAuth(dag2)

	ingest(dag2, spine[cut:], forwardOrder(len(spine)-cut))
	dag2.checkCommits()

	if got := dag2.LastCommittedRound(); got <= cursor {
		t.Fatalf("restart did not resume deciding: cursor stuck at %d (was %d)", got, cursor)
	}
	if committed := drainCommitted(dag2); len(committed) == 0 {
		t.Fatal("restart advanced the cursor but applied nothing; resume decided no batch")
	}
}

// TestCheckCommits_DecidedEpochTailNotReDerivedAfterChurn is the resolve-before-
// transition regression: an epoch-tail round is decided via the one-epoch-ahead
// holder proxy, the boundary transition then churns the holder set, and a restart
// must resume PAST the decided tail from the persisted cursor — never re-derive it
// against the churned holders, which could answer differently and fork the log.
//
// The persisted cursor is what carries the reopened node strictly past the decided
// tail, so it is never re-derived — the guarantee this test pins, independent of the
// holder set. The reopened node's epoch state is now also restored (Task 0.5a), so a
// post-boundary node can itself resume deciding; that resume is covered by
// TestRestartResumesPastEpochBoundary.
func TestCheckCommits_DecidedEpochTailNotReDerivedAfterChurn(t *testing.T) {
	db := newTestStorage(t)
	vals, _ := newTestValidatorSet(4)
	_, vs := reuseSet(vals)

	dag := New(db, vs, nil, testSystemPod, 0, vals[0].privKey, nil, WithEpochLength(4))
	setEqualStake(dag, vals, 25)

	// Simulate the state after the first epoch transition: epoch-1 holders frozen and
	// a one-epoch-ahead proxy frozen, so round-9 producers (epoch 2) can be weighed.
	dag.currentEpoch = 1
	dag.epochHolders = snapshotOf(dag.validators)
	dag.nextEpochHolders = snapshotOf(dag.validators)

	// Round 8 is the epoch-1 tail (2*epochLength); it is undecided directly and
	// resolves through a round-9 (epoch 2) certified anchor weighed via the proxy.
	const tail = 8
	v, anchor := buildSplitRound(t, dag, vals, tail, true)
	certifyAnchor(t, dag, vals, tail, anchor)
	_ = v

	// Churn: a pending removal so the boundary transition changes the holder set,
	// making a later re-derivation of the tail potentially divergent.
	dag.pendingRemovals[vals[3].pubKey] = true

	// Decide from the tail: the loop resolves round 8 via the proxy, advances and
	// persists the cursor, then transitions the epoch at the boundary.
	dag.lastCommitted = tail
	dag.checkCommits()

	if dag.Epoch() != 2 {
		t.Fatalf("epoch boundary did not transition via the commit path: epoch=%d", dag.Epoch())
	}
	if dag.LastCommittedRound() <= tail {
		t.Fatalf("cursor did not advance past the decided tail: cursor=%d", dag.LastCommittedRound())
	}
	persisted := dag.LastCommittedRound()
	dag.Close()

	// Re-open over the same storage with a clean epoch state: the churned holders
	// would re-derive round 8 differently, but the persisted cursor resumes past it.
	_, vs2 := reuseSet(vals)
	dag2 := New(db, vs2, nil, testSystemPod, 0, vals[0].privKey, nil, WithEpochLength(4))
	t.Cleanup(func() { dag2.Close() })

	if got := dag2.LastCommittedRound(); got != persisted {
		t.Fatalf("restart re-derived the decided tail: cursor=%d, want persisted %d", got, persisted)
	}
	if got := dag2.LastCommittedRound(); got <= tail {
		t.Fatalf("restart resumed at or before the decided tail %d: cursor=%d", tail, got)
	}
}
