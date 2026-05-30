package aggregation

import (
	"bytes"
	"fmt"

	"BluePods/internal/attest"
	"BluePods/internal/types"
	"BluePods/internal/validators"
)

// EpochResolver supplies the epoch-boundary information the ATX verifier needs:
// the holder snapshot for a given epoch, the commit epoch a round belongs to,
// the per-epoch round count, and the grace window. The consensus DAG implements
// it; injecting it keeps aggregation free of a consensus import.
type EpochResolver struct {
	// HoldersForEpoch returns the holder snapshot for an epoch, or false when the
	// epoch is too old or in the future.
	HoldersForEpoch func(epoch uint64) (*validators.ValidatorSet, bool)

	// CommitEpochForRound returns the epoch an ATX committed at a round belongs to.
	CommitEpochForRound func(round uint64) uint64

	// EpochLength is the number of rounds per epoch (0 when epochs are disabled).
	EpochLength uint64

	// GraceRounds is the number of rounds after a boundary that the previous
	// epoch's attestations are still accepted.
	GraceRounds uint64
}

// ATXVerifier verifies the BLS quorum proofs carried in an attested transaction.
// It selects the holder snapshot from the epoch the attestations were collected
// in and validates that epoch against the commit round (same epoch, or the
// previous epoch within the grace window).
type ATXVerifier struct {
	// epoch resolves epoch-boundary information from the consensus layer.
	epoch EpochResolver
}

// NewATXVerifier creates an ATXVerifier driven by the given epoch resolver.
func NewATXVerifier(epoch EpochResolver) *ATXVerifier {
	return &ATXVerifier{epoch: epoch}
}

// Verify checks every QuorumProof in the ATX against its included object and the
// holder snapshot of the attestation epoch carried on the wire. It validates the
// attestation epoch against the deterministic commit epoch and returns an error
// on the first invalid proof.
func (v *ATXVerifier) Verify(atx *types.AttestedTransaction, commitRound uint64) error {
	vs, err := v.resolveHolders(atx.AttestationEpoch(), commitRound)
	if err != nil {
		return err
	}

	var proof types.QuorumProof

	for i := 0; i < atx.ProofsLength(); i++ {
		if !atx.Proofs(&proof, i) {
			return fmt.Errorf("cannot read proof %d", i)
		}

		if err := v.verifySingleProof(atx, &proof, vs); err != nil {
			return fmt.Errorf("proof %d:\n%w", i, err)
		}
	}

	return nil
}

// resolveHolders validates the attestation epoch against the commit round and
// returns the holder snapshot to recompute the quorum against. It accepts the
// commit epoch, or the previous epoch when the commit round is within the grace
// window of the boundary.
func (v *ATXVerifier) resolveHolders(attestationEpoch, commitRound uint64) (*validators.ValidatorSet, error) {
	commitEpoch := v.epoch.CommitEpochForRound(commitRound)

	switch {
	case attestationEpoch == commitEpoch:
		// Current epoch: always valid.
	case commitEpoch > 0 && attestationEpoch == commitEpoch-1:
		if !v.withinGrace(commitRound) {
			return nil, fmt.Errorf("attestation epoch %d past grace at round %d", attestationEpoch, commitRound)
		}
	default:
		return nil, fmt.Errorf("attestation epoch %d invalid for commit epoch %d", attestationEpoch, commitEpoch)
	}

	vs, ok := v.epoch.HoldersForEpoch(attestationEpoch)
	if !ok {
		return nil, fmt.Errorf("no holder snapshot for epoch %d", attestationEpoch)
	}

	return vs, nil
}

// withinGrace reports whether a commit round is still within the grace window of
// the boundary that separates the commit epoch from the previous one. That
// boundary is round commitEpoch*epochLength (the round where the previous epoch
// transitioned into the commit epoch).
func (v *ATXVerifier) withinGrace(commitRound uint64) bool {
	if v.epoch.EpochLength == 0 {
		return false
	}

	commitEpoch := v.epoch.CommitEpochForRound(commitRound)
	boundary := commitEpoch * v.epoch.EpochLength

	if commitRound < boundary {
		return false
	}

	return commitRound-boundary < v.epoch.GraceRounds
}

// verifySingleProof verifies one QuorumProof against the ATX objects and the
// given holder snapshot.
func (v *ATXVerifier) verifySingleProof(atx *types.AttestedTransaction, proof *types.QuorumProof, vs *validators.ValidatorSet) error {
	objIdx := findATXObjectIndex(atx, proof.ObjectIdBytes())
	if objIdx < 0 {
		return fmt.Errorf("object not found in ATX")
	}

	var obj types.Object
	if !atx.Objects(&obj, objIdx) {
		return fmt.Errorf("cannot read object at index %d", objIdx)
	}

	// Recompute the canonical hash from the object's content bytes.
	hash := attest.ComputeObjectHash(obj.ContentBytes(), obj.Version())

	var objectID [32]byte
	copy(objectID[:], proof.ObjectIdBytes())
	holders := attest.ComputeHolders(vs, objectID, int(obj.Replication()))

	blsKeys, signerCount := extractSignerBLSKeys(proof.SignerBitmapBytes(), holders, vs)
	if signerCount == 0 {
		return fmt.Errorf("no signers in bitmap")
	}

	quorum := attest.QuorumSize(len(holders))
	if signerCount < quorum {
		return fmt.Errorf("insufficient signers: got %d, need %d", signerCount, quorum)
	}

	if !attest.VerifyAggregated(proof.BlsSignatureBytes(), hash[:], blsKeys) {
		return fmt.Errorf("aggregated BLS signature invalid")
	}

	return nil
}

// findATXObjectIndex returns the index of the object with the given ID in the ATX, or -1.
func findATXObjectIndex(atx *types.AttestedTransaction, objectID []byte) int {
	var obj types.Object

	for i := 0; i < atx.ObjectsLength(); i++ {
		if !atx.Objects(&obj, i) {
			continue
		}

		if bytes.Equal(obj.IdBytes(), objectID) {
			return i
		}
	}

	return -1
}

// extractSignerBLSKeys maps a signer bitmap to BLS public keys via rendezvous holders.
// Returns the BLS keys and the number of signers.
func extractSignerBLSKeys(bitmap []byte, holders []validators.Hash, vs *validators.ValidatorSet) ([][]byte, int) {
	indices := attest.ParseSignerBitmap(bitmap)
	var keys [][]byte

	for _, idx := range indices {
		if idx >= len(holders) {
			continue
		}

		info := vs.Get(holders[idx])
		if info == nil || info.BLSPubkey == [48]byte{} {
			continue
		}

		keys = append(keys, info.BLSPubkey[:])
	}

	return keys, len(keys)
}
