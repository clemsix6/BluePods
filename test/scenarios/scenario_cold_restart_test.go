package scenarios

import (
	"testing"

	"BluePods/internal/network"
	"BluePods/pkg/client"
	"BluePods/test/harness"
)

const (
	// coldRestartScenarioSize is the validator count for TestScenarioColdRestart.
	coldRestartScenarioSize = 5

	// coldRestartFunding funds the wallet driving Phase A's ordinary splits.
	coldRestartFunding = uint64(5_000_000)

	// coldRestartSplitAmount/Count size Phase A's "normal life" traffic.
	coldRestartSplitAmount = uint64(500_000)
	coldRestartSplitCount  = 3

	// coldRestartFounderBondAmount is bonded by the FOUNDER, on top of its
	// genesis self-stake, before extinction. Without this step the founder's
	// live self-stake never diverges from its genesis value in the first
	// place, and a restart re-seeding it back to that SAME value would be
	// indistinguishable from correct behavior: this is what turns BUGS.md
	// entry 10 from a latent defect into an observable one.
	coldRestartFounderBondAmount = uint64(7_000_000)

	// coldRestartFounderFundAmount funds the founder's own extra bond coin:
	// the bonded amount plus headroom for the bond transaction's own gas fee
	// (mirrors test/harness/setup.go's bondFeeMargin).
	coldRestartFounderFundAmount = coldRestartFounderBondAmount + 10_000

	// coldRestartTrafficFunding/Amount size the post-restart liveness check's
	// split.
	coldRestartTrafficFunding = uint64(100_000)
	coldRestartTrafficAmount  = uint64(1_000)
)

// TestScenarioColdRestart is the one scenario in this corpus that stops the
// ENTIRE cluster, bootstrap node included, and brings it back from a cold
// start. No other scenario ever restarts the bootstrap: TestScenarioCrash
// explicitly kills a non-bootstrap victim instead, because bootstrap's
// restart takes a different code path (cmd/node's seedGenesisState /
// consensus.DAG.SeedGenesisValidator), and that path is exactly where
// BUGS.md entry 10 lives.
//
// Phase A gives the founder's self-stake a live divergence from its genesis
// value (an extra bond on top of the genesis self-stake) before stopping
// everything, so the restart has something to lose. Phase B stops every
// node gracefully (non-bootstrap first, bootstrap last), also exercising the
// node.stopping event path (graceful SIGTERM), which no other scenario
// triggers (every other adversarial scenario uses Kill). Phase C brings the
// bootstrap node back first, in bootstrap mode, over its own data directory,
// then resyncs the other four against it. Phase D confirms the cardinal
// properties hold across the cold restart, with founder_stake_preserved as
// the discriminating sub-test for entry 10.
//
// Expected red at teardown, per test/BUGS.md: entry 1 (checksum divergence,
// here also showing up as the five nodes disagreeing on the validator
// COUNT itself once four non-founders re-register against the freshly
// cold-started bootstrap) and entry 8's own small per-registration leak.
// But the dominant teardown supply-identity failure observed here is much
// larger than entry 8 alone accounts for (a ~4*10^11 gap, not entry 8's few
// thousand): that gap is entry 10's own mechanism at cluster scale — see
// founder_stake_preserved below and the entry's updated evidence.
//
// founder_stake_preserved is ALSO expected red, in the body, not just at
// teardown: it asserts the CORRECT behavior (the founder's totalBonded and
// coinsTotal survive a bootstrap restart unchanged), and BUGS.md entry 10
// predicts this node re-seeds its self-stake to genesis on every bootstrap
// start, discarding the bond applied in Phase A while the coin debit that
// paid for it persists. A red result here is the intended live reproduction
// of that entry, not a defect in this scenario. The observed magnitude goes
// beyond the entry's original founder-only claim: totalBonded does not just
// lose the founder's extra bond, it collapses all the way down to the
// founder's bare genesis self-stake, confirming the entry's own "suspected
// ... missing validator-set persistence path" co-factor is real and not
// founder-specific — buildValidatorSet (cmd/node/init.go) starts every
// process run, restart included, from an in-memory validator set with
// nothing but the local identity, and nothing outside SeedGenesisValidator
// restores anyone into it. See the sub-test for the exact before/after
// numbers logged as evidence, and test/BUGS.md entry 10 for the full
// analysis.
func TestScenarioColdRestart(t *testing.T) {
	if testing.Short() {
		t.Skip("scenario")
	}

	c := harness.NewCluster(t, coldRestartScenarioSize)
	node0 := c.Node(0)
	cli0 := c.Client(0)

	preRound, preFingerprints, w, coin := coldRestartPhaseA(t, c, node0, cli0)
	coldRestartPhaseB(t, c, node0)
	coldRestartPhaseC(t, c, node0)
	coldRestartPhaseD(t, c, node0, cli0, w, coin, preRound, preFingerprints)
}

