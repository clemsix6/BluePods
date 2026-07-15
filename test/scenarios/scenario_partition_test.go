package scenarios

import (
	"context"
	"testing"
	"time"

	"BluePods/pkg/client"
	"BluePods/test/harness"
)

const (
	// partitionScenarioSize is the validator count for every sub-scenario of
	// TestScenarioPartition: 5 equally staked validators, so the exact
	// quorum test 3*capped_sum >= 2*total gives a 4-side quorum (12 >= 10)
	// and denies quorum to both a 1-side isolate (3 >= 10 false) and either
	// side of a 3|2 split (9 >= 10 and 6 >= 10, both false), regardless of
	// which side holds the founder.
	partitionScenarioSize = 5

	// partitionEpochLength makes the first epoch boundary arrive quickly.
	// The boundary matters for the quorum arithmetic above: the strict-regime
	// latch freezes the GENESIS committee's stakes as they stood when the
	// last founding registration committed (internal/consensus/regime.go,
	// refreezeGenesisRegime) — BEFORE the harness's equal bonds commit — and
	// that founder-heavy frozen snapshot governs quorum until the first
	// epoch boundary refreezes from live (bonded) stakes. Partitioning
	// inside epoch 0 therefore tests the founder-heavy regime, with weights
	// that even vary run to run (whichever bonds happened to commit before
	// the latch). Every sub-scenario waits out boundary 1 before
	// partitioning so the equal-stake arithmetic is actually in force.
	partitionEpochLength = 50

	// plateauWindow bounds how long a side WITHOUT quorum is watched for an
	// anchor.committed that must never come. It is a poll with a deadline,
	// never a sleep-assert.
	plateauWindow = 10 * time.Second

	// plateauSettle is how long a node's anchor.committed count must sit
	// still before the plateau watch freezes its baseline: decisions already
	// in flight when the partition lands (vertices gossiped and buffered
	// BEFORE the blocklist applied) may legitimately commit moments after
	// Partition returns, and are not quorum violations.
	plateauSettle = 3 * time.Second

	// plateauSettleBound caps the settle phase: a count that never sits
	// still was never going to plateau, and that IS the violation.
	plateauSettleBound = 30 * time.Second
)

// TestScenarioPartition drives the CP-promise scenarios on a 5-node,
// equal-stake cluster: a minority partition halts only the isolated side, a
// symmetric split halts the WHOLE network (neither side has quorum), and a
// heal always resumes commits and lets a stalled side catch up, without ever
// contradicting a committed anchor.
//
// Each sub-test proves zero rollback in-scenario (requireZeroRollback),
// independent of teardown's automatic convergence check, which is expected
// red per test/BUGS.md entries 1/2.
func TestScenarioPartition(t *testing.T) {
	if testing.Short() {
		t.Skip("scenario")
	}

	t.Run("minority", testMinorityPartition)
	t.Run("symmetric", testSymmetricPartition)
	t.Run("heal_under_traffic", testHealUnderTraffic)
}

// newPartitionCluster starts an equal-stake 5-node cluster and waits until
// EVERY node has crossed the first epoch boundary, so the epoch snapshot
// governing quorum carries the equal bonded stakes rather than the
// founder-heavy genesis freeze (see partitionEpochLength).
func newPartitionCluster(t *testing.T) *harness.Cluster {
	t.Helper()

	c := harness.NewCluster(t, partitionScenarioSize, harness.WithEpochLength(partitionEpochLength))

	if err := c.WaitAll(stepCtx(t), "epoch.transitioned", harness.AttrGE("epoch", 1)); err != nil {
		c.Dump(t)
		t.Fatalf("cluster never crossed the first epoch boundary: %v", err)
	}

	return c
}

// testMinorityPartition isolates one node (4|1): the 4-side keeps emitting
// anchor.committed (12 >= 10, has quorum); the isolated node's committed
// round plateaus (3 >= 10, no quorum). Healing lets it catch up, and a
// fresh transaction lands uniformly again.
func testMinorityPartition(t *testing.T) {
	c := newPartitionCluster(t)
	node0 := c.Node(0)
	cli := c.Client(0)

	isolated := partitionScenarioSize - 1
	majority := []int{0, 1, 2, 3}

	c.Partition(majority, []int{isolated})

	majoritySeen := len(node0.Journal().Events("consensus.anchor.committed"))
	waitEventCount(stepCtx(t), t, node0, "consensus.anchor.committed", majoritySeen+2)

	requirePlateau(t, c.Node(isolated))
	settled := len(c.Node(isolated).Journal().Events("consensus.anchor.committed"))

	c.Heal()

	resumeCtx, cancel := context.WithTimeout(context.Background(), stepTimeout)
	defer cancel()
	waitEventCount(resumeCtx, t, c.Node(isolated), "consensus.anchor.committed", settled+1)

	w, coin := fundedWallet(stepCtx(t), t, cli, node0, 200_000)
	_, hash, err := w.Split(cli, coin, 1_000, client.NewWallet().Pubkey())
	requireNoErr(t, err)
	requireVerdictAll(stepCtx(t), t, c, hash, true, "")

	requireZeroRollback(t, c)
}

