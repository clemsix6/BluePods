package scenarios

import (
	"context"
	"testing"

	"BluePods/pkg/client"
	"BluePods/test/harness"
)

// joinLoadBaseSize is the initial cluster size for TestScenarioJoinLoad.
const joinLoadBaseSize = 5

// TestScenarioJoinLoad spawns a new validator into a 5-node cluster while a
// background traffic loop runs through node 0, and confirms the newcomer
// syncs (Cluster.Spawn blocks on sync.completed), self-registers, and is
// included in a subsequent uniform commit, all without the join breaking the
// traffic.
//
// Zero rollback is proven in-scenario (requireZeroRollback), independent of
// teardown's automatic convergence check.
func TestScenarioJoinLoad(t *testing.T) {
	if testing.Short() {
		t.Skip("scenario")
	}

	c := harness.NewCluster(t, joinLoadBaseSize)
	node0 := c.Node(0)
	cli := c.Client(0)

	w, coin := fundedWallet(stepCtx(t), t, cli, node0, 5_000_000)

	trafficCtx, stopTraffic := context.WithCancel(context.Background())
	defer stopTraffic()
	progress, results := startTraffic(trafficCtx, cli, node0, w, coin)

	waitProgress(stepCtx(t), t, progress, 2)

	newcomer := c.Spawn()
	requireRegistered(t, node0, newcomer)

	waitProgress(stepCtx(t), t, progress, 4)

	stopTraffic()
	res := <-results
	requireNoErr(t, res.err)
	if res.committed == 0 {
		t.Fatalf("background traffic committed nothing during the join")
	}

	// The newcomer must be reachable through a uniform commit, not merely
	// reported ready: Cluster.Alive includes it once Spawn returns.
	w2, coin2 := fundedWallet(stepCtx(t), t, cli, node0, 100_000)
	_, hash, err := w2.Split(cli, coin2, 1_000, client.NewWallet().Pubkey())
	requireNoErr(t, err)
	requireVerdictAll(stepCtx(t), t, c, hash, true, "")

	requireZeroRollback(t, c)
}
