package scenarios

import (
	"context"
	"sync/atomic"
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

	// partitionEpochLength must keep epoch 0 open LONGER than the harness's
	// sequential founder bootstrap takes to commit; that is the whole point of
	// the value, not speed. A partition only halts a quorum-starved side once
	// the strict BFT regime is latched, and that latch is evaluated only during
	// epoch 0: refreezeGenesisRegime (internal/consensus/regime.go) fires it the
	// round all founding registrations have committed. The harness commits its
	// five founder registrations one after another, landing around rounds
	// 0/24/50/76/100. If epoch 0 closes before the fifth commits, the latch
	// never arms and consensus stays in the RELAXED single-supporter regime,
	// where a partition halts no side at all: each side certifies its own
	// disjoint rounds alone, no committed round ever plateaus, and every
	// requirePlateau below is doomed. (That relaxed-regime gap, the node's
	// inability to wedge a partitioned side, is tracked in issue #8.) At 150 all
	// five registrations commit inside epoch 0, the latch arms near round 100,
	// and boundary 1 at round 150 hands the equal-stake arithmetic above to
	// every sub-scenario, each of which waits out that boundary before it
	// partitions.
	partitionEpochLength = 150

	// plateauWindow bounds how long a quorum-starved side is watched for its
	// committed round to reach the production frontier captured at the cut,
	// progress it cannot make. It is a poll with a deadline, never a
	// sleep-assert.
	plateauWindow = 10 * time.Second

	// flappingCycles is how many Partition/Heal cycles testFlappingPartitions
	// drives under sustained background traffic.
	flappingCycles = 3
)

// TestScenarioPartition drives the CP-promise scenarios on a 5-node,
// equal-stake cluster: a minority partition halts only the isolated side, a
// symmetric split halts the WHOLE network (neither side has quorum), and a
// heal always resumes commits and lets a stalled side catch up, without ever
// contradicting a committed anchor.
//
// Each sub-test proves zero rollback in-scenario (requireZeroRollback),
// independent of teardown's automatic convergence check.
func TestScenarioPartition(t *testing.T) {
	if testing.Short() {
		t.Skip("scenario")
	}

	t.Run("minority", testMinorityPartition)
	t.Run("symmetric", testSymmetricPartition)
	t.Run("heal_under_traffic", testHealUnderTraffic)
	t.Run("across_epoch_boundary", testAcrossEpochBoundary)
	t.Run("flapping_partitions", testFlappingPartitions)
}

