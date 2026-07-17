package scenarios

import (
	"context"
	"encoding/hex"
	"testing"
	"time"

	"BluePods/pkg/client"
	"BluePods/test/harness"
)

const (
	// churnBaseSize is the initial cluster size: a founder plus exactly one
	// non-founder validator. One non-founder keeps the base cluster's own
	// genesis registration at exactly churnMaxChurn additions for the first
	// boundary, so it is admitted whole rather than itself contending with
	// the churn cap (see newChurnCluster).
	churnBaseSize = 2

	// churnEpochLength makes boundaries land roughly every 20-30s (matching
	// the epochsLength precedent in scenario_epochs_test.go) so the scenario
	// crosses several within its budget.
	churnEpochLength = 50

	// churnMaxChurn is the cluster's max validator additions admitted per
	// epoch boundary (harness.WithMaxChurn), the option under test.
	churnMaxChurn = 1

	// churnSpawnCount is how many nodes are spawned onto the base cluster,
	// one at a time, each expected to cross its own boundary before the next
	// spawns, so growth is spread over churnSpawnCount separate boundaries.
	churnSpawnCount = 3

	// churnBoundaryTimeout bounds one wait for the next epoch boundary.
	churnBoundaryTimeout = 3 * time.Minute
)

// TestScenarioChurn drives harness.WithMaxChurn, whitepaper §10's churn
// limiting at epoch boundaries: a cluster admits at most maxChurn new
// validators into the frozen epoch-holder set per boundary, deferring the
// rest rather than admitting every pending registration at once.
//
// The base cluster is 2 nodes (founder plus one non-founder) built with
// WithoutStakeSetup: a bigger default cluster's own non-founder registrations
// would themselves contend with maxChurn=1 during genesis (every
// registration, genesis included, feeds the same churn-tracked
// epochAdditions list), which can strand the initial set for boundaries
// before any scenario-spawned node is even involved. One non-founder keeps
// that base registration exactly at the cap, so the scenario's own spawns are
// the only thing churn limiting is observed acting on.
//
// Per test/BUGS.md entry 8 (now fixed): teardown's supply-identity check holds
// on every node. The four non-founder registrations (the base non-founder plus
// the three spawned nodes) each now stamp a zero deposit instead of an unpaid
// 1000, so the identity is exact rather than off by +4000. Both in-scenario
// subtests are green.
func TestScenarioChurn(t *testing.T) {
	if testing.Short() {
		t.Skip("scenario")
	}

	c := newChurnCluster(t)
	node0 := c.Node(0)
	cli := c.Client(0)

	spawned := make([]*harness.Node, 0, churnSpawnCount)

	t.Run("additions_capped_per_boundary", func(t *testing.T) {
		for i := 0; i < churnSpawnCount; i++ {
			n := c.Spawn()
			spawned = append(spawned, n)

			requireSoleAddition(t, node0, n)
		}

		requireEveryBoundaryCapped(t, node0)
	})

	t.Run("all_eventually_join", func(t *testing.T) {
		waitEpochHolders(t, cli, churnBaseSize+churnSpawnCount)
		requireSpreadAcrossBoundaries(t, node0)

		for _, n := range spawned {
			requireRegistered(t, node0, n)
		}
	})
}

// newChurnCluster starts the base cluster and waits out the first epoch
// boundary, so the non-founder's own genesis registration (one addition,
// exactly at churnMaxChurn) is admitted before any scenario-spawned node
// registers.
func newChurnCluster(t *testing.T) *harness.Cluster {
	t.Helper()

	c := harness.NewCluster(t, churnBaseSize,
		harness.WithoutStakeSetup(),
		harness.WithMaxChurn(churnMaxChurn),
		harness.WithEpochLength(churnEpochLength),
	)

	ev := waitChurnBoundary(t, c.Node(0))
	if got := len(attrStrings(ev.Attrs["added"])); got > churnMaxChurn {
		t.Fatalf("base cluster's own boundary: added %d exceeds maxChurn %d: %v",
			got, churnMaxChurn, ev.Attrs["added"])
	}

	return c
}

// requireSoleAddition waits for the next boundary after spawning n and
// asserts its "added" attribute is exactly n's pubkey: one addition, admitted
// whole, never deferred or batched with another.
func requireSoleAddition(t *testing.T, node0 *harness.Node, n *harness.Node) {
	t.Helper()

	ev := waitChurnBoundary(t, node0)
	added := attrStrings(ev.Attrs["added"])

	w := walletFromNodeKey(t, n)
	pubkey := w.Pubkey()
	pk := hex.EncodeToString(pubkey[:])

	if len(added) != 1 || added[0] != pk {
		t.Fatalf("boundary after spawning node %d: added = %v, want [%s]", n.Index, added, pk)
	}
}

// requireEveryBoundaryCapped asserts every epoch.transitioned event node0 has
// recorded so far carries an "added" list no longer than churnMaxChurn, the
// cap itself, independent of which addition it names.
func requireEveryBoundaryCapped(t *testing.T, node0 *harness.Node) {
	t.Helper()

	for _, ev := range node0.Journal().Events("epoch.transitioned") {
		if got := len(attrStrings(ev.Attrs["added"])); got > churnMaxChurn {
			t.Fatalf("epoch %v: added %d validators, exceeds maxChurn %d: %v",
				ev.Attrs["epoch"], got, churnMaxChurn, ev.Attrs["added"])
		}
	}
}

// requireSpreadAcrossBoundaries asserts node0 needed at least one boundary
// per spawned node (plus the base cluster's own) to fully admit the churned
// additions, confirming growth was actually spread out rather than
// (incorrectly) admitted all at once.
func requireSpreadAcrossBoundaries(t *testing.T, node0 *harness.Node) {
	t.Helper()

	boundaries := len(node0.Journal().Events("epoch.transitioned"))
	if boundaries < churnSpawnCount+1 {
		t.Fatalf("only %d epoch boundaries elapsed, want at least %d (one per spawn plus the base cluster's own): "+
			"churn cap did not spread admissions", boundaries, churnSpawnCount+1)
	}
}

// waitChurnBoundary blocks until node0 records one more epoch.transitioned
// than it has now, bounded by churnBoundaryTimeout, and returns that new
// event.
func waitChurnBoundary(t *testing.T, node0 *harness.Node) harness.Event {
	t.Helper()

	seen := len(node0.Journal().Events("epoch.transitioned"))

	ctx, cancel := context.WithTimeout(context.Background(), churnBoundaryTimeout)
	defer cancel()

	waitEventCount(ctx, t, node0, "epoch.transitioned", seen+1)

	events := node0.Journal().Events("epoch.transitioned")
	return events[len(events)-1]
}

// waitEpochHolders polls cli's Status until EpochHolders (the churn-gated
// frozen-snapshot count, distinct from the unfiltered live Validators count)
// reaches want, bounded by churnBoundaryTimeout. By the time all_eventually_join
// runs, requireSoleAddition has already waited out every boundary the growth
// needed, so this returns almost immediately; the bound is a safety margin,
// not an expected wait.
func waitEpochHolders(t *testing.T, cli *client.Client, want int) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), churnBoundaryTimeout)
	defer cancel()

	ticker := time.NewTicker(eventPollInterval)
	defer ticker.Stop()

	for {
		status, err := cli.Status()
		if err == nil && int(status.EpochHolders) == want {
			return
		}

		select {
		case <-ticker.C:
			continue
		case <-ctx.Done():
			got := -1
			if err == nil {
				got = int(status.EpochHolders)
			}
			t.Fatalf("epoch-holder count never reached %d (last: %d)", want, got)
			return
		}
	}
}
