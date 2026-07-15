package scenarios

import (
	"encoding/hex"
	"testing"

	"BluePods/pkg/client"
	"BluePods/test/harness"
)

// TestScenarioFees drives a 5-node cluster through the fee rules: full
// deduction visible through fees.deducted events and balance reads, the
// underfunded-gas-coin rejection with its partial amount pooled and the
// per-node supply identity asserted right after, and the three gas-coin
// validation rejections (missing, not owned, not a singleton).
//
// Expected red, per test/BUGS.md: the underfunded step's supply-identity
// assertion fails against entry 8 (validator registration stamps a deposit
// no coin pays, inflating the identity by 1000 per registration — the
// partial-fee pooling itself is conserving, proven on a single node), and
// teardown's automatic convergence check fails against entry 1.
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

// testUnderfundedGasCoin funds a coin with 1 unit (below any fee), attempts a
// split, and asserts the typed fee_rejected verdict on every node, the
// partial (covered=false) deduction event, and the supply identity intact on
// every node afterwards: the drained unit must enter the epoch pool instead
// of vanishing (the Task 3 fix). The identity assertion is RED against
// BUGS.md entry 8: registration-stamped deposits inflate it by 1000 per
// non-founder validator before this step even runs.
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
