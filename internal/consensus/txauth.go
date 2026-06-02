package consensus

import (
	"bytes"
	"crypto/ed25519"
	"fmt"

	"github.com/zeebo/blake3"

	"BluePods/internal/genesis"
	"BluePods/internal/types"
)

// verifyTxAuthenticity re-derives a transaction's canonical body hash and checks
// it against the declared hash and the sender's Ed25519 signature. It runs in the
// commit path on every node so a transaction that reaches commit via a gossiped
// vertex (bypassing local ingress validation) cannot commit unverified.
func verifyTxAuthenticity(tx *types.Transaction) error {
	body := rebuildUnsignedTxBody(tx)
	hash := blake3.Sum256(body)

	if !bytes.Equal(hash[:], tx.HashBytes()) {
		return fmt.Errorf("tx hash mismatch: recomputed body hash differs from declared hash")
	}

	sender := tx.SenderBytes()
	if len(sender) != ed25519.PublicKeySize {
		return fmt.Errorf("invalid sender length: %d", len(sender))
	}

	if !ed25519.Verify(sender, hash[:], tx.SignatureBytes()) {
		return fmt.Errorf("tx signature does not verify against sender")
	}

	return nil
}

// rebuildUnsignedTxBody reconstructs the unsigned transaction bytes via the same
// shared primitive client ingress uses (genesis.BuildUnsignedTxBytesWithRefs), so
// the builder, ingress validation, and commit-time check cannot drift.
func rebuildUnsignedTxBody(tx *types.Transaction) []byte {
	mutableRefs := extractTxRefData(tx, true)
	readRefs := extractTxRefData(tx, false)
	cor := extractTxCreatedReps(tx)

	var pod [32]byte
	if b := tx.PodBytes(); len(b) == 32 {
		copy(pod[:], b)
	}

	return genesis.BuildUnsignedTxBytesSponsored(
		tx.SenderBytes(),
		pod,
		string(tx.FunctionName()),
		tx.ArgsBytes(),
		cor,
		tx.MaxCreateDomains(),
		tx.MaxGas(),
		tx.GasCoinBytes(),
		mutableRefs,
		readRefs,
		genesis.Sponsorship{FeePayer: tx.FeePayerBytes(), ValidUntil: tx.ValidUntil()},
	)
}

// extractTxCreatedReps reads the created_objects_replication vector from a tx.
func extractTxCreatedReps(tx *types.Transaction) []uint16 {
	count := tx.CreatedObjectsReplicationLength()
	if count == 0 {
		return nil
	}

	result := make([]uint16, count)
	for i := 0; i < count; i++ {
		result[i] = tx.CreatedObjectsReplication(i)
	}

	return result
}

// extractTxRefData extracts ObjectRefData from a transaction's mutable or read
// refs, preserving the order the canonical body builder expects.
func extractTxRefData(tx *types.Transaction, mutable bool) []genesis.ObjectRefData {
	count := tx.ReadRefsLength()
	if mutable {
		count = tx.MutableRefsLength()
	}

	if count == 0 {
		return nil
	}

	refs := make([]genesis.ObjectRefData, count)
	var ref types.ObjectRef

	for i := 0; i < count; i++ {
		if mutable {
			tx.MutableRefs(&ref, i)
		} else {
			tx.ReadRefs(&ref, i)
		}

		if idBytes := ref.IdBytes(); len(idBytes) == 32 {
			copy(refs[i].ID[:], idBytes)
		}

		refs[i].Version = ref.Version()
		refs[i].Domain = string(ref.Domain())
	}

	return refs
}
