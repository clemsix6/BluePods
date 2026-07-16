package scenarios

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"os"
	"testing"
	"time"

	"BluePods/pkg/client"
	"BluePods/test/harness"
)

const (
	// epochsScenarioSize is the validator count for TestScenarioEpochs.
	epochsScenarioSize = 10

	// epochsLength makes boundaries land every ~20-30s so the scenario
	// crosses several within its budget.
	epochsLength = 50

	// epochsBoundaryTimeout bounds one wait for the next epoch boundary.
	epochsBoundaryTimeout = 3 * time.Minute
)

// TestScenarioEpochs drives a 10-node, 50-round-epoch cluster through the
// epoch lifecycle: transitions observed as a strictly increasing, contiguous
// epoch sequence on every node; rewards distribution observed as a typed
// event; the per-node supply identity checked across a boundary; and
// validator deregistration deferred to the boundary, witnessed by the
// epoch.transitioned removed list and the shrinking validator count.
//
// Expected red, per test/BUGS.md: the supply-identity step fails against
// entry 8 (registration-stamped deposits), and teardown's convergence check
// fails against entries 1 and 2.
func TestScenarioEpochs(t *testing.T) {
	if testing.Short() {
		t.Skip("scenario")
	}

	c := harness.NewCluster(t, epochsScenarioSize, harness.WithEpochLength(epochsLength))
	node0 := c.Node(0)
	cli := c.Client(0)

	t.Run("transitions_on_all_nodes", func(t *testing.T) {
		testEpochTransitions(t, c, node0)
	})

	t.Run("rewards_distributed", func(t *testing.T) {
		testRewardsDistributed(t, c, cli, node0)
	})

	t.Run("supply_identity_across_boundary", func(t *testing.T) {
		waitNextBoundary(t, node0)
		requireSupplyIdentity(t, c)
	})

	t.Run("deregister_deferred_to_boundary", func(t *testing.T) {
		testDeregisterDeferred(t, c, cli, node0)
	})
}

// waitNextBoundary blocks until node0 records one more epoch.transitioned
// than it has now.
func waitNextBoundary(t *testing.T, node0 *harness.Node) {
	t.Helper()

	seen := len(node0.Journal().Events("epoch.transitioned"))

	ctx, cancel := context.WithTimeout(context.Background(), epochsBoundaryTimeout)
	defer cancel()

	waitEventCount(ctx, t, node0, "epoch.transitioned", seen+1)
}

// testEpochTransitions waits until every node observed a common boundary,
// then asserts each node's epoch.transitioned sequence is strictly
// increasing and contiguous from the first epoch it observed: identical
// history, allowing only a late join's truncated prefix.
func testEpochTransitions(t *testing.T, c *harness.Cluster, node0 *harness.Node) {
	t.Helper()

	waitNextBoundary(t, node0)

	latest := latestEpoch(node0)

	ctx, cancel := context.WithTimeout(context.Background(), epochsBoundaryTimeout)
	defer cancel()

	if err := c.WaitAll(ctx, "epoch.transitioned", harness.AttrGE("epoch", latest)); err != nil {
		c.Dump(t)
		t.Fatalf("not every node reached epoch %d: %v", latest, err)
	}

	for _, n := range c.Alive() {
		requireContiguousEpochs(t, n)
	}
}

// latestEpoch returns the highest epoch node0 has transitioned to.
func latestEpoch(node0 *harness.Node) uint64 {
	var latest uint64
	for _, ev := range node0.Journal().Events("epoch.transitioned") {
		if e, ok := ev.Attrs["epoch"].(float64); ok && uint64(e) > latest {
			latest = uint64(e)
		}
	}

	return latest
}

// requireContiguousEpochs asserts one node's observed epoch sequence is
// strictly increasing with no gaps.
func requireContiguousEpochs(t *testing.T, n *harness.Node) {
	t.Helper()

	events := n.Journal().Events("epoch.transitioned")
	prev := uint64(0)

	for i, ev := range events {
		e, ok := ev.Attrs["epoch"].(float64)
		if !ok {
			t.Fatalf("node %d: epoch.transitioned without numeric epoch: %v", n.Index, ev.Attrs)
		}

		epoch := uint64(e)
		if i > 0 && epoch != prev+1 {
			t.Fatalf("node %d: epoch sequence broken: %d follows %d", n.Index, epoch, prev)
		}
		prev = epoch
	}

	if len(events) == 0 {
		t.Fatalf("node %d: no epoch.transitioned observed", n.Index)
	}
}

