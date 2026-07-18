package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	flatbuffers "github.com/google/flatbuffers/go"

	"BluePods/internal/events"
	"BluePods/internal/genesis"
	"BluePods/internal/network"
	"BluePods/internal/state"
	"BluePods/internal/types"
)

// captureEvents swaps the default slog logger for a JSON handler writing into a
// fresh buffer, restoring the previous default on test cleanup, mirroring the
// consensus package's helper of the same name (internal/consensus is a
// different package, so this is not directly reusable from here).
func captureEvents(t *testing.T) *bytes.Buffer {
	t.Helper()

	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })

	return &buf
}

// eventsNamed decodes one JSON object per captured line and returns those whose
// reserved events.Key attribute equals name, in emission order.
func eventsNamed(t *testing.T, buf *bytes.Buffer, name string) []map[string]any {
	t.Helper()

	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}

		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("captured line is not JSON: %v (line=%q)", err, line)
		}

		if rec[events.Key] == name {
			out = append(out, rec)
		}
	}

	return out
}

// submitTestNode builds a bootstrap-mode Node the way bootstrapTestNode does
// (init_test.go), plus the txIndex handleSubmitTx/handleFaucet require. Both
// handlers are driven directly here (no network), so a nil n.network is fine:
// GossipTx and the faucet path are both nil-safe without one.
func submitTestNode(t *testing.T) *Node {
	t.Helper()

	_, privKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	n, db := bootstrapTestNode(t, t.TempDir(), privKey)
	t.Cleanup(func() { n.dag.Close(); db.Close() })

	n.txIndex = newTxStatusIndex()

	return n
}

// =============================================================================
// ingress.tx.received / ingress.tx.rejected (handleSubmitTx)
// =============================================================================

// TestHandleSubmitTx_RawTx_EmitsIngressTxReceived checks a raw (singleton-only)
// transaction submission emits ingress.tx.received with kind=raw.
func TestHandleSubmitTx_RawTx_EmitsIngressTxReceived(t *testing.T) {
	n := submitTestNode(t)

	rawTx := genesis.BuildDeregisterValidatorRawTx(n.cfg.PrivateKey, n.systemPod)
	req := network.EncodeSubmitTx(&network.SubmitTxRequest{Body: rawTx})

	buf := captureEvents(t)
	respBytes, err := n.handleSubmitTx(req)
	if err != nil {
		t.Fatalf("handleSubmitTx: %v", err)
	}

	resp, err := network.DecodeSubmitTxResp(respBytes)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Err != "" {
		t.Fatalf("unexpected refusal: %q", resp.Err)
	}

	recs := eventsNamed(t, buf, events.EvIngressTxReceived)
	if len(recs) != 1 {
		t.Fatalf("want 1 %s event, got %d: %v", events.EvIngressTxReceived, len(recs), recs)
	}
	rec := recs[0]
	if rec["kind"] != "raw" {
		t.Errorf("kind = %v, want raw", rec["kind"])
	}
	if rec["tx"] != hex.EncodeToString(resp.Hash) {
		t.Errorf("tx = %v, want %s", rec["tx"], hex.EncodeToString(resp.Hash))
	}
}

// TestHandleSubmitTx_AttestedTx_EmitsIngressTxReceived checks a full ATX
// submission (one that fails raw-transaction structural validation) emits
// ingress.tx.received with kind=attested.
func TestHandleSubmitTx_AttestedTx_EmitsIngressTxReceived(t *testing.T) {
	n := submitTestNode(t)

	atxBytes := genesis.BuildAttestedTx(n.cfg.PrivateKey, n.systemPod, "noop", nil, nil, 0, 0, nil)
	req := network.EncodeSubmitTx(&network.SubmitTxRequest{Body: atxBytes})

	buf := captureEvents(t)
	respBytes, err := n.handleSubmitTx(req)
	if err != nil {
		t.Fatalf("handleSubmitTx: %v", err)
	}

	resp, err := network.DecodeSubmitTxResp(respBytes)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Err != "" {
		t.Fatalf("unexpected refusal: %q", resp.Err)
	}

	recs := eventsNamed(t, buf, events.EvIngressTxReceived)
	if len(recs) != 1 {
		t.Fatalf("want 1 %s event, got %d: %v", events.EvIngressTxReceived, len(recs), recs)
	}
	if recs[0]["kind"] != "attested" {
		t.Errorf("kind = %v, want attested", recs[0]["kind"])
	}
}

