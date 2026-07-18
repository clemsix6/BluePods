package scenarios

import (
	"context"
	"testing"
	"time"

	"BluePods/pkg/client"
	"BluePods/test/harness"
)

const (
	// epochCrashScenarioSize is the validator count for TestScenarioEpochCrash.
	epochCrashScenarioSize = 10

	// epochCrashLength keeps epoch boundaries rolling within the scenario's budget.
	epochCrashLength = 50

	// epochSweepInterval paces the epoch-convergence poll: each sweep dials
	// every alive node for its status, so the tick is gentler than
	// eventPollInterval (a journal read).
	epochSweepInterval = 500 * time.Millisecond
)

// TestScenarioEpochCrash hard-kills two non-founder validators right after
// an epoch boundary lands on node 0, restarts both, and confirms every node
// converges on the epoch counter. Killing two NON-founders (never the
// founder) keeps the surviving stake share above two thirds for ANY bond
// amount at this cluster size, so liveness afterward is guaranteed by
// arithmetic, not tuning.
//
// Teardown is still red on the per-node supply identity: the
// register_validator traffic the default stake setup generates stamps a
// storage deposit no coin pays (+1000 per registration), inflating the
// supply term. Epoch-counter agreement and zero rollback are asserted
// in-scenario (requireEpochConvergence, requireZeroRollback), independent of
// that teardown chain.
func TestScenarioEpochCrash(t *testing.T) {
	if testing.Short() {
		t.Skip("scenario")
	}

	c := harness.NewCluster(t, epochCrashScenarioSize, harness.WithEpochLength(epochCrashLength))
	node0 := c.Node(0)
	cli := c.Client(0)

	waitNextBoundary(t, node0)

	victims := []int{epochCrashScenarioSize - 2, epochCrashScenarioSize - 1}

	for _, v := range victims {
		c.Kill(v)
	}
	for _, v := range victims {
		if c.Node(v).Alive() {
			t.Fatalf("node %d still alive after Kill", v)
		}
	}

	// The surviving 8 keep transitioning epochs.
	waitNextBoundary(t, node0)

	for _, v := range victims {
		c.Restart(v)
	}

	requireEpochConvergence(t, c)

	// A fresh transaction lands uniformly once every node is back.
	w, coin := fundedWallet(stepCtx(t), t, cli, node0, 200_000)
	_, hash, err := w.Split(cli, coin, 1_000, client.NewWallet().Pubkey())
	requireNoErr(t, err)
	requireVerdictAll(stepCtx(t), t, c, hash, true, "")

	requireZeroRollback(t, c)
}

// requireEpochConvergence polls every alive node's epoch counter until one
// sweep finds them all identical, bounded by epochsBoundaryTimeout. A node
// briefly unreachable right after Restart is tolerated by retrying, unlike
// Cluster.Client, which fails the test outright on a connection error.
func requireEpochConvergence(t *testing.T, c *harness.Cluster) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), epochsBoundaryTimeout)
	defer cancel()

	ticker := time.NewTicker(epochSweepInterval)
	defer ticker.Stop()

	var lastEpochs map[int]uint64

	for {
		epochs, ok := sweepEpochs(c)
		if ok {
			lastEpochs = epochs
			if epochsAgree(epochs) {
				return
			}
		}

		select {
		case <-ticker.C:
			continue
		case <-ctx.Done():
			c.Dump(t)
			t.Fatalf("epoch counters never converged: %v", lastEpochs)
			return
		}
	}
}

// sweepEpochs reads every alive node's epoch counter, degrading to ok=false
// (instead of failing the test) on any connection or status error, so a
// node's brief post-restart unreachability is a retry, not a hard failure.
func sweepEpochs(c *harness.Cluster) (map[int]uint64, bool) {
	out := make(map[int]uint64)

	for _, n := range c.Alive() {
		cli, err := client.NewClient(n.QUICAddr, c.SystemPod())
		if err != nil {
			return nil, false
		}

		status, err := cli.Status()
		if err != nil {
			return nil, false
		}

		out[n.Index] = status.Epoch
	}

	return out, true
}

// epochsAgree reports whether every epoch counter in epochs is identical.
func epochsAgree(epochs map[int]uint64) bool {
	first := true
	var want uint64

	for _, e := range epochs {
		if first {
			want = e
			first = false
			continue
		}
		if e != want {
			return false
		}
	}

	return true
}
