package scenarios

import (
	"context"
	"encoding/binary"
	"strings"
	"testing"

	"BluePods/internal/network"
	"BluePods/pkg/client"
	"BluePods/test/harness"
)

const (
	// stakeScenarioSize is the validator count for TestScenarioStake.
	stakeScenarioSize = 5

	// unbondCoinFunding funds the fresh singleton coin a non-founder validator
	// unbonds part of its self-stake into. It only needs to cover the unbond
	// tx's own gas fee (it is both the credited coin and the gas coin), so a
	// modest amount comfortably below the validator's setup self-stake
	// (defaultStakeTarget, ~1e11 on the harness's default mint) is enough.
	unbondCoinFunding = uint64(1_000_000)

	// unbondAmount is the PART of self-stake unbonded: far below the
	// validator's setup self-stake, so the operation never touches the
	// minimum-stake floor.
	unbondAmount = uint64(500_000)

	// delegateFunding funds the fresh wallet that delegates to a validator; it
	// must comfortably cover delegateAmount plus the delegate tx's own fee.
	delegateFunding = uint64(5_000_000)

	// delegateAmount is the amount delegated, and (delegate never being
	// partial) also the exact principal undelegate later returns.
	delegateAmount = uint64(1_000_000)
)

// stakeDelta captures one stake operation's fingerprint terms immediately
// before and after it commits, both queried from the SAME node (cli's): each
// snapshot is one node's own locally-evaluable view, and the delta between
// two of them cancels any constant cross-scenario baseline (like the
// per-registration supply-identity inflation from this cluster's own
// non-founder registrations) that never changes across the single operation
// being measured. amount is the operation's signed effect on TotalBonded
// (positive for delegate, negative for unbond/undelegate); fee is the tx's
// own fees.deducted amount.
type stakeDelta struct {
	op     string
	amount int64
	fee    uint64
	before *network.FingerprintResponse
	after  *network.FingerprintResponse
}

// TestScenarioStake drives a 5-node cluster through the stake lifecycle no
// prior scenario exercised: a non-founder validator partially unbonding its
// self-stake back to a coin, a fresh wallet delegating to that validator, and
// the same delegator fully undelegating. All three effects are IMMEDIATE at
// commit, in the same transaction that applies them (unlike register/
// deregister validator, which defer to the next epoch boundary): handleBond,
// handleUnbond, handleDelegate and handleUndelegate (internal/consensus/
// commit.go) mutate self-stake/delegated totals and credit/debit the coin
// synchronously, so this scenario asserts on typed events and object/coin
// state, never on the epoch-snapshotted consensus weight that would make a
// weight assertion racy.
//
// supply_delta_conserved is the discriminating sub-test: instead of the raw
// per-node supply identity (polluted by the +4000-per-registration deposit
// leak baked into this cluster's own 4 non-founder setup registrations), it
// captures the fingerprint's coins/bonded/deposits/fees terms immediately
// before and after each operation and checks the DELTA conserves exactly,
// with the bonded<->coins transfer equal to the amount moved and the
// residual accounted for by that operation's own fee.
//
// Teardown is still red on the per-node supply identity: this cluster's 4
// non-founder setup registrations each stamp a +1000 storage deposit no coin
// pays, inflating the raw supply identity by +4000 — the same circumstance
// as every other 5-node functional scenario in this corpus.
//
// undelegate_returns_principal is also expected red IN THE BODY, per BUGS.md
// entry 11: confirming the deleted delegation position is no longer readable
// calls cli.GetObject (routed, non-local) on an object no node holds, which
// this scenario is the first to exercise, and that path cascades across the
// mesh instead of returning a prompt not-found, timing out the client. Every
// other sub-test is expected green.
func TestScenarioStake(t *testing.T) {
	if testing.Short() {
		t.Skip("scenario")
	}

	c := harness.NewCluster(t, stakeScenarioSize)
	node0 := c.Node(0)
	cli := c.Client(0)

	validator := walletFromNodeKey(t, c.Node(1))
	validatorPub := validator.Pubkey()

	var deltas []stakeDelta

	t.Run("unbond_credits_coin", func(t *testing.T) {
		deltas = append(deltas, testUnbondCreditsCoin(t, c, cli, node0, validator))
	})

	var delegator *client.Wallet
	var delegateCoinID, posID [32]byte

	t.Run("delegate_creates_position", func(t *testing.T) {
		var d stakeDelta
		delegator, delegateCoinID, posID, d = testDelegateCreatesPosition(t, c, cli, node0, validatorPub)
		deltas = append(deltas, d)
	})

	t.Run("undelegate_returns_principal", func(t *testing.T) {
		deltas = append(deltas, testUndelegateReturnsPrincipal(t, c, cli, node0, validatorPub, delegator, delegateCoinID, posID))
	})

	t.Run("supply_delta_conserved", func(t *testing.T) {
		requireDeltasConserved(t, deltas)
	})
}