// TestHandleSubmitTx_Rejection_EmitsIngressTxRejected checks a submission that
// fails ingestion (malformed body, neither a valid raw transaction nor a
// parseable ATX) emits ingress.tx.rejected and no ingress.tx.received.
func TestHandleSubmitTx_Rejection_EmitsIngressTxRejected(t *testing.T) {
	n := submitTestNode(t)

	req := network.EncodeSubmitTx(&network.SubmitTxRequest{Body: []byte("not a valid transaction")})

	buf := captureEvents(t)
	respBytes, err := n.handleSubmitTx(req)
	if err != nil {
		t.Fatalf("handleSubmitTx: %v", err)
	}

	resp, err := network.DecodeSubmitTxResp(respBytes)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Err == "" {
		t.Fatal("expected a submission error for a malformed body")
	}

	recs := eventsNamed(t, buf, events.EvIngressTxRejected)
	if len(recs) != 1 {
		t.Fatalf("want 1 %s event, got %d: %v", events.EvIngressTxRejected, len(recs), recs)
	}
	if recs[0]["reason"] != "invalid_submission" {
		t.Errorf("reason = %v, want invalid_submission", recs[0]["reason"])
	}

	if recs := eventsNamed(t, buf, events.EvIngressTxReceived); len(recs) != 0 {
		t.Fatalf("a rejected submission must emit no %s event, got %d", events.EvIngressTxReceived, len(recs))
	}
}

// =============================================================================
// ingress.tx.received (handleFaucet)
// =============================================================================

// TestHandleFaucet_EmitsIngressTxReceived checks an accepted faucet split emits
// ingress.tx.received with kind=faucet.
func TestHandleFaucet_EmitsIngressTxReceived(t *testing.T) {
	n := submitTestNode(t)

	var recipient [32]byte
	recipient[0] = 0x42
	req := network.EncodeFaucet(&network.FaucetRequest{Pubkey: recipient, Amount: 100})

	buf := captureEvents(t)
	respBytes, err := n.handleFaucet(req)
	if err != nil {
		t.Fatalf("handleFaucet: %v", err)
	}

	resp, err := network.DecodeFaucetResp(respBytes)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Err != "" {
		t.Fatalf("unexpected refusal: %q", resp.Err)
	}

	recs := eventsNamed(t, buf, events.EvIngressTxReceived)
	if len(recs) != 1 {
		t.Fatalf("want 1 %s event, got %d: %v", events.EvIngressTxReceived, len(recs), recs)
	}
	rec := recs[0]
	if rec["kind"] != "faucet" {
		t.Errorf("kind = %v, want faucet", rec["kind"])
	}
	if rec["tx"] != hex.EncodeToString(resp.Hash) {
		t.Errorf("tx = %v, want %s", rec["tx"], hex.EncodeToString(resp.Hash))
	}
}

// =============================================================================
// inter-node holder probes (fetchObjectFromHolder / requestObjectFrom)
// =============================================================================

// =============================================================================
// routed reads defer to holders for objects this node does not hold (handleGetObject)
// =============================================================================

// storeReplicatedObject writes an object straight into a node's state, standing
// in for a copy the node retained locally. version and owner let a test assert
// which copy a routed read returns; replication drives the holder check.
func storeReplicatedObject(st *state.State, id, owner [32]byte, version uint64, replication uint16) {
	b := flatbuffers.NewBuilder(256)
	idVec := b.CreateByteVector(id[:])
	ownerVec := b.CreateByteVector(owner[:])
	contentVec := b.CreateByteVector([]byte{0x01})

	types.ObjectStart(b)
	types.ObjectAddId(b, idVec)
	types.ObjectAddVersion(b, version)
	types.ObjectAddOwner(b, ownerVec)
	types.ObjectAddReplication(b, replication)
	types.ObjectAddContent(b, contentVec)
	types.ObjectAddFees(b, 0)
	b.Finish(types.ObjectEnd(b))

	st.SetObject(b.FinishedBytes())
}

