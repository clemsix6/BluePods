package aggregation

import "BluePods/internal/attest"

// Attestation message-type and reason bytes re-exported from the attest codec.
const (
	msgTypePositive = attest.MsgTypePositive // positive attestation response
	msgTypeNegative = attest.MsgTypeNegative // negative attestation response

	reasonNotFound     = attest.ReasonNotFound     // object not found
	reasonWrongVersion = attest.ReasonWrongVersion // object version mismatch
)

// AttestationRequest is the request sent to holders.
type AttestationRequest = attest.AttestationRequest

// PositiveResponse is a positive attestation response.
type PositiveResponse = attest.PositiveResponse

// NegativeResponse is a negative attestation response.
type NegativeResponse = attest.NegativeResponse

// EncodeRequest encodes an attestation request to bytes.
func EncodeRequest(req *AttestationRequest) []byte {
	return attest.EncodeRequest(req)
}

// DecodeRequest decodes an attestation request from bytes.
func DecodeRequest(data []byte) (*AttestationRequest, error) {
	return attest.DecodeRequest(data)
}

// EncodePositiveResponse encodes a positive response to bytes.
func EncodePositiveResponse(resp *PositiveResponse) []byte {
	return attest.EncodePositiveResponse(resp)
}

// DecodePositiveResponse decodes a positive response from bytes.
func DecodePositiveResponse(data []byte) (*PositiveResponse, error) {
	return attest.DecodePositiveResponse(data)
}

// EncodeNegativeResponse encodes a negative response to bytes.
func EncodeNegativeResponse(resp *NegativeResponse) []byte {
	return attest.EncodeNegativeResponse(resp)
}

// DecodeNegativeResponse decodes a negative response from bytes.
func DecodeNegativeResponse(data []byte) (*NegativeResponse, error) {
	return attest.DecodeNegativeResponse(data)
}

// GetMessageType returns the type byte of an encoded message.
func GetMessageType(data []byte) (byte, error) {
	return attest.GetMessageType(data)
}

// IsAttestationRequest returns true if data is an attestation request.
func IsAttestationRequest(data []byte) bool {
	return attest.IsAttestationRequest(data)
}
