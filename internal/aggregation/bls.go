package aggregation

import (
	"crypto/ed25519"

	"BluePods/internal/attest"
)

// BLS sizes re-exported from the attest package.
const (
	// BLSPublicKeySize is the size of a BLS public key in bytes.
	BLSPublicKeySize = attest.BLSPublicKeySize

	// BLSSignatureSize is the size of a BLS signature in bytes.
	BLSSignatureSize = attest.BLSSignatureSize
)

// BLSKeyPair is the BLS key type, re-exported from the attest package.
type BLSKeyPair = attest.BLSKeyPair

// DeriveFromED25519 derives a deterministic BLS key pair from an ED25519 private key.
func DeriveFromED25519(privKey ed25519.PrivateKey) (*BLSKeyPair, error) {
	return attest.DeriveFromED25519(privKey)
}

// GenerateBLSKey creates a new BLS key pair from a random seed.
func GenerateBLSKey() (*BLSKeyPair, error) {
	return attest.GenerateBLSKey()
}

// GenerateBLSKeyFromSeed creates a BLS key pair from a deterministic seed.
func GenerateBLSKeyFromSeed(seed []byte) (*BLSKeyPair, error) {
	return attest.GenerateBLSKeyFromSeed(seed)
}

// Verify checks a BLS signature against a message and public key.
func Verify(signature, message, publicKey []byte) bool {
	return attest.Verify(signature, message, publicKey)
}

// AggregateSignatures combines multiple BLS signatures over the same message into one.
func AggregateSignatures(signatures [][]byte) ([]byte, error) {
	return attest.AggregateSignatures(signatures)
}

// BuildSignerBitmap creates a bitmap indicating which validators signed.
func BuildSignerBitmap(indices []int, total int) []byte {
	return attest.BuildSignerBitmap(indices, total)
}