// testSymmetricPartition splits the cluster 3|2: NEITHER side has quorum (9
// >= 10 and 6 >= 10 both false), so both plateau; the whole network halts,
// and nobody contradicts a commit while it does. This is the CP-promise
// scenario.
func testSymmetricPartition(t *testing.T) {
	c := newPartitionCluster(t)
	node0 := c.Node(0)
	cli := c.Client(0)

	sideA := []int{0, 1, 2}
	sideB := []int{3, 4}

	c.Partition(sideA, sideB)

	requirePlateau(t, node0)
	requirePlateau(t, c.Node(3))

	c.Heal()

	w, coin := fundedWallet(stepCtx(t), t, cli, node0, 200_000)
	_, hash, err := w.Split(cli, coin, 1_000, client.NewWallet().Pubkey())
	requireNoErr(t, err)
	requireVerdictAll(stepCtx(t), t, c, hash, true, "")

	requireZeroRollback(t, c)
}

// testHealUnderTraffic partitions 4|1 with sustained majority traffic
// running, heals, and confirms the isolated node catches up PAST the whole
// burst it missed while cut off.
func testHealUnderTraffic(t *testing.T) {
	c := newPartitionCluster(t)
	node0 := c.Node(0)
	cli := c.Client(0)

	isolated := partitionScenarioSize - 1
	majority := []int{0, 1, 2, 3}

	w, coin := fundedWallet(stepCtx(t), t, cli, node0, 5_000_000)

	c.Partition(majority, []int{isolated})

	trafficCtx, stopTraffic := context.WithCancel(context.Background())
	defer stopTraffic()
	progress, results := startTraffic(trafficCtx, cli, node0, w, coin)

	waitProgress(stepCtx(t), t, progress, 5)
	requirePlateau(t, c.Node(isolated))

	burstRound := latestAnchorRound(node0)

	c.Heal()

	stopTraffic()
	res := <-results
	requireNoErr(t, res.err)

	if _, err := c.Node(isolated).WaitEvent(stepCtx(t), "consensus.anchor.committed", harness.AttrGE("round", burstRound)); err != nil {
		c.Dump(t)
		t.Fatalf("isolated node %d never caught up past round %d after heal: %v", isolated, burstRound, err)
	}

	requireZeroRollback(t, c)
}

// requirePlateau asserts n's consensus.anchor.committed count plateaus: a
// bounded two-phase poll, never a sleep-assert. The settle phase absorbs
// decisions already in flight when the partition landed, waiting for the
// count to sit still for plateauSettle (bounded by plateauSettleBound — a
// count that never settles is itself the quorum violation). The watch phase
// then fails the instant the frozen baseline rises within plateauWindow.
func requirePlateau(t *testing.T, n *harness.Node) {
	t.Helper()

	baseline := settleAnchorCount(t, n)

	deadline := time.After(plateauWindow)
	ticker := time.NewTicker(eventPollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			got := len(n.Journal().Events("consensus.anchor.committed"))
			if got > baseline {
				t.Fatalf("node %d: anchor.committed advanced from %d to %d during an expected no-quorum plateau",
					n.Index, baseline, got)
			}
		case <-deadline:
			return
		}
	}
}

// settleAnchorCount polls n's anchor.committed count until it has sat still
// for plateauSettle, returning the settled count, and fails the test if it
// never settles within plateauSettleBound.
func settleAnchorCount(t *testing.T, n *harness.Node) int {
	t.Helper()

	bound := time.After(plateauSettleBound)
	ticker := time.NewTicker(eventPollInterval)
	defer ticker.Stop()

	count := len(n.Journal().Events("consensus.anchor.committed"))
	stableSince := time.Now()

	for {
		select {
		case <-ticker.C:
			got := len(n.Journal().Events("consensus.anchor.committed"))
			if got != count {
				count = got
				stableSince = time.Now()
				continue
			}
			if time.Since(stableSince) >= plateauSettle {
				return count
			}
		case <-bound:
			t.Fatalf("node %d: anchor.committed never stopped advancing under an expected no-quorum partition (count %d)",
				n.Index, count)
			return 0
		}
	}
}

// latestAnchorRound returns the highest round n has committed an anchor for.
func latestAnchorRound(n *harness.Node) uint64 {
	var latest uint64
	for _, ev := range n.Journal().Events("consensus.anchor.committed") {
		if r, ok := ev.Attrs["round"].(float64); ok && uint64(r) > latest {
			latest = uint64(r)
		}
	}

	return latest
}
