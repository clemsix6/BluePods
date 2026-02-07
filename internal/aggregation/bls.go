package aggregation

import (
	"crypto/ed25519"
	"crypto/rand"
	"fmt"

	blst "github.com/supranational/blst/bindings/go"
	"github.com/zeebo/blake3"
)

const (
	// BLSPublicKeySize is the size of a BLS public key in bytes.
	BLSPublicKeySize = 48

	// BLSSignatureSize is the size of a BLS signature in bytes.
	BLSSignatureSize = 96
)

// blsDST is the domain separation tag for BLS signatures.
var blsDST = []byte("BLS_SIG_BLS12381G2_XMD:SHA-256_SSWU_RO_NUL_")

// BLSKeyPair holds a BLS private/public key pair.
type BLSKeyPair struct {
	secret *blst.SecretKey  // secret is the private key
	public *blst.P1Affine   // public is the public key
}

// DeriveFromED25519 derives a deterministic BLS key pair from an ED25519 private key.
// The BLS key is bound to the validator's identity via BLAKE3("bluepods-bls-keygen" || seed).
func DeriveFromED25519(privKey ed25519.PrivateKey) (*BLSKeyPair, error) {
	seed := privKey.Seed()
	h := blake3.New()
	h.Write([]byte("bluepods-bls-keygen"))
	h.Write(seed)

	var derived [32]byte
	h.Sum(derived[:0])

	return GenerateBLSKeyFromSeed(derived[:])
}

// GenerateBLSKey creates a new BLS key pair from random seed.
func GenerateBLSKey() (*BLSKeyPair, error) {
	var ikm [32]byte
	if _, err := rand.Read(ikm[:]); err != nil {
		return nil, fmt.Errorf("generate random seed:\n%w", err)
	}

	return GenerateBLSKeyFromSeed(ikm[:])
}

// GenerateBLSKeyFromSeed creates a BLS key pair from a deterministic seed.
// The seed must be at least 32 bytes.
func GenerateBLSKeyFromSeed(seed []byte) (*BLSKeyPair, error) {
	if len(seed) < 32 {
		return nil, fmt.Errorf("seed must be at least 32 bytes")
	}

	secret := blst.KeyGen(seed)
	if secret == nil {
		return nil, fmt.Errorf("failed to generate BLS key")
	}

	public := new(blst.P1Affine).From(secret)

	return &BLSKeyPair{
		secret: secret,
		public: public,
	}, nil
}

// Sign creates a BLS signature over the message.
func (k *BLSKeyPair) Sign(message []byte) []byte {
	sig := new(blst.P2Affine).Sign(k.secret, message, blsDST)
	return sig.Compress()
}

// PublicKeyBytes returns the compressed public key bytes.
func (k *BLSKeyPair) PublicKeyBytes() []byte {
	return k.public.Compress()
}

// Verify checks a BLS signature against a message and public key.
func Verify(signature, message, publicKey []byte) bool {
	if len(signature) != BLSSignatureSize || len(publicKey) != BLSPublicKeySize {
		return false
	}

	sig := new(blst.P2Affine).Uncompress(signature)
	if sig == nil {
		return false
	}

	pk := new(blst.P1Affine).Uncompress(publicKey)
	if pk == nil {
		return false
	}

	return sig.Verify(true, pk, true, message, blsDST)
}

// AggregateSignatures combines multiple BLS signatures into one.
// All signatures must be over the same message.
func AggregateSignatures(signatures [][]byte) ([]byte, error) {
	if len(signatures) == 0 {
		return nil, fmt.Errorf("no signatures to aggregate")
	}

	sigs := make([]*blst.P2Affine, len(signatures))

	for i, sigBytes := range signatures {
		if len(sigBytes) != BLSSignatureSize {
			return nil, fmt.Errorf("invalid signature size at index %d", i)
		}

		sig := new(blst.P2Affine).Uncompress(sigBytes)
		if sig == nil {
			return nil, fmt.Errorf("invalid signature at index %d", i)
		}

		sigs[i] = sig
	}

	agg := new(blst.P2Aggregate)
	if !agg.Aggregate(sigs, true) {
		return nil, fmt.Errorf("signature aggregation failed")
	}

	return agg.ToAffine().Compress(), nil
}

// VerifyAggregated verifies an aggregated signature against a message and multiple public keys.
func VerifyAggregated(signature, message []byte, publicKeys [][]byte) bool {
	if len(signature) != BLSSignatureSize || len(publicKeys) == 0 {
		return false
	}

	sig := new(blst.P2Affine).Uncompress(signature)
	if sig == nil {
		return false
	}

	// Aggregate public keys
	pks := make([]*blst.P1Affine, len(publicKeys))

	for i, pkBytes := range publicKeys {
		if len(pkBytes) != BLSPublicKeySize {
			return false
		}

		pk := new(blst.P1Affine).Uncompress(pkBytes)
		if pk == nil {
			return false
		}

		pks[i] = pk
	}

	aggPk := new(blst.P1Aggregate)
	if !aggPk.Aggregate(pks, true) {
		return false
	}

	return sig.Verify(true, aggPk.ToAffine(), true, message, blsDST)
}

// BuildSignerBitmap creates a bitmap indicating which validators signed.
// indices contains the validator indices that signed, total is the validator count.
func BuildSignerBitmap(indices []int, total int) []byte {
	// Calculate number of bytes needed
	numBytes := (total + 7) / 8
	bitmap := make([]byte, numBytes)

	for _, idx := range indices {
		if idx >= 0 && idx < total {
			bitmap[idx/8] |= 1 << (idx % 8)
		}
	}

	return bitmap
}

// ParseSignerBitmap extracts the validator indices from a bitmap.
func ParseSignerBitmap(bitmap []byte) []int {
	var indices []int

	for byteIdx, b := range bitmap {
		for bit := 0; bit < 8; bit++ {
			if b&(1<<bit) != 0 {
				indices = append(indices, byteIdx*8+bit)
			}
		}
	}

	return indices
}