// testRewardsDistributed drives one fee-bearing transaction (so the epoch
// pool is non-empty), waits for the next boundary, and asserts the
// epoch.rewards.distributed event fires with a positive pool.
func testRewardsDistributed(t *testing.T, c *harness.Cluster, cli *client.Client, node0 *harness.Node) {
	t.Helper()

	w, coinID := fundedWallet(stepCtx(t), t, cli, node0, 1_000_000)

	_, hash, err := w.Split(cli, coinID, 1_000, client.NewWallet().Pubkey())
	requireNoErr(t, err)
	requireCommittedSuccess(stepCtx(t), t, node0, hash)

	seen := len(node0.Journal().Events("epoch.rewards.distributed"))

	ctx, cancel := context.WithTimeout(context.Background(), epochsBoundaryTimeout)
	defer cancel()

	waitEventCount(ctx, t, node0, "epoch.rewards.distributed", seen+1)

	events := node0.Journal().Events("epoch.rewards.distributed")
	last := events[len(events)-1]

	if pool, ok := last.Attrs["pool"].(float64); !ok || pool <= 0 {
		t.Fatalf("rewards distributed with no positive pool: %v", last.Attrs)
	}
}

// testDeregisterDeferred deregisters the last two validators and asserts the
// removal is deferred to an epoch boundary: the typed deregistration events
// land at commit, both pubkeys appear in subsequent epoch.transitioned
// removed lists, and the validator count reported by the node drops to 8
// only after those boundaries.
func testDeregisterDeferred(t *testing.T, c *harness.Cluster, cli *client.Client, node0 *harness.Node) {
	t.Helper()

	removedKeys := make(map[string]bool)

	for _, idx := range []int{epochsScenarioSize - 2, epochsScenarioSize - 1} {
		w := walletFromNodeKey(t, c.Node(idx))
		pub := w.Pubkey()
		pubHex := hex.EncodeToString(pub[:])
		removedKeys[pubHex] = true

		requireNoErr(t, w.DeregisterValidator(cli))

		ev, err := node0.WaitEvent(stepCtx(t), "epoch.validator.deregistered", harness.Attr("validator", pubHex))
		requireNoErr(t, err)
		_ = ev
	}

	waitRemovedAtBoundary(t, node0, removedKeys)

	status, err := cli.Status()
	requireNoErr(t, err)
	if int(status.Validators) != epochsScenarioSize-2 {
		t.Fatalf("validator count after removal boundaries: got %d, want %d", status.Validators, epochsScenarioSize-2)
	}
}

// waitRemovedAtBoundary waits for epoch boundaries until every pubkey in
// keys has appeared in some epoch.transitioned removed list, bounded by a
// few boundaries' worth of time.
func waitRemovedAtBoundary(t *testing.T, node0 *harness.Node, keys map[string]bool) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 2*epochsBoundaryTimeout)
	defer cancel()

	ticker := time.NewTicker(eventPollInterval)
	defer ticker.Stop()

	for {
		pending := len(keys)
		for _, ev := range node0.Journal().Events("epoch.transitioned") {
			for _, removed := range attrStrings(ev.Attrs["removed"]) {
				if keys[removed] {
					pending--
				}
			}
		}

		if pending <= 0 {
			return
		}

		select {
		case <-ticker.C:
			continue
		case <-ctx.Done():
			t.Fatalf("deregistered validators never appeared in epoch.transitioned removed lists (%d pending)", pending)
			return
		}
	}
}

// attrStrings coerces a JSON-decoded event attribute into a string slice
// (slog.Any([]string) decodes as []any of strings).
func attrStrings(v any) []string {
	items, ok := v.([]any)
	if !ok {
		return nil
	}

	out := make([]string, 0, len(items))
	for _, item := range items {
		if s, ok := item.(string); ok {
			out = append(out, s)
		}
	}

	return out
}

// walletFromNodeKey loads a node's Ed25519 key file into a wallet, so
// validator-identity transactions (deregister) can be signed with the node's
// real on-chain identity.
func walletFromNodeKey(t *testing.T, n *harness.Node) *client.Wallet {
	t.Helper()

	data, err := os.ReadFile(n.KeyPath)
	requireNoErr(t, err)
	if len(data) != ed25519.PrivateKeySize {
		t.Fatalf("node %d key file has unexpected size %d", n.Index, len(data))
	}

	return client.NewWalletFromKey(ed25519.PrivateKey(data))
}
