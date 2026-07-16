package scenarios

import (
	"encoding/hex"
	"testing"

	"BluePods/pkg/client"
	"BluePods/test/harness"
)

// TestScenarioFees drives a 5-node cluster through the fee rules: full
// deduction visible through fees.deducted events and balance reads, a split
// whose amount exceeds the coin's balance failing execution rather than fee
// deduction, the underfunded-gas-coin rejection with its partial amount
// pooled and the per-node supply identity asserted right after, and the
// three gas-coin validation rejections (missing, not owned, not a
// singleton).
//
// Expected red, per test/BUGS.md: the execution_error step's before/after
// Fingerprint delta check fails against entry 12 (a failed execution debits
// the fee's storage component from the coin but credits it nowhere), the
// underfunded step's supply-identity assertion fails against entry 8
// (validator registration stamps a deposit no coin pays, inflating the
// identity by 1000 per registration) compounded with entry 12's own
// deflation from the step before it (the partial-fee pooling itself is
// conserving, proven on a single node), and teardown's automatic
// convergence check fails against entry 1.
func TestScenarioFees(t *testing.T) {
	if testing.Short() {
		t.Skip("scenario")
	}

	c := harness.NewCluster(t, 5)
	node0 := c.Node(0)
	cli := c.Client(0)
	systemPod := cli.SystemPod()

	t.Run("full_deduction", func(t *testing.T) {
		testFullDeduction(t, c, cli, node0)
	})

	t.Run("split_exceeds_balance_is_execution_error", func(t *testing.T) {
		testSplitExceedsBalance(t, c, cli, node0)
	})

	t.Run("underfunded_gas_coin_pools_partial", func(t *testing.T) {
		testUnderfundedGasCoin(t, c, cli, node0)
	})

	t.Run("gas_coin_missing", func(t *testing.T) {
		testGasCoinMissing(t, c, cli, node0, systemPod)
	})

	t.Run("gas_coin_not_owned", func(t *testing.T) {
		testGasCoinNotOwned(t, c, cli, node0, systemPod)
	})

	t.Run("gas_coin_not_singleton", func(t *testing.T) {
		testGasCoinNotSingleton(t, c, cli, node0, systemPod)
	})
}

// testFullDeduction splits a well-funded coin and confirms the fee flow: a
// fees.deducted event with covered=true, and the source balance reduced by
// exactly split amount plus the event's deducted fee.
func testFullDeduction(t *testing.T, c *harness.Cluster, cli *client.Client, node0 *harness.Node) {
	t.Helper()

	const funding, splitAmount = uint64(1_000_000), uint64(100_000)

	w, coinID := fundedWallet(stepCtx(t), t, cli, node0, funding)
	recipient := client.NewWallet()

	_, hash, err := w.Split(cli, coinID, splitAmount, recipient.Pubkey())
	requireNoErr(t, err)
	requireVerdictAll(stepCtx(t), t, c, hash, true, "")

	ev, err := node0.WaitEvent(stepCtx(t), "fees.deducted",
		harness.Attr("tx", hex.EncodeToString(hash[:])), harness.Attr("covered", true))
	requireNoErr(t, err)

	fee, ok := ev.Attrs["amount"].(float64)
	if !ok || fee <= 0 {
		t.Fatalf("fees.deducted carries no positive amount: %v", ev.Attrs)
	}

	requireNoErr(t, w.RefreshCoin(cli, coinID))

	got := w.GetCoin(coinID).Balance
	want := funding - splitAmount - uint64(fee)
	if got != want {
		t.Fatalf("source balance after split: got %d, want %d (funding %d - split %d - fee %d)",
			got, want, funding, splitAmount, uint64(fee))
	}
}

