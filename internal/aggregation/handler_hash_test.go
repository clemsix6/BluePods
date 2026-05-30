package aggregation

import (
	"testing"

	"BluePods/internal/attest"
	"BluePods/internal/podvm"
	"BluePods/internal/state"
	"BluePods/internal/storage"
	"BluePods/internal/types"
)

// TestHandlerHashAgreement verifies that the signature produced by the handler
// is over attest.ComputeObjectHash(ContentBytes(), version) and verifies under
// the holder's BLS public key. This pins signer and verifier to the same hash.
func TestHandlerHashAgreement(t *testing.T) {
	dir := t.TempDir()
	db, err := storage.New(dir)
	if err != nil {
		t.Fatalf("create storage: %v", err)
	}
	defer db.Close()

	st := state.New(db, &podvm.Pool{})

	blsKey, err := GenerateBLSKey()
	if err != nil {
		t.Fatalf("generate BLS key: %v", err)
	}

	handler := NewHandler(st, blsKey)

	objectID := [32]byte{0xAA, 0xBB}
	version := uint64(9)
	content := []byte("content under attestation")

	objData := createTestObject(t, objectID, version, 5, content)
	st.SetObject(objData)

	req := &AttestationRequest{ObjectID: objectID, Version: version}

	respData, err := handler.processRequest(req)
	if err != nil {
		t.Fatalf("process request: %v", err)
	}

	resp, err := DecodePositiveResponse(respData)
	if err != nil {
		t.Fatalf("decode positive response: %v", err)
	}

	// The handler must hash the content bytes, not the full object bytes.
	fbObj := types.GetRootAsObject(objData, 0)
	expected := attest.ComputeObjectHash(fbObj.ContentBytes(), version)
	if resp.Hash != expected {
		t.Fatalf("handler hash mismatch:\n got %x\nwant %x", resp.Hash, expected)
	}

	// The signature must verify under the holder's BLS key on that exact hash.
	if !attest.Verify(resp.Signature, expected[:], blsKey.PublicKeyBytes()) {
		t.Fatal("handler signature does not verify under attest hash")
	}
}
