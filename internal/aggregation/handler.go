package aggregation

import (
	"encoding/binary"
	"fmt"

	"BluePods/internal/network"
	"BluePods/internal/state"
	"BluePods/internal/types"

	"github.com/zeebo/blake3"
)

// Handler processes attestation requests from other validators.
type Handler struct {
	state  *state.State // state is the local storage for object lookup
	blsKey *BLSKeyPair  // blsKey is the validator's BLS signing key
}

// NewHandler creates a new attestation request Handler.
func NewHandler(st *state.State, blsKey *BLSKeyPair) *Handler {
	return &Handler{
		state:  st,
		blsKey: blsKey,
	}
}

// HandleRequest processes an attestation request and returns the response.
// Designed to be used as network.Node.OnRequest handler.
func (h *Handler) HandleRequest(peer *network.Peer, data []byte) ([]byte, error) {
	req, err := DecodeRequest(data)
	if err != nil {
		return nil, fmt.Errorf("decode request:\n%w", err)
	}

	return h.processRequest(req)
}

// processRequest handles a decoded attestation request.
func (h *Handler) processRequest(req *AttestationRequest) ([]byte, error) {
	objectData := h.state.GetObject(req.ObjectID)
	if objectData == nil {
		return h.buildNegativeResponse(reasonNotFound), nil
	}

	fbObj := types.GetRootAsObject(objectData, 0)
	if fbObj.Version() != req.Version {
		return h.buildNegativeResponse(reasonWrongVersion), nil
	}

	hash := computeObjectHash(objectData, req.Version)
	sig := h.blsKey.Sign(hash[:])

	return h.buildPositiveResponse(objectData, hash, sig, req.WantFull), nil
}

// buildPositiveResponse creates a positive attestation response.
func (h *Handler) buildPositiveResponse(obj []byte, hash [32]byte, sig []byte, wantFull bool) []byte {
	resp := &PositiveResponse{
		Hash:      hash,
		Signature: sig,
	}

	if wantFull {
		resp.Data = obj
	}

	return EncodePositiveResponse(resp)
}

// buildNegativeResponse creates a negative attestation response.
func (h *Handler) buildNegativeResponse(reason byte) []byte {
	// Sign the negative attestation for accountability
	msg := []byte{reason}
	sig := h.blsKey.Sign(msg)

	resp := &NegativeResponse{
		Reason:    reason,
		Signature: sig,
	}

	return EncodeNegativeResponse(resp)
}

// computeObjectHash computes BLAKE3(content || version) for signing.
func computeObjectHash(content []byte, version uint64) [32]byte {
	hasher := blake3.New()
	hasher.Write(content)

	var versionBytes [8]byte
	binary.BigEndian.PutUint64(versionBytes[:], version)
	hasher.Write(versionBytes[:])

	var hash [32]byte
	hasher.Sum(hash[:0])

	return hash
}
