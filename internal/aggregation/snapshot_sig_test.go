package aggregation

import (
	"testing"

	"BluePods/internal/attest"
	"BluePods/internal/podvm"
	"BluePods/internal/state"
	"BluePods/internal/sync"
	"BluePods/internal/types"
)

// TestSnapshotSignatureRoundTripServesWithoutSigning verifies that eager
// signatures travel in snapshots: a node applying a snapshot serves an
// attestation for a held object from the stored signature without signing.
//
// The applied node's isHolder returns false, so the bounded sign-on-miss
// fallback cannot run. A positive response therefore proves the signature
// came from the snapshot, not from fresh signing.
func TestSnapshotSignatureRoundTripServesWithoutSigning(t *testing.T) {
	source := newSigTestStorage(t)
	applied := newSigTestStorage(t)

	blsKey, err := GenerateBLSKey()
	if err != nil {
		t.Fatalf("generate BLS key: %v", err)
	}

	// Store an object (32-byte key) and its eager signature in the source store.
	objectID := [32]byte{0x11, 0x22}
	version := uint64(3)
	content := []byte("snapshot-carried content")

	objData := createTestObject(t, objectID, version, 5, content)
	if err := source.Set(objectID[:], objData); err != nil {
		t.Fatalf("store object: %v", err)
	}

	hash := computeHashForTest(objData, version)
	sig := blsKey.Sign(hash[:])
	if err := PutObjectSig(source, objectID, version, sig); err != nil {
		t.Fatalf("put object sig: %v", err)
	}

	// Snapshot the source and apply it to the destination store.
	snapData, err := sync.CreateSnapshot(source, 7, nil, nil, nil, nil, 0, 0)
	if err != nil {
		t.Fatalf("create snapshot: %v", err)
	}

	if _, err := sync.ApplySnapshot(applied, snapData); err != nil {
		t.Fatalf("apply snapshot: %v", err)
	}

	// Confirm the signature survived the round trip.
	gotVersion, gotSig, ok := GetObjectSig(applied, objectID)
	if !ok || gotVersion != version {
		t.Fatalf("signature not carried: ok=%v version=%d", ok, gotVersion)
	}

	// Build a handler whose isHolder is false: it must never sign on a miss.
	st := state.New(applied, &podvm.Pool{})
	neverHolds := func(id [32]byte, replication uint16) bool { return false }
	handler := NewHandler(st, blsKey, applied, neverHolds)

	req := &AttestationRequest{ObjectID: objectID, Version: version}

	respData, err := handler.processRequest(req)
	if err != nil {
		t.Fatalf("process request: %v", err)
	}

	msgType, _ := GetMessageType(respData)
	if msgType != msgTypePositive {
		t.Fatalf("expected positive response served from store, got type 0x%02x", msgType)
	}

	resp, err := DecodePositiveResponse(respData)
	if err != nil {
		t.Fatalf("decode positive response: %v", err)
	}

	if string(resp.Signature) != string(gotSig) {
		t.Fatal("served signature does not match the snapshot-carried signature")
	}
}

// computeHashForTest mirrors the handler's content-hash computation.
func computeHashForTest(objData []byte, version uint64) [32]byte {
	fbObj := types.GetRootAsObject(objData, 0)
	return attest.ComputeObjectHash(fbObj.ContentBytes(), version)
}
