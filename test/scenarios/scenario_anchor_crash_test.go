package scenarios

import (
	"context"
	"testing"
	"time"

	"BluePods/pkg/client"
	"BluePods/test/harness"
)

const (
	// anchorCrashScenarioSize is the validator count for TestScenarioAnchorCrash.
	anchorCrashScenarioSize = 5

	// anchorCrashSearchTimeout bounds the search for a round designated to a
	// non-founder producer. consensus.round.advanced fires for every newly
	// advanced round (checkCommits, on a short tick) and the per-round
	// designation is a deterministic hash spread roughly evenly across the 5
	// equally staked validators, so a non-founder round turns up within a
	// handful of rounds.
	anchorCrashSearchTimeout = 60 * time.Second
)

// TestScenarioAnchorCrash reads a fresh consensus.round.advanced event's
// designated anchor producer and kills that node before its round is
// expected to decide. This is best effort: vertex production leads the
// commit cursor by design, but nothing GUARANTEES the kill wins a race
// against a very fast decision. If the designated node is the bootstrap,
// the search keeps going for a later round designating a non-founder
// (matching scenario_crash_test's non-bootstrap-victim scope). Either way
// the network must keep deciding: the target round is either explicitly
// skipped or covered by a later anchor's causal batch, and new traffic
// keeps committing. The killed node is restarted and must resync.
//
// Zero rollback is proven in-scenario (requireZeroRollback), independent of
// teardown's automatic convergence check.
func TestScenarioAnchorCrash(t *testing.T) {
	if testing.Short() {
		t.Skip("scenario")
	}

	c := harness.NewCluster(t, anchorCrashScenarioSize)
	node0 := c.Node(0)
	cli := c.Client(0)

	byPubkey := indexByPubkey(t, c)
	founderHex := hexPubkey(t, node0)

	targetRound, victim := waitNonFounderDesignation(t, node0, founderHex, byPubkey)
	t.Logf("anchor crash: round %d designated node %d", targetRound, victim)

	c.Kill(victim)
	if c.Node(victim).Alive() {
		t.Fatalf("node %d still alive after Kill", victim)
	}

	requireRoundHandled(t, c, targetRound)

	// New traffic still commits on the surviving 4.
	w, coin := fundedWallet(stepCtx(t), t, cli, node0, 200_000)
	_, hash, err := w.Split(cli, coin, 1_000, client.NewWallet().Pubkey())
	requireNoErr(t, err)
	requireVerdictAll(stepCtx(t), t, c, hash, true, "")

	c.Restart(victim)

	if _, err := c.Node(victim).WaitEvent(stepCtx(t), "sync.completed"); err != nil {
		c.Dump(t)
		t.Fatalf("restarted anchor-crash victim %d did not resync: %v", victim, err)
	}

	requireZeroRollback(t, c)
}

// hexPubkey returns node n's Ed25519 public key as the lowercase hex string
// events carry.
func hexPubkey(t *testing.T, n *harness.Node) string {
	t.Helper()

	return hexID(walletFromNodeKey(t, n).Pubkey())
}

// indexByPubkey maps every cluster node's hex pubkey to its index.
func indexByPubkey(t *testing.T, c *harness.Cluster) map[string]int {
	t.Helper()

	out := make(map[string]int)
	for _, n := range c.Nodes() {
		out[hexPubkey(t, n)] = n.Index
	}

	return out
}

// waitNonFounderDesignation scans consensus.round.advanced events in round
// order, starting from round 1, until one designates a node other than the
// founder, returning that round and the designated node's index.
func waitNonFounderDesignation(t *testing.T, node0 *harness.Node, founderHex string, byPubkey map[string]int) (uint64, int) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), anchorCrashSearchTimeout)
	defer cancel()

	var nextMin uint64 = 1
	for {
		ev, err := node0.WaitEvent(ctx, "consensus.round.advanced", harness.AttrGE("round", nextMin))
		if err != nil {
			t.Fatalf("no round.advanced designating a non-founder producer within %v: %v", anchorCrashSearchTimeout, err)
		}

		round, _ := ev.Attrs["round"].(float64)
		designated, _ := ev.Attrs["designated"].(string)
		nextMin = uint64(round) + 1

		if designated == founderHex {
			continue
		}

		idx, ok := byPubkey[designated]
		if !ok {
			t.Fatalf("round.advanced designated an unrecognized pubkey %s", designated)
		}

		return uint64(round), idx
	}
}

// requireRoundHandled waits for the network to move past round: either an
// explicit consensus.round.skipped for it, or a consensus.anchor.committed
// at or past it. A later anchor's causal batch can cover an undecided
// round's vertices without ever emitting an anchor specifically FOR that
// round, so either shape is forward progress and neither is preferred.
func requireRoundHandled(t *testing.T, c *harness.Cluster, round uint64) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), stepTimeout)
	defer cancel()

	skipped := make(chan struct{})
	committed := make(chan struct{})

	go func() {
		if _, err := c.Node(0).WaitEvent(ctx, "consensus.round.skipped", harness.Attr("round", round)); err == nil {
			close(skipped)
		}
	}()
	go func() {
		if _, err := c.Node(0).WaitEvent(ctx, "consensus.anchor.committed", harness.AttrGE("round", round)); err == nil {
			close(committed)
		}
	}()

	select {
	case <-skipped:
		t.Logf("round %d handled: explicit consensus.round.skipped", round)
	case <-committed:
		t.Logf("round %d handled: covered by an anchor at or past it", round)
	case <-ctx.Done():
		c.Dump(t)
		t.Fatalf("round %d was neither skipped nor covered by a later anchor within %v", round, stepTimeout)
	}
}