// routedGetObject drives handleGetObject with a routed (non-local-only) request
// and returns the decoded response.
func routedGetObject(t *testing.T, n *Node, id [32]byte) *network.GetObjectResponse {
	t.Helper()

	respBytes, err := n.handleGetObject(network.EncodeGetObject(&network.GetObjectRequest{ObjectID: id}))
	if err != nil {
		t.Fatalf("handleGetObject: %v", err)
	}

	resp, err := network.DecodeGetObjectResp(respBytes)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}

	return resp
}

// TestHandleGetObject_NonHolderDoesNotServeStaleCopy pins the wedge's root
// cause: a routed GetObject for a replicated object this node does not currently
// hold must not be answered from the node's own retained copy. A node that loses
// holdership keeps its copy lazily past the epoch boundary, so that copy can name
// a stale owner once a transfer applies on the real holders. The routed read must
// defer to the holders; here none are reachable (rendezvous unset), so the answer
// is a definitive not-found rather than the stale local copy.
func TestHandleGetObject_NonHolderDoesNotServeStaleCopy(t *testing.T) {
	n := submitTestNode(t)
	n.isHolder = func(_ [32]byte, _ uint16) bool { return false }

	var id, staleOwner [32]byte
	id[0] = 0x11
	staleOwner[0] = 0xAA
	storeReplicatedObject(n.state, id, staleOwner, 0, 3)

	resp := routedGetObject(t, n, id)

	if resp.Found {
		t.Fatal("routed read served a non-holder's retained copy; it must defer to the holders")
	}
}

// TestHandleGetObject_HolderServesLocalCopy checks the fast path stays intact: a
// current holder answers a routed read from its own authoritative local copy,
// without probing, and returns the version it holds.
func TestHandleGetObject_HolderServesLocalCopy(t *testing.T) {
	n := submitTestNode(t)
	n.isHolder = func(_ [32]byte, _ uint16) bool { return true }

	var id, owner [32]byte
	id[0] = 0x22
	owner[0] = 0xBB
	storeReplicatedObject(n.state, id, owner, 5, 3)

	resp := routedGetObject(t, n, id)

	if !resp.Found {
		t.Fatal("current holder must serve its local copy")
	}
	if got := types.GetRootAsObject(resp.Data, 0).Version(); got != 5 {
		t.Errorf("served version = %d, want 5", got)
	}
}

// TestHandleGetObject_SingletonServedLocally checks a singleton (replication 0),
// which every validator holds, is served from local state on a routed read even
// when the holder check reports this node holds no replicated objects.
func TestHandleGetObject_SingletonServedLocally(t *testing.T) {
	n := submitTestNode(t)
	n.isHolder = func(_ [32]byte, _ uint16) bool { return false }

	var id, owner [32]byte
	id[0] = 0x33
	owner[0] = 0xCC
	storeReplicatedObject(n.state, id, owner, 1, 0)

	resp := routedGetObject(t, n, id)

	if !resp.Found {
		t.Fatal("singleton must be served from local state")
	}
}

// TestBuildHolderObjectRequest_SetsLocalOnly checks the inter-node GetObject
// request sent to a computed holder is always LocalOnly. A holder that lacks
// the object must answer a definitive not-found from its own local state
// rather than re-entering the non-local path and probing the rest of the
// mesh itself, which is what stops a globally-absent object from cascading.
func TestBuildHolderObjectRequest_SetsLocalOnly(t *testing.T) {
	var id [32]byte
	id[0] = 0x7a

	req := buildHolderObjectRequest(id)

	if !req.LocalOnly {
		t.Error("LocalOnly = false, want true: a holder probe must never re-route")
	}
	if req.ObjectID != id {
		t.Errorf("ObjectID = %x, want %x", req.ObjectID, id)
	}
}
