package api

import (
	"crypto/ed25519"
	"fmt"

	flatbuffers "github.com/google/flatbuffers/go"
	"github.com/zeebo/blake3"

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

	// objectRefSize is the size of one object reference (32-byte ID + 8-byte version).
	objectRefSize = 40

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

	return nil
}

// validateObjectRefs checks that object references are well-formed and within limits.
func validateObjectRefs(tx *types.Transaction) error {
	readBytes := tx.ReadObjectsBytes()
	mutableBytes := tx.MutableObjectsBytes()

	if len(readBytes)%objectRefSize != 0 {
		return fmt.Errorf("read_objects length %d is not a multiple of %d", len(readBytes), objectRefSize)
	}

	if len(mutableBytes)%objectRefSize != 0 {
		return fmt.Errorf("mutable_objects length %d is not a multiple of %d", len(mutableBytes), objectRefSize)
	}

	readCount := len(readBytes) / objectRefSize
	mutableCount := len(mutableBytes) / objectRefSize
	totalRefs := readCount + mutableCount

	if totalRefs > maxObjectRefs {
		return fmt.Errorf("too many object refs: %d (max %d)", totalRefs, maxObjectRefs)
	}

	return validateNoDuplicateRefs(readBytes, mutableBytes)
}

// validateNoDuplicateRefs ensures no object ID appears twice across read and mutable lists.
func validateNoDuplicateRefs(readBytes, mutableBytes []byte) error {
	seen := make(map[[32]byte]bool)

	if err := checkDuplicates(mutableBytes, seen, "mutable_objects"); err != nil {
		return err
	}

	return checkDuplicates(readBytes, seen, "read_objects")
}

// checkDuplicates scans a concatenated ref list for duplicates against the seen set.
func checkDuplicates(data []byte, seen map[[32]byte]bool, fieldName string) error {
	numRefs := len(data) / objectRefSize

	for i := 0; i < numRefs; i++ {
		var id [32]byte
		copy(id[:], data[i*objectRefSize:i*objectRefSize+32])

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
// Must match the client's buildUnsignedTxBytes construction order exactly.
func rebuildUnsignedTx(tx *types.Transaction) []byte {
	builder := flatbuffers.NewBuilder(512)

	argsVec := builder.CreateByteVector(tx.ArgsBytes())
	senderVec := builder.CreateByteVector(tx.SenderBytes())
	podVec := builder.CreateByteVector(tx.PodBytes())
	funcNameOff := builder.CreateString(string(tx.FunctionName()))

	mutableBytes := tx.MutableObjectsBytes()
	readBytes := tx.ReadObjectsBytes()

	var mutObjVec flatbuffers.UOffsetT
	if len(mutableBytes) > 0 {
		mutObjVec = builder.CreateByteVector(mutableBytes)
	}

	var readObjVec flatbuffers.UOffsetT
	if len(readBytes) > 0 {
		readObjVec = builder.CreateByteVector(readBytes)
	}

	types.TransactionStart(builder)
	types.TransactionAddSender(builder, senderVec)
	types.TransactionAddPod(builder, podVec)
	types.TransactionAddFunctionName(builder, funcNameOff)
	types.TransactionAddArgs(builder, argsVec)
	types.TransactionAddCreatesObjects(builder, tx.CreatesObjects())

	if len(mutableBytes) > 0 {
		types.TransactionAddMutableObjects(builder, mutObjVec)
	}

	if len(readBytes) > 0 {
		types.TransactionAddReadObjects(builder, readObjVec)
	}

	txOff := types.TransactionEnd(builder)
	builder.Finish(txOff)

	return builder.FinishedBytes()
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
