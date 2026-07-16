package scenarios

import (
	"encoding/hex"
	"testing"

	"github.com/zeebo/blake3"

	"BluePods/internal/genesis"
	"BluePods/pkg/client"
	"BluePods/test/harness"
)

// sponsoredScenarioSize is the validator count for TestScenarioSponsored.
const sponsoredScenarioSize = 5

// sponsoredClientMaxGas mirrors pkg/client's unexported clientMaxGas default:
// SubmitSponsored/SignSponsoredOp hardcode it into the canonical body they
// build and sign, so computeSponsoredHash must use the same value to
// reproduce the exact bytes both parties hashed.
const sponsoredClientMaxGas = uint64(1000)

// TestScenarioSponsored drives a 5-node cluster through sponsored
// transactions: a sender who owns the operated coin but has a distinct
// sponsor pay gas from its own coin (the sender's signature authorizes the
// operation, the sponsor's co-signature binds fee_payer/valid_until), and the
// commit-time valid_until rejection when a sponsorship carries no bound.
//
// Expected red, per test/BUGS.md: teardown's automatic convergence check
// fails against entry 1, and entry 8 keeps the supply term inflated by the
// default stake setup's non-founder registrations — the same circumstance as
// every other 5-node functional scenario in this corpus.
func TestScenarioSponsored(t *testing.T) {
	if testing.Short() {
		t.Skip("scenario")
	}

	c := harness.NewCluster(t, sponsoredScenarioSize)
	node0 := c.Node(0)
	cli := c.Client(0)
	systemPod := cli.SystemPod()

	t.Run("sponsored_success", func(t *testing.T) {
		testSponsoredSuccess(t, c, cli, node0, systemPod)
	})

	t.Run("expired_sponsorship_rejected", func(t *testing.T) {
		testExpiredSponsorshipRejected(t, c, cli, node0, systemPod)
	})
}

// testSponsoredSuccess has a sender transfer its own coin to a fresh
// recipient while a separate sponsor pays gas from its own coin. It confirms
// the doubly-signed transaction commits successfully on every node, that
// fees.deducted names the SPONSOR's coin (never the sender's), that the
// sponsor's coin balance drops by exactly the deducted fee, and that the
// sender's coin carries only the transfer's effect (new owner, untouched
// balance) with no fee trace on it at all.
func testSponsoredSuccess(t *testing.T, c *harness.Cluster, cli *client.Client, node0 *harness.Node, systemPod [32]byte) {
	t.Helper()

	const funding = uint64(1_000_000)

	sender, coinID := fundedWallet(stepCtx(t), t, cli, node0, funding)
	sponsor, gasCoin := fundedWallet(stepCtx(t), t, cli, node0, funding)
	recipient := client.NewWallet()
	recipientPub := recipient.Pubkey()

	op := client.SponsoredOp{
		Pod:         systemPod,
		FuncName:    "transfer",
		Args:        sponsoredTransferArgs(recipientPub),
		MutableRefs: []genesis.ObjectRefData{{ID: coinID, Version: sender.GetCoin(coinID).Version}},
	}

	// Comfortably beyond any epoch this scenario could reach: the point of
	// this step is a successful sponsorship, not a race against epoch
	// transitions (see testExpiredSponsorshipRejected for the deterministic
	// rejection case instead of chasing a real epoch boundary).
	const farFutureEpoch = uint64(1_000_000)

	signed := sender.SignSponsoredOp(op, sponsor.Pubkey(), gasCoin, farFutureEpoch)
	hash := computeSponsoredHash(sender.Pubkey(), sponsor.Pubkey(), op, gasCoin, farFutureEpoch)

	requireNoErr(t, sponsor.SubmitSponsored(cli, signed))
	requireVerdictAll(stepCtx(t), t, c, hash, true, "")

	ev, err := node0.WaitEvent(stepCtx(t), "fees.deducted",
		harness.Attr("tx", hex.EncodeToString(hash[:])),
		harness.Attr("coin", hex.EncodeToString(gasCoin[:])),
		harness.Attr("covered", true))
	requireNoErr(t, err)

	fee, ok := ev.Attrs["amount"].(float64)
	if !ok || fee <= 0 {
		t.Fatalf("fees.deducted carries no positive amount: %v", ev.Attrs)
	}

	requireNoErr(t, sponsor.RefreshCoin(cli, gasCoin))
	if got, want := sponsor.GetCoin(gasCoin).Balance, funding-uint64(fee); got != want {
		t.Fatalf("sponsor gas coin balance after sponsored transfer: got %d, want %d (funding %d - fee %d)",
			got, want, funding, uint64(fee))
	}

	obj, err := cli.GetObject(coinID)
	requireNoErr(t, err)
	if obj.Owner != recipientPub {
		t.Fatalf("sponsored transfer did not move ownership: got %x, want recipient %x", obj.Owner[:8], recipientPub[:8])
	}
	if coinBalance(obj) != funding {
		t.Fatalf("sender coin balance after sponsored transfer: got %d, want unchanged %d (fees must land on the sponsor, not the sender)",
			coinBalance(obj), funding)
	}
}

