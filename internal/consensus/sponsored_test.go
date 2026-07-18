package consensus

import (
	"crypto/ed25519"
	"testing"

	flatbuffers "github.com/google/flatbuffers/go"
	"github.com/zeebo/blake3"

	"BluePods/internal/genesis"
	"BluePods/internal/types"
)

// sponsoredTestTx builds a doubly-signed sponsored ATX and returns its inner
// Transaction. The sender and sponsor are distinct keys; gasCoin/validUntil are
// carried into the canonical body.
func sponsoredTestTx(t *testing.T, senderKey, sponsorKey ed25519.PrivateKey, gasCoin [32]byte, validUntil uint64) *types.Transaction {
	t.Helper()

	atxBytes := genesis.BuildSponsoredTx(
		senderKey, sponsorKey, testSystemPod, "create_object", []byte("x"), []uint16{1}, 0, 1000, gasCoin, validUntil, nil, nil,
	)

	return types.GetRootAsAttestedTransaction(atxBytes, 0).Transaction(nil)
}

// TestVerifyTxAuthenticity_SponsoredValid confirms a correctly doubly-signed
// sponsored transaction passes commit-time authenticity.
func TestVerifyTxAuthenticity_SponsoredValid(t *testing.T) {
	_, senderKey, _ := ed25519.GenerateKey(nil)
	_, sponsorKey, _ := ed25519.GenerateKey(nil)

	var gasCoin [32]byte
	gasCoin[0] = 0x77

	tx := sponsoredTestTx(t, senderKey, sponsorKey, gasCoin, 9)

	if err := verifyTxAuthenticity(tx); err != nil {
		t.Fatalf("valid sponsored tx rejected: %v", err)
	}
}

// TestVerifyTxAuthenticity_SponsorForged confirms a sponsored transaction with an
// invalid sponsor signature is rejected AT COMMIT, so a gossiped forged sponsored
// tx naming a victim as fee_payer cannot drain that victim's coin.
func TestVerifyTxAuthenticity_SponsorForged(t *testing.T) {
	_, senderKey, _ := ed25519.GenerateKey(nil)
	victimPub, _, _ := ed25519.GenerateKey(nil)
	_, attackerKey, _ := ed25519.GenerateKey(nil)

	var gasCoin [32]byte
	gasCoin[0] = 0x77

	// Sender signs the real body; the body names the VICTIM as fee_payer, but the
	// attacker (who does not hold the victim's key) signs the sponsor slot.
	senderPub := senderKey.Public().(ed25519.PublicKey)
	sponsor := genesis.Sponsorship{FeePayer: victimPub, ValidUntil: 9}
	body := genesis.BuildUnsignedTxBytesSponsored(senderPub, testSystemPod, "create_object", []byte("x"), []uint16{1}, 0, 1000, gasCoin[:], nil, nil, sponsor, nil, nil)

	hash := blake3.Sum256(body)
	senderSig := ed25519.Sign(senderKey, hash[:])
	attackerSig := ed25519.Sign(attackerKey, hash[:]) // forged sponsor sig

	builder := flatbuffers.NewBuilder(1024)
	txOff := genesis.BuildTxTableSponsored(
		builder, senderPub, testSystemPod, "create_object", []byte("x"), []uint16{1}, 0, 1000, gasCoin[:], hash, senderSig, nil, nil, sponsor, attackerSig, nil, nil,
	)
	builder.Finish(txOff)
	tx := types.GetRootAsTransaction(builder.FinishedBytes(), 0)

	if err := verifyTxAuthenticity(tx); err == nil {
		t.Fatal("forged sponsor signature accepted, want rejection")
	}
}

