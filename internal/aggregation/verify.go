package aggregation

import (
	"bytes"
	"fmt"

	"BluePods/internal/attest"
	"BluePods/internal/types"
	"BluePods/internal/validators"
)

// ATXVerifier verifies the BLS quorum proofs carried in an attested transaction.
// It recomputes each object's holders from the validator set provided by holdersFn,
// so it always uses the current epoch's holder snapshot.
type ATXVerifier struct {
	// holdersFn returns the validator set used to recompute object holders.
	holdersFn func() *validators.ValidatorSet
}

// NewATXVerifier creates an ATXVerifier that resolves holders via holdersFn.
func NewATXVerifier(holdersFn func() *validators.ValidatorSet) *ATXVerifier {
	return &ATXVerifier{holdersFn: holdersFn}
}

// Verify checks every QuorumProof in the ATX against its included object and
// the validators' BLS keys. It returns an error on the first invalid proof.
func (v *ATXVerifier) Verify(atx *types.AttestedTransaction) error {
	var proof types.QuorumProof

	for i := 0; i < atx.ProofsLength(); i++ {
		if !atx.Proofs(&proof, i) {
			return fmt.Errorf("cannot read proof %d", i)
		}

		if err := v.verifySingleProof(atx, &proof); err != nil {
			return fmt.Errorf("proof %d:\n%w", i, err)
		}
	}

	return nil
}

// verifySingleProof verifies one QuorumProof against the ATX objects and BLS keys.
func (v *ATXVerifier) verifySingleProof(atx *types.AttestedTransaction, proof *types.QuorumProof) error {
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

	vs := v.holdersFn()

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
