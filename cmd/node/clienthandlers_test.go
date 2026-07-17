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

	"BluePods/internal/events"
	"BluePods/internal/genesis"
	"BluePods/internal/network"
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
