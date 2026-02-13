package client

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/binary"
	"testing"

	"github.com/zeebo/blake3"

	"BluePods/internal/types"
)

// =============================================================================
// Borsh Encoding Tests (ATP 15.2-15.4)
// =============================================================================

// TestEncodeSplitArgs verifies split args encode to 40 bytes: u64 LE amount + 32-byte owner.
func TestEncodeSplitArgs(t *testing.T) {
	var owner [32]byte
	for i := range owner {
		owner[i] = byte(i + 1)
	}

	args := encodeSplitArgs(42000, owner)

	if len(args) != 40 {
		t.Fatalf("expected 40 bytes, got %d", len(args))
	}

	amount := binary.LittleEndian.Uint64(args[:8])
	if amount != 42000 {
		t.Errorf("expected amount 42000, got %d", amount)
	}

	var decoded [32]byte
	copy(decoded[:], args[8:])

	if decoded != owner {
		t.Error("owner mismatch in encoded args")
	}
}

// TestEncodeSplitArgs_ZeroAmount verifies zero amount encodes correctly.
func TestEncodeSplitArgs_ZeroAmount(t *testing.T) {
	var owner [32]byte
	args := encodeSplitArgs(0, owner)

	if len(args) != 40 {
		t.Fatalf("expected 40 bytes, got %d", len(args))
	}

	amount := binary.LittleEndian.Uint64(args[:8])
	if amount != 0 {
		t.Errorf("expected amount 0, got %d", amount)
	}

	// All first 8 bytes should be zero
	for i := 0; i < 8; i++ {
		if args[i] != 0 {
			t.Errorf("byte %d should be 0, got %d", i, args[i])
		}
	}
}

// TestEncodeTransferArgs verifies transfer args encode to 32 bytes = recipient pubkey.
func TestEncodeTransferArgs(t *testing.T) {
	var recipient [32]byte
	for i := range recipient {
		recipient[i] = byte(0xAA + i)
	}

	args := encodeTransferArgs(recipient)

	if len(args) != 32 {
		t.Fatalf("expected 32 bytes, got %d", len(args))
	}

	var decoded [32]byte
	copy(decoded[:], args)

	if decoded != recipient {
		t.Error("recipient mismatch in encoded args")
	}
}

// TestEncodeCreateNftArgs verifies create_nft args: 32(owner) + 2(rep LE) + 4(meta_len LE) + metadata.
func TestEncodeCreateNftArgs(t *testing.T) {
	var owner [32]byte
	for i := range owner {
		owner[i] = byte(i)
	}

	metadata := []byte("test metadata content")
	args := encodeCreateNftArgs(owner, 500, metadata)

	expectedLen := 32 + 2 + 4 + len(metadata)
	if len(args) != expectedLen {
		t.Fatalf("expected %d bytes, got %d", expectedLen, len(args))
	}

	// Check owner
	var decodedOwner [32]byte
	copy(decodedOwner[:], args[:32])
	if decodedOwner != owner {
		t.Error("owner mismatch")
	}

	// Check replication
	rep := binary.LittleEndian.Uint16(args[32:34])
	if rep != 500 {
		t.Errorf("expected replication 500, got %d", rep)
	}

	// Check metadata length
	metaLen := binary.LittleEndian.Uint32(args[34:38])
	if metaLen != uint32(len(metadata)) {
		t.Errorf("expected metadata_len %d, got %d", len(metadata), metaLen)
	}

	// Check metadata content
	if string(args[38:]) != string(metadata) {
		t.Error("metadata content mismatch")
	}
}

// TestEncodeCreateNftArgs_EmptyMetadata verifies 38 bytes total with 0 metadata_len.
func TestEncodeCreateNftArgs_EmptyMetadata(t *testing.T) {
	var owner [32]byte
	args := encodeCreateNftArgs(owner, 10, nil)

	if len(args) != 38 {
		t.Fatalf("expected 38 bytes with empty metadata, got %d", len(args))
	}

	metaLen := binary.LittleEndian.Uint32(args[34:38])
	if metaLen != 0 {
		t.Errorf("expected metadata_len 0, got %d", metaLen)
	}
}

// =============================================================================
// ComputeNewObjectID Tests
// =============================================================================

