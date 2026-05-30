package aggregation

import (
	"testing"

	flatbuffers "github.com/google/flatbuffers/go"

	"BluePods/internal/attest"
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

	isHolder := func(objectID [32]byte, replication uint16) bool { return true }
	handler := NewHandler(st, blsKey, db, isHolder)

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

	// Verify hash over the object's content bytes (canonical form)
	fbObj := types.GetRootAsObject(objData, 0)
	expectedHash := attest.ComputeObjectHash(fbObj.ContentBytes(), version)
	if resp.Hash != expectedHash {
		t.Error("hash mismatch")
	}

	// Verify signature
	if len(resp.Signature) != BLSSignatureSize {
		t.Errorf("signature size: got %d, want %d", len(resp.Signature), BLSSignatureSize)
	}
}

// TestHandleRequest_Singleton tests that a singleton (replication 0) is never
// attested: the handler returns a static negative without signing.
func TestHandleRequest_Singleton(t *testing.T) {
	handler, st, cleanup := setupTestHandler(t)
	defer cleanup()

	objectID := [32]byte{0xAB, 0xCD}
	version := uint64(2)

	objData := createTestObject(t, objectID, version, 0, []byte("coin"))
	st.SetObject(objData)

	req := &AttestationRequest{ObjectID: objectID, Version: version}

	respData, err := handler.processRequest(req)
	if err != nil {
		t.Fatalf("process request: %v", err)
	}

	msgType, _ := GetMessageType(respData)
	if msgType != msgTypeNegative {
		t.Fatalf("expected negative response for singleton, got type 0x%02x", msgType)
	}

	resp, err := DecodeNegativeResponse(respData)
	if err != nil {
		t.Fatalf("decode response: %v", err)
	}

	if resp.Reason != reasonNotFound {
		t.Errorf("reason: got 0x%02x, want 0x%02x", resp.Reason, reasonNotFound)
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
