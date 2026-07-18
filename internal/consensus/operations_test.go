package consensus

import (
	"bytes"
	"crypto/ed25519"
	"testing"

	flatbuffers "github.com/google/flatbuffers/go"
	"github.com/zeebo/blake3"

	"BluePods/internal/genesis"
	"BluePods/internal/types"
)

// TestVerifyTxAuthenticity_WithOperations confirms a transaction carrying a
// declared operation passes commit-time authenticity: the canonical body must
// cover operations for the recomputed hash and sender signature to verify.
func TestVerifyTxAuthenticity_WithOperations(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	var gasCoin, newParent [32]byte
	gasCoin[0] = 0x77
	newParent[0] = 0x33

	ops := []genesis.DeclaredOp{{
		Kind:       0,
		ObjectID:   bytes.Repeat([]byte{0xA1}, 32),
		TargetKind: 0,
		Target:     newParent[:],
	}}

	body := genesis.BuildUnsignedTxBytesSponsored(pub, testSystemPod, "noop", nil, nil, 0, 1000, gasCoin[:], nil, nil, genesis.Sponsorship{}, nil, ops)
	hash := blake3.Sum256(body)
	sig := ed25519.Sign(priv, hash[:])

	builder := flatbuffers.NewBuilder(1024)
	txOff := genesis.BuildTxTableSponsored(
		builder, pub, testSystemPod, "noop", nil, nil, 0, 1000, gasCoin[:], hash, sig, nil, nil, genesis.Sponsorship{}, nil, nil, ops,
	)
	builder.Finish(txOff)
	tx := types.GetRootAsTransaction(builder.FinishedBytes(), 0)

	if err := verifyTxAuthenticity(tx); err != nil {
		t.Fatalf("valid tx with operations rejected: %v", err)
	}
}

// TestVerifyTxAuthenticity_SponsoredWithOperations mirrors the self-paid
// variant with a sponsor signature: both the sender and the sponsor sign the
// SAME body hash, which only holds if operations is bound by the canonical
// body used to verify both signatures.
func TestVerifyTxAuthenticity_SponsoredWithOperations(t *testing.T) {
	senderPub, senderKey, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("generate sender key: %v", err)
	}

	sponsorPub, sponsorKey, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("generate sponsor key: %v", err)
	}

	var gasCoin, newParent [32]byte
	gasCoin[0] = 0x88
	newParent[0] = 0x44

	ops := []genesis.DeclaredOp{{
		Kind:       0,
		ObjectID:   bytes.Repeat([]byte{0xB2}, 32),
		TargetKind: 0,
		Target:     newParent[:],
	}}

	sponsor := genesis.Sponsorship{FeePayer: sponsorPub, ValidUntil: 9}
	body := genesis.BuildUnsignedTxBytesSponsored(senderPub, testSystemPod, "noop", nil, nil, 0, 1000, gasCoin[:], nil, nil, sponsor, nil, ops)
	hash := blake3.Sum256(body)
	senderSig := ed25519.Sign(senderKey, hash[:])
	sponsorSig := ed25519.Sign(sponsorKey, hash[:])

	builder := flatbuffers.NewBuilder(1024)
	txOff := genesis.BuildTxTableSponsored(
		builder, senderPub, testSystemPod, "noop", nil, nil, 0, 1000, gasCoin[:], hash, senderSig, nil, nil, sponsor, sponsorSig, nil, ops,
	)
	builder.Finish(txOff)
	tx := types.GetRootAsTransaction(builder.FinishedBytes(), 0)

	if err := verifyTxAuthenticity(tx); err != nil {
		t.Fatalf("valid sponsored tx with operations rejected: %v", err)
	}
}