// TestComputeNewObjectID verifies objectID = blake3(txHash || 0_u32_LE).
func TestComputeNewObjectID(t *testing.T) {
	txHash := blake3.Sum256([]byte("test tx"))

	objectID := computeNewObjectID(txHash)

	// Compute expected value manually
	var buf [36]byte
	copy(buf[:32], txHash[:])
	binary.LittleEndian.PutUint32(buf[32:], 0)
	expected := blake3.Sum256(buf[:])

	if objectID != expected {
		t.Error("computeNewObjectID does not match manual computation")
	}
}

// TestComputeNewObjectID_Deterministic verifies same hash produces same ID.
func TestComputeNewObjectID_Deterministic(t *testing.T) {
	txHash := blake3.Sum256([]byte("determinism"))

	id1 := computeNewObjectID(txHash)
	id2 := computeNewObjectID(txHash)

	if id1 != id2 {
		t.Error("computeNewObjectID should be deterministic")
	}
}

// =============================================================================
// BuildSignedTx Tests (ATP 15.2, 15.7)
// =============================================================================

// TestBuildSignedTx_Valid verifies the returned bytes parse as a Transaction with valid hash and signature.
func TestBuildSignedTx_Valid(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	var pod [32]byte
	copy(pod[:], []byte("system_pod_id_for_testing_1234567"))

	txBytes, txHash := buildSignedTx(priv, pod, "transfer", []byte("args"), nil, nil, nil)

	// Parse the FlatBuffer
	tx := types.GetRootAsTransaction(txBytes, 0)

	// Verify function name
	if string(tx.FunctionName()) != "transfer" {
		t.Errorf("expected function 'transfer', got %q", tx.FunctionName())
	}

	// Verify hash matches what we got back
	hashBytes := tx.HashBytes()
	if len(hashBytes) != 32 {
		t.Fatalf("expected 32-byte hash, got %d", len(hashBytes))
	}

	var parsedHash [32]byte
	copy(parsedHash[:], hashBytes)
	if parsedHash != txHash {
		t.Error("returned hash does not match hash in FlatBuffer")
	}

	// Verify signature is valid
	sigBytes := tx.SignatureBytes()
	if len(sigBytes) != 64 {
		t.Fatalf("expected 64-byte signature, got %d", len(sigBytes))
	}

	pubKey := priv.Public().(ed25519.PublicKey)
	if !ed25519.Verify(pubKey, txHash[:], sigBytes) {
		t.Error("signature verification failed")
	}
}

// TestBuildSignedTx_WithMutableRefs verifies mutable refs appear in the FlatBuffer.
func TestBuildSignedTx_WithMutableRefs(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	var pod [32]byte

	mutRef := buildMutableRef([32]byte{0xAA}, 5)

	txBytes, _ := buildSignedTx(priv, pod, "test", nil, nil, mutRef, nil)

	tx := types.GetRootAsTransaction(txBytes, 0)

	if tx.MutableRefsLength() != 1 {
		t.Fatalf("expected 1 mutable ref, got %d", tx.MutableRefsLength())
	}

	var ref types.ObjectRef
	tx.MutableRefs(&ref, 0)

	if ref.Version() != 5 {
		t.Errorf("expected version 5, got %d", ref.Version())
	}

	idBytes := ref.IdBytes()
	if len(idBytes) != 32 || idBytes[0] != 0xAA {
		t.Error("mutable ref ID mismatch")
	}
}

// TestBuildSignedTx_WithCreatedObjects verifies created_objects_replication is present.
func TestBuildSignedTx_WithCreatedObjects(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	var pod [32]byte

	createdReps := []uint16{0, 10, 50}

	txBytes, _ := buildSignedTx(priv, pod, "create", nil, createdReps, nil, nil)

	tx := types.GetRootAsTransaction(txBytes, 0)

	if tx.CreatedObjectsReplicationLength() != 3 {
		t.Fatalf("expected 3 created objects, got %d", tx.CreatedObjectsReplicationLength())
	}

	if tx.CreatedObjectsReplication(0) != 0 {
		t.Errorf("created[0]: expected 0, got %d", tx.CreatedObjectsReplication(0))
	}

	if tx.CreatedObjectsReplication(1) != 10 {
		t.Errorf("created[1]: expected 10, got %d", tx.CreatedObjectsReplication(1))
	}

	if tx.CreatedObjectsReplication(2) != 50 {
		t.Errorf("created[2]: expected 50, got %d", tx.CreatedObjectsReplication(2))
	}
}