// coldRestartPhaseA drives normal cluster life before extinction: a funded
// wallet performs a few ordinary splits (all committed everywhere), then the
// founder bonds extra self-stake on top of its genesis figure so the
// restart in Phase C has a live divergence to erase. It returns the
// committed round reached, every alive node's fingerprint, and the wallet
// and coin whose state Phase D confirms survives.
func coldRestartPhaseA(t *testing.T, c *harness.Cluster, node0 *harness.Node, cli0 *client.Client) (uint64, map[int]*network.FingerprintResponse, *client.Wallet, [32]byte) {
	t.Helper()

	w, coin := fundedWallet(stepCtx(t), t, cli0, node0, coldRestartFunding)

	for i := 0; i < coldRestartSplitCount; i++ {
		_, hash, err := w.Split(cli0, coin, coldRestartSplitAmount, client.NewWallet().Pubkey())
		requireNoErr(t, err)
		requireVerdictAll(stepCtx(t), t, c, hash, true, "")
		requireNoErr(t, w.RefreshCoin(cli0, coin))
	}

	founder := walletFromNodeKey(t, node0)

	founderCoin, faucetHash, err := cli0.Faucet(founder.Pubkey(), coldRestartFounderFundAmount)
	requireNoErr(t, err)
	requireCommittedSuccess(stepCtx(t), t, node0, faucetHash)
	requireNoErr(t, founder.RefreshCoin(cli0, founderCoin))

	bondHash, err := founder.Bond(cli0, founderCoin, founder.GetCoin(founderCoin).Version, coldRestartFounderBondAmount)
	requireNoErr(t, err)
	requireVerdictAll(stepCtx(t), t, c, bondHash, true, "")

	preFingerprints := make(map[int]*network.FingerprintResponse, len(c.Alive()))
	for _, n := range c.Alive() {
		fp, err := client.NewQUICTransport(n.QUICAddr).Fingerprint()
		requireNoErr(t, err)
		preFingerprints[n.Index] = fp
	}
	preRound := preFingerprints[node0.Index].Round

	t.Logf("pre-extinction: committed round=%d, node 0 totalBonded=%d coinsTotal=%d totalSupply=%d (founder bonded +%d on top of genesis self-stake)",
		preRound, preFingerprints[node0.Index].TotalBonded, preFingerprints[node0.Index].CoinsTotal,
		preFingerprints[node0.Index].TotalSupply, coldRestartFounderBondAmount)

	return preRound, preFingerprints, w, coin
}

// coldRestartPhaseB stops every node gracefully, non-bootstrap nodes first
// and the bootstrap node last, confirms none of them is alive afterward, and
// confirms Stop's graceful SIGTERM actually exercised the node.stopping
// event path (no other scenario in this corpus uses Stop; every other
// adversarial scenario kills instead).
func coldRestartPhaseB(t *testing.T, c *harness.Cluster, node0 *harness.Node) {
	t.Helper()

	for i := coldRestartScenarioSize - 1; i >= 1; i-- {
		requireNoErr(t, c.Node(i).Stop())
	}
	requireNoErr(t, node0.Stop())

	for _, n := range c.Nodes() {
		if n.Alive() {
			t.Fatalf("node %d still alive after Stop", n.Index)
		}
	}

	for _, n := range c.Nodes() {
		if evs := n.Journal().Events("node.stopping", harness.Attr("reason", "terminated")); len(evs) == 0 {
			t.Fatalf("node %d: graceful Stop recorded no node.stopping(reason=terminated) event", n.Index)
		}
	}
}

