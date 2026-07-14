package consensus

import (
	"bytes"
	"crypto/ed25519"
	"encoding/binary"
	"fmt"
	"math/rand"
	"testing"
	"time"

	flatbuffers "github.com/google/flatbuffers/go"

	"BluePods/internal/types"
)

// Divergence regression: the deterministic anchor commit rule (Tasks 0.1-0.6) must
// produce a byte-identical committed log and an identical object tracker on every
// node, no matter the order vertices arrive in. These property-style tests deliver a
// generated DAG to several observer nodes under independent per-node delivery
// permutations (with one vertex per round held back to a late wave) and assert the
// nodes converge. Delivery runs through AddVertex, so a vertex whose parents are not
// yet local is buffered and re-materialised in causal order exactly as gossip would
// deliver it — a node never holds a child before its parents, which is the invariant
// that keeps a transiently-absent anchor from being spuriously blamed.

const (
	// divValidators is the committee size for the divergence tests.
	divValidators = 3
	// divRounds is the number of generated rounds (0..divRounds), so the loop
	// commits rounds 0..divRounds-1 (the top round has no round above to certify it).
	divRounds = 20
	// divNodes is how many independently-permuted nodes must converge each trial.
	divNodes = 3
	// divTrials is how many random per-node permutation trials to run.
	divTrials = 12
)

// objTaggedATX builds an AttestedTransaction whose inner transaction carries a unique
// nonzero hash, a unique function name derived from idx (its commit-order label), and
// one mutable_ref to a unique object at version 0. Committing it increments that
// object to version 1 in the tracker, so the tracker export is populated and the
// cross-node identical-tracker assertion is meaningful rather than empty-vs-empty.
func objTaggedATX(t *testing.T, idx int) []byte {
	t.Helper()

	builder := flatbuffers.NewBuilder(1024)

	var txHash [32]byte
	binary.LittleEndian.PutUint64(txHash[:], uint64(idx)+1)

	var objID [32]byte
	objID[0] = 0x0B // marker so object ids never collide with the zero hash
	binary.LittleEndian.PutUint64(objID[8:], uint64(idx)+1)

	refOff := buildMutableRef(builder, objID)
	types.TransactionStartMutableRefsVector(builder, 1)
	builder.PrependUOffsetT(refOff)
	mutVec := builder.EndVector(1)

	txOff := buildTaggedTx(builder, txHash, idx, mutVec)

	return finishATX(builder, txOff)
}

// buildMutableRef builds one mutable ObjectRef for objID at version 0 and returns its
// offset. The ref and its id vector are built before the enclosing transaction table.
func buildMutableRef(builder *flatbuffers.Builder, objID [32]byte) flatbuffers.UOffsetT {
	idVec := builder.CreateByteVector(objID[:])

	types.ObjectRefStart(builder)
	types.ObjectRefAddId(builder, idVec)
	types.ObjectRefAddVersion(builder, 0)

	return types.ObjectRefEnd(builder)
}

// buildTaggedTx builds the inner Transaction with the given hash, tag function name,
// and mutable-refs vector, and returns its offset.
func buildTaggedTx(builder *flatbuffers.Builder, txHash [32]byte, idx int, mutVec flatbuffers.UOffsetT) flatbuffers.UOffsetT {
	hashVec := builder.CreateByteVector(txHash[:])
	senderVec := builder.CreateByteVector(make([]byte, 32))
	podVec := builder.CreateByteVector(make([]byte, 32))
	funcNameOff := builder.CreateString(fmt.Sprintf("t%d", idx))

	types.TransactionStart(builder)
	types.TransactionAddHash(builder, hashVec)
	types.TransactionAddSender(builder, senderVec)
	types.TransactionAddPod(builder, podVec)
	types.TransactionAddFunctionName(builder, funcNameOff)
	types.TransactionAddMutableRefs(builder, mutVec)

	return types.TransactionEnd(builder)
}

