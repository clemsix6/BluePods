package client

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"testing"

	flatbuffers "github.com/google/flatbuffers/go"
	"github.com/zeebo/blake3"

	"BluePods/internal/genesis"
	"BluePods/internal/types"
)

// TestSponsoredTxAssembly confirms the client assembles a doubly-signed sponsored
// transaction: both the sender and sponsor signatures verify against the same body
// hash, the fee_payer is the sponsor, valid_until is carried, and the gas coin is
// the sponsor's coin. This is the artifact SubmitSponsored sends to the network.
func TestSponsoredTxAssembly(t *testing.T) {
	_, senderPriv, _ := ed25519.GenerateKey(rand.Reader)
	_, sponsorPriv, _ := ed25519.GenerateKey(rand.Reader)

	sender := &Wallet{privKey: senderPriv, pubKey: senderPriv.Public().(ed25519.PublicKey), coins: map[[32]byte]*CoinInfo{}}
	sponsor := &Wallet{privKey: sponsorPriv, pubKey: sponsorPriv.Public().(ed25519.PublicKey), coins: map[[32]byte]*CoinInfo{}}

	var gasCoin [32]byte
	gasCoin[0] = 0xAB

	op := SponsoredOp{
		Pod:         testPod(),
		FuncName:    "create_object",
		Args:        encodeCreateObjectArgs(sender.Pubkey(), 1, []byte("meta")),
		CreatedReps: []uint16{1},
	}

	signed := sender.SignSponsoredOp(op, sponsor.Pubkey(), gasCoin, 7)

	// Reproduce the sponsor's assembly to inspect the wire artifact.
	txBytes := assembleSponsored(sponsor, signed)
	tx := types.GetRootAsTransaction(txBytes, 0)

	if !bytes.Equal(tx.SenderBytes(), sender.pubKey) {
		t.Errorf("sender mismatch: got %x", tx.SenderBytes())
	}

	if !bytes.Equal(tx.FeePayerBytes(), sponsor.pubKey) {
		t.Errorf("fee_payer: got %x, want sponsor %x", tx.FeePayerBytes(), []byte(sponsor.pubKey))
	}

	if tx.ValidUntil() != 7 {
		t.Errorf("valid_until: got %d, want 7", tx.ValidUntil())
	}

	if gc := tx.GasCoinBytes(); len(gc) != 32 || !bytes.Equal(gc, gasCoin[:]) {
		t.Errorf("gas coin: got %x, want %x", gc, gasCoin[:])
	}

	hash := tx.HashBytes()

	if !ed25519.Verify(sender.pubKey, hash, tx.SignatureBytes()) {
		t.Error("sender signature does not verify")
	}

	if !ed25519.Verify(sponsor.pubKey, hash, tx.SponsorSignatureBytes()) {
		t.Error("sponsor signature does not verify")
	}
}

// assembleSponsored mirrors SubmitSponsored's tx assembly without a live node, so
// the wire artifact can be inspected in a unit test.
func assembleSponsored(sponsor *Wallet, signed SignedSponsoredOp) []byte {
	sponsorPubkey := sponsor.Pubkey()
	body := sponsoredBody(signed.Sender, signed.Op, sponsorPubkey, signed.GasCoin, signed.ValidUntil)
	hash := blake3.Sum256(body)

	s := genesis.Sponsorship{FeePayer: sponsor.pubKey, ValidUntil: signed.ValidUntil}
	sponsorSig := ed25519.Sign(sponsor.privKey, hash[:])

	builder := flatbuffers.NewBuilder(1024)
	txOff := genesis.BuildTxTableSponsored(
		builder, signed.Sender, signed.Op.Pod, signed.Op.FuncName, signed.Op.Args, signed.Op.CreatedReps,
		0, clientMaxGas, signed.GasCoin[:], hash, signed.SenderSig, signed.Op.MutableRefs, signed.Op.ReadRefs, s, sponsorSig, nil, signed.Op.Operations,
	)
	builder.Finish(txOff)

	return builder.FinishedBytes()
}
