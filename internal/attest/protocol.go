package attest

import (
	"encoding/binary"
	"fmt"
)

// Message types for the attestation protocol.
const (
	msgTypeRequest  = 0x01 // Request for attestation
	msgTypePositive = 0x02 // Positive attestation (object found)
	msgTypeNegative = 0x03 // Negative attestation (object not found)
)

// Negative response reasons.
const (
	ReasonNotFound     = 0x01 // ReasonNotFound: object not found
	ReasonWrongVersion = 0x02 // ReasonWrongVersion: object version mismatch
)

// MsgTypePositive is the message-type byte of a positive attestation response.
const MsgTypePositive = msgTypePositive

// MsgTypeNegative is the message-type byte of a negative attestation response.
const MsgTypeNegative = msgTypeNegative

// AttestationRequest is the request sent to holders. It carries only the object
// and the expected version; the response never includes the object itself, so a
// caller fetches the data with a separate GetObject request when it needs it.
type AttestationRequest struct {
	ObjectID [32]byte // ObjectID is the object to attest
	Version  uint64   // Version is the expected version
}

// EncodeRequest encodes an attestation request to bytes.
// Format: [1B type] [32B objectID] [8B version]
func EncodeRequest(req *AttestationRequest) []byte {
	buf := make([]byte, 41)
	buf[0] = msgTypeRequest
	copy(buf[1:33], req.ObjectID[:])
	binary.BigEndian.PutUint64(buf[33:41], req.Version)

	return buf
}

// DecodeRequest decodes an attestation request from bytes.
func DecodeRequest(data []byte) (*AttestationRequest, error) {
	if len(data) < 41 {
		return nil, fmt.Errorf("request too short: %d < 41", len(data))
	}

	if data[0] != msgTypeRequest {
		return nil, fmt.Errorf("invalid message type: 0x%02x", data[0])
	}

	req := &AttestationRequest{
		Version: binary.BigEndian.Uint64(data[33:41]),
	}
	copy(req.ObjectID[:], data[1:33])

	return req, nil
}

// PositiveResponse is a positive attestation response. It carries only the hash
// and the BLS signature; the object data travels in a separate GetObject reply.
type PositiveResponse struct {
	Hash      [32]byte // Hash is BLAKE3(content || version)
	Signature []byte   // Signature is the BLS signature (96 bytes)
}

// EncodePositiveResponse encodes a positive response to bytes.
// Format: [1B type] [32B hash] [96B sig]
func EncodePositiveResponse(resp *PositiveResponse) []byte {
	buf := make([]byte, 1+32+96)

	buf[0] = msgTypePositive
	copy(buf[1:33], resp.Hash[:])
	copy(buf[33:129], resp.Signature)

	return buf
}

// DecodePositiveResponse decodes a positive response from bytes.
func DecodePositiveResponse(data []byte) (*PositiveResponse, error) {
	if len(data) < 129 {
		return nil, fmt.Errorf("positive response too short: %d < 129", len(data))
	}

	if data[0] != msgTypePositive {
		return nil, fmt.Errorf("invalid message type: 0x%02x", data[0])
	}

	resp := &PositiveResponse{
		Signature: make([]byte, 96),
	}
	copy(resp.Hash[:], data[1:33])
	copy(resp.Signature, data[33:129])

	return resp, nil
}

// NegativeResponse is a negative attestation response. It carries only a reason
// code and is never signed: an invalid request costs a cheap static reply, not a
// BLS signature.
type NegativeResponse struct {
	Reason byte // Reason is the error code
}

// EncodeNegativeResponse encodes a negative response to bytes.
// Format: [1B type] [1B reason]
func EncodeNegativeResponse(resp *NegativeResponse) []byte {
	return []byte{msgTypeNegative, resp.Reason}
}

// DecodeNegativeResponse decodes a negative response from bytes.
func DecodeNegativeResponse(data []byte) (*NegativeResponse, error) {
	if len(data) < 2 {
		return nil, fmt.Errorf("negative response too short: %d < 2", len(data))
	}

	if data[0] != msgTypeNegative {
		return nil, fmt.Errorf("invalid message type: 0x%02x", data[0])
	}

	return &NegativeResponse{Reason: data[1]}, nil
}

// GetMessageType returns the type byte of an encoded message.
func GetMessageType(data []byte) (byte, error) {
	if len(data) == 0 {
		return 0, fmt.Errorf("empty message")
	}

	return data[0], nil
}

// IsAttestationRequest returns true if data is an attestation request.
// Check: data[0] == 0x01. No conflict with sync (FlatBuffers starts with
// a 4-byte offset, never 0x01 as first byte for valid messages).
func IsAttestationRequest(data []byte) bool {
	return len(data) > 0 && data[0] == msgTypeRequest
}