// finishATX wraps a transaction offset in an empty-objects, empty-proofs
// AttestedTransaction and returns the finished buffer bytes.
func finishATX(builder *flatbuffers.Builder, txOff flatbuffers.UOffsetT) []byte {
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

// parentRef is one parent citation: the parent's hash and its producer. Unlike the
// zero-producer links the store-level test helpers emit, these carry the real
// producer so a vertex passes validateParentsQuorum on the AddVertex ingress path.
type parentRef struct {
	hash     Hash // hash is the parent vertex hash
	producer Hash // producer is the parent's producing validator
}

// genVertex builds a signed, object-tagged vertex for v at round citing parents, and
// returns it as a scenarioVertex. The parent links carry the real producer, and the
// epoch is 0 to match the observer DAGs, so the vertex survives full ingress
// validation (producer, signature, epoch, parents quorum) rather than only store.add.
func genVertex(t *testing.T, v testValidator, round uint64, parents []parentRef, idx int) scenarioVertex {
	t.Helper()

	atx := objTaggedATX(t, idx)
	unsigned := buildObjVertex(nil, v, round, parents, atx, Hash{})
	hash := hashVertex(unsigned)
	sig := ed25519.Sign(v.privKey, hash[:])
	data := buildObjVertex(sig, v, round, parents, atx, hash)

	return scenarioVertex{data: data, hash: hash, round: round, producer: v.pubKey, tag: fmt.Sprintf("t%d", idx)}
}

// buildObjVertex serialises a vertex carrying one object-tagged ATX. A nil signature
// produces the unsigned form (hash and signature fields omitted) whose bytes are
// hashed; passing the signature and hash produces the final signed form. Both passes
// lay out identical round/producer/parents/tx/epoch fields so the hash covers them.
func buildObjVertex(sig []byte, v testValidator, round uint64, parents []parentRef, atx []byte, hash Hash) []byte {
	builder := flatbuffers.NewBuilder(4096)

	atxOff := rebuildATXInBuilder(builder, atx)
	parentsVec := buildParentLinks(builder, parents)

	types.VertexStartTransactionsVector(builder, 1)
	builder.PrependUOffsetT(atxOff)
	txsVec := builder.EndVector(1)

	producerVec := builder.CreateByteVector(v.pubKey[:])
	var hashVec, sigVec flatbuffers.UOffsetT
	if sig != nil {
		hashVec = builder.CreateByteVector(hash[:])
		sigVec = builder.CreateByteVector(sig)
	}

	types.VertexStart(builder)
	if sig != nil {
		types.VertexAddHash(builder, hashVec)
		types.VertexAddSignature(builder, sigVec)
	}
	types.VertexAddRound(builder, round)
	types.VertexAddProducer(builder, producerVec)
	types.VertexAddParents(builder, parentsVec)
	types.VertexAddTransactions(builder, txsVec)
	types.VertexAddEpoch(builder, 0)
	builder.Finish(types.VertexEnd(builder))

	return builder.FinishedBytes()
}

// buildParentLinks builds the parents vector, each link carrying the parent's hash and
// its real producer, and returns the vector offset.
func buildParentLinks(builder *flatbuffers.Builder, parents []parentRef) flatbuffers.UOffsetT {
	offsets := make([]flatbuffers.UOffsetT, len(parents))
	for i, p := range parents {
		hVec := builder.CreateByteVector(p.hash[:])
		pVec := builder.CreateByteVector(p.producer[:])

		types.VertexLinkStart(builder)
		types.VertexLinkAddHash(builder, hVec)
		types.VertexLinkAddProducer(builder, pVec)
		offsets[i] = types.VertexLinkEnd(builder)
	}

	types.VertexStartParentsVector(builder, len(offsets))
	for i := len(offsets) - 1; i >= 0; i-- {
		builder.PrependUOffsetT(offsets[i])
	}

	return builder.EndVector(len(offsets))
}

// buildObjSpine builds a fully connected object-tagged DAG over rounds 0..maxRound:
// every validator produces one vertex per round citing all vertices of the previous
// round. Vertices are returned in round-major, validator-minor order, so round r's
// vertices occupy indices [r*len(vals), r*len(vals)+len(vals)).
func buildObjSpine(t *testing.T, vals []testValidator, maxRound uint64) []scenarioVertex {
	t.Helper()

	var out []scenarioVertex
	var prev []parentRef
	idx := 0

	for r := uint64(0); r <= maxRound; r++ {
		var cur []parentRef
		for _, v := range vals {
			sv := genVertex(t, v, r, prev, idx)
			out = append(out, sv)
			cur = append(cur, parentRef{hash: sv.hash, producer: v.pubKey})
			idx++
		}
		prev = cur
	}

	return out
}

// newObserverDAG builds a non-producing observer over the committee: its identity key
// is not in the validator set, so no path (vertex-triggered or the liveness loop) ever
// self-produces and pollutes the DAG under test. The committee's stake is set equal and
// the genesis snapshot frozen, so the anchor rule resolves epoch 0 deterministically.
func newObserverDAG(t *testing.T, vals []testValidator, opts ...Option) *DAG {
	t.Helper()

	dag := newUnfrozenObserver(t, vals, opts...)
	setEqualStake(dag, vals, 25)

	return dag
}

// newUnfrozenObserver builds an observer whose genesis snapshot is NOT yet frozen, so
// HoldersForEpoch(0) is unresolved and the background commit loop is a no-op. Delivery
// runs against this quiet DAG; the caller freezes the genesis (freezeGenesis) only once
// the full DAG is local, so the commit rule is exercised on a causally-closed store and
// never on a transient store where a child was ingested ahead of its parent. Stake is
// set but the freeze is deferred to the caller.
func newUnfrozenObserver(t *testing.T, vals []testValidator, opts ...Option) *DAG {
	t.Helper()

	observer := newTestValidator()
	dag := New(newTestStorage(t), NewValidatorSet(keysOf(vals)), nil, testSystemPod, 0, observer.privKey, nil, opts...)
	t.Cleanup(func() { dag.Close() })

	for _, v := range vals {
		dag.validators.SetSelfStake(v.pubKey, 25)
	}
	disableTxAuth(dag)

	return dag
}

// settleDrain drives the commit rule to completion and captures the whole committed
// log. It sweeps and drains repeatedly until the log stops growing, so an emission the
// background loop and the explicit sweep race to produce is still captured in commit
// order (the channel is FIFO and commits are serialised under commitMu).
func settleDrain(dag *DAG) []CommittedTx {
	var log []CommittedTx
	stable := 0

	for iter := 0; iter < 200; iter++ {
		dag.checkCommits()
		batch := drainLog(dag)
		log = append(log, batch...)

		if len(batch) == 0 {
			if stable++; stable >= 3 {
				break
			}
			time.Sleep(time.Millisecond)
			continue
		}
		stable = 0
	}

	return log
}

// drainLog returns every CommittedTx currently queued on the DAG's committed channel,
// in commit order.
func drainLog(dag *DAG) []CommittedTx {
	var out []CommittedTx
	for {
		select {
		case c := <-dag.committed:
			out = append(out, c)
		default:
			return out
		}
	}
}

// encodeLog canonically serialises a committed log to bytes for a byte-identical
// comparison: for each entry the tx hash, a success byte, and the function name.
func encodeLog(log []CommittedTx) []byte {
	var buf bytes.Buffer
	for _, c := range log {
		buf.Write(c.Hash[:])
		if c.Success {
			buf.WriteByte(1)
		} else {
			buf.WriteByte(0)
		}
		buf.WriteString(c.Function)
		buf.WriteByte(0)
	}

	return buf.Bytes()
}

// encodeTracker canonically serialises a tracker export to bytes. The export iterates
// the object-id key prefix, so entries are already in ascending id order.
func encodeTracker(entries []ObjectTrackerEntry) []byte {
	var buf bytes.Buffer
	for _, e := range entries {
		buf.Write(e.ID[:])
		_ = binary.Write(&buf, binary.LittleEndian, e.Version)
		_ = binary.Write(&buf, binary.LittleEndian, e.Replication)
		_ = binary.Write(&buf, binary.LittleEndian, e.Fees)
	}

	return buf.Bytes()
}

// tags returns the function-name labels of a committed log, for readable diagnostics.
func tags(log []CommittedTx) []string {
	out := make([]string, len(log))
	for i, c := range log {
		out[i] = c.Function
	}

	return out
}

// pickDelayed selects one vertex index per round to hold back to the late delivery
// wave, chosen randomly within each round from the round-major spine layout.
func pickDelayed(rng *rand.Rand, rounds int, valCount int) map[int]bool {
	delayed := make(map[int]bool, rounds)
	for r := 0; r < rounds; r++ {
		delayed[r*valCount+rng.Intn(valCount)] = true
	}

	return delayed
}

// deliverPermuted feeds the spine to dag under a fresh random permutation: every
// non-delayed vertex first (in permuted order), then the one-per-round delayed vertices
// (also permuted). AddVertex buffers any vertex delivered before its parents, so the
// permutation need not be causal; once both waves are in, the store holds the complete
// DAG. No commit is driven here — the caller freezes the genesis and commits afterwards.
func deliverPermuted(dag *DAG, spine []scenarioVertex, rng *rand.Rand, rounds, valCount int) {
	delayed := pickDelayed(rng, rounds, valCount)
	order := rng.Perm(len(spine))

	var wave1, wave2 []int
	for _, i := range order {
		if delayed[i] {
			wave2 = append(wave2, i)
		} else {
			wave1 = append(wave1, i)
		}
	}

	for _, i := range wave1 {
		dag.AddVertex(spine[i].data)
	}
	for _, i := range wave2 {
		dag.AddVertex(spine[i].data)
	}
}

// TestDivergence_IdenticalLogsUnderDeliveryPermutations is the core divergence
// regression: several nodes each ingest the same generated 3-validator, 20-round DAG
// under an independent random delivery permutation with one delayed vertex per round,
// and must end with byte-identical committed logs AND identical object trackers. A
// view-dependent rule (arrival-order commit, lowest-arrived-hash anchor) would fork
// two nodes here; the vote-determined causal-batch rule keeps them identical.
func TestDivergence_IdenticalLogsUnderDeliveryPermutations(t *testing.T) {
	vals, _ := newTestValidatorSet(divValidators)
	spine := buildObjSpine(t, vals, divRounds)

	for trial := 0; trial < divTrials; trial++ {
		seed := int64(1000 + trial)
		runPermutationTrial(t, vals, spine, seed)
	}
}

// runPermutationTrial builds divNodes observer nodes, delivers the spine to each under
// a distinct per-node permutation seeded from the trial seed, and asserts every node's
// committed log and tracker match the first node's byte-for-byte.
func runPermutationTrial(t *testing.T, vals []testValidator, spine []scenarioVertex, seed int64) {
	t.Helper()

	logs := make([][]byte, divNodes)
	trackers := make([][]byte, divNodes)
	var refLog []CommittedTx

	for node := 0; node < divNodes; node++ {
		dag := newUnfrozenObserver(t, vals)
		rng := rand.New(rand.NewSource(seed*100 + int64(node)))

		deliverPermuted(dag, spine, rng, divRounds+1, divValidators)
		freezeGenesis(dag) // the full DAG is local; now let the commit rule run

		log := settleDrain(dag)
		logs[node] = encodeLog(log)
		trackers[node] = encodeTracker(dag.ExportTrackerEntries())
		if node == 0 {
			refLog = log
		}
	}

	assertConverged(t, seed, logs, trackers, refLog)
}

// assertConverged fails with the reproducing seed if any node's committed log or
// tracker export differs from node 0's, or if nothing was committed.
func assertConverged(t *testing.T, seed int64, logs, trackers [][]byte, refLog []CommittedTx) {
	t.Helper()

	if len(refLog) == 0 {
		t.Fatalf("seed %d: node 0 committed nothing; scenario did not exercise the commit path", seed)
	}

	for node := 1; node < len(logs); node++ {
		if !bytes.Equal(logs[0], logs[node]) {
			t.Fatalf("seed %d: committed logs diverge (node 0 vs %d)\n node0=%v\n node%d=%v",
				seed, node, tags(refLog), node, node)
		}
		if !bytes.Equal(trackers[0], trackers[node]) {
			t.Fatalf("seed %d: tracker exports diverge (node 0 vs %d)", seed, node)
		}
	}
}

// concat returns a fresh slice of a followed by b, never aliasing either input's
// backing array (so two deliveries built from a shared base stay independent).
func concat(a, b []scenarioVertex) []scenarioVertex {
	out := make([]scenarioVertex, 0, len(a)+len(b))
	out = append(out, a...)
	out = append(out, b...)

	return out
}

// ref makes a parentRef from a scenario vertex.
func ref(sv scenarioVertex) parentRef {
	return parentRef{hash: sv.hash, producer: sv.producer}
}

// refsOf makes a parentRef slice from scenario vertices.
func refsOf(svs ...scenarioVertex) []parentRef {
	out := make([]parentRef, len(svs))
	for i, sv := range svs {
		out[i] = ref(sv)
	}

	return out
}

// higherLower returns the two vertices ordered by hash: win is the HIGHER-hash vertex,
// decoy the lower. The scenarios make the higher-hash vertex the vote-determined
// winner, so a lowest-arrived-hash rule (the reverted first attempt's bug) would
// wrongly prefer the lower-hash decoy — the exact fork these tests rule out.
func higherLower(a, b scenarioVertex) (win, decoy scenarioVertex) {
	if bytes.Compare(a.hash[:], b.hash[:]) > 0 {
		return a, b
	}

	return b, a
}

// equivScenario is a built round-0..2 equivocation DAG. The designated round-1
// producer equivocates (win, decoy); phaseWin/phaseDecoy are the two delivery groups
// so a test can feed one half first to one node and the other half first to another.
type equivScenario struct {
	phaseWin   []scenarioVertex // phaseWin carries the winner and the round-2 vertices certifying it
	phaseDecoy []scenarioVertex // phaseDecoy carries the decoy (and, when strict, its lone citer)
	base       []scenarioVertex // base is round 0 plus the round-1 non-anchor vertices
	winTag     string           // winTag is the winner's commit label
	decoyTag   string           // decoyTag is the decoy's commit label
}

// buildEquivScenario builds the equivocation DAG on ref's committee. The round-1
// designated producer produces two vertices; the winner is the higher-hash one. In the
// strict regime two of three round-2 validators cite the winner (a 2/3 quorum) and one
// cites the decoy; in the relaxed regime every round-2 validator cites the winner and
// the decoy is left uncited. Either way the winner is the one the citations determine.
func buildEquivScenario(t *testing.T, refDAG *DAG, vals []testValidator, relaxed bool) equivScenario {
	t.Helper()

	r0 := []scenarioVertex{
		genVertex(t, vals[0], 0, nil, 0),
		genVertex(t, vals[1], 0, nil, 1),
		genVertex(t, vals[2], 0, nil, 2),
	}
	r0refs := refsOf(r0...)

	pE := designatedProducer(t, refDAG, vals, 1)
	rest := others(vals, pE)

	va := genVertex(t, pE, 1, r0refs, 100)
	vb := genVertex(t, pE, 1, r0refs, 101)
	win, decoy := higherLower(va, vb)
	o1 := genVertex(t, rest[0], 1, r0refs, 102)
	o2 := genVertex(t, rest[1], 1, r0refs, 103)

	sc := equivScenario{base: append(r0, o1, o2), winTag: win.tag, decoyTag: decoy.tag}
	sc.buildRound2(t, vals, win, decoy, o1, o2, relaxed)

	return sc
}

// buildRound2 builds the round-2 citers and files them into the winner/decoy delivery
// phases. Strict splits the three validators 2-1 (winner-decoy); relaxed points all
// three at the winner and leaves the decoy uncited.
func (sc *equivScenario) buildRound2(t *testing.T, vals []testValidator, win, decoy, o1, o2 scenarioVertex, relaxed bool) {
	winRefs := refsOf(win, o1, o2)
	sc.phaseWin = []scenarioVertex{win}
	sc.phaseDecoy = []scenarioVertex{decoy}

	if relaxed {
		for i := 0; i < 3; i++ {
			sc.phaseWin = append(sc.phaseWin, genVertex(t, vals[i], 2, winRefs, 200+i))
		}
		return
	}

	sc.phaseWin = append(sc.phaseWin,
		genVertex(t, vals[0], 2, winRefs, 200),
		genVertex(t, vals[1], 2, winRefs, 201))
	sc.phaseDecoy = append(sc.phaseDecoy, genVertex(t, vals[2], 2, refsOf(decoy, o1, o2), 202))
}

// runEquivNode builds a fresh observer node, delivers first then second in two waves
// with a commit sweep between, and returns the committed log plus the commit cursor
// observed after the first wave. The mid cursor proves whether the node committed the
// equivocation round prematurely on a partial view.
func runEquivNode(t *testing.T, vals []testValidator, opts []Option, first, second []scenarioVertex) ([]CommittedTx, uint64) {
	t.Helper()

	dag := newObserverDAG(t, vals, opts...)

	for _, sv := range first {
		dag.AddVertex(sv.data)
	}
	dag.checkCommits()
	midCursor := dag.LastCommittedRound()

	for _, sv := range second {
		dag.AddVertex(sv.data)
	}
	dag.checkCommits()

	return drainLog(dag), midCursor
}

// assertEquivConverged checks both nodes committed the byte-identical log, that log
// commits the vote-determined winner and never the decoy, and that neither node
// committed the equivocation round (round 1) before its view was complete.
func assertEquivConverged(t *testing.T, winLog, decoyLog []CommittedTx, midWin, midDecoy uint64, winTag, decoyTag string) {
	t.Helper()

	if !bytes.Equal(encodeLog(winLog), encodeLog(decoyLog)) {
		t.Fatalf("equivocation forked the committed log:\n winner-first=%v\n decoy-first=%v", tags(winLog), tags(decoyLog))
	}
	if !contains(tags(winLog), winTag) {
		t.Fatalf("vote-determined winner %s was not committed: %v", winTag, tags(winLog))
	}
	if contains(tags(winLog), decoyTag) {
		t.Fatalf("decoy %s (lower hash) was committed; the rule picked a view-dependent candidate: %v", decoyTag, tags(winLog))
	}
	if midWin < 2 {
		t.Fatalf("winner-first node did not commit the equivocation round from the citing quorum: mid cursor %d", midWin)
	}
	if midDecoy > 1 {
		t.Fatalf("decoy-first node committed round 1 on a partial view (mid cursor %d); the decoy could fork the log", midDecoy)
	}
}

// TestDivergence_StrictAnchorEquivocationConverges is the strict-regime equivocation
// case the 0.5b review asked for: the designated anchor producer publishes two round-1
// vertices, and the two halves of the network see them in opposite orders. Two of the
// three next-round validators cite the higher-hash vertex (a 2/3 quorum), one cites the
// lower-hash decoy. Both nodes commit the quorum-certified winner, never the
// lowest-arrived decoy, and the decoy-first node waits rather than committing prematurely.
func TestDivergence_StrictAnchorEquivocationConverges(t *testing.T) {
	vals, _ := newTestValidatorSet(divValidators)
	refDAG := newObserverDAG(t, vals)
	if refDAG.roundIsRelaxed(1) {
		t.Fatal("round 1 must be strict for this case (minValidators 0)")
	}

	sc := buildEquivScenario(t, refDAG, vals, false)

	winLog, midWin := runEquivNode(t, vals, nil, concat(sc.base, sc.phaseWin), sc.phaseDecoy)
	decoyLog, midDecoy := runEquivNode(t, vals, nil, concat(sc.base, sc.phaseDecoy), sc.phaseWin)

	assertEquivConverged(t, winLog, decoyLog, midWin, midDecoy, sc.winTag, sc.decoyTag)
}

// TestDivergence_RelaxedAnchorEquivocationConverges is the relaxed-regime companion:
// during the unlatched bootstrap window the designated producer equivocates, every
// next-round validator builds on the higher-hash vertex, and the lower-hash decoy is
// left uncited. Under the relaxed single-supporter certificate a lowest-arrived-hash
// rule would latch the decoy on the half that received it first; the vote-determined
// rule commits the cited winner on both halves.
func TestDivergence_RelaxedAnchorEquivocationConverges(t *testing.T) {
	vals, _ := newTestValidatorSet(divValidators)
	opts := []Option{WithMinValidators(divValidators)}
	refDAG := newObserverDAG(t, vals, opts...)
	if !refDAG.roundIsRelaxed(1) {
		t.Fatal("round 1 must be relaxed for this case (unlatched bootstrap)")
	}

	sc := buildEquivScenario(t, refDAG, vals, true)

	winLog, midWin := runEquivNode(t, vals, opts, concat(sc.base, sc.phaseWin), sc.phaseDecoy)
	decoyLog, midDecoy := runEquivNode(t, vals, opts, concat(sc.base, sc.phaseDecoy), sc.phaseWin)

	assertEquivConverged(t, winLog, decoyLog, midWin, midDecoy, sc.winTag, sc.decoyTag)
}
