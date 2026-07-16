package scenarios

import (
	"context"
	"testing"

	"BluePods/pkg/client"
	"BluePods/test/harness"
)

const (
	// crashScenarioSize is the validator count for TestScenarioCrash.
	crashScenarioSize = 5

	// crashVictim is a non-bootstrap node index. Killing the bootstrap
	// exercises a different restart path (harness.Node.Restart uses
	// --bootstrap with no --bootstrap-addr for it) and is out of scope here.
	crashVictim = 3
)

// TestScenarioCrash hard-kills a non-bootstrap node while a background
// traffic loop runs through node 0, confirms the surviving 4 keep
// committing (including a fresh transaction submitted with the victim down,
// required on every survivor), restarts the killed node, and confirms it
// resyncs and rejoins a uniform commit. Segment handling and the
// truncated-line exemption (test/harness/node.go) are exercised for real by
// the SIGKILL.
//
// Zero rollback is proven in-scenario (requireZeroRollback), independent of
// teardown's automatic convergence check, which is expected red per
// test/BUGS.md entries 1/2 on any multi-node cluster.
func TestScenarioCrash(t *testing.T) {
	if testing.Short() {
		t.Skip("scenario")
	}

	c := harness.NewCluster(t, crashScenarioSize)
	node0 := c.Node(0)
	cli := c.Client(0)

	w, coin := fundedWallet(stepCtx(t), t, cli, node0, 5_000_000)

	trafficCtx, stopTraffic := context.WithCancel(context.Background())
	defer stopTraffic()
	progress, results := startTraffic(trafficCtx, cli, node0, w, coin)

	// Traffic is flowing before the kill, so the post-kill comparison below
	// is against a moving baseline, not a loop that never got started.
	waitProgress(stepCtx(t), t, progress, 2)
	beforeKill := progress.Load()

	c.Kill(crashVictim)
	if c.Node(crashVictim).Alive() {
		t.Fatalf("node %d still alive after Kill", crashVictim)
	}

	// The surviving 4 keep committing the background traffic.
	waitProgress(stepCtx(t), t, progress, beforeKill+2)

	// A fresh transaction, submitted with the victim down, must land on
	// every SURVIVING node (Cluster.Alive no longer includes it).
	w2, coin2 := fundedWallet(stepCtx(t), t, cli, node0, 100_000)
	_, hash, err := w2.Split(cli, coin2, 1_000, client.NewWallet().Pubkey())
	requireNoErr(t, err)
	requireVerdictAll(stepCtx(t), t, c, hash, true, "")

	c.Restart(crashVictim)

	restarted := c.Node(crashVictim)
	if _, err := restarted.WaitEvent(stepCtx(t), "sync.completed"); err != nil {
		c.Dump(t)
		t.Fatalf("restarted node %d did not report sync.completed: %v", crashVictim, err)
	}

	stopTraffic()
	res := <-results
	requireNoErr(t, res.err)
	if res.committed == 0 {
		t.Fatalf("background traffic never committed a single split")
	}

	// One more commit, required on every alive node including the rejoined
	// victim, proves it actually caught back up rather than merely
	// reporting readiness.
	w3, coin3 := fundedWallet(stepCtx(t), t, cli, node0, 100_000)
	_, hash3, err := w3.Split(cli, coin3, 1_000, client.NewWallet().Pubkey())
	requireNoErr(t, err)
	requireVerdictAll(stepCtx(t), t, c, hash3, true, "")

	requireZeroRollback(t, c)
}