// testExpiredSponsorshipRejected submits the same sponsored shape with a
// valid_until of zero. sponsoredTxStillValid (internal/consensus/commit.go)
// treats a sponsored transaction's zero valid_until as expired/unbounded
// unconditionally, regardless of the current epoch: a real sponsorship must
// always carry a positive bound, so this is a deterministic way to land on
// the expired_sponsorship rejection without racing an actual epoch boundary.
// It confirms every node rejects the transaction with that exact reason, and
// that neither the sender's coin nor the sponsor's gas coin was touched: the
// check runs before fee deduction, so an expired sponsorship never charges
// anyone.
func testExpiredSponsorshipRejected(t *testing.T, c *harness.Cluster, cli *client.Client, node0 *harness.Node, systemPod [32]byte) {
	t.Helper()

	const funding = uint64(1_000_000)

	sender, coinID := fundedWallet(stepCtx(t), t, cli, node0, funding)
	sponsor, gasCoin := fundedWallet(stepCtx(t), t, cli, node0, funding)
	recipient := client.NewWallet()

	op := client.SponsoredOp{
		Pod:         systemPod,
		FuncName:    "transfer",
		Args:        sponsoredTransferArgs(recipient.Pubkey()),
		MutableRefs: []genesis.ObjectRefData{{ID: coinID, Version: sender.GetCoin(coinID).Version}},
	}

	const expiredValidUntil = uint64(0)

	signed := sender.SignSponsoredOp(op, sponsor.Pubkey(), gasCoin, expiredValidUntil)
	hash := computeSponsoredHash(sender.Pubkey(), sponsor.Pubkey(), op, gasCoin, expiredValidUntil)

	requireNoErr(t, sponsor.SubmitSponsored(cli, signed))
	requireVerdictAll(stepCtx(t), t, c, hash, false, "expired_sponsorship")

	requireUnchangedOwner(t, cli, coinID, sender.Pubkey())

	obj, err := cli.GetObject(coinID)
	requireNoErr(t, err)
	if coinBalance(obj) != funding {
		t.Fatalf("rejected sponsored tx changed the sender coin balance: got %d, want unchanged %d", coinBalance(obj), funding)
	}

	requireNoErr(t, sponsor.RefreshCoin(cli, gasCoin))
	if got := sponsor.GetCoin(gasCoin).Balance; got != funding {
		t.Fatalf("rejected sponsored tx charged the sponsor's gas coin: got %d, want unchanged %d", got, funding)
	}
}

// sponsoredTransferArgs encodes a sponsored "transfer" op's arguments in the
// same Borsh format pkg/client's own (unexported) encodeTransferArgs uses:
// the 32-byte new owner, nothing else.
func sponsoredTransferArgs(newOwner [32]byte) []byte {
	args := make([]byte, 32)
	copy(args, newOwner[:])

	return args
}

// computeSponsoredHash re-derives the canonical sponsored body hash that both
// the sender and the sponsor sign (pkg/client/sponsored.go's unexported
// sponsoredBody), since SubmitSponsored does not return the hash to its
// caller. This is the same hash tx.committed reports for the submitted
// transaction, so a scenario can wait on it.
func computeSponsoredHash(sender, sponsorPubkey [32]byte, op client.SponsoredOp, gasCoin [32]byte, validUntil uint64) [32]byte {
	sponsor := genesis.Sponsorship{FeePayer: sponsorPubkey[:], ValidUntil: validUntil}

	body := genesis.BuildUnsignedTxBytesSponsored(
		sender[:], op.Pod, op.FuncName, op.Args, op.CreatedReps, 0, sponsoredClientMaxGas, gasCoin[:], op.MutableRefs, op.ReadRefs, sponsor,
	)

	return blake3.Sum256(body)
}
