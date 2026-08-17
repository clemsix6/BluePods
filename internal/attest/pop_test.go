package attest

import (
	"testing"

	blst "github.com/supranational/blst/bindings/go"
)

// testIdentity is a stand-in Ed25519 identity claiming a BLS key.
var testIdentity = [32]byte{0x01, 0x02, 0x03}

// TestProveKeyPossessionVerifies checks that a freshly produced proof verifies
// against the key and identity it was produced for.
func TestProveKeyPossessionVerifies(t *testing.T) {
	key, err := GenerateBLSKey()
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	proof := key.ProveKeyPossession(testIdentity)

	if len(proof) != BLSSignatureSize {
		t.Fatalf("proof size = %d, want %d", len(proof), BLSSignatureSize)
	}

	if !VerifyKeyPossession(proof, testIdentity, key.PublicKeyBytes()) {
		t.Error("a valid proof of possession must verify")
	}
}

// TestVerifyKeyPossessionRejectsOtherKey checks that a proof does not carry over
// to another public key: this is what stops a registrant from claiming a key it
// does not hold the secret for.
func TestVerifyKeyPossessionRejectsOtherKey(t *testing.T) {
	key, err := GenerateBLSKey()
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	other, err := GenerateBLSKey()
	if err != nil {
		t.Fatalf("generate other key: %v", err)
	}

	proof := key.ProveKeyPossession(testIdentity)

	if VerifyKeyPossession(proof, testIdentity, other.PublicKeyBytes()) {
		t.Error("a proof over one key must not verify against another")
	}
}

// TestVerifyKeyPossessionRejectsOtherIdentity checks that a proof is bound to the
// identity that produced it, so it cannot be lifted onto a second registrant
// claiming the same BLS key.
func TestVerifyKeyPossessionRejectsOtherIdentity(t *testing.T) {
	key, err := GenerateBLSKey()
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	proof := key.ProveKeyPossession(testIdentity)

	otherIdentity := testIdentity
	otherIdentity[0] ^= 0xFF

	if VerifyKeyPossession(proof, otherIdentity, key.PublicKeyBytes()) {
		t.Error("a proof bound to one identity must not verify under another")
	}
}

// TestVerifyKeyPossessionRejectsAttestationDST checks the domain separation: a
// signature over the very same message under the basic-scheme tag attestations
// use is not a proof of possession, and vice versa. Without this, an attestation
// signature harvested from the network could stand in for a proof.
func TestVerifyKeyPossessionRejectsAttestationDST(t *testing.T) {
	key, err := GenerateBLSKey()
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	message := possessionMessage(testIdentity, key.PublicKeyBytes())
	underAttestationDST := key.Sign(message)

	if VerifyKeyPossession(underAttestationDST, testIdentity, key.PublicKeyBytes()) {
		t.Error("a signature under the attestation tag must not pass as a proof of possession")
	}

	proof := key.ProveKeyPossession(testIdentity)

	if Verify(proof, message, key.PublicKeyBytes()) {
		t.Error("a proof of possession must not pass as an attestation signature")
	}
}

// TestVerifyKeyPossessionRejectsMalformed checks the length and encoding guards.
func TestVerifyKeyPossessionRejectsMalformed(t *testing.T) {
	key, err := GenerateBLSKey()
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	pubkey := key.PublicKeyBytes()
	proof := key.ProveKeyPossession(testIdentity)

	if VerifyKeyPossession(nil, testIdentity, pubkey) {
		t.Error("an absent proof must not verify")
	}

	if VerifyKeyPossession(proof[:BLSSignatureSize-1], testIdentity, pubkey) {
		t.Error("a truncated proof must not verify")
	}

	if VerifyKeyPossession(proof, testIdentity, pubkey[:BLSPublicKeySize-1]) {
		t.Error("a truncated public key must not verify")
	}

	if VerifyKeyPossession(make([]byte, BLSSignatureSize), testIdentity, pubkey) {
		t.Error("an unparseable proof must not verify")
	}

	if VerifyKeyPossession(proof, testIdentity, make([]byte, BLSPublicKeySize)) {
		t.Error("an unparseable public key must not verify")
	}
}

// TestRogueKeyHasNoProofOfPossession is the reason this file exists. It builds
// the textbook rogue key against an aggregate over a single message,
// pk_rogue = pk_attacker - sum(pk_honest), and shows both halves of the story:
// the rogue key really does let its holder forge an aggregate signature that
// verifies as if every honest holder had signed, and it cannot be accompanied by
// a proof of possession, because its discrete logarithm is unknown.
func TestRogueKeyHasNoProofOfPossession(t *testing.T) {
	honest := make([]*BLSKeyPair, 3)
	honestKeys := make([][]byte, len(honest))

	for i := range honest {
		key, err := GenerateBLSKey()
		if err != nil {
			t.Fatalf("generate honest key %d: %v", i, err)
		}

		honest[i] = key
		honestKeys[i] = key.PublicKeyBytes()
	}

	attacker, err := GenerateBLSKey()
	if err != nil {
		t.Fatalf("generate attacker key: %v", err)
	}

	rogue := rogueKeyBytes(t, attacker.PublicKeyBytes(), honestKeys)

	// The attacker alone signs, and the aggregate over the honest keys plus the
	// rogue key verifies: pk_rogue + sum(pk_honest) == pk_attacker.
	message := []byte("attested object state nobody attested")
	forged := attacker.Sign(message)

	if !VerifyAggregated(forged, message, append(honestKeys, rogue)) {
		t.Fatal("the rogue-key forgery is expected to pass aggregate verification; this test's premise is gone")
	}

	// A proof of possession is the gate the forgery cannot pass. The attacker
	// holds the secret behind pk_attacker, not behind pk_rogue, so no signature
	// it can produce verifies against the rogue key.
	proofAttempt := attacker.ProveKeyPossession(testIdentity)

	if VerifyKeyPossession(proofAttempt, testIdentity, rogue) {
		t.Error("a rogue key must not carry a verifiable proof of possession")
	}
}

// rogueKeyBytes returns the compressed public key pk_target - sum(others), the
// key whose inclusion in an aggregate cancels every other member's key and
// leaves pk_target alone.
func rogueKeyBytes(t *testing.T, target []byte, others [][]byte) []byte {
	t.Helper()

	targetPoint := new(blst.P1Affine).Uncompress(target)
	if targetPoint == nil {
		t.Fatal("uncompress target key")
	}

	points := make([]*blst.P1Affine, 0, len(others))
	for i, key := range others {
		point := new(blst.P1Affine).Uncompress(key)
		if point == nil {
			t.Fatalf("uncompress key %d", i)
		}

		points = append(points, point)
	}

	sum := blst.P1AffinesAdd(points)

	rogue := new(blst.P1)
	rogue.FromAffine(targetPoint)
	rogue.SubAssign(sum.ToAffine())

	return rogue.ToAffine().Compress()
}
