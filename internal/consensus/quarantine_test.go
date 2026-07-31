package consensus

import (
	"bytes"
	"crypto/ed25519"
	"encoding/hex"
	"os"
	"sync"
	"testing"

	"github.com/zeebo/blake3"

	"BluePods/internal/events"
	"BluePods/internal/index"
	"BluePods/internal/storage"
	"BluePods/internal/types"
)

// wedge is the partition-lever shape quarantine exists to defuse, built out of
// real signed vertices and real ingress.
//
//	L  a vertex anchoring a root the AHEAD node has itself committed and can
//	   therefore disprove, carrying one transaction so the batch it lands in has
//	   observable execution.
//	C  a child referencing L, produced by a LAGGARD that has no index and could
//	   not disprove anything — the honest reference spec §5 says suffices to
//	   smuggle a lie into committed causal history.
//
// Rounds 0 and 1 are the smallest shape that reaches ingress honestly: L needs
// no parents at round 0, and C's single parent link then carries L's real
// producer, which is what makes the ahead node's own validateParentLink refuse
// to admit C while L is absent. The anchored frontier is independent of the
// round, so L can anchor receiverFrontier from round 0.
type wedge struct {
	ahead     *DAG   // ahead has committed the frontier L lies about
	laggard   *DAG   // laggard has no index wired and can disprove nothing
	committed Hash   // committed is the ahead node's own root at that frontier
	lieData   []byte // lieData is L's serialized bytes, as they arrive over gossip
	lie       Hash   // lie is L's identity
	childData []byte // childData is C's serialized bytes
	child     Hash   // child is C's identity
	stopAhead func() // stopAhead closes the ahead node, safe to call more than once
}

// newWedge assembles the shape: the lie is built, the laggard accepts it and
// produces the reference, and nothing has touched the ahead node yet.
func newWedge(t *testing.T, aheadDB *storage.Storage) *wedge {
	t.Helper()

	validators, vs := newTestValidatorSet(3)

	ahead, stopAhead := newIndexedNode(t, aheadDB, vs, validators[0], indexAtReceiverFrontier(t))
	committed, ok := ahead.indexer.RootAt(receiverFrontier)
	if !ok {
		t.Fatalf("the ahead node retains no root at its own committed frontier %d", receiverFrontier)
	}

	wrong := committed
	wrong[0] ^= 0xFF

	lieData, lie := newAnchoredVertices(t, validators[1])(0, receiverFrontier, wrong, nil, taggedATX(t, 1))

	laggard := New(newTestStorage(t), vs, nil, testSystemPod, 0, validators[2].privKey, nil)
	t.Cleanup(func() { laggard.Close() })
	disableTxAuth(laggard)

	if !laggard.AddVertex(lieData) {
		t.Fatal("the laggard must accept L: with no index wired it can disprove nothing")
	}

	childData := laggard.buildVertex(1, []Hash{lie}, nil)
	child := hashFrom(types.GetRootAsVertex(childData, 0).HashBytes())

	if !laggard.AddVertex(childData) {
		t.Fatal("the laggard must accept its own reference to L")
	}

	disableTxAuth(ahead)

	return &wedge{
		ahead:     ahead,
		laggard:   laggard,
		committed: committed,
		lieData:   lieData,
		lie:       lie,
		childData: childData,
		child:     child,
		stopAhead: stopAhead,
	}
}

// indexAtReceiverFrontier builds an index manager that has committed
// receiverFrontier, the history an ahead node checks incoming anchors against.
func indexAtReceiverFrontier(t *testing.T) *index.Manager {
	t.Helper()

	mgr := index.NewManager()
	mgr.ApplyEdge([32]byte{0x11}, index.KeyRootKind, [32]byte{0x01})
	mgr.SetFrontier(receiverFrontier)

	return mgr
}

// newIndexedNode builds a DAG over the given storage and wires it to an index,
// the way a node that has caught up runs. The returned stop closes it and is
// safe to call more than once, so a restart test can release the storage lock
// early and still leave the cleanup registered.
func newIndexedNode(t *testing.T, db *storage.Storage, vs *ValidatorSet, key testValidator, mgr *index.Manager) (*DAG, func()) {
	t.Helper()

	dag := New(db, vs, nil, testSystemPod, 0, key.privKey, nil)
	dag.SetIndexer(mgr)

	var once sync.Once
	stop := func() { once.Do(dag.Close) }
	t.Cleanup(stop)

	return dag, stop
}

