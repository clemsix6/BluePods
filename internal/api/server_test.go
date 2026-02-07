package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	flatbuffers "github.com/google/flatbuffers/go"
	"github.com/zeebo/blake3"

	"BluePods/internal/types"
)

// mockSubmitter captures submitted transactions.
type mockSubmitter struct {
	txs [][]byte
}

func (m *mockSubmitter) SubmitTx(tx []byte) {
	m.txs = append(m.txs, tx)
}

func TestHealthEndpoint(t *testing.T) {
	submitter := &mockSubmitter{}
	server := New(":0", submitter, nil, nil, nil, nil)

	req := httptest.NewRequest("GET", "/health", nil)
	w := httptest.NewRecorder()

	server.handleHealth(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", w.Code)
	}

	var resp map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}

	if resp["status"] != "ok" {
		t.Errorf("expected status ok, got %s", resp["status"])
	}
}

func TestSubmitTx_Success(t *testing.T) {
	submitter := &mockSubmitter{}
	server := New(":0", submitter, nil, nil, nil, nil)

	// Build a valid AttestedTransaction
	txData := buildTestAttestedTx()

	req := httptest.NewRequest("POST", "/tx", bytes.NewReader(txData))
	w := httptest.NewRecorder()

	server.handleSubmitTx(w, req)

	if w.Code != http.StatusAccepted {
		t.Errorf("expected status 202, got %d: %s", w.Code, w.Body.String())
	}

	if len(submitter.txs) != 1 {
		t.Errorf("expected 1 tx submitted, got %d", len(submitter.txs))
	}

	var resp map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}

	if resp["hash"] == "" {
		t.Error("expected hash in response")
	}
}

func TestSubmitTx_EmptyBody(t *testing.T) {
	submitter := &mockSubmitter{}
	server := New(":0", submitter, nil, nil, nil, nil)

	req := httptest.NewRequest("POST", "/tx", nil)
	w := httptest.NewRecorder()

	server.handleSubmitTx(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected status 400, got %d", w.Code)
	}

	if len(submitter.txs) != 0 {
		t.Error("should not submit on error")
	}
}

func TestSubmitTx_InvalidData(t *testing.T) {
	submitter := &mockSubmitter{}
	server := New(":0", submitter, nil, nil, nil, nil)

	req := httptest.NewRequest("POST", "/tx", bytes.NewReader([]byte("invalid")))
	w := httptest.NewRecorder()

	server.handleSubmitTx(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected status 400, got %d", w.Code)
	}
}

// buildTestAttestedTx creates a minimal valid AttestedTransaction for testing.
func buildTestAttestedTx() []byte {
	builder := flatbuffers.NewBuilder(512)

	// Create a minimal transaction
	hash := blake3.Sum256([]byte("test"))
	hashVec := builder.CreateByteVector(hash[:])
	senderVec := builder.CreateByteVector(make([]byte, 32))
	sigVec := builder.CreateByteVector(make([]byte, 64))
	podVec := builder.CreateByteVector(make([]byte, 32))
	funcName := builder.CreateString("test")
	argsVec := builder.CreateByteVector([]byte{})

	types.TransactionStart(builder)
	types.TransactionAddHash(builder, hashVec)
	types.TransactionAddSender(builder, senderVec)
	types.TransactionAddSignature(builder, sigVec)
	types.TransactionAddPod(builder, podVec)
	types.TransactionAddFunctionName(builder, funcName)
	types.TransactionAddArgs(builder, argsVec)
	txOffset := types.TransactionEnd(builder)

	// Empty objects and proofs
	types.AttestedTransactionStartObjectsVector(builder, 0)
	objectsVec := builder.EndVector(0)

	types.AttestedTransactionStartProofsVector(builder, 0)
	proofsVec := builder.EndVector(0)

	types.AttestedTransactionStart(builder)
	types.AttestedTransactionAddTransaction(builder, txOffset)
	types.AttestedTransactionAddObjects(builder, objectsVec)
	types.AttestedTransactionAddProofs(builder, proofsVec)
	atxOffset := types.AttestedTransactionEnd(builder)

	builder.Finish(atxOffset)

	return builder.FinishedBytes()
}
