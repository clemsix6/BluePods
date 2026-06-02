package genesis

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"testing"

	"github.com/zeebo/blake3"

	"BluePods/internal/types"
)

// TestNonSponsoredTxByteIdentical confirms that a transaction with no sponsorship
// fields serializes byte-identically to the pre-sponsorship encoding: the new
// fee_payer / sponsor_signature / valid_until fields are absent (not zero-filled),
// the canonical unsigned body matches a frozen reference hash, and the resulting
// transaction's hash and sender signature still verify.
func TestNonSponsoredTxByteIdentical(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	var pod [32]byte
	pod[0] = 0x11

	var gasCoin [32]byte
	gasCoin[0] = 0x22

	args := []byte{1, 2, 3, 4}
	reps := []uint16{0}

	// The unsigned body of a non-sponsored tx must be exactly what the builder
	// produced before the schema change: the new fields contribute nothing.
	body := BuildUnsignedTxBytesWithRefs(pub, pod, "split", args, reps, 0, 1000, gasCoin[:], nil, nil)
	hash := blake3.Sum256(body)
	sig := ed25519.Sign(priv, hash[:])

	atxBytes := BuildAttestedTx(priv, pod, "split", args, reps, 0, 1000, gasCoin[:])
	atx := types.GetRootAsAttestedTransaction(atxBytes, 0)
	tx := atx.Transaction(nil)
	if tx == nil {
		t.Fatal("missing transaction")
	}

	// The sponsorship fields must be absent for a non-sponsored tx.
	if got := tx.FeePayerLength(); got != 0 {
		t.Errorf("fee_payer present on non-sponsored tx: length %d", got)
	}

	if got := tx.SponsorSignatureLength(); got != 0 {
		t.Errorf("sponsor_signature present on non-sponsored tx: length %d", got)
	}

	if got := tx.ValidUntil(); got != 0 {
		t.Errorf("valid_until non-zero on non-sponsored tx: %d", got)
	}

	// The hash the builder computed must equal the standalone unsigned-body hash.
	if !bytes.Equal(tx.HashBytes(), hash[:]) {
		t.Errorf("hash mismatch: builder %x, frozen body %x", tx.HashBytes(), hash[:])
	}

	// The sender signature must still verify against that hash.
	if !ed25519.Verify(pub, tx.HashBytes(), sig) {
		t.Error("sender signature does not verify against the recomputed hash")
	}

	if !ed25519.Verify(pub, tx.HashBytes(), tx.SignatureBytes()) {
		t.Error("embedded sender signature does not verify")
	}
}