// TestValidateGasCoin_SponsoredOwnedByFeePayer confirms a sponsored tx's gas coin
// must be owned by the fee_payer (not the sender), and a non-sponsored tx still
// requires owner == sender.
func TestValidateGasCoin_SponsoredOwnedByFeePayer(t *testing.T) {
	db := newTestStorage(t)
	validators, vs := newTestValidatorSet(3)
	dag := New(db, vs, &mockBroadcaster{}, testSystemPod, 0, validators[0].privKey, nil)
	defer dag.Close()

	coinStore := newMockCoinStore()
	params := DefaultFeeParams()
	dag.SetFeeSystem(coinStore, &params, nil)

	senderPub, senderKey, _ := ed25519.GenerateKey(nil)
	sponsorPub, sponsorKey, _ := ed25519.GenerateKey(nil)

	var gasCoinID, sponsorOwner, senderOwner [32]byte
	copy(sponsorOwner[:], sponsorPub)
	copy(senderOwner[:], senderPub)
	gasCoinID[0] = 0xCC

	// Gas coin owned by the SPONSOR (fee_payer).
	coinStore.SetObject(buildTestCoinObject(gasCoinID, 100000, sponsorOwner, 0))

	sponsored := sponsoredTestTx(t, senderKey, sponsorKey, gasCoinID, 9)
	if err := dag.validateGasCoin(sponsored, gasCoinID); err != nil {
		t.Fatalf("sponsored gas coin owned by fee_payer rejected: %v", err)
	}

	// A non-sponsored tx with the same gas coin (owned by the sponsor, not the
	// sender) must be rejected: owner must equal sender.
	nonSponsored := signedGasCoinTx(t, senderKey, gasCoinID)
	if err := dag.validateGasCoin(nonSponsored, gasCoinID); err == nil {
		t.Fatal("non-sponsored tx accepted a gas coin not owned by the sender")
	}

	// Re-own the coin to the sender; the non-sponsored tx now passes.
	coinStore.SetObject(buildTestCoinObject(gasCoinID, 100000, senderOwner, 0))
	if err := dag.validateGasCoin(nonSponsored, gasCoinID); err != nil {
		t.Fatalf("non-sponsored tx rejected a gas coin owned by the sender: %v", err)
	}
}

// signedGasCoinTx builds a self-paid tx referencing gasCoin, owned by the sender.
func signedGasCoinTx(t *testing.T, senderKey ed25519.PrivateKey, gasCoin [32]byte) *types.Transaction {
	t.Helper()

	atxBytes := genesis.BuildAttestedTx(senderKey, testSystemPod, "create_object", []byte("x"), []uint16{1}, 0, 1000, gasCoin[:])

	return types.GetRootAsAttestedTransaction(atxBytes, 0).Transaction(nil)
}

// TestSponsoredTxValidUntil confirms valid_until enforcement against the round's
// commit epoch: an expired bound is rejected, a current/future bound proceeds, a
// sponsored tx with valid_until==0 is rejected, and a non-sponsored tx is never
// subject to the check. The boundary round (which maps to epoch k-1) is exercised
// explicitly so a naive round/epochLength mapping would fail the test.
func TestSponsoredTxValidUntil(t *testing.T) {
	db := newTestStorage(t)
	validators, vs := newTestValidatorSet(4)
	dag := New(db, vs, nil, testSystemPod, 0, validators[0].privKey, nil, WithEpochLength(100))
	defer dag.Close()

	_, senderKey, _ := ed25519.GenerateKey(nil)
	_, sponsorKey, _ := ed25519.GenerateKey(nil)

	var gasCoin [32]byte
	gasCoin[0] = 0x55

	build := func(validUntil uint64) *types.Transaction {
		return sponsoredTestTx(t, senderKey, sponsorKey, gasCoin, validUntil)
	}

	// round 250 → commit epoch 2.
	const round = uint64(250)
	if dag.commitEpochForRound(round) != 2 {
		t.Fatalf("test setup: commitEpochForRound(%d) = %d, want 2", round, dag.commitEpochForRound(round))
	}

	if dag.sponsoredTxStillValid(build(0), round) {
		t.Error("sponsored tx with valid_until==0 accepted, want rejection")
	}

	if dag.sponsoredTxStillValid(build(1), round) {
		t.Error("sponsored tx with valid_until(1) < commit epoch(2) accepted, want rejection")
	}

	if !dag.sponsoredTxStillValid(build(2), round) {
		t.Error("sponsored tx with valid_until(2) == commit epoch(2) rejected, want acceptance")
	}

	if !dag.sponsoredTxStillValid(build(3), round) {
		t.Error("sponsored tx with valid_until(3) > commit epoch(2) rejected, want acceptance")
	}

	// Boundary round 200 maps to epoch 1 (not 2): valid_until==1 must proceed.
	const boundary = uint64(200)
	if dag.commitEpochForRound(boundary) != 1 {
		t.Fatalf("test setup: commitEpochForRound(%d) = %d, want 1", boundary, dag.commitEpochForRound(boundary))
	}

	if !dag.sponsoredTxStillValid(build(1), boundary) {
		t.Error("boundary round: valid_until(1) == commit epoch(1) rejected, want acceptance")
	}

	if dag.sponsoredTxStillValid(build(0), boundary) {
		t.Error("boundary round: valid_until==0 accepted, want rejection")
	}

	// A non-sponsored tx (no fee_payer, valid_until absent) is always valid.
	nonSponsored := signedGasCoinTx(t, senderKey, gasCoin)
	if !dag.sponsoredTxStillValid(nonSponsored, round) {
		t.Error("non-sponsored tx rejected by valid_until check")
	}
}