// coldRestartPhaseC brings the cluster back from total extinction: the
// bootstrap node first, in bootstrap mode over its own data directory
// (Node.Restart's syncFrom argument is irrelevant for the node that started
// as the cluster's bootstrap identity, since it always restarts with
// --bootstrap and no --bootstrap-addr — see test/harness/node.go), waited
// for via node.ready in the NEW journal segment (mirroring
// Cluster.Restart's own e.Seg >= nextSeg pattern, test/harness/cluster.go);
// then the other four through the ordinary Cluster.Restart, now that
// node 0 is alive to serve as their sync source.
func coldRestartPhaseC(t *testing.T, c *harness.Cluster, node0 *harness.Node) {
	t.Helper()

	var preSeg int
	if ready := node0.Journal().Events("node.ready"); len(ready) > 0 {
		preSeg = ready[len(ready)-1].Seg
	}
	nextSeg := preSeg + 1
	inNewSegment := func(e harness.Event) bool { return e.Seg >= nextSeg }

	requireNoErr(t, node0.Restart(""))
	if _, err := node0.WaitEvent(stepCtx(t), "node.ready", inNewSegment); err != nil {
		c.Dump(t)
		t.Fatalf("bootstrap node did not become ready after cold restart: %v", err)
	}

	for i := 1; i < coldRestartScenarioSize; i++ {
		c.Restart(i)

		n := c.Node(i)
		if _, err := n.WaitEvent(stepCtx(t), "sync.completed"); err != nil {
			c.Dump(t)
			t.Fatalf("node %d did not report sync.completed after cold restart: %v", i, err)
		}
	}
}

// coldRestartPhaseD runs Phase D's five sub-tests: zero rollback across the
// restart, the wallet's coin surviving unchanged, the committed round
// resuming past its pre-extinction value, the founder's stake/coin
// conservation (the discriminating sub-test for BUGS.md entry 10), and
// ordinary traffic committing on all five nodes again.
func coldRestartPhaseD(t *testing.T, c *harness.Cluster, node0 *harness.Node, cli0 *client.Client, w *client.Wallet, coin [32]byte, preRound uint64, preFingerprints map[int]*network.FingerprintResponse) {
	t.Helper()

	t.Run("zero_rollback_across_restart", func(t *testing.T) {
		requireZeroRollback(t, c)
	})

	t.Run("state_survives", func(t *testing.T) {
		requireUnchangedOwner(t, cli0, coin, w.Pubkey())

		obj, err := cli0.GetObject(coin)
		requireNoErr(t, err)

		wantBalance := w.GetCoin(coin).Balance
		if got := coinBalance(obj); got != wantBalance {
			t.Fatalf("wallet coin balance across cold restart: got %d, want unchanged %d", got, wantBalance)
		}
	})

	t.Run("resumes_not_regresses", func(t *testing.T) {
		ctx := stepCtx(t)
		if err := c.WaitAll(ctx, "consensus.anchor.committed", harness.AttrGE("round", preRound+1)); err != nil {
			c.Dump(t)
			t.Fatalf("cluster did not resume past its pre-extinction committed round %d: %v", preRound, err)
		}
	})

	t.Run("founder_stake_preserved", func(t *testing.T) {
		postFp, err := client.NewQUICTransport(node0.QUICAddr).Fingerprint()
		requireNoErr(t, err)
		preFp := preFingerprints[node0.Index]

		t.Logf("node 0 fingerprint across cold restart: totalBonded pre=%d post=%d (delta %d), coinsTotal pre=%d post=%d (delta %d)",
			preFp.TotalBonded, postFp.TotalBonded, int64(postFp.TotalBonded)-int64(preFp.TotalBonded),
			preFp.CoinsTotal, postFp.CoinsTotal, int64(postFp.CoinsTotal)-int64(preFp.CoinsTotal))

		if postFp.TotalBonded != preFp.TotalBonded {
			t.Fatalf("BUGS.md entry 10 reproduced: node 0 totalBonded not conserved across a bootstrap restart: pre=%d post=%d "+
				"(SeedGenesisValidator re-seeds the founder's self-stake to its genesis value on every bootstrap start, "+
				"discarding the %d bonded on top of it before extinction)",
				preFp.TotalBonded, postFp.TotalBonded, coldRestartFounderBondAmount)
		}
		if postFp.CoinsTotal != preFp.CoinsTotal {
			t.Fatalf("BUGS.md entry 10 reproduced: node 0 coinsTotal not conserved across a bootstrap restart: pre=%d post=%d "+
				"(the coin debited to fund the founder's extra bond does not come back, while totalBonded above may already show the bonded amount was discarded)",
				preFp.CoinsTotal, postFp.CoinsTotal)
		}
	})

	t.Run("traffic_after_restart", func(t *testing.T) {
		w2, coin2 := fundedWallet(stepCtx(t), t, cli0, node0, coldRestartTrafficFunding)

		_, hash, err := w2.Split(cli0, coin2, coldRestartTrafficAmount, client.NewWallet().Pubkey())
		requireNoErr(t, err)
		requireVerdictAll(stepCtx(t), t, c, hash, true, "")
	})
}