// testUnbondCreditsCoin has validator (a non-founder validator whose wallet is
// rebuilt from its own node key, already self-staked by the harness's default
// setup) unbond part of that self-stake into a fresh singleton coin it owns.
// It confirms the commit succeeds on every node, the stake.unbonded event
// carries the validator/coin/amount, and the coin's balance nets the credit
// against the unbond tx's own gas fee (the coin is both credited and gas
// coin).
func testUnbondCreditsCoin(t *testing.T, c *harness.Cluster, cli *client.Client, node0 *harness.Node, validator *client.Wallet) stakeDelta {
	t.Helper()

	coinID, faucetHash, err := cli.Faucet(validator.Pubkey(), unbondCoinFunding)
	requireNoErr(t, err)
	requireCommittedSuccess(stepCtx(t), t, node0, faucetHash)
	requireNoErr(t, validator.RefreshCoin(cli, coinID))

	before, err := cli.Fingerprint()
	requireNoErr(t, err)

	hash, err := validator.Unbond(cli, coinID, validator.GetCoin(coinID).Version, unbondAmount)
	requireNoErr(t, err)
	requireVerdictAll(stepCtx(t), t, c, hash, true, "")

	ev, err := node0.WaitEvent(stepCtx(t), "stake.unbonded",
		harness.Attr("validator", hexID(validator.Pubkey())),
		harness.Attr("coin", hexID(coinID)),
		harness.Attr("amount", unbondAmount))
	requireNoErr(t, err)
	_ = ev

	fee := waitFeeDeducted(stepCtx(t), t, node0, hash)

	requireNoErr(t, validator.RefreshCoin(cli, coinID))
	if got, want := validator.GetCoin(coinID).Balance, unbondCoinFunding-fee+unbondAmount; got != want {
		t.Fatalf("unbond credited coin balance: got %d, want %d (funding %d - fee %d + unbond %d)",
			got, want, unbondCoinFunding, fee, unbondAmount)
	}

	after, err := cli.Fingerprint()
	requireNoErr(t, err)

	return stakeDelta{op: "unbond", amount: -int64(unbondAmount), fee: fee, before: before, after: after}
}

// testDelegateCreatesPosition has a freshly faucet-funded wallet delegate part
// of its coin to validator. It confirms the commit succeeds on every node,
// the stake.delegated event carries the validator/position/amount, the
// delegator's coin balance nets the debit against the delegate tx's own gas
// fee (the coin is both debited and gas coin), and the SDK-predicted position
// object exists, owned by the delegator and encoding validator and amount.
// Returns the delegator wallet, its coin ID (needed again to undelegate), the
// position ID, and this operation's fingerprint delta.
func testDelegateCreatesPosition(t *testing.T, c *harness.Cluster, cli *client.Client, node0 *harness.Node, validator [32]byte) (*client.Wallet, [32]byte, [32]byte, stakeDelta) {
	t.Helper()

	delegator, coinID := fundedWallet(stepCtx(t), t, cli, node0, delegateFunding)

	before, err := cli.Fingerprint()
	requireNoErr(t, err)

	posID, hash, err := delegator.Delegate(cli, coinID, delegator.GetCoin(coinID).Version, validator, delegateAmount)
	requireNoErr(t, err)
	requireVerdictAll(stepCtx(t), t, c, hash, true, "")

	ev, err := node0.WaitEvent(stepCtx(t), "stake.delegated",
		harness.Attr("validator", hexID(validator)),
		harness.Attr("position", hexID(posID)),
		harness.Attr("amount", delegateAmount))
	requireNoErr(t, err)
	_ = ev

	fee := waitFeeDeducted(stepCtx(t), t, node0, hash)

	requireNoErr(t, delegator.RefreshCoin(cli, coinID))
	if got, want := delegator.GetCoin(coinID).Balance, delegateFunding-delegateAmount-fee; got != want {
		t.Fatalf("delegator coin balance after delegate: got %d, want %d (funding %d - delegated %d - fee %d)",
			got, want, delegateFunding, delegateAmount, fee)
	}

	requireDelegationPosition(t, cli, posID, delegator.Pubkey(), validator, delegateAmount)

	after, err := cli.Fingerprint()
	requireNoErr(t, err)

	delta := stakeDelta{op: "delegate", amount: int64(delegateAmount), fee: fee, before: before, after: after}

	return delegator, coinID, posID, delta
}

