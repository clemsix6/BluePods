package aggregation

import (
	"crypto/ed25519"

	"BluePods/internal/attest"
)

// BLSSignatureSize is the size of a BLS signature in bytes, re-exported from
// the attest package.
const BLSSignatureSize = attest.BLSSignatureSize

// BLSKeyPair is the BLS key type, re-exported from the attest package.
type BLSKeyPair = attest.BLSKeyPair

// DeriveFromED25519 derives a deterministic BLS key pair from an ED25519 private key.
func DeriveFromED25519(privKey ed25519.PrivateKey) (*BLSKeyPair, error) {
	return attest.DeriveFromED25519(privKey)
}