// TestQuarantine_WedgedCausalBatchCompletes is the regression this fix exists
// for. Terminal ingress rejection made a proven lie unstorable, and a node that
// cannot store a vertex cannot complete any causal batch containing it: the
// commit loop's walk aborts on the absent vertex, the fetcher re-requests it,
// ingress refuses it again, and the cursor never moves — one byzantine producer
// plus one honest laggard partitions the validator set, evidence-free.
//
// Quarantine keeps the vertex out of the two places it could do harm (it is
// never relayed and never referenced) and puts it in the one place liveness
// needs it: the store. The batch then completes, the transactions L carried
// execute exactly as they do on the nodes that could not disprove it, and the
// commit path convicts its producer with his own signature.
func TestQuarantine_WedgedCausalBatchCompletes(t *testing.T) {
	w := newWedge(t, newTestStorage(t))

	buf := captureEvents(t)

	// Ingress, in arrival order: the lie first, then the reference to it.
	if w.ahead.AddVertex(w.lieData) {
		t.Fatal("a proven liar must never be relayed: AddVertex must report it as not-for-relay")
	}

	if !w.ahead.store.has(w.lie) {
		t.Fatal("wedge: the ahead node did not store the vertex it disproved, so no causal batch containing it can ever complete")
	}

	if !w.ahead.AddVertex(w.childData) {
		t.Fatal("the honest child referencing a quarantined parent must be admitted normally")
	}

	// The refetch loop that never terminated: the commit loop asks a peer for the
	// missing ancestor and hands it back through the same door.
	if w.ahead.AddVertex(w.lieData) {
		t.Fatal("a re-delivered quarantined vertex must still not be relayed")
	}
	if !w.ahead.store.has(w.lie) {
		t.Fatal("the re-delivered vertex must still be held")
	}

	// Served on request: a peer asking for the hash gets the bytes, so the
	// quarantining node does not become a hole in the mesh's fetch topology.
	if w.ahead.VertexBytes(w.lie) == nil {
		t.Fatal("a quarantined vertex must still be served to a peer that asks for it by hash")
	}

	// Never referenced: production still refuses to build on it.
	if containsHash(w.ahead.collectParents(1), w.lie) {
		t.Fatal("a quarantined vertex must never be referenced by this node's production")
	}

	batch, ok := w.ahead.store.causalBatch(w.child)
	if !ok {
		t.Fatal("wedge: the causal batch of an anchor whose history contains a quarantined vertex must complete")
	}

	want := []Hash{w.lie, w.child}
	if len(batch) != len(want) || batch[0] != want[0] || batch[1] != want[1] {
		t.Fatalf("causal batch = %v, want [L C]", batch)
	}

	w.ahead.commitMu.Lock()
	w.ahead.applyBatch(1, batch)
	w.ahead.commitMu.Unlock()

	assertOneAnchorFault(t, w, buf)
	aheadTxs := eventsNamed(t, buf, events.EvTxCommitted)

	// The transactions L carried executed here exactly as they do on the laggard
	// that never disproved it. Divergence here would be the fork quarantine
	// exists to prevent: the same committed history, two different executions.
	laggardBuf := captureEvents(t)

	w.laggard.commitMu.Lock()
	w.laggard.applyBatch(1, batch)
	w.laggard.commitMu.Unlock()

	laggardTxs := eventsNamed(t, laggardBuf, events.EvTxCommitted)

	assertSameTxOutcomes(t, aheadTxs, laggardTxs, w.lie)

	if faults := storedFaults(t, w.laggard); len(faults) != 0 {
		t.Fatalf("the laggard can prove nothing and must record no fault, got %d", len(faults))
	}
}

// assertOneAnchorFault checks the conviction the quarantining node now reaches:
// exactly one fault record, keyed by the lying vertex, with evidence a third
// party verifies from the stored bytes alone, and exactly one event.
func assertOneAnchorFault(t *testing.T, w *wedge, buf *bytes.Buffer) {
	t.Helper()

	faults := storedFaults(t, w.ahead)
	if len(faults) != 1 {
		t.Fatalf("want exactly 1 fault record, got %d", len(faults))
	}

	evidence, ok := faults[w.lie]
	if !ok {
		t.Fatalf("the fault record is not keyed by the quarantined vertex %x", w.lie[:8])
	}

	assertEvidenceVerifies(t, evidence, w.committed)

	recs := eventsNamed(t, buf, events.EvAnchorFault)
	if len(recs) != 1 {
		t.Fatalf("want exactly 1 %s event, got %d", events.EvAnchorFault, len(recs))
	}
}

