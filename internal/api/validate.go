package api

import (
	"crypto/ed25519"
	"fmt"

	"github.com/zeebo/blake3"

	"BluePods/internal/genesis"
	"BluePods/internal/types"
)

const (
	// hashSize is the expected size of a transaction hash.
	hashSize = 32

	// senderSize is the expected size of an Ed25519 public key.
	senderSize = 32

	// signatureSize is the expected size of an Ed25519 signature.
	signatureSize = 64

	// podSize is the expected size of a pod identifier.
	podSize = 32

	// maxObjectRefs is the maximum total number of object references per transaction.
	// Spec: 8 standard objects + 32 singletons = 40 max refs.
	maxObjectRefs = 40
)

// validateTx validates a raw Transaction before aggregation.
// Checks structural integrity, hash correctness, and Ed25519 signature.
func validateTx(data []byte) (retErr error) {
	// FlatBuffers panics on malformed data, recover gracefully
	defer func() {
		if r := recover(); r != nil {
			retErr = fmt.Errorf("malformed transaction data")
		}
	}()

	if len(data) < 8 {
		return fmt.Errorf("transaction data too short")
	}

	tx := types.GetRootAsTransaction(data, 0)

	if err := validateFieldSizes(tx); err != nil {
		return err
	}

	if err := validateObjectRefs(tx); err != nil {
		return err
	}

	if err := validateHash(tx); err != nil {
		return err
	}

	return validateSignature(tx)
}

// validateFieldSizes checks that all fixed-size fields have the correct length.
func validateFieldSizes(tx *types.Transaction) error {
	if len(tx.HashBytes()) != hashSize {
		return fmt.Errorf("invalid hash size: got %d, want %d", len(tx.HashBytes()), hashSize)
	}

	if len(tx.SenderBytes()) != senderSize {
		return fmt.Errorf("invalid sender size: got %d, want %d", len(tx.SenderBytes()), senderSize)
	}

	if len(tx.SignatureBytes()) != signatureSize {
		return fmt.Errorf("invalid signature size: got %d, want %d", len(tx.SignatureBytes()), signatureSize)
	}

	if len(tx.PodBytes()) != podSize {
		return fmt.Errorf("invalid pod size: got %d, want %d", len(tx.PodBytes()), podSize)
	}

	if len(tx.FunctionName()) == 0 {
		return fmt.Errorf("empty function name")
	}

	// gas_coin must be 0 (absent) or 32 bytes
	gasCoinLen := len(tx.GasCoinBytes())
	if gasCoinLen != 0 && gasCoinLen != 32 {
		return fmt.Errorf("invalid gas_coin size: got %d, want 0 or 32", gasCoinLen)
	}

	return nil
}

// validateObjectRefs checks that object references are well-formed and within limits.
func validateObjectRefs(tx *types.Transaction) error {
	readCount := tx.ReadRefsLength()
	mutableCount := tx.MutableRefsLength()
	totalRefs := readCount + mutableCount

	if totalRefs > maxObjectRefs {
		return fmt.Errorf("too many object refs: %d (max %d)", totalRefs, maxObjectRefs)
	}

	return validateNoDuplicateRefs(tx)
}

// validateNoDuplicateRefs ensures no object ID or domain name appears twice across read and mutable refs.
func validateNoDuplicateRefs(tx *types.Transaction) error {
	seen := make(map[[32]byte]bool)
	seenDomains := make(map[string]bool)

	if err := checkRefDuplicates(tx, true, seen, seenDomains); err != nil {
		return err
	}

	return checkRefDuplicates(tx, false, seen, seenDomains)
}

// checkRefDuplicates scans ObjectRef entries for duplicate IDs or domain names.
func checkRefDuplicates(tx *types.Transaction, mutable bool, seen map[[32]byte]bool, seenDomains map[string]bool) error {
	var count int
	if mutable {
		count = tx.MutableRefsLength()
	} else {
		count = tx.ReadRefsLength()
	}

	fieldName := "read_refs"
	if mutable {
		fieldName = "mutable_refs"
	}

	var ref types.ObjectRef
	for i := 0; i < count; i++ {
		if mutable {
			tx.MutableRefs(&ref, i)
		} else {
			tx.ReadRefs(&ref, i)
		}

		// Domain refs: check for duplicate domain names
		if len(ref.Domain()) > 0 {
			domain := string(ref.Domain())
			if seenDomains[domain] {
				return fmt.Errorf("duplicate domain ref: %q", domain)
			}
			seenDomains[domain] = true
			continue
		}

		idBytes := ref.IdBytes()
		if len(idBytes) != 32 {
			return fmt.Errorf("invalid object ref ID size in %s: got %d, want 32", fieldName, len(idBytes))
		}

		var id [32]byte
		copy(id[:], idBytes)

		if seen[id] {
			return fmt.Errorf("duplicate object ref in %s: %x", fieldName, id[:8])
		}

		seen[id] = true
	}

	return nil
}

// validateHash recomputes the transaction hash and compares it to the declared hash.
// The hash is blake3 of the unsigned transaction (all fields except hash and signature).
func validateHash(tx *types.Transaction) error {
	unsignedBytes := rebuildUnsignedTx(tx)
	expected := blake3.Sum256(unsignedBytes)

	hash := tx.HashBytes()

	for i := 0; i < hashSize; i++ {
		if hash[i] != expected[i] {
			return fmt.Errorf("hash mismatch")
		}
	}

	return nil
}

// rebuildUnsignedTx reconstructs the unsigned transaction bytes for hash verification.
// Must match the client's BuildUnsignedTxBytesWithRefs construction order exactly.
func rebuildUnsignedTx(tx *types.Transaction) []byte {
	mutableRefs := extractRefData(tx, true)
	readRefs := extractRefData(tx, false)
	cor := extractCreatedObjectsReplication(tx)

	return genesis.BuildUnsignedTxBytesWithRefs(
		tx.SenderBytes(),
		extractPod(tx),
		string(tx.FunctionName()),
		tx.ArgsBytes(),
		cor,
		tx.MaxCreateDomains(),
		tx.MaxGas(),
		tx.GasCoinBytes(),
		mutableRefs,
		readRefs,
	)
}

// extractCreatedObjectsReplication extracts the created_objects_replication vector from a tx.
func extractCreatedObjectsReplication(tx *types.Transaction) []uint16 {
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

// extractRefData extracts ObjectRefData from a transaction's refs.
func extractRefData(tx *types.Transaction, mutable bool) []genesis.ObjectRefData {
	var count int
	if mutable {
		count = tx.MutableRefsLength()
	} else {
		count = tx.ReadRefsLength()
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

// extractPod extracts the pod ID from a transaction as [32]byte.
func extractPod(tx *types.Transaction) [32]byte {
	var pod [32]byte
	if b := tx.PodBytes(); len(b) == 32 {
		copy(pod[:], b)
	}
	return pod
}

// validateSignature verifies the Ed25519 signature over the transaction hash.
func validateSignature(tx *types.Transaction) error {
	sender := tx.SenderBytes()
	hash := tx.HashBytes()
	sig := tx.SignatureBytes()

	if !ed25519.Verify(sender, hash, sig) {
		return fmt.Errorf("invalid signature")
	}

	return nil
}
