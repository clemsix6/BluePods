package network

import (
	"encoding/binary"
	"fmt"
)

// Test-control operations carried by TestControlRequest.Op.
const (
	// TestControlOpSetPartition replaces the node's blocklist with the request's
	// pubkeys.
	TestControlOpSetPartition byte = 1

	// TestControlOpClearPartition empties the node's blocklist.
	TestControlOpClearPartition byte = 2
)

// FingerprintResponse carries a node's convergence fingerprint: the last
// committed round, the BLAKE3 checksum over canonically sorted globally
// replicated state, and the locally evaluable supply terms. Err is non-empty
// when the node refuses the request (most commonly because --test-hooks is
// disabled), in which case every other field is zero.
type FingerprintResponse struct {
	Round        uint64   // Round is the last committed round at the cut
	Checksum     [32]byte // Checksum is the BLAKE3 digest of globally replicated state
	TotalSupply  uint64   // TotalSupply is the protocol supply counter
	CoinsTotal   uint64   // CoinsTotal is the protocol sum-of-coin-balances counter
	TotalBonded  uint64   // TotalBonded is the sum of effective stake over the active set
	Deposits     uint64   // Deposits is the sum of locked storage deposits from the tracker
	FeesInFlight uint64   // FeesInFlight is the epoch's accumulated, undistributed consumed fees
	Err          string   // Err is a non-empty refusal message on failure
}

// EncodeStateFingerprint encodes a state-fingerprint request.
// Format: [1B tag].
func EncodeStateFingerprint() []byte {
	return []byte{MsgTagStateFingerprint}
}

// fingerprintRespFixedLen is the length of the fixed-size fields following the
// tag byte: round(8) + checksum(32) + totalSupply(8) + coinsTotal(8) +
// totalBonded(8) + deposits(8) + feesInFlight(8).
const fingerprintRespFixedLen = 8 + 32 + 8 + 8 + 8 + 8 + 8

// EncodeStateFingerprintResp encodes a state-fingerprint response.
// Format: [1B tag] [8B round] [32B checksum] [8B totalSupply] [8B coinsTotal]
// [8B totalBonded] [8B deposits] [8B feesInFlight] [errString].
func EncodeStateFingerprintResp(resp *FingerprintResponse) []byte {
	buf := make([]byte, 1+fingerprintRespFixedLen+len(resp.Err))
	buf[0] = MsgTagStateFingerprintResp

	off := 1
	binary.BigEndian.PutUint64(buf[off:], resp.Round)
	off += 8
	copy(buf[off:], resp.Checksum[:])
	off += 32
	binary.BigEndian.PutUint64(buf[off:], resp.TotalSupply)
	off += 8
	binary.BigEndian.PutUint64(buf[off:], resp.CoinsTotal)
	off += 8
	binary.BigEndian.PutUint64(buf[off:], resp.TotalBonded)
	off += 8
	binary.BigEndian.PutUint64(buf[off:], resp.Deposits)
	off += 8
	binary.BigEndian.PutUint64(buf[off:], resp.FeesInFlight)
	off += 8
	copy(buf[off:], resp.Err)

	return buf
}

// DecodeStateFingerprintResp decodes a state-fingerprint response.
func DecodeStateFingerprintResp(data []byte) (*FingerprintResponse, error) {
	if len(data) < 1+fingerprintRespFixedLen || data[0] != MsgTagStateFingerprintResp {
		return nil, fmt.Errorf("not a state-fingerprint response")
	}

	resp := &FingerprintResponse{}

	off := 1
	resp.Round = binary.BigEndian.Uint64(data[off:])
	off += 8
	copy(resp.Checksum[:], data[off:])
	off += 32
	resp.TotalSupply = binary.BigEndian.Uint64(data[off:])
	off += 8
	resp.CoinsTotal = binary.BigEndian.Uint64(data[off:])
	off += 8
	resp.TotalBonded = binary.BigEndian.Uint64(data[off:])
	off += 8
	resp.Deposits = binary.BigEndian.Uint64(data[off:])
	off += 8
	resp.FeesInFlight = binary.BigEndian.Uint64(data[off:])
	off += 8
	resp.Err = string(data[off:])

	return resp, nil
}

// TestControlRequest carries a test-only network-control operation: Op
// TestControlOpSetPartition replaces the node's blocklist with Pubkeys, Op
// TestControlOpClearPartition clears it (Pubkeys is ignored). Served only when
// the node was started with --test-hooks.
type TestControlRequest struct {
	Op      byte       // Op selects the operation (TestControlOp* constants)
	Pubkeys [][32]byte // Pubkeys is the partition side to block, for the set operation
}

// EncodeTestControl encodes a test-control request.
// Format: [1B tag] [1B op] [4B count] [32B pubkey]*count.
func EncodeTestControl(req *TestControlRequest) []byte {
	buf := make([]byte, 1+1+4+32*len(req.Pubkeys))
	buf[0] = MsgTagTestControl
	buf[1] = req.Op
	binary.BigEndian.PutUint32(buf[2:6], uint32(len(req.Pubkeys)))

	off := 6
	for _, pk := range req.Pubkeys {
		copy(buf[off:], pk[:])
		off += 32
	}

	return buf
}

// DecodeTestControl decodes a test-control request.
func DecodeTestControl(data []byte) (*TestControlRequest, error) {
	if len(data) < 6 || data[0] != MsgTagTestControl {
		return nil, fmt.Errorf("not a test-control message")
	}

	req := &TestControlRequest{Op: data[1]}
	count := int(binary.BigEndian.Uint32(data[2:6]))

	off := 6
	if len(data) < off+32*count {
		return nil, fmt.Errorf("test-control message truncated")
	}

	req.Pubkeys = make([][32]byte, count)
	for i := 0; i < count; i++ {
		copy(req.Pubkeys[i][:], data[off:])
		off += 32
	}

	return req, nil
}

// TestControlResponse is the response to a test-control operation. Err is
// non-empty on failure (e.g. --test-hooks disabled, or an unknown Op).
type TestControlResponse struct {
	Err string // Err is a non-empty error message on failure
}

// EncodeTestControlResp encodes a test-control response.
// Format: [1B tag] [errString].
func EncodeTestControlResp(resp *TestControlResponse) []byte {
	buf := make([]byte, 1+len(resp.Err))
	buf[0] = MsgTagTestControlResp
	copy(buf[1:], resp.Err)

	return buf
}

// DecodeTestControlResp decodes a test-control response.
func DecodeTestControlResp(data []byte) (*TestControlResponse, error) {
	if len(data) < 1 || data[0] != MsgTagTestControlResp {
		return nil, fmt.Errorf("not a test-control response")
	}

	return &TestControlResponse{Err: string(data[1:])}, nil
}