// assertSameTxOutcomes checks that both nodes reported the identical committed
// transaction outcomes for the batch, all of them carried by the quarantined
// vertex.
func assertSameTxOutcomes(t *testing.T, ahead, laggard []map[string]any, carrier Hash) {
	t.Helper()

	if len(ahead) != 1 {
		t.Fatalf("want 1 %s event on the quarantining node, got %d", events.EvTxCommitted, len(ahead))
	}

	if len(laggard) != len(ahead) {
		t.Fatalf("the laggard reported %d committed transactions, the quarantining node %d", len(laggard), len(ahead))
	}

	for _, key := range []string{"tx", "vertex", "round", "success", "reason"} {
		if ahead[0][key] != laggard[0][key] {
			t.Fatalf("%s diverged: quarantining node %v, laggard %v", key, ahead[0][key], laggard[0][key])
		}
	}

	if ahead[0]["vertex"] != hex.EncodeToString(carrier[:]) {
		t.Fatalf("the committed transaction names vertex %v, want the quarantined vertex", ahead[0]["vertex"])
	}

	if ahead[0]["success"] != true {
		t.Fatalf("the quarantined vertex's transaction reported success %v: a quarantined vertex's transactions execute like any other", ahead[0]["success"])
	}
}

// assertEvidenceVerifies reproduces the conviction the way a third party will,
// from the stored bytes alone: the identity is BLAKE3(0x01 || the first 120
// bytes), the producer's signature is checked over it, and the claimed root is
// read out of the same header at the normative offset. Re-encoding the record
// through this package's own header struct would prove only that the encoder
// round-trips.
func assertEvidenceVerifies(t *testing.T, evidence []byte, computed Hash) {
	t.Helper()

	if len(evidence) != faultRecordSize {
		t.Fatalf("evidence is %d bytes, want %d", len(evidence), faultRecordSize)
	}

	digest := blake3.New()
	_, _ = digest.Write([]byte{0x01})
	_, _ = digest.Write(evidence[:headerSize])

	var identity Hash
	copy(identity[:], digest.Sum(nil))

	producer := evidence[:32]
	signature := evidence[headerSize : headerSize+ed25519.SignatureSize]

	if !ed25519.Verify(producer, identity[:], signature) {
		t.Fatal("the producer's signature does not verify over BLAKE3(0x01 || header) taken from the stored bytes")
	}

	claimed := hashFrom(evidence[56:88])
	if claimed == computed {
		t.Fatal("evidence that exhibits no mismatch convicts nobody")
	}

	if got := hashFrom(evidence[headerSize+ed25519.SignatureSize:]); got != computed {
		t.Fatalf("evidence computes root %x, want this node's own root %x", got[:8], computed[:8])
	}
}

// TestQuarantine_MarkSurvivesRestart pins the reason the verdict is persisted
// rather than re-derived on read. A restarted node rebuilds its index from
// committed state, and that rebuild restores no round history at all — so
// anchorLie, asked again about the same vertex, answers "unverifiable" and the
// vertex this node once proved wrong would become referenceable and relayable.
// The stored mark is what makes the verdict outlive the history that proved it,
// while the vertex itself stays in the store so causal batches still complete.
func TestQuarantine_MarkSurvivesRestart(t *testing.T) {
	dir, err := os.MkdirTemp("", "consensus_quarantine_restart_*")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })

	db, err := storage.New(dir)
	if err != nil {
		t.Fatalf("open storage: %v", err)
	}

	w := newWedge(t, db)
	validators := w.ahead.validators

	w.ahead.AddVertex(w.lieData)
	if !w.ahead.store.has(w.lie) {
		t.Fatal("setup: the vertex was not quarantined before the restart")
	}

	w.stopAhead()
	db.Close()

	reopened, err := storage.New(dir)
	if err != nil {
		t.Fatalf("reopen storage: %v", err)
	}
	t.Cleanup(func() { reopened.Close() })

	// The restarted node's index carries no round history, exactly as
	// BuildFromState leaves it: nothing here can re-derive the verdict.
	restarted := New(reopened, validators, nil, testSystemPod, 0, w.ahead.privKey, nil)
	t.Cleanup(func() { restarted.Close() })
	restarted.SetIndexer(index.NewManager())

	if _, ok := restarted.indexer.RootAt(receiverFrontier); ok {
		t.Fatal("the restarted node retains a root at the lied-about frontier: the test no longer covers the rebuild")
	}

	if !restarted.store.has(w.lie) {
		t.Fatal("the quarantined vertex must survive the restart, or causal batches containing it stop completing")
	}

	if containsHash(restarted.collectParents(1), w.lie) {
		t.Fatal("a restarted node must still refuse to reference a vertex it proved wrong before the restart")
	}

	if restarted.AddVertex(w.lieData) {
		t.Fatal("a restarted node must still refuse to relay a vertex it proved wrong before the restart")
	}
}

