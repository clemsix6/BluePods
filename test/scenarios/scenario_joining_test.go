package scenarios

import (
	"context"
	"encoding/hex"
	"testing"
	"time"

	"BluePods/pkg/client"
	"BluePods/test/harness"
)

// joiningBaseSize is the initial cluster size for TestScenarioJoining.
const joiningBaseSize = 5

// TestScenarioJoining starts a 5-node cluster and grows it: one spawned
// node, then a batch of two more. Each newcomer must sync (sync.completed),
// self-register (epoch.validator.registered observed by the founder), and
// join replication (a transaction submitted after the joins commits on every
// node, newcomers included). The founder's status must count all 8.
func TestScenarioJoining(t *testing.T) {
	if testing.Short() {
		t.Skip("scenario")
	}

	c := harness.NewCluster(t, joiningBaseSize)
	node0 := c.Node(0)
	cli := c.Client(0)

	t.Run("single_join", func(t *testing.T) {
		n := c.Spawn()
		requireRegistered(t, node0, n)
	})

	t.Run("batch_join", func(t *testing.T) {
		first := c.Spawn()
		second := c.Spawn()
		requireRegistered(t, node0, first)
		requireRegistered(t, node0, second)
	})

	t.Run("all_counted", func(t *testing.T) {
		waitValidatorCount(t, cli, joiningBaseSize+3)
	})

	t.Run("commit_reaches_newcomers", func(t *testing.T) {
		w, coinID := fundedWallet(stepCtx(t), t, cli, node0, 1_000_000)

		_, hash, err := w.Split(cli, coinID, 1_000, client.NewWallet().Pubkey())
		requireNoErr(t, err)

		// Alive() includes the three spawned nodes, so this proves the
		// commit stream reaches every joiner, not just the founders.
		requireVerdictAll(stepCtx(t), t, c, hash, true, "")
	})
}

// requireRegistered asserts the founder observed the spawned node's
// self-registration as a committed epoch.validator.registered event.
func requireRegistered(t *testing.T, node0, spawned *harness.Node) {
	t.Helper()

	w := walletFromNodeKey(t, spawned)
	pk := w.Pubkey()

	_, err := node0.WaitEvent(stepCtx(t), "epoch.validator.registered",
		harness.Attr("validator", hex.EncodeToString(pk[:])))
	requireNoErr(t, err)
}

// waitValidatorCount polls the founder's status until it reports want
// validators, bounded.
func waitValidatorCount(t *testing.T, cli *client.Client, want int) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	ticker := time.NewTicker(eventPollInterval)
	defer ticker.Stop()

	for {
		status, err := cli.Status()
		if err == nil && int(status.Validators) == want {
			return
		}

		select {
		case <-ticker.C:
			continue
		case <-ctx.Done():
			got := -1
			if err == nil {
				got = int(status.Validators)
			}
			t.Fatalf("validator count never reached %d (last: %d)", want, got)
			return
		}
	}
}
