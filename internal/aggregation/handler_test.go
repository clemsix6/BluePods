package aggregation

import (
	"bytes"
	"testing"

	flatbuffers "github.com/google/flatbuffers/go"

	"BluePods/internal/podvm"
	"BluePods/internal/state"
	"BluePods/internal/storage"
	"BluePods/internal/types"
)

// createTestObject creates a test object with the given ID, version, and replication.
func createTestObject(t *testing.T, id [32]byte, version uint64, replication uint16, content []byte) []byte {
	t.Helper()

	builder := flatbuffers.NewBuilder(256)

	idVec := builder.CreateByteVector(id[:])
	contentVec := builder.CreateByteVector(content)

	types.ObjectStart(builder)
	types.ObjectAddId(builder, idVec)
	types.ObjectAddVersion(builder, version)
	types.ObjectAddReplication(builder, replication)
	types.ObjectAddContent(builder, contentVec)
	objOffset := types.ObjectEnd(builder)

	builder.Finish(objOffset)

	return builder.FinishedBytes()
}

// setupTestHandler creates a Handler with test storage and BLS key.
func setupTestHandler(t *testing.T) (*Handler, *state.State, func()) {
	t.Helper()

	// Create temp storage
	dir := t.TempDir()
	db, err := storage.New(dir)
	if err != nil {
		t.Fatalf("create storage: %v", err)
	}

	// Create state without podvm (not needed for handler tests)
	st := state.New(db, &podvm.Pool{})

	// Generate BLS key
	blsKey, err := GenerateBLSKey()
	if err != nil {
		t.Fatalf("generate BLS key: %v", err)
	}

	handler := NewHandler(st, blsKey)

	cleanup := func() {
		db.Close()
	}

	return handler, st, cleanup
}

// TestHandleRequest_ObjectFound tests positive response when object exists.
func TestHandleRequest_ObjectFound(t *testing.T) {
	handler, st, cleanup := setupTestHandler(t)
	defer cleanup()

	// Create and store test object
	objectID := [32]byte{1, 2, 3}
	version := uint64(5)
	content := []byte("test content")

	objData := createTestObject(t, objectID, version, 10, content)
	st.SetObject(objData)

	// Create request
	req := &AttestationRequest{
		ObjectID: objectID,
		Version:  version,
		WantFull: false,
	}

	respData, err := handler.processRequest(req)
	if err != nil {
		t.Fatalf("process request: %v", err)
	}

	// Parse response
	msgType, err := GetMessageType(respData)
	if err != nil {
		t.Fatalf("get message type: %v", err)
	}

	if msgType != msgTypePositive {
		t.Fatalf("expected positive response, got type 0x%02x", msgType)
	}

	resp, err := DecodePositiveResponse(respData)
	if err != nil {
		t.Fatalf("decode response: %v", err)
	}

	// Verify hash
	expectedHash := ComputeObjectHash(objData, version)
	if resp.Hash != expectedHash {
		t.Error("hash mismatch")
	}

	// Verify signature
	if len(resp.Signature) != BLSSignatureSize {
		t.Errorf("signature size: got %d, want %d", len(resp.Signature), BLSSignatureSize)
	}

	// WantFull=false should not include data
	if len(resp.Data) != 0 {
		t.Errorf("expected no data, got %d bytes", len(resp.Data))
	}
}

// TestHandleRequest_WantFull tests that full object is returned when requested.
func TestHandleRequest_WantFull(t *testing.T) {
	handler, st, cleanup := setupTestHandler(t)
	defer cleanup()

	// Create and store test object
	objectID := [32]byte{4, 5, 6}
	version := uint64(1)
	content := []byte("full object content")

	objData := createTestObject(t, objectID, version, 10, content)
	st.SetObject(objData)

	// Create request with WantFull=true
	req := &AttestationRequest{
		ObjectID: objectID,
		Version:  version,
		WantFull: true,
	}

	respData, err := handler.processRequest(req)
	if err != nil {
		t.Fatalf("process request: %v", err)
	}

	resp, err := DecodePositiveResponse(respData)
	if err != nil {
		t.Fatalf("decode response: %v", err)
	}

	// Should include full object data
	if !bytes.Equal(resp.Data, objData) {
		t.Error("object data mismatch")
	}
}