// testSplitExceedsBalance funds a coin and submits a split whose amount
// equals the coin's FULL pre-fee balance. Protocol-level fee deduction
// (internal/consensus/commit.go deductFees) runs before execution and
// strictly reduces the coin, and pods/pod-system/src/functions/split's
// execute checks source.balance < args.amount against that POST-fee balance
// (internal/state's resolveMutableObjects reads a mutable ref fresh from
// local state rather than trusting a stale ATX-supplied snapshot), so an
// amount equal to the PRE-fee balance always exceeds what remains and the
// pod rejects with ERR_INSUFFICIENT_BALANCE regardless of the exact fee
// value. Confirms the typed execution_error verdict on every node, that the
// fee still landed (fee deduction is unconditional and precedes the WASM
// call), and that no new coin was ever created: the pod's ExecuteResult::err
// short-circuits before building one.
func testSplitExceedsBalance(t *testing.T, c *harness.Cluster, cli *client.Client, node0 *harness.Node) {
	t.Helper()

	const funding = uint64(100_000)

	w, coinID := fundedWallet(stepCtx(t), t, cli, node0, funding)
	recipient := client.NewWallet()

	before, err := cli.Fingerprint()
	requireNoErr(t, err)

	newCoinID, hash, err := w.Split(cli, coinID, funding, recipient.Pubkey())
	requireNoErr(t, err)
	requireVerdictAll(stepCtx(t), t, c, hash, false, "execution_error")

	ev, err := node0.WaitEvent(stepCtx(t), "fees.deducted",
		harness.Attr("tx", hex.EncodeToString(hash[:])), harness.Attr("covered", true))
	requireNoErr(t, err)

	fee, ok := ev.Attrs["amount"].(float64)
	if !ok || fee <= 0 {
		t.Fatalf("fees.deducted carries no positive amount: %v", ev.Attrs)
	}

	requireNoErr(t, w.RefreshCoin(cli, coinID))
	if got, want := w.GetCoin(coinID).Balance, funding-uint64(fee); got != want {
		t.Fatalf("source balance after failed split: got %d, want %d (funding %d - fee %d; the split itself never applied)",
			got, want, funding, uint64(fee))
	}

	data, err := client.NewQUICTransport(node0.QUICAddr).GetObjectLocal(newCoinID)
	requireNoErr(t, err)
	if data != nil {
		t.Fatalf("failed split created a new coin anyway: %x", newCoinID[:4])
	}

	// The fee's storage component (calculateTxFeeSplit, sized off the tx's
	// DECLARED created_objects_replication regardless of outcome) is debited
	// from the coin unconditionally, before execution, but deductFees pools
	// only the "consumed" portion (SplitFee(consumed, ...)); the storage
	// portion is meant to become a locked deposit, credited only inside
	// applyCreatedObjects (internal/state/state.go), which state.Execute
	// never reaches on a failed pod execution. So on THIS scenario's
	// execution_error path, what left the coin must exceed what
	// deposits+feesInFlight gained by exactly that storage component: a
	// deflationary supply leak, the mirror image of BUGS.md entry 8's
	// inflationary one. See test/BUGS.md entry 12 (RED ON PURPOSE: the
	// underlying leak is not fixed as part of adding this coverage).
	after, err := cli.Fingerprint()
	requireNoErr(t, err)

	coinsLost := before.CoinsTotal - after.CoinsTotal
	accountedFor := (after.Deposits - before.Deposits) + (after.FeesInFlight - before.FeesInFlight)
	if accountedFor != coinsLost {
		t.Fatalf("failed-execution fee not fully accounted: coin lost %d, deposits+fees_in_flight only gained %d (BUGS.md entry 12)",
			coinsLost, accountedFor)
	}
}

// testUnderfundedGasCoin funds a coin with 1 unit (below any fee), attempts a
// split, and asserts the typed fee_rejected verdict on every node, the
// partial (covered=false) deduction event, and the supply identity intact on
// every node afterwards: the drained unit must enter the epoch pool instead
// of vanishing (the Task 3 fix). The identity assertion is RED against
// BUGS.md entry 8: registration-stamped deposits inflate it by 1000 per
// non-founder validator before this step even runs. On this cluster the
// observed delta is entry 8's +4000 (four non-founder registrations)
// compounded with entry 12's own -1000 (the execution_error step just before
// this one silently drops one created-object transaction's storage fee), net
// +3000 — both leaks superimposed on the same ledger, not a new discrepancy.
func testUnderfundedGasCoin(t *testing.T, c *harness.Cluster, cli *client.Client, node0 *harness.Node) {
	t.Helper()

	w, coinID := fundedWallet(stepCtx(t), t, cli, node0, 1)
	recipient := client.NewWallet()

	_, hash, err := w.Split(cli, coinID, 1, recipient.Pubkey())
	requireNoErr(t, err)
	requireVerdictAll(stepCtx(t), t, c, hash, false, "fee_rejected")

	_, err = node0.WaitEvent(stepCtx(t), "fees.deducted",
		harness.Attr("tx", hex.EncodeToString(hash[:])), harness.Attr("covered", false))
	requireNoErr(t, err)

	requireSupplyIdentity(t, c)
}