// testUndelegateReturnsPrincipal has delegator withdraw its full delegation
// from validator (undelegate carries no amount: handleUndelegate always
// withdraws the position's entire principal). It confirms the commit succeeds
// on every node, the stake.undelegated event carries the validator/position/
// the full delegateAmount, the coin's balance nets the credited principal
// against the undelegate tx's own gas fee, and the position object is
// destroyed (handleUndelegate deletes it, so it must no longer be readable).
func testUndelegateReturnsPrincipal(t *testing.T, c *harness.Cluster, cli *client.Client, node0 *harness.Node, validator [32]byte, delegator *client.Wallet, coinID, posID [32]byte) stakeDelta {
	t.Helper()

	balanceBefore := delegator.GetCoin(coinID).Balance

	before, err := cli.Fingerprint()
	requireNoErr(t, err)

	hash, err := delegator.Undelegate(cli, coinID, delegator.GetCoin(coinID).Version, validator)
	requireNoErr(t, err)
	requireVerdictAll(stepCtx(t), t, c, hash, true, "")

	ev, err := node0.WaitEvent(stepCtx(t), "stake.undelegated",
		harness.Attr("validator", hexID(validator)),
		harness.Attr("position", hexID(posID)),
		harness.Attr("amount", delegateAmount))
	requireNoErr(t, err)
	_ = ev

	fee := waitFeeDeducted(stepCtx(t), t, node0, hash)

	requireNoErr(t, delegator.RefreshCoin(cli, coinID))
	if got, want := delegator.GetCoin(coinID).Balance, balanceBefore+delegateAmount-fee; got != want {
		t.Fatalf("delegator coin balance after undelegate: got %d, want %d (before %d + principal %d - fee %d)",
			got, want, balanceBefore, delegateAmount, fee)
	}

	requirePositionGone(t, cli, posID)

	after, err := cli.Fingerprint()
	requireNoErr(t, err)

	return stakeDelta{op: "undelegate", amount: -int64(delegateAmount), fee: fee, before: before, after: after}
}

// delegationContentSize is the serialized length of a delegation position's
// content: validator pubkey (32) followed by the delegated amount (8, little-
// endian). It mirrors internal/consensus's own (unexported)
// encodeDelegationContent, duplicated here rather than imported, following the
// same client-side convention pkg/client's delegationPositionID already uses
// for the position ID itself.
const delegationContentSize = 32 + 8

// requireDelegationPosition asserts the delegation position posID exists, is
// owned by delegator, is a singleton, and its content encodes validator and
// amount exactly as handleDelegate wrote them.
func requireDelegationPosition(t *testing.T, cli *client.Client, posID, delegator, validator [32]byte, amount uint64) {
	t.Helper()

	obj, err := cli.GetObject(posID)
	requireNoErr(t, err)

	if obj.Owner != delegator {
		t.Fatalf("delegation position owner: got %x, want delegator %x", obj.Owner[:8], delegator[:8])
	}
	if obj.Replication != 0 {
		t.Fatalf("delegation position replication: got %d, want 0 (singleton)", obj.Replication)
	}
	if len(obj.Content) != delegationContentSize {
		t.Fatalf("delegation position content length: got %d, want %d", len(obj.Content), delegationContentSize)
	}

	var gotValidator [32]byte
	copy(gotValidator[:], obj.Content[:32])
	if gotValidator != validator {
		t.Fatalf("delegation position validator: got %x, want %x", gotValidator[:8], validator[:8])
	}

	if got := binary.LittleEndian.Uint64(obj.Content[32:]); got != amount {
		t.Fatalf("delegation position amount: got %d, want %d", got, amount)
	}
}

