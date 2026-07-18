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

// TestUnsignedBodyBindsOperations confirms the canonical unsigned body binds
// the declared operations: an absent operations list reproduces the legacy
// (pre-field) bytes exactly, and a declared reparent op changes the body. This
// is what makes an operation covered by the sender's (and, when present, the
// sponsor's) signature, so a tampered op cannot ride a valid signature.
func TestUnsignedBodyBindsOperations(t *testing.T) {
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	var pod, gasCoin, newParent [32]byte
	pod[0] = 0x11
	gasCoin[0] = 0x22
	newParent[0] = 0x33

	reparent := []DeclaredOp{{
		Kind:       0,
		ObjectID:   bytes.Repeat([]byte{0xA1}, 32),
		TargetKind: 0,
		Target:     newParent[:],
	}}

	none := BuildUnsignedTxBytesSponsored(pub, pod, "noop", nil, nil, 0, 1000, gasCoin[:], nil, nil, Sponsorship{}, nil, nil)
	legacy := BuildUnsignedTxBytesWithRefs(pub, pod, "noop", nil, nil, 0, 1000, gasCoin[:], nil, nil)

	// No declared operations must reproduce the legacy (pre-field) bytes exactly.
	if !bytes.Equal(none, legacy) {
		t.Error("empty operations body differs from the legacy body")
	}

	withOp := BuildUnsignedTxBytesSponsored(pub, pod, "noop", nil, nil, 0, 1000, gasCoin[:], nil, nil, Sponsorship{}, nil, reparent)

	// A declared operation must change the body.
	if bytes.Equal(none, withOp) {
		t.Error("declared operation did not change the canonical body")
	}
}

// TestDeclaredOpRoundTrip_SelfPaid confirms a transaction carrying one
// reparent op round-trips through build -> rebuild -> verify: the recomputed
// canonical body hashes back to the declared hash, and the sender signature
// verifies against it.
func TestDeclaredOpRoundTrip_SelfPaid(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	var pod, gasCoin, newParent [32]byte
	pod[0] = 0x11
	gasCoin[0] = 0x22
	newParent[0] = 0x33

	reparent := []DeclaredOp{{
		Kind:       0,
		ObjectID:   bytes.Repeat([]byte{0xA1}, 32),
		TargetKind: 0,
		Target:     newParent[:],
	}}

	body := BuildUnsignedTxBytesSponsored(pub, pod, "noop", nil, nil, 0, 1000, gasCoin[:], nil, nil, Sponsorship{}, nil, reparent)
	hash := blake3.Sum256(body)
	sig := ed25519.Sign(priv, hash[:])

	builder := flatbuffers.NewBuilder(1024)
	txOff := BuildTxTableSponsored(
		builder, pub, pod, "noop", nil, nil, 0, 1000, gasCoin[:], hash, sig, nil, nil, Sponsorship{}, nil, nil, reparent,
	)
	atxBytes := finishAttestedTx(builder, txOff)
	tx := types.GetRootAsAttestedTransaction(atxBytes, 0).Transaction(nil)

	if got := tx.OperationsLength(); got != 1 {
		t.Fatalf("operations length = %d, want 1", got)
	}

	var op types.DeclaredOp
	tx.Operations(&op, 0)

	if op.Kind() != 0 {
		t.Errorf("op kind = %d, want 0 (reparent)", op.Kind())
	}

	if !bytes.Equal(op.TargetBytes(), newParent[:]) {
		t.Errorf("op target = %x, want %x", op.TargetBytes(), newParent[:])
	}

	rebuilt := BuildUnsignedTxBytesSponsored(
		tx.SenderBytes(), pod, string(tx.FunctionName()), tx.ArgsBytes(), nil,
		tx.MaxCreateDomains(), tx.MaxGas(), tx.GasCoinBytes(), nil, nil,
		Sponsorship{FeePayer: tx.FeePayerBytes(), ValidUntil: tx.ValidUntil()},
		tx.DeletedObjectsBytes(), ExtractOperations(tx),
	)
	rebuiltHash := blake3.Sum256(rebuilt)

	if !bytes.Equal(rebuiltHash[:], tx.HashBytes()) {
		t.Error("rebuilt body hash does not match the declared hash")
	}

	if !ed25519.Verify(pub, tx.HashBytes(), tx.SignatureBytes()) {
		t.Error("sender signature does not verify against the rebuilt hash")
	}
}

