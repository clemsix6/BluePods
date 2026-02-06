package sync

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	flatbuffers "github.com/google/flatbuffers/go"

	"BluePods/internal/logger"
	"BluePods/internal/types"
)

const (
	// requestTimeout is the timeout for snapshot requests.
	requestTimeout = 60 * time.Second
)

// requestID is a global counter for snapshot requests.
var requestID atomic.Uint64

// Requester can send requests and receive responses.
type Requester interface {
	Request(ctx context.Context, data []byte) ([]byte, error)
}

// RequestSnapshot requests a snapshot from a remote peer.
// Returns the decompressed snapshot data.
func RequestSnapshot(peer Requester) ([]byte, error) {
	reqID := requestID.Add(1)

	// Build request
	reqData := buildSnapshotRequest(reqID)

	// Send request and wait for response
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()

	logger.Debug("requesting snapshot", "request_id", reqID)

	respData, err := peer.Request(ctx, reqData)
	if err != nil {
		return nil, fmt.Errorf("send request:\n%w", err)
	}

	// Parse response
	resp := types.GetRootAsSnapshotResponse(respData, 0)
	if resp.RequestId() != reqID {
		return nil, fmt.Errorf("request ID mismatch: got %d, want %d", resp.RequestId(), reqID)
	}

	compressedData := resp.DataBytes()
	if len(compressedData) == 0 {
		return nil, fmt.Errorf("empty snapshot data")
	}

	logger.Debug("received snapshot",
		"request_id", reqID,
		"compressed_size", len(compressedData),
		"uncompressed_size", resp.UncompressedSize(),
	)

	// Decompress
	data, err := DecompressSnapshot(compressedData)
	if err != nil {
		return nil, fmt.Errorf("decompress snapshot:\n%w", err)
	}

	return data, nil
}

// buildSnapshotRequest creates a FlatBuffers snapshot request.
func buildSnapshotRequest(reqID uint64) []byte {
	builder := flatbuffers.NewBuilder(64)

	types.SnapshotRequestStart(builder)
	types.SnapshotRequestAddRequestId(builder, reqID)
	offset := types.SnapshotRequestEnd(builder)
	builder.Finish(offset)

	return builder.FinishedBytes()
}

// HandleSnapshotRequest handles an incoming snapshot request.
// Returns the response data to send back.
func HandleSnapshotRequest(reqData []byte, manager *SnapshotManager) ([]byte, error) {
	req := types.GetRootAsSnapshotRequest(reqData, 0)
	reqID := req.RequestId()

	logger.Debug("handling snapshot request", "request_id", reqID)

	// Get latest snapshot
	compressedData, round := manager.Latest()
	if compressedData == nil {
		return nil, fmt.Errorf("no snapshot available")
	}

	logger.Debug("sending snapshot",
		"request_id", reqID,
		"round", round,
		"size", len(compressedData),
	)

	// Build response
	respData := buildSnapshotResponse(reqID, compressedData)

	return respData, nil
}

// buildSnapshotResponse creates a FlatBuffers snapshot response.
func buildSnapshotResponse(reqID uint64, compressedData []byte) []byte {
	builder := flatbuffers.NewBuilder(len(compressedData) + 64)

	dataOffset := builder.CreateByteVector(compressedData)

	types.SnapshotResponseStart(builder)
	types.SnapshotResponseAddRequestId(builder, reqID)
	types.SnapshotResponseAddData(builder, dataOffset)
	types.SnapshotResponseAddUncompressedSize(builder, 0) // TODO: track uncompressed size
	offset := types.SnapshotResponseEnd(builder)
	builder.Finish(offset)

	return builder.FinishedBytes()
}

// IsSnapshotRequest checks if data is a snapshot request.
// Used to route messages to the appropriate handler.
func IsSnapshotRequest(data []byte) bool {
	if len(data) < 8 {
		return false
	}

	// Try to parse as SnapshotRequest
	// FlatBuffers tables have a specific structure we can check
	req := types.GetRootAsSnapshotRequest(data, 0)

	// A valid request has a request_id > 0
	return req.RequestId() > 0
}
