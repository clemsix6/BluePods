package sync

import (
	"testing"

	"BluePods/internal/types"
)

func TestBuildSnapshotRequest(t *testing.T) {
	data := buildSnapshotRequest(42)

	req := types.GetRootAsSnapshotRequest(data, 0)
	if req.RequestId() != 42 {
		t.Errorf("request_id = %d, want 42", req.RequestId())
	}
}

func TestBuildSnapshotResponse(t *testing.T) {
	compressedData := []byte("compressed snapshot data")
	data := buildSnapshotResponse(123, compressedData)

	resp := types.GetRootAsSnapshotResponse(data, 0)

	if resp.RequestId() != 123 {
		t.Errorf("request_id = %d, want 123", resp.RequestId())
	}

	if string(resp.DataBytes()) != string(compressedData) {
		t.Errorf("data mismatch")
	}
}

func TestIsSnapshotRequest_Valid(t *testing.T) {
	data := buildSnapshotRequest(1)

	if !IsSnapshotRequest(data) {
		t.Error("should recognize valid snapshot request")
	}
}

func TestIsSnapshotRequest_Invalid(t *testing.T) {
	// Too short
	if IsSnapshotRequest([]byte{1, 2, 3}) {
		t.Error("should reject too short data")
	}

	// Request with ID 0 (invalid)
	data := buildSnapshotRequest(0)
	if IsSnapshotRequest(data) {
		t.Error("should reject request with ID 0")
	}
}
