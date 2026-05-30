package aggregation

import (
	"fmt"

	"BluePods/internal/attest"
	"BluePods/internal/network"
	"BluePods/internal/state"
	"BluePods/internal/storage"
	"BluePods/internal/types"
)

// Handler processes attestation requests from other validators.
type Handler struct {
	state    *state.State                                     // state is the local storage for object lookup
	blsKey   *BLSKeyPair                                      // blsKey is the validator's BLS signing key
	db       *storage.Storage                                 // db backs the durable per-object signature store
	isHolder func(objectID [32]byte, replication uint16) bool // isHolder reports whether this node holds an object
}

// NewHandler creates a new attestation request Handler.
// db backs the durable signature store and isHolder bounds the sign-on-miss
// fallback to objects this node actually holds.
func NewHandler(st *state.State, blsKey *BLSKeyPair, db *storage.Storage, isHolder func(objectID [32]byte, replication uint16) bool) *Handler {
	return &Handler{
		state:    st,
		blsKey:   blsKey,
		db:       db,
		isHolder: isHolder,
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

// processRequest handles a decoded attestation request. The object must exist
// locally, be replicated (singletons are never attested), and be at the
// requested current version. It serves the stored signature when present, and
// otherwise signs once and stores it, but only for an object this node holds at
// its current version. Every rejection is a cheap static negative with no BLS
// signature, so an invalid request never costs a signature.
func (h *Handler) processRequest(req *AttestationRequest) ([]byte, error) {
	objectData := h.state.GetObject(req.ObjectID)
	if objectData == nil {
		return buildNegativeResponse(reasonNotFound), nil
	}

	fbObj := types.GetRootAsObject(objectData, 0)

	// Singletons (replication 0) are held by every validator and never attested.
	// Reject before any signing work.
	if fbObj.Replication() == 0 {
		return buildNegativeResponse(reasonNotFound), nil
	}

	if fbObj.Version() != req.Version {
		return buildNegativeResponse(reasonWrongVersion), nil
	}

	hash := attest.ComputeObjectHash(fbObj.ContentBytes(), req.Version)

	sig, ok := h.signatureForCurrent(req.ObjectID, req.Version, fbObj, hash)
	if !ok {
		return buildNegativeResponse(reasonNotFound), nil
	}

	return EncodePositiveResponse(&PositiveResponse{Hash: hash, Signature: sig}), nil
}

// signatureForCurrent returns the BLS signature for the object's current version.
// It serves a matching stored signature, otherwise (bounded fallback) signs and
// stores one only when this node holds the object at the requested version.
// It returns ok=false when the request is not for a held, current version.
func (h *Handler) signatureForCurrent(id [32]byte, version uint64, obj *types.Object, hash [32]byte) ([]byte, bool) {
	if h.db != nil {
		if storedVersion, sig, found := GetObjectSig(h.db, id); found && storedVersion == version {
			return sig, true
		}
	}

	// Store miss: only sign for an object we actually hold at its current version.
	if h.isHolder != nil && !h.isHolder(id, obj.Replication()) {
		return nil, false
	}

	sig := h.blsKey.Sign(hash[:])

	if h.db != nil {
		if err := PutObjectSig(h.db, id, version, sig); err != nil {
			// Storage failure is non-fatal: the signature is still returned.
			_ = err
		}
	}

	return sig, true
}

// buildNegativeResponse creates a static negative attestation response. Negative
// responses are never signed: an invalid request costs a lookup, not a signature.
func buildNegativeResponse(reason byte) []byte {
	return EncodeNegativeResponse(&NegativeResponse{Reason: reason})
}
