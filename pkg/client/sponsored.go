package client

import (
	"crypto/ed25519"
	"fmt"

	flatbuffers "github.com/google/flatbuffers/go"
	"github.com/zeebo/blake3"

	"BluePods/internal/genesis"
)

// SponsoredOp describes the operation a sender wants a sponsor to pay for. The
// sender fills it and signs the resulting canonical body; the sponsor then adds
// its fee_payer binding, signs the same body, and submits. The two signatures
// cover one shared body hash, so neither can be lifted onto another body.
type SponsoredOp struct {
	Pod         [32]byte                // Pod is the executing pod ID.
	FuncName    string                  // FuncName is the pod function to call.
	Args        []byte                  // Args is the serialized function arguments.
	CreatedReps []uint16                // CreatedReps is the replication of each created object.
	MutableRefs []genesis.ObjectRefData // MutableRefs are the mutated/deleted object references.
	ReadRefs    []genesis.ObjectRefData // ReadRefs are the read-only object references.
}

// SignedSponsoredOp is a sender-signed operation awaiting a sponsor. It carries
// everything the sponsor needs to reconstruct and co-sign the SAME body: the op,
// the gas budget and bound the sponsor will honor, and the sender's signature.
type SignedSponsoredOp struct {
	Op         SponsoredOp       // Op is the operation the sender authorized.
	Sender     ed25519.PublicKey // Sender is the sender's public key.
	GasCoin    [32]byte          // GasCoin is the sponsor's coin that pays gas.
	ValidUntil uint64            // ValidUntil is the last epoch the tx may commit.
	SenderSig  []byte            // SenderSig is the sender's signature over the body hash.
}

// SignSponsoredOp builds the canonical sponsored body for op (bound to the
// sponsor's GasCoin and ValidUntil) and signs it with the sender's key. The
// sponsor's pubkey must be supplied so the fee_payer it binds matches the body
// the sender signs. The result is handed to the sponsor for co-signing.
func (w *Wallet) SignSponsoredOp(op SponsoredOp, sponsorPubkey [32]byte, gasCoin [32]byte, validUntil uint64) SignedSponsoredOp {
	body := sponsoredBody(w.pubKey, op, sponsorPubkey, gasCoin, validUntil)
	hash := blake3.Sum256(body)

	return SignedSponsoredOp{
		Op:         op,
		Sender:     w.pubKey,
		GasCoin:    gasCoin,
		ValidUntil: validUntil,
		SenderSig:  ed25519.Sign(w.privKey, hash[:]),
	}
}

// SubmitSponsored co-signs a sender-authorized operation as the sponsor (fee_payer)
// and submits the doubly-signed transaction. The sponsor signs the SAME body hash
// the sender signed; a mismatch (a tampered op) yields a body the sender's
// signature no longer covers, which is rejected at commit. Returns the tx hash
// (the same canonical body hash both parties signed), so a caller can wait on
// tx.committed without recomputing it independently.
func (w *Wallet) SubmitSponsored(c *Client, signed SignedSponsoredOp) ([32]byte, error) {
	sponsorPubkey := w.Pubkey()
	body := sponsoredBody(signed.Sender, signed.Op, sponsorPubkey, signed.GasCoin, signed.ValidUntil)
	hash := blake3.Sum256(body)

	sponsor := genesis.Sponsorship{FeePayer: w.pubKey, ValidUntil: signed.ValidUntil}
	sponsorSig := ed25519.Sign(w.privKey, hash[:])

	builder := flatbuffers.NewBuilder(1024)
	txOff := genesis.BuildTxTableSponsored(
		builder, signed.Sender, signed.Op.Pod, signed.Op.FuncName, signed.Op.Args, signed.Op.CreatedReps,
		0, clientMaxGas, signed.GasCoin[:], hash, signed.SenderSig, signed.Op.MutableRefs, signed.Op.ReadRefs, sponsor, sponsorSig, nil,
	)
	builder.Finish(txOff)

	if err := c.submit(builder.FinishedBytes()); err != nil {
		return [32]byte{}, fmt.Errorf("submit sponsored tx:\n%w", err)
	}

	return hash, nil
}

// sponsoredBody builds the canonical unsigned body shared by the sender and the
// sponsor. It is the single construction both parties hash, so their signatures
// always cover identical bytes.
func sponsoredBody(sender ed25519.PublicKey, op SponsoredOp, sponsorPubkey [32]byte, gasCoin [32]byte, validUntil uint64) []byte {
	sponsor := genesis.Sponsorship{FeePayer: sponsorPubkey[:], ValidUntil: validUntil}

	return genesis.BuildUnsignedTxBytesSponsored(
		sender, op.Pod, op.FuncName, op.Args, op.CreatedReps, 0, clientMaxGas, gasCoin[:], op.MutableRefs, op.ReadRefs, sponsor, nil,
	)
}
