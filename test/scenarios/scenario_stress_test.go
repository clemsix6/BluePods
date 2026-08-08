package scenarios

import (
	"context"
	"sync"
	"testing"
	"time"

	"BluePods/pkg/client"
	"BluePods/test/harness"
)

const (
	// stressScenarioSize is the validator count for TestScenarioStress.
	stressScenarioSize = 12

	// stressEpochLength keeps epoch boundaries rolling under the traffic.
	stressEpochLength = 50

	// stressWallets is the number of concurrent traffic wallets.
	stressWallets = 6

	// stressSplitsPerWallet is how many sequential splits each wallet drives.
	stressSplitsPerWallet = 8

	// stressDoubleSpenders is how many contending transfers of one coin at
	// one version the double-spend storm submits.
	stressDoubleSpenders = 6
)

// TestScenarioStress hammers a 12-node, 50-round-epoch cluster: concurrent
// wallets driving sequential splits while epoch boundaries roll, and a
// double-spend storm submitting contending transfers of one coin at one
// version through different nodes, of which exactly one may succeed.
// Throughput is observed (logged), never asserted.
func TestScenarioStress(t *testing.T) {
	if testing.Short() {
		t.Skip("scenario")
	}

	c := harness.NewCluster(t, stressScenarioSize, harness.WithEpochLength(stressEpochLength))
	node0 := c.Node(0)
	cli := c.Client(0)

	t.Run("concurrent_traffic_across_boundaries", func(t *testing.T) {
		testConcurrentTraffic(t, c, cli, node0)
	})

	t.Run("double_spend_storm", func(t *testing.T) {
		testDoubleSpendStorm(t, c, cli, node0)
	})
}

// testConcurrentTraffic funds stressWallets wallets sequentially (faucets
// contend on the reserve coin, so funding is not racy), then drives
// stressSplitsPerWallet sequential splits per wallet concurrently, each
// awaited to a successful commit on the founder, while at least one epoch
// boundary rolls past.
func testConcurrentTraffic(t *testing.T, c *harness.Cluster, cli *client.Client, node0 *harness.Node) {
	t.Helper()

	boundariesBefore := len(node0.Journal().Events("epoch.transitioned"))

	type funded struct {
		cli  *client.Client
		w    *client.Wallet
		coin [32]byte
	}

	// Wallets, coins and clients are all created in the test goroutine (the
	// cluster accessors may fail the test, which only the test goroutine may
	// do); the workers below only submit and wait.
	wallets := make([]funded, stressWallets)
	for i := range wallets {
		w, coin := fundedWallet(stepCtx(t), t, cli, node0, 2_000_000)
		wallets[i] = funded{cli: c.Client(0), w: w, coin: coin}
	}

	start := time.Now()

	var wg sync.WaitGroup
	errs := make(chan error, stressWallets)

	for i := range wallets {
		wg.Add(1)
		go func(f funded) {
			defer wg.Done()
			errs <- driveSplits(f.cli, node0, f.w, f.coin)
		}(wallets[i])
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		requireNoErr(t, err)
	}

	total := stressWallets * stressSplitsPerWallet
	t.Logf("throughput observed: %d committed splits in %v", total, time.Since(start))

	// The traffic ran across at least one epoch boundary, or the scenario
	// waits for the next one: commits continuing across boundaries is the
	// property, not the timing.
	if len(node0.Journal().Events("epoch.transitioned")) == boundariesBefore {
		ctx, cancel := context.WithTimeout(context.Background(), epochsBoundaryTimeout)
		defer cancel()
		waitEventCount(ctx, t, node0, "epoch.transitioned", boundariesBefore+1)
	}

	// One more commit AFTER the boundary proves the cluster kept accepting.
	w, coin := fundedWallet(stepCtx(t), t, cli, node0, 100_000)
	_, hash, err := w.Split(cli, coin, 1_000, client.NewWallet().Pubkey())
	requireNoErr(t, err)
	requireCommittedSuccess(stepCtx(t), t, node0, hash)
}