// newPartitionCluster starts an equal-stake 5-node cluster and waits until
// EVERY node has crossed the first epoch boundary, so the epoch snapshot
// governing quorum carries the equal bonded stakes rather than the
// founder-heavy genesis freeze (see partitionEpochLength).
func newPartitionCluster(t *testing.T) *harness.Cluster {
	t.Helper()

	c := harness.NewCluster(t, partitionScenarioSize, harness.WithEpochLength(partitionEpochLength))

	// Boundary 1 now lands at round 150, so wait it out on the epoch-boundary
	// timeout the epoch scenarios use, not the tighter per-step budget.
	ctx, cancel := context.WithTimeout(context.Background(), epochsBoundaryTimeout)
	defer cancel()

	if err := c.WaitAll(ctx, "epoch.transitioned", harness.AttrGE("epoch", 1)); err != nil {
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

	f0 := productionFrontier(t, c, isolated)
	c.Partition(majority, []int{isolated})

	majoritySeen := len(node0.Journal().Events("consensus.anchor.committed"))
	waitEventCount(stepCtx(t), t, node0, "consensus.anchor.committed", majoritySeen+2)

	requirePlateau(t, c.Node(isolated), f0)
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
//
// May still fail: the reintegrated side can still be replaying the commits
// it missed while partitioned when teardown's convergence window checks it.
// Under investigation.
func testSymmetricPartition(t *testing.T) {
	c := newPartitionCluster(t)
	node0 := c.Node(0)
	cli := c.Client(0)

	sideA := []int{0, 1, 2}
	sideB := []int{3, 4}

	f0A := productionFrontier(t, c, 0)
	f0B := productionFrontier(t, c, 3)
	c.Partition(sideA, sideB)

	requirePlateau(t, node0, f0A)
	requirePlateau(t, c.Node(3), f0B)

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
//
// May still fail: the reintegrated side can still be replaying the commits
// it missed while partitioned when teardown's convergence window checks it.
// Under investigation.
func testHealUnderTraffic(t *testing.T) {
	c := newPartitionCluster(t)
	node0 := c.Node(0)
	cli := c.Client(0)

	isolated := partitionScenarioSize - 1
	majority := []int{0, 1, 2, 3}

	w, coin := fundedWallet(stepCtx(t), t, cli, node0, 5_000_000)

	f0 := productionFrontier(t, c, isolated)
	c.Partition(majority, []int{isolated})

	trafficCtx, stopTraffic := context.WithCancel(context.Background())
	defer stopTraffic()
	progress, results := startTraffic(trafficCtx, cli, node0, w, coin)

	waitProgress(stepCtx(t), t, progress, 5)
	requirePlateau(t, c.Node(isolated), f0)

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

// testAcrossEpochBoundary isolates one node (4|1) for long enough that the
// majority crosses at least one epoch boundary while the isolate is cut off
// entirely — the epoch snapshot governing quorum, and the attested-tx epoch
// validity window, both change under it. Healing must let the isolate
// observe the SAME epoch transition the majority already committed (not a
// diverged one) and catch up to the majority's anchor round, without ever
// contradicting a committed anchor.
func testAcrossEpochBoundary(t *testing.T) {
	c := newPartitionCluster(t)
	node0 := c.Node(0)

	isolated := partitionScenarioSize - 1
	majority := []int{0, 1, 2, 3}

	startEpoch := latestEpoch(node0)

	f0 := productionFrontier(t, c, isolated)
	c.Partition(majority, []int{isolated})

	requirePlateau(t, c.Node(isolated), f0)

	waitNextBoundary(t, node0)
	crossedEpoch := latestEpoch(node0)
	if crossedEpoch <= startEpoch {
		t.Fatalf("majority did not cross an epoch boundary while partitioned: start=%d, now=%d", startEpoch, crossedEpoch)
	}

	majorityRound := latestAnchorRound(node0)

	c.Heal()

	if _, err := c.Node(isolated).WaitEvent(stepCtx(t), "epoch.transitioned", harness.AttrGE("epoch", crossedEpoch)); err != nil {
		c.Dump(t)
		t.Fatalf("isolated node %d never caught up to epoch %d after heal: %v", isolated, crossedEpoch, err)
	}

	if _, err := c.Node(isolated).WaitEvent(stepCtx(t), "consensus.anchor.committed", harness.AttrGE("round", majorityRound)); err != nil {
		c.Dump(t)
		t.Fatalf("isolated node %d never caught up past round %d after heal: %v", isolated, majorityRound, err)
	}

	requireZeroRollback(t, c)
}

// testFlappingPartitions drives 3 Partition(4|1)/Heal cycles under sustained
// background traffic, confirming after every heal that the isolated node
// catches up past the majority's round at heal time, then proves zero
// rollback over the whole flapping run.
func testFlappingPartitions(t *testing.T) {
	c := newPartitionCluster(t)
	node0 := c.Node(0)
	cli := c.Client(0)

	isolated := partitionScenarioSize - 1
	majority := []int{0, 1, 2, 3}

	w, coin := fundedWallet(stepCtx(t), t, cli, node0, 5_000_000)

	trafficCtx, stopTraffic := context.WithCancel(context.Background())
	defer stopTraffic()
	progress, results := startTraffic(trafficCtx, cli, node0, w, coin)

	for cycle := 0; cycle < flappingCycles; cycle++ {
		flapOnce(t, c, node0, isolated, majority, progress, cycle)
	}

	stopTraffic()
	res := <-results
	requireNoErr(t, res.err)

	requireZeroRollback(t, c)
}

// flapOnce drives one Partition/Heal cycle of testFlappingPartitions: it
// partitions the isolate away, waits for visible majority progress under the
// already-running traffic, heals, and confirms the isolate catches up past
// the majority's round at heal time.
func flapOnce(t *testing.T, c *harness.Cluster, node0 *harness.Node, isolated int, majority []int, progress *atomic.Int64, cycle int) {
	t.Helper()

	before := progress.Load()

	c.Partition(majority, []int{isolated})
	waitProgress(stepCtx(t), t, progress, before+2)

	burstRound := latestAnchorRound(node0)

	c.Heal()

	if _, err := c.Node(isolated).WaitEvent(stepCtx(t), "consensus.anchor.committed", harness.AttrGE("round", burstRound)); err != nil {
		c.Dump(t)
		t.Fatalf("cycle %d: isolated node %d never caught up past round %d after heal: %v", cycle, isolated, burstRound, err)
	}
}

// productionFrontier returns node i's current consensus round: the frontier it
// is building at, one past the highest round for which any vertex, and so any
// certification, can exist. Captured the instant before a partition, it is the
// ceiling a quorum-starved side may drain up to but never reach. Everything
// below it was produced and gossiped before the cut, so those rounds can still
// be assembled and committed from buffered state; nothing at or above it was.
func productionFrontier(t *testing.T, c *harness.Cluster, i int) uint64 {
	t.Helper()

	status, err := c.Client(i).Status()
	requireNoErr(t, err)

	return status.Round
}

// requirePlateau asserts n's committed anchor round never reaches f0, the
// production frontier n reported the instant before the partition. A side
// without quorum may still drain commits for rounds below f0 (their vertices
// were gossiped before the cut, so their certifications can be assembled from
// buffered state), so this deliberately tolerates that drain rather than
// freezing a baseline. But no vertex, and therefore no certification, exists at
// or above f0: reaching f0 as a committed anchor demands genuine post-cut
// production, which needs a quorum this side does not have, and that is exactly
// the violation. A bounded poll over plateauWindow, never a sleep-assert.
func requirePlateau(t *testing.T, n *harness.Node, f0 uint64) {
	t.Helper()

	deadline := time.After(plateauWindow)
	ticker := time.NewTicker(eventPollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			if got := latestAnchorRound(n); got >= f0 {
				t.Fatalf("node %d: committed anchor round reached %d, at or past the pre-partition production frontier %d, during an expected no-quorum plateau",
					n.Index, got, f0)
			}
		case <-deadline:
			return
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
