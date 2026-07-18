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

// DecodeRequest decodes an attestation request from bytes.
func DecodeRequest(data []byte) (*AttestationRequest, error) {
	return attest.DecodeRequest(data)
}

// EncodePositiveResponse encodes a positive response to bytes.
func EncodePositiveResponse(resp *PositiveResponse) []byte {
	return attest.EncodePositiveResponse(resp)
}

// EncodeNegativeResponse encodes a negative response to bytes.
func EncodeNegativeResponse(resp *NegativeResponse) []byte {
	return attest.EncodeNegativeResponse(resp)
}

// IsAttestationRequest returns true if data is an attestation request.
func IsAttestationRequest(data []byte) bool {
	return attest.IsAttestationRequest(data)
}