// driveSplits performs the per-wallet sequential split loop, submitting each
// split through the wallet's own client against node 0 and waiting for its
// successful commit before the next. It returns the first error instead of
// failing the test, so concurrent wallets report through one channel.
func driveSplits(cli *client.Client, node0 *harness.Node, w *client.Wallet, coin [32]byte) error {
	for i := 0; i < stressSplitsPerWallet; i++ {
		_, hash, err := w.Split(cli, coin, 1_000, client.NewWallet().Pubkey())
		if err != nil {
			return err
		}

		ctx, cancel := context.WithTimeout(context.Background(), stepTimeout)
		_, err = node0.WaitEvent(ctx, "tx.committed",
			harness.Attr("tx", hexID(hash)), harness.Attr("success", true))
		cancel()
		if err != nil {
			return err
		}

		if err := w.RefreshCoin(cli, coin); err != nil {
			return err
		}
	}

	return nil
}

// testDoubleSpendStorm submits stressDoubleSpenders hand-built transfers of
// ONE coin at ONE version to distinct recipients, each through a different
// node, and asserts exactly one first-verdict success across the storm, the
// rest rejected as version conflicts.
func testDoubleSpendStorm(t *testing.T, c *harness.Cluster, cli *client.Client, node0 *harness.Node) {
	t.Helper()

	priv, sender := generateRawKey(t)

	coinID, faucetHash, err := cli.Faucet(sender, 1_000_000)
	requireNoErr(t, err)
	requireCommittedSuccess(stepCtx(t), t, node0, faucetHash)

	obj, err := cli.GetObject(coinID)
	requireNoErr(t, err)

	hashes := make([][32]byte, stressDoubleSpenders)
	landed := make([]bool, stressDoubleSpenders)

	var wg sync.WaitGroup
	for i := 0; i < stressDoubleSpenders; i++ {
		txBytes, hash := buildSignedTransferTx(priv, coinID, obj.Version, randomID(t))
		hashes[i] = hash

		target := c.Node(i % stressScenarioSize)

		wg.Add(1)
		go func(contender int, body []byte, addr string) {
			defer wg.Done()

			// The faucet coin is committed cluster-wide, but a target node's
			// ingress view may lag it on loaded hardware and reject the spend
			// as referencing an unknown object. Retry within a bounded window
			// so the race is between spends that actually entered consensus;
			// a contender that never lands is excluded from the verdict wait
			// rather than awaited forever (a rejected submission produces no
			// verdict). A late loser may also stay rejected at ingress once
			// the winner bumps the coin version — equally a non-entrant.
			deadline := time.Now().Add(20 * time.Second)
			for {
				if _, err := client.NewQUICTransport(addr).SubmitTx(body); err == nil {
					landed[contender] = true
					return
				} else if time.Now().After(deadline) {
					t.Logf("contender %d: submit to %s never landed: %v", contender, addr, err)
					return
				}
				time.Sleep(250 * time.Millisecond)
			}
		}(i, txBytes, target.QUICAddr)
	}
	wg.Wait()

	entrants := 0
	successes := 0
	for i, hash := range hashes {
		if !landed[i] {
			continue
		}
		entrants++

		verdicts := collectVerdicts(stepCtx(t), t, c, hash)
		if verdicts[0] == "success" {
			successes++
		} else if verdicts[0] != "failed:version_conflict" {
			t.Fatalf("double-spend contender %x got unexpected verdict %s", hash[:4], verdicts[0])
		}
	}

	if entrants < 2 {
		t.Fatalf("double-spend storm: only %d contenders entered consensus, need at least 2 for a race", entrants)
	}

	if successes != 1 {
		t.Fatalf("double-spend storm: %d of %d contenders succeeded, want exactly 1", successes, entrants)
	}
}