// TestQuarantine_StructurallyInvalidLiarStaysRejected pins the ordering the
// anchor check now sits at. Quarantine STORES a vertex, so it may only be
// reached by a vertex that is valid in every other respect: a wrong-root vertex
// that also fails a structural check must stay terminally rejected, or the
// quarantine door becomes a way to put arbitrary junk in every honest node's
// store.
func TestQuarantine_StructurallyInvalidLiarStaysRejected(t *testing.T) {
	validators, vs := newTestValidatorSet(3)
	ahead, _ := newIndexedNode(t, newTestStorage(t), vs, validators[0], indexAtReceiverFrontier(t))

	committed, _ := ahead.indexer.RootAt(receiverFrontier)
	wrong := committed
	wrong[0] ^= 0xFF

	// Round 5 with no parents: a wrong anchor AND a structural violation.
	data, hash := newAnchoredVertices(t, validators[1])(5, receiverFrontier, wrong, nil)

	buf := captureEvents(t)

	if ahead.AddVertex(data) {
		t.Fatal("a structurally invalid vertex must not be admitted")
	}

	if ahead.store.has(hash) {
		t.Fatal("a vertex that fails a structural check must not be stored, however its anchor reads")
	}

	if recs := eventsNamed(t, buf, events.EvVertexQuarantined); len(recs) != 0 {
		t.Fatalf("want no %s events for a structurally invalid vertex, got %d", events.EvVertexQuarantined, len(recs))
	}

	recs := eventsNamed(t, buf, events.EvVertexRejected)
	if len(recs) != 1 {
		t.Fatalf("want 1 %s event, got %d", events.EvVertexRejected, len(recs))
	}

	if recs[0]["reason"] != "parent_round" {
		t.Fatalf("reason = %v, want the structural failure that came first", recs[0]["reason"])
	}
}

// TestQuarantine_EmitsQuarantinedEvent pins the taxonomy change: a proven liar
// is no longer reported as rejected — it is stored — so it gets its own event
// naming the frontier whose root it lied about.
func TestQuarantine_EmitsQuarantinedEvent(t *testing.T) {
	w := newWedge(t, newTestStorage(t))

	buf := captureEvents(t)
	w.ahead.AddVertex(w.lieData)

	if recs := eventsNamed(t, buf, events.EvVertexRejected); len(recs) != 0 {
		t.Fatalf("a quarantined vertex is stored, not rejected: got %d %s events", len(recs), events.EvVertexRejected)
	}

	if recs := eventsNamed(t, buf, events.EvVertexReceived); len(recs) != 0 {
		t.Fatalf("a quarantined vertex is not an ordinary reception either: got %d %s events", len(recs), events.EvVertexReceived)
	}

	recs := eventsNamed(t, buf, events.EvVertexQuarantined)
	if len(recs) != 1 {
		t.Fatalf("want 1 %s event, got %d", events.EvVertexQuarantined, len(recs))
	}

	rec := recs[0]
	if rec["vertex"] != hex.EncodeToString(w.lie[:]) {
		t.Errorf("vertex = %v, want the quarantined vertex", rec["vertex"])
	}
	if rec["round"] != float64(0) {
		t.Errorf("round = %v, want 0", rec["round"])
	}
	if rec["frontier"] != float64(receiverFrontier) {
		t.Errorf("frontier = %v, want %d", rec["frontier"], receiverFrontier)
	}
}

// TestQuarantine_PendingChildResolvesOnParentQuarantine covers the buffered
// path: a child that arrived before its parent sits in the pending buffer, and
// the parent's quarantine must promote it exactly as an ordinary parent's
// arrival does. Without that, the wedge simply moves from the store to the
// buffer.
func TestQuarantine_PendingChildResolvesOnParentQuarantine(t *testing.T) {
	w := newWedge(t, newTestStorage(t))

	// The child arrives first: its parent is absent and its producer is known,
	// so it is buffered rather than rejected.
	if w.ahead.AddVertex(w.childData) {
		t.Fatal("a child whose parent is absent must be buffered, not admitted")
	}
	if w.ahead.store.has(w.child) {
		t.Fatal("setup: the child was admitted without its parent")
	}

	w.ahead.AddVertex(w.lieData)

	if !w.ahead.store.has(w.child) {
		t.Fatal("the buffered child must be promoted once its parent is quarantined into the store")
	}

	if _, ok := w.ahead.store.causalBatch(w.child); !ok {
		t.Fatal("the causal batch must complete once the buffered child is promoted")
	}
}
