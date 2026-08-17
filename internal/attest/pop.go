package attest

import blst "github.com/supranational/blst/bindings/go"

// blsPoPDST is the domain separation tag of the proof-of-possession ciphersuite
// of draft-irtf-cfrg-bls-signature. It MUST differ from blsDST, the basic scheme
// tag every attestation signature uses: the two tags are what keeps a proof of
// possession from being replayed as an attestation signature, and an attestation
// signature from being replayed as a proof of possession.
var blsPoPDST = []byte("BLS_POP_BLS12381G2_XMD:SHA-256_SSWU_RO_POP_")

// ProveKeyPossession signs the key pair's own public key, prefixed by the
// Ed25519 identity that will claim it, under the proof-of-possession domain tag.
// Only a holder of the BLS secret key can produce it, which is what a verifier
// needs before admitting a public key into an aggregate: a key chosen as a
// function of other members' keys (the rogue-key attack on aggregation over a
// single message) has no known secret and therefore no proof. Prefixing the
// identity binds the proof to one registrant, so it cannot be lifted onto a
// second registration of the same public key.
func (k *BLSKeyPair) ProveKeyPossession(identity [32]byte) []byte {
	message := possessionMessage(identity, k.PublicKeyBytes())
	proof := new(blst.P2Affine).Sign(k.secret, message, blsPoPDST)

	return proof.Compress()
}

// VerifyKeyPossession checks a proof of possession of blsPublicKey by identity.
// It returns false for a malformed proof or key, for a proof over a different
// key or identity, and for a signature produced under any other domain tag.
func VerifyKeyPossession(proof []byte, identity [32]byte, blsPublicKey []byte) bool {
	if len(proof) != BLSSignatureSize || len(blsPublicKey) != BLSPublicKeySize {
		return false
	}

	sig := new(blst.P2Affine).Uncompress(proof)
	if sig == nil {
		return false
	}

	pk := new(blst.P1Affine).Uncompress(blsPublicKey)
	if pk == nil {
		return false
	}

	return sig.Verify(true, pk, true, possessionMessage(identity, blsPublicKey), blsPoPDST)
}

// possessionMessage builds the signed message of a proof of possession: the
// claiming Ed25519 identity followed by the BLS public key being claimed.
func possessionMessage(identity [32]byte, blsPublicKey []byte) []byte {
	message := make([]byte, 0, len(identity)+len(blsPublicKey))
	message = append(message, identity[:]...)
	message = append(message, blsPublicKey...)

	return message
}
