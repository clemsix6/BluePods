package aggregation

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

// Request flags.
const (
	flagWantFull = 0x01 // Requester wants the full object data
)

// Negative response reasons.
const (
	reasonNotFound    = 0x01 // Object not found
	reasonWrongVersion = 0x02 // Object version mismatch
)

// AttestationRequest is the request sent to holders.
type AttestationRequest struct {
	ObjectID [32]byte // ObjectID is the object to attest
	Version  uint64   // Version is the expected version
	WantFull bool     // WantFull indicates if full object data is requested
}

// EncodeRequest encodes an attestation request to bytes.
// Format: [1B type] [32B objectID] [8B version] [1B flags]
func EncodeRequest(req *AttestationRequest) []byte {
	buf := make([]byte, 42)
	buf[0] = msgTypeRequest
	copy(buf[1:33], req.ObjectID[:])
	binary.BigEndian.PutUint64(buf[33:41], req.Version)

	if req.WantFull {
		buf[41] = flagWantFull
	}

	return buf
}

// DecodeRequest decodes an attestation request from bytes.
func DecodeRequest(data []byte) (*AttestationRequest, error) {
	if len(data) < 42 {
		return nil, fmt.Errorf("request too short: %d < 42", len(data))
	}

	if data[0] != msgTypeRequest {
		return nil, fmt.Errorf("invalid message type: 0x%02x", data[0])
	}

	req := &AttestationRequest{
		Version:  binary.BigEndian.Uint64(data[33:41]),
		WantFull: data[41]&flagWantFull != 0,
	}
	copy(req.ObjectID[:], data[1:33])

	return req, nil
}

// PositiveResponse is a positive attestation response.
type PositiveResponse struct {
	Hash      [32]byte // Hash is BLAKE3(content || version)
	Signature []byte   // Signature is the BLS signature (96 bytes)
	Data      []byte   // Data is the full object (optional, only if requested)
}

// EncodePositiveResponse encodes a positive response to bytes.
// Format: [1B type] [32B hash] [96B sig] [4B dataLen] [NB data]
func EncodePositiveResponse(resp *PositiveResponse) []byte {
	dataLen := len(resp.Data)
	buf := make([]byte, 1+32+96+4+dataLen)

	buf[0] = msgTypePositive
	copy(buf[1:33], resp.Hash[:])
	copy(buf[33:129], resp.Signature)
	binary.BigEndian.PutUint32(buf[129:133], uint32(dataLen))

	if dataLen > 0 {
		copy(buf[133:], resp.Data)
	}

	return buf
}

// DecodePositiveResponse decodes a positive response from bytes.
func DecodePositiveResponse(data []byte) (*PositiveResponse, error) {
	if len(data) < 133 {
		return nil, fmt.Errorf("positive response too short: %d < 133", len(data))
	}

	if data[0] != msgTypePositive {
		return nil, fmt.Errorf("invalid message type: 0x%02x", data[0])
	}

	dataLen := binary.BigEndian.Uint32(data[129:133])

	if len(data) < 133+int(dataLen) {
		return nil, fmt.Errorf("data truncated: need %d, have %d", 133+dataLen, len(data))
	}

	resp := &PositiveResponse{
		Signature: make([]byte, 96),
	}
	copy(resp.Hash[:], data[1:33])
	copy(resp.Signature, data[33:129])

	if dataLen > 0 {
		resp.Data = make([]byte, dataLen)
		copy(resp.Data, data[133:133+dataLen])
	}

	return resp, nil
}

// NegativeResponse is a negative attestation response.
type NegativeResponse struct {
	Reason    byte   // Reason is the error code
	Signature []byte // Signature is the BLS signature over the negative attestation
}

// EncodeNegativeResponse encodes a negative response to bytes.
// Format: [1B type] [1B reason] [96B sig]
func EncodeNegativeResponse(resp *NegativeResponse) []byte {
	buf := make([]byte, 1+1+96)

	buf[0] = msgTypeNegative
	buf[1] = resp.Reason
	copy(buf[2:98], resp.Signature)

	return buf
}

// DecodeNegativeResponse decodes a negative response from bytes.
func DecodeNegativeResponse(data []byte) (*NegativeResponse, error) {
	if len(data) < 98 {
		return nil, fmt.Errorf("negative response too short: %d < 98", len(data))
	}

	if data[0] != msgTypeNegative {
		return nil, fmt.Errorf("invalid message type: 0x%02x", data[0])
	}

	resp := &NegativeResponse{
		Reason:    data[1],
		Signature: make([]byte, 96),
	}
	copy(resp.Signature, data[2:98])

	return resp, nil
}

// GetMessageType returns the type byte of an encoded message.
func GetMessageType(data []byte) (byte, error) {
	if len(data) == 0 {
		return 0, fmt.Errorf("empty message")
	}

	return data[0], nil
}