// TestHandleRequest_NotFound tests negative response when object doesn't exist.
func TestHandleRequest_NotFound(t *testing.T) {
	handler, _, cleanup := setupTestHandler(t)
	defer cleanup()

	// Request non-existent object
	req := &AttestationRequest{
		ObjectID: [32]byte{99, 99, 99},
		Version:  1,
		WantFull: false,
	}

	respData, err := handler.processRequest(req)
	if err != nil {
		t.Fatalf("process request: %v", err)
	}

	// Parse response
	msgType, err := GetMessageType(respData)
	if err != nil {
		t.Fatalf("get message type: %v", err)
	}

	if msgType != msgTypeNegative {
		t.Fatalf("expected negative response, got type 0x%02x", msgType)
	}

	resp, err := DecodeNegativeResponse(respData)
	if err != nil {
		t.Fatalf("decode response: %v", err)
	}

	if resp.Reason != reasonNotFound {
		t.Errorf("reason: got 0x%02x, want 0x%02x", resp.Reason, reasonNotFound)
	}

	// Negative responses should be signed
	if len(resp.Signature) != BLSSignatureSize {
		t.Errorf("signature size: got %d, want %d", len(resp.Signature), BLSSignatureSize)
	}
}

// TestHandleRequest_WrongVersion tests negative response when version mismatches.
func TestHandleRequest_WrongVersion(t *testing.T) {
	handler, st, cleanup := setupTestHandler(t)
	defer cleanup()

	// Create and store test object with version 5
	objectID := [32]byte{7, 8, 9}
	content := []byte("versioned content")

	objData := createTestObject(t, objectID, 5, 10, content)
	st.SetObject(objData)

	// Request with wrong version
	req := &AttestationRequest{
		ObjectID: objectID,
		Version:  3, // Wrong version
		WantFull: false,
	}

	respData, err := handler.processRequest(req)
	if err != nil {
		t.Fatalf("process request: %v", err)
	}

	msgType, _ := GetMessageType(respData)
	if msgType != msgTypeNegative {
		t.Fatalf("expected negative response, got type 0x%02x", msgType)
	}

	resp, err := DecodeNegativeResponse(respData)
	if err != nil {
		t.Fatalf("decode response: %v", err)
	}

	if resp.Reason != reasonWrongVersion {
		t.Errorf("reason: got 0x%02x, want 0x%02x", resp.Reason, reasonWrongVersion)
	}
}

// TestHandleRequest_InvalidRequest tests error handling for malformed requests.
func TestHandleRequest_InvalidRequest(t *testing.T) {
	handler, _, cleanup := setupTestHandler(t)
	defer cleanup()

	// Send invalid request data
	_, err := handler.HandleRequest(nil, []byte{0x00, 0x01, 0x02})
	if err == nil {
		t.Error("expected error for invalid request")
	}
}

// TestComputeObjectHash tests hash computation consistency.
func TestComputeObjectHash(t *testing.T) {
	content := []byte("test content for hashing")
	version := uint64(42)

	hash1 := ComputeObjectHash(content, version)
	hash2 := ComputeObjectHash(content, version)

	// Same input should produce same hash
	if hash1 != hash2 {
		t.Error("hash not deterministic")
	}

	// Different version should produce different hash
	hash3 := ComputeObjectHash(content, version+1)
	if hash1 == hash3 {
		t.Error("different version should produce different hash")
	}

	// Different content should produce different hash
	hash4 := ComputeObjectHash([]byte("different content"), version)
	if hash1 == hash4 {
		t.Error("different content should produce different hash")
	}
}