// testGasCoinMissing submits a transfer whose gas coin does not exist:
// fee_rejected on every node, and the operated coin untouched.
func testGasCoinMissing(t *testing.T, c *harness.Cluster, cli *client.Client, node0 *harness.Node, systemPod [32]byte) {
	t.Helper()

	priv, sender := generateRawKey(t)

	coinID, faucetHash, err := cli.Faucet(sender, 1_000_000)
	requireNoErr(t, err)
	requireCommittedSuccess(stepCtx(t), t, node0, faucetHash)

	obj, err := cli.GetObject(coinID)
	requireNoErr(t, err)

	fakeGas := randomID(t)
	txBytes, hash := buildSignedTransferTxWithGasCoin(priv, systemPod, coinID, obj.Version, fakeGas, randomID(t))

	_, err = client.NewQUICTransport(node0.QUICAddr).SubmitTx(txBytes)
	requireNoErr(t, err) // structurally valid; the gas coin check is commit-time

	requireVerdictAll(stepCtx(t), t, c, hash, false, "fee_rejected")
	requireUnchangedOwner(t, cli, coinID, sender)
}

// testGasCoinNotOwned has a sender pay gas from another wallet's coin:
// fee_rejected on every node, and the operated coin untouched.
func testGasCoinNotOwned(t *testing.T, c *harness.Cluster, cli *client.Client, node0 *harness.Node, systemPod [32]byte) {
	t.Helper()

	alicePriv, alice := generateRawKey(t)
	_, bob := generateRawKey(t)

	aliceCoin, aliceFaucet, err := cli.Faucet(alice, 1_000_000)
	requireNoErr(t, err)
	requireCommittedSuccess(stepCtx(t), t, node0, aliceFaucet)

	bobCoin, bobFaucet, err := cli.Faucet(bob, 1_000_000)
	requireNoErr(t, err)
	requireCommittedSuccess(stepCtx(t), t, node0, bobFaucet)

	obj, err := cli.GetObject(aliceCoin)
	requireNoErr(t, err)

	txBytes, hash := buildSignedTransferTxWithGasCoin(alicePriv, systemPod, aliceCoin, obj.Version, bobCoin, randomID(t))

	_, err = client.NewQUICTransport(node0.QUICAddr).SubmitTx(txBytes)
	requireNoErr(t, err)

	requireVerdictAll(stepCtx(t), t, c, hash, false, "fee_rejected")
	requireUnchangedOwner(t, cli, aliceCoin, alice)
}

// testGasCoinNotSingleton uses a replicated (replication 3) object as the gas
// coin: fee_rejected on every node (holders reject it as non-singleton,
// non-holders as not found; both are commit-time fee rejections).
func testGasCoinNotSingleton(t *testing.T, c *harness.Cluster, cli *client.Client, node0 *harness.Node, systemPod [32]byte) {
	t.Helper()

	w, gasCoin := fundedWallet(stepCtx(t), t, cli, node0, 1_000_000)

	objectID, createHash, err := w.CreateObject(cli, 3, []byte("not-a-coin"), gasCoin)
	requireNoErr(t, err)
	requireVerdictAll(stepCtx(t), t, c, createHash, true, "")

	priv, sender := generateRawKey(t)

	coinID, faucetHash, err := cli.Faucet(sender, 1_000_000)
	requireNoErr(t, err)
	requireCommittedSuccess(stepCtx(t), t, node0, faucetHash)

	obj, err := cli.GetObject(coinID)
	requireNoErr(t, err)

	txBytes, hash := buildSignedTransferTxWithGasCoin(priv, systemPod, coinID, obj.Version, objectID, randomID(t))

	_, err = client.NewQUICTransport(node0.QUICAddr).SubmitTx(txBytes)
	requireNoErr(t, err)

	requireVerdictAll(stepCtx(t), t, c, hash, false, "fee_rejected")
	requireUnchangedOwner(t, cli, coinID, sender)
}

// requireUnchangedOwner asserts a coin still belongs to owner after a
// rejected mutation attempt.
func requireUnchangedOwner(t *testing.T, cli *client.Client, coinID, owner [32]byte) {
	t.Helper()

	obj, err := cli.GetObject(coinID)
	requireNoErr(t, err)
	if obj.Owner != owner {
		t.Fatalf("rejected tx changed the coin owner: got %x, want %x", obj.Owner[:8], owner[:8])
	}
}
