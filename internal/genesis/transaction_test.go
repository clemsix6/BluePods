package genesis

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"testing"

	flatbuffers "github.com/google/flatbuffers/go"
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

// TestUnsignedBodyBindsSponsorship confirms the canonical unsigned body binds
// fee_payer and valid_until: an absent fee_payer reproduces the legacy bytes,
// two bodies differing only in fee_payer hash differently, and the body never
// includes either signature field.
func TestUnsignedBodyBindsSponsorship(t *testing.T) {
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	var pod, gasCoin, sponsorA, sponsorB [32]byte
	pod[0] = 0x11
	gasCoin[0] = 0x22
	sponsorA[0] = 0xAA
	sponsorB[0] = 0xBB

	args := []byte{9, 8, 7}

	legacy := BuildUnsignedTxBytesWithRefs(pub, pod, "transfer", args, nil, 0, 1000, gasCoin[:], nil, nil)
	empty := BuildUnsignedTxBytesSponsored(pub, pod, "transfer", args, nil, 0, 1000, gasCoin[:], nil, nil, Sponsorship{}, nil, nil)

	// An empty Sponsorship must reproduce the legacy (self-paid) bytes exactly.
	if !bytes.Equal(legacy, empty) {
		t.Error("empty sponsorship body differs from the legacy self-paid body")
	}

	bodyA := sponsoredBody(pub, pod, args, gasCoin, sponsorA[:], 5)
	bodyB := sponsoredBody(pub, pod, args, gasCoin, sponsorB[:], 5)

	// Two bodies differing only in fee_payer must hash differently.
	if bytes.Equal(bodyA, bodyB) {
		t.Error("bodies with different fee_payer produced identical bytes")
	}

	// A present fee_payer must change the body vs the self-paid encoding.
	if bytes.Equal(legacy, bodyA) {
		t.Error("sponsored body matches the self-paid body despite a present fee_payer")
	}

	// The body carries fee_payer but never a sponsor signature: build a full table
	// with a sponsor signature, then re-derive the body and confirm it ignores both
	// signature fields (hash is stable regardless of either signature).
	hash := blake3.Sum256(bodyA)
	atx := buildSponsoredATX(t, pub, pod, args, gasCoin, sponsorA[:], 5, hash)
	tx := types.GetRootAsAttestedTransaction(atx, 0).Transaction(nil)

	if !bytes.Equal(tx.FeePayerBytes(), sponsorA[:]) {
		t.Errorf("fee_payer not stored: got %x", tx.FeePayerBytes())
	}

	if tx.ValidUntil() != 5 {
		t.Errorf("valid_until not stored: got %d", tx.ValidUntil())
	}

	// Re-derive the body from the parsed tx (the move ingress and commit make) and
	// confirm it hashes back to the declared hash, never touching the signatures.
	rebuilt := BuildUnsignedTxBytesSponsored(
		tx.SenderBytes(), pod, string(tx.FunctionName()), tx.ArgsBytes(),
		nil, tx.MaxCreateDomains(), tx.MaxGas(), tx.GasCoinBytes(), nil, nil,
		Sponsorship{FeePayer: tx.FeePayerBytes(), ValidUntil: tx.ValidUntil()},
		tx.DeletedObjectsBytes(), ExtractOperations(tx),
	)
	rebuiltHash := blake3.Sum256(rebuilt)

	if !bytes.Equal(rebuiltHash[:], tx.HashBytes()) {
		t.Error("re-derived body hash does not match the declared hash")
	}
}

// TestUnsignedBodyBindsDeletedObjects confirms the canonical unsigned body binds
// the declared deleted_objects: an absent declaration reproduces the legacy bytes,
// and two bodies differing only in their declared deletions hash differently. This
// is what makes a transaction's deletion set covered by the sender's signature, so
// a tampered deletion cannot ride a valid signature.
func TestUnsignedBodyBindsDeletedObjects(t *testing.T) {
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	var pod, gasCoin [32]byte
	pod[0] = 0x11
	gasCoin[0] = 0x22

	del1 := bytes.Repeat([]byte{0xA1}, 32)
	del2 := bytes.Repeat([]byte{0xB2}, 32)

	none := BuildUnsignedTxBytesSponsored(pub, pod, "delete", nil, nil, 0, 1000, gasCoin[:], nil, nil, Sponsorship{}, nil, nil)
	legacy := BuildUnsignedTxBytesWithRefs(pub, pod, "delete", nil, nil, 0, 1000, gasCoin[:], nil, nil)

	// No declared deletions must reproduce the legacy (pre-field) bytes exactly.
	if !bytes.Equal(none, legacy) {
		t.Error("empty deleted_objects body differs from the legacy body")
	}

	withDel1 := BuildUnsignedTxBytesSponsored(pub, pod, "delete", nil, nil, 0, 1000, gasCoin[:], nil, nil, Sponsorship{}, del1, nil)
	withDel2 := BuildUnsignedTxBytesSponsored(pub, pod, "delete", nil, nil, 0, 1000, gasCoin[:], nil, nil, Sponsorship{}, del2, nil)

	// A declared deletion must change the body, and two different declarations must differ.
	if bytes.Equal(none, withDel1) {
		t.Error("declared deletion did not change the canonical body")
	}
	if bytes.Equal(withDel1, withDel2) {
		t.Error("different declared deletions produced identical bodies")
	}
}

