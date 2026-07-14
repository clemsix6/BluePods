package integration

import (
	"sort"
	"testing"
	"time"
)

// TestSimProgressiveJoining tests that existing nodes accept vertices from new validators.
// Scenario: start 5 nodes, wait for convergence, add 5 more, verify ALL 10 converge.
// This catches the bug where vertices from unknown producers were silently dropped
// instead of being buffered until the producer's register_validator tx commits.
func TestSimProgressiveJoining(t *testing.T) {
	cluster := NewCluster(t, 5,
		WithHTTPBase(18400),
		WithQUICBase(18400+920),
		WithMinValidators(5),
		WithGossipFanout(15),
		WithTransitionGrace(50),
		WithTransitionBuffer(50),
		WithSyncBuffer(12),
	)

	// Phase 1: wait for the initial 5 nodes to converge
	cluster.WaitReady(90 * time.Second)
	verifyAllSeeValidators(t, cluster, 5)

	// Phase 2: add 5 more nodes
	t.Log("Adding 5 new nodes...")
	cluster.AddNodes(5)

	// Phase 3: wait for ALL 10 nodes (including the original 5) to see 10 validators
	t.Log("Waiting for all 10 nodes to converge...")
	cluster.WaitForValidators(10, 120*time.Second)

	// Phase 4: the C-1 guard. Every joiner's committed projection (its exact validator
	// set and epoch state) must equal an original node's. A joiner that dropped a
	// committed transaction on its mid-load join forks this state permanently; the
	// count-only WaitForValidators above cannot see a fork that keeps the count right.
	assertCommittedProjectionAgrees(t, cluster)

	// Phase 5: verify round convergence across all nodes
	verifyRoundConvergence(t, cluster, 20)
}

// assertCommittedProjectionAgrees checks that every node reconstructs the identical
// committed state a live node holds: the exact validator pubkey set (derived purely
// from committed register_validator history — the tracker-level projection a mid-load
// joiner must not fork), the epoch counter, and the frozen holder-snapshot size. It
// is the purpose-built assertion the C-1 fix demands: with the round-grain commit cut,
// a joiner marked an anchor's uncommitted same-round siblings committed without their
// effects and dropped those transactions, forking its validator set forever.
func assertCommittedProjectionAgrees(t *testing.T, cluster *Cluster) {
	t.Helper()

	ref := cluster.Node(0).Addr()
	wantVals := sortedPubkeys(QueryValidators(t, ref))
	wantStatus := QueryStatus(t, ref)

	for i := 1; i < cluster.Size(); i++ {
		addr := cluster.Node(i).Addr()

		gotVals := sortedPubkeys(QueryValidators(t, addr))
		if !equalStringSlices(wantVals, gotVals) {
			t.Errorf("node %d validator set forks node 0's committed history:\n node0=%v\n node%d=%v",
				i, wantVals, i, gotVals)
		}

		got := QueryStatus(t, addr)
		if got.Epoch != wantStatus.Epoch {
			t.Errorf("node %d epoch=%d, want %d (node 0)", i, got.Epoch, wantStatus.Epoch)
		}
		if got.EpochHolders != wantStatus.EpochHolders {
			t.Errorf("node %d epochHolders=%d, want %d (node 0)", i, got.EpochHolders, wantStatus.EpochHolders)
		}
	}

	t.Logf("Committed projection agrees across all %d nodes: %d validators, epoch %d",
		cluster.Size(), len(wantVals), wantStatus.Epoch)
}

// sortedPubkeys returns the validators' hex pubkeys sorted, for order-independent
// set comparison across nodes.
func sortedPubkeys(vals []validatorResponse) []string {
	keys := make([]string, len(vals))
	for i, v := range vals {
		keys[i] = v.Pubkey
	}

	sort.Strings(keys)

	return keys
}

// equalStringSlices reports whether two string slices are element-wise equal.
func equalStringSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}

	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}

	return true
}

// TestSimBatchJoining simulates deployment-style drip-feed startup.
// Scenario: 5 initial nodes, then 3 batches of 5 added with short delays.
// This reproduces the production deployment where ansible adds batches of nodes
// with batch_delay seconds between each batch.
// Before the pending-vertex fix, early nodes would get stuck because vertices
// from batch N+1 arrived before their register_validator txs committed.
func TestSimBatchJoining(t *testing.T) {
	cluster := NewCluster(t, 5,
		WithHTTPBase(18600),
		WithQUICBase(18600+920),
		WithMinValidators(5),
		WithGossipFanout(25),
		WithTransitionGrace(100),
		WithTransitionBuffer(100),
		WithSyncBuffer(12),
	)

	// Phase 1: wait for the initial 5 nodes to converge
	cluster.WaitReady(90 * time.Second)
	verifyAllSeeValidators(t, cluster, 5)

	// Phase 2: add 3 batches of 5, simulating ansible drip-feed
	batchDelay := 5 * time.Second

	t.Log("Batch 1: adding 5 nodes...")
	cluster.AddNodes(5)
	time.Sleep(batchDelay)

	t.Log("Batch 2: adding 5 nodes...")
	cluster.AddNodes(5)
	time.Sleep(batchDelay)

	t.Log("Batch 3: adding 5 nodes...")
	cluster.AddNodes(5)

	// Phase 3: wait for ALL 20 nodes to see 20 validators
	t.Log("Waiting for all 20 nodes to converge...")
	cluster.WaitForValidators(20, 180*time.Second)

	// Phase 4: verify round convergence
	verifyRoundConvergence(t, cluster, 30)
}

// verifyAllSeeValidators checks that every node sees the expected validator count.
func verifyAllSeeValidators(t *testing.T, cluster *Cluster, expected int) {
	t.Helper()

	for i := 0; i < cluster.Size(); i++ {
		status := QueryStatus(t, cluster.Node(i).Addr())
		if status.Validators != expected {
			t.Errorf("node %d: validators=%d, expected %d", i, status.Validators, expected)
		}
	}

	t.Logf("All %d nodes see %d validators", cluster.Size(), expected)
}

// verifyRoundConvergence checks that round delta across all nodes is within maxDelta.
func verifyRoundConvergence(t *testing.T, cluster *Cluster, maxDelta uint64) {
	t.Helper()

	minRound := uint64(1<<63 - 1)
	maxRound := uint64(0)

	for i := 0; i < cluster.Size(); i++ {
		status := QueryStatusSafe(cluster.Node(i).Addr())
		if status == nil {
			t.Errorf("node %d: no status response", i)
			continue
		}

		t.Logf("Node %d: round=%d validators=%d", i, status.Round, status.Validators)

		if status.Round < minRound {
			minRound = status.Round
		}
		if status.Round > maxRound {
			maxRound = status.Round
		}
	}

	if maxRound-minRound > maxDelta {
		t.Errorf("round divergence too large: min=%d max=%d (delta=%d, max=%d)",
			minRound, maxRound, maxRound-minRound, maxDelta)
	} else {
		t.Logf("Round convergence OK: [%d, %d] (delta=%d)", minRound, maxRound, maxRound-minRound)
	}
}