// requirePositionGone asserts posID is no longer readable, the expected fate
// of a delegation position once handleUndelegate deletes it. Any error other
// than "not found" is reported distinctly, so a real connectivity problem is
// never mistaken for confirmation that the position was destroyed.
//
// cli.GetObject routes to every other holder when this node lacks the
// object; the inter-node request is LocalOnly, so a holder that also lacks
// it (every node, here) answers a direct not-found instead of cascading into
// probing further. The call returns promptly with "not found" well inside
// the QUIC client's 8s request deadline.
func requirePositionGone(t *testing.T, cli *client.Client, posID [32]byte) {
	t.Helper()

	_, err := cli.GetObject(posID)
	if err == nil {
		t.Fatalf("delegation position %x still readable after undelegate: handleUndelegate is documented to delete it", posID[:8])
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Fatalf("delegation position %x: unexpected error (want \"not found\"): %v", posID[:8], err)
	}
}

// waitFeeDeducted waits for hash's fees.deducted event on n and returns the
// deducted amount: the fee supply_delta_conserved must find leaving the coin
// on top of the operation's own stake amount.
func waitFeeDeducted(ctx context.Context, t *testing.T, n *harness.Node, hash [32]byte) uint64 {
	t.Helper()

	ev, err := n.WaitEvent(ctx, "fees.deducted", harness.Attr("tx", hexID(hash)))
	requireNoErr(t, err)

	fee, ok := ev.Attrs["amount"].(float64)
	if !ok || fee <= 0 {
		t.Fatalf("fees.deducted carries no positive amount: %v", ev.Attrs)
	}

	return uint64(fee)
}

// requireDeltasConserved checks every recorded stakeDelta conserves the
// protocol supply identity's four locally-evaluable terms across the single
// operation it spans: the bonded<->coins transfer matches the operation's
// amount exactly, deposits never move (none of these three operations touch
// the tracked-object deposit path), the fees-in-flight delta matches the
// operation's own fees.deducted amount exactly (BurnBPS is 0 by default, so a
// consumed fee is entirely pooled, never burned), and the four terms'
// combined delta is exactly zero.
func requireDeltasConserved(t *testing.T, deltas []stakeDelta) {
	t.Helper()

	for _, d := range deltas {
		coinsDelta := int64(d.after.CoinsTotal) - int64(d.before.CoinsTotal)
		bondedDelta := int64(d.after.TotalBonded) - int64(d.before.TotalBonded)
		depositsDelta := int64(d.after.Deposits) - int64(d.before.Deposits)
		feesDelta := int64(d.after.FeesInFlight) - int64(d.before.FeesInFlight)

		if bondedDelta != d.amount {
			t.Fatalf("%s: bonded delta: got %d, want %d", d.op, bondedDelta, d.amount)
		}
		if depositsDelta != 0 {
			t.Fatalf("%s: deposits delta: got %d, want 0 (no tracked object created or deleted)", d.op, depositsDelta)
		}
		if feesDelta != int64(d.fee) {
			t.Fatalf("%s: fees-in-flight delta: got %d, want %d (this operation's own fees.deducted amount)", d.op, feesDelta, d.fee)
		}

		wantCoinsDelta := -bondedDelta - feesDelta
		if coinsDelta != wantCoinsDelta {
			t.Fatalf("%s: coins delta: got %d, want %d (= -bondedDelta(%d) - feesDelta(%d))",
				d.op, coinsDelta, wantCoinsDelta, bondedDelta, feesDelta)
		}

		if sum := coinsDelta + bondedDelta + depositsDelta + feesDelta; sum != 0 {
			t.Fatalf("%s: supply delta not conserved: coins(%d)+bonded(%d)+deposits(%d)+fees(%d) = %d, want 0",
				d.op, coinsDelta, bondedDelta, depositsDelta, feesDelta, sum)
		}
	}
}