// TestDeclaredOpRoundTrip_Sponsored mirrors TestDeclaredOpRoundTrip_SelfPaid
// with a sponsor signature: both the sender and the sponsor sign the SAME body
// hash, which only holds if operations is bound by the canonical body.
func TestDeclaredOpRoundTrip_Sponsored(t *testing.T) {
	senderPub, senderKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate sender key: %v", err)
	}

	sponsorPub, sponsorKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate sponsor key: %v", err)
	}

	var pod, gasCoin, newParent [32]byte
	pod[0] = 0x11
	gasCoin[0] = 0x99
	newParent[0] = 0x44

	reparent := []DeclaredOp{{
		Kind:       0,
		ObjectID:   bytes.Repeat([]byte{0xB2}, 32),
		TargetKind: 0,
		Target:     newParent[:],
	}}

	sponsor := Sponsorship{FeePayer: sponsorPub, ValidUntil: 9}
	body := BuildUnsignedTxBytesSponsored(senderPub, pod, "noop", nil, nil, 0, 1000, gasCoin[:], nil, nil, sponsor, nil, reparent)
	hash := blake3.Sum256(body)
	senderSig := ed25519.Sign(senderKey, hash[:])
	sponsorSig := ed25519.Sign(sponsorKey, hash[:])

	builder := flatbuffers.NewBuilder(1024)
	txOff := BuildTxTableSponsored(
		builder, senderPub, pod, "noop", nil, nil, 0, 1000, gasCoin[:], hash, senderSig, nil, nil, sponsor, sponsorSig, nil, reparent,
	)
	atxBytes := finishAttestedTx(builder, txOff)
	tx := types.GetRootAsAttestedTransaction(atxBytes, 0).Transaction(nil)

	if got := tx.OperationsLength(); got != 1 {
		t.Fatalf("operations length = %d, want 1", got)
	}

	rebuilt := BuildUnsignedTxBytesSponsored(
		tx.SenderBytes(), pod, string(tx.FunctionName()), tx.ArgsBytes(), nil,
		tx.MaxCreateDomains(), tx.MaxGas(), tx.GasCoinBytes(), nil, nil,
		Sponsorship{FeePayer: tx.FeePayerBytes(), ValidUntil: tx.ValidUntil()},
		tx.DeletedObjectsBytes(), ExtractOperations(tx),
	)
	rebuiltHash := blake3.Sum256(rebuilt)

	if !bytes.Equal(rebuiltHash[:], tx.HashBytes()) {
		t.Error("rebuilt body hash does not match the declared hash")
	}

	if !ed25519.Verify(senderPub, tx.HashBytes(), tx.SignatureBytes()) {
		t.Error("sender signature does not verify against the rebuilt hash")
	}

	if !ed25519.Verify(sponsorPub, tx.HashBytes(), tx.SponsorSignatureBytes()) {
		t.Error("sponsor signature does not verify against the rebuilt hash")
	}
}

// TestRebuildTxInBuilder_PreservesOperations confirms that re-serializing a
// Transaction (the gossip / ATX-wrap path) preserves its declared operations,
// so a gossiped op-carrying transaction is not silently stripped on re-wrap.
func TestRebuildTxInBuilder_PreservesOperations(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	var pod, gasCoin, newParent [32]byte
	pod[0] = 0x11
	gasCoin[0] = 0x22
	newParent[0] = 0x55

	reparent := []DeclaredOp{{
		Kind:       0,
		ObjectID:   bytes.Repeat([]byte{0xC3}, 32),
		TargetKind: 0,
		Target:     newParent[:],
	}}

	body := BuildUnsignedTxBytesSponsored(pub, pod, "noop", nil, nil, 0, 1000, gasCoin[:], nil, nil, Sponsorship{}, nil, reparent)
	hash := blake3.Sum256(body)
	sig := ed25519.Sign(priv, hash[:])

	rawBuilder := flatbuffers.NewBuilder(1024)
	rawTxOff := BuildTxTableSponsored(
		rawBuilder, pub, pod, "noop", nil, nil, 0, 1000, gasCoin[:], hash, sig, nil, nil, Sponsorship{}, nil, nil, reparent,
	)
	rawBuilder.Finish(rawTxOff)

	atxBytes := WrapInATX(rawBuilder.FinishedBytes())
	tx := types.GetRootAsAttestedTransaction(atxBytes, 0).Transaction(nil)

	if got := tx.OperationsLength(); got != 1 {
		t.Fatalf("operations length after WrapInATX = %d, want 1", got)
	}

	var op types.DeclaredOp
	tx.Operations(&op, 0)

	if !bytes.Equal(op.ObjectIdBytes(), reparent[0].ObjectID) {
		t.Errorf("object_id after rebuild = %x, want %x", op.ObjectIdBytes(), reparent[0].ObjectID)
	}

	if !bytes.Equal(op.TargetBytes(), reparent[0].Target) {
		t.Errorf("target after rebuild = %x, want %x", op.TargetBytes(), reparent[0].Target)
	}
}