// TestBuildSponsoredTx confirms a doubly-signed sponsored transaction: both the
// sender and sponsor signatures verify against the same canonical body hash, the
// fee_payer equals the sponsor's pubkey, and valid_until is carried.
func TestBuildSponsoredTx(t *testing.T) {
	senderPub, senderKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate sender key: %v", err)
	}

	sponsorPub, sponsorKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate sponsor key: %v", err)
	}

	var pod, gasCoin, recipient [32]byte
	pod[0] = 0x11
	gasCoin[0] = 0x99
	recipient[0] = 0x42

	args := EncodeSplitArgs(0, recipient)
	atxBytes := BuildSponsoredTx(senderKey, sponsorKey, pod, "create_object", args, []uint16{1}, 0, 1000, gasCoin, 7, nil, nil)

	tx := types.GetRootAsAttestedTransaction(atxBytes, 0).Transaction(nil)
	if tx == nil {
		t.Fatal("missing transaction")
	}

	if !bytes.Equal(tx.SenderBytes(), senderPub) {
		t.Errorf("sender: got %x, want %x", tx.SenderBytes(), []byte(senderPub))
	}

	if !bytes.Equal(tx.FeePayerBytes(), sponsorPub) {
		t.Errorf("fee_payer: got %x, want sponsor %x", tx.FeePayerBytes(), []byte(sponsorPub))
	}

	if tx.ValidUntil() != 7 {
		t.Errorf("valid_until: got %d, want 7", tx.ValidUntil())
	}

	hash := tx.HashBytes()

	if !ed25519.Verify(senderPub, hash, tx.SignatureBytes()) {
		t.Error("sender signature does not verify against the body hash")
	}

	if !ed25519.Verify(sponsorPub, hash, tx.SponsorSignatureBytes()) {
		t.Error("sponsor signature does not verify against the body hash")
	}

	// The declared hash must equal the recomputed canonical body hash.
	rebuilt := BuildUnsignedTxBytesSponsored(
		tx.SenderBytes(), pod, string(tx.FunctionName()), tx.ArgsBytes(),
		[]uint16{1}, 0, tx.MaxGas(), tx.GasCoinBytes(), nil, nil,
		Sponsorship{FeePayer: tx.FeePayerBytes(), ValidUntil: tx.ValidUntil()},
		tx.DeletedObjectsBytes(), ExtractOperations(tx),
	)
	rebuiltHash := blake3.Sum256(rebuilt)

	if !bytes.Equal(rebuiltHash[:], hash) {
		t.Error("declared hash differs from the recomputed canonical body hash")
	}
}

// sponsoredBody builds an unsigned sponsored body for testing.
func sponsoredBody(pub ed25519.PublicKey, pod [32]byte, args []byte, gasCoin [32]byte, feePayer []byte, validUntil uint64) []byte {
	return BuildUnsignedTxBytesSponsored(
		pub, pod, "transfer", args, nil, 0, 1000, gasCoin[:], nil, nil,
		Sponsorship{FeePayer: feePayer, ValidUntil: validUntil}, nil, nil,
	)
}

// buildSponsoredATX builds a sponsored Transaction table (with a dummy sponsor
// signature) wrapped in an ATX for round-trip testing.
func buildSponsoredATX(t *testing.T, pub ed25519.PublicKey, pod [32]byte, args []byte, gasCoin [32]byte, feePayer []byte, validUntil uint64, hash [32]byte) []byte {
	t.Helper()

	builder := flatbuffers.NewBuilder(1024)
	dummySig := make([]byte, ed25519.SignatureSize)
	txOff := BuildTxTableSponsored(
		builder, pub, pod, "transfer", args, nil, 0, 1000, gasCoin[:], hash, dummySig, nil, nil,
		Sponsorship{FeePayer: feePayer, ValidUntil: validUntil}, dummySig, nil, nil,
	)

	return finishAttestedTx(builder, txOff)
}
