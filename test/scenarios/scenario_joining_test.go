package scenarios

import (
	"context"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"
	"time"

	"BluePods/pkg/client"
	"BluePods/test/harness"
)

// joiningBaseSize is the initial cluster size for TestScenarioJoining.
const joiningBaseSize = 5

// wrongCheckpointEpoch is the epoch named by the deliberately-wrong
// checkpoint testWrongCheckpointRefused pins: 0, since this scenario never
// runs long enough to cross an epoch boundary, so the cluster's genesis
// committee is still the one any real checkpoint would name.
const wrongCheckpointEpoch = 0

// TestScenarioJoining starts a 5-node cluster and grows it: one spawned
// node, then a batch of two more. Each newcomer must sync (sync.completed),
// self-register (epoch.validator.registered observed by the founder), and
// join replication (a transaction submitted after the joins commits on every
// node, newcomers included). The founder's status must count all 8. It also
// covers the verified join path end to end: a joined node's anchored index
// root agrees with a founder's, and a join pinned to a checkpoint naming no
// real committee is refused rather than let onto the convergence set.
func TestScenarioJoining(t *testing.T) {
	if testing.Short() {
		t.Skip("scenario")
	}

	c := harness.NewCluster(t, joiningBaseSize)
	node0 := c.Node(0)
	cli := c.Client(0)

	var joined *harness.Node

	t.Run("single_join", func(t *testing.T) {
		joined = c.Spawn()
		requireRegistered(t, node0, joined)
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

	t.Run("joined_root_matches_founders", func(t *testing.T) {
		requireIndexAnchorMatches(t, c, node0, joined)
	})

	t.Run("wrong_checkpoint_refused", func(t *testing.T) {
		testWrongCheckpointRefused(t, c)
	})
}

// requireIndexAnchorMatches polls GetIndexAnchor on founder and joined until
// both report a quorate bundle at the SAME frontier, then asserts their index
// roots agree at it. The join succeeding proves the joiner's own rebuilt
// state passed verification; this proves that state is not merely internally
// consistent but the one the rest of the cluster anchors.
func requireIndexAnchorMatches(t *testing.T, c *harness.Cluster, founder, joined *harness.Node) {
	t.Helper()

	ctx := stepCtx(t)
	ticker := time.NewTicker(eventPollInterval)
	defer ticker.Stop()

	for {
		founderBundle, err := c.Client(founder.Index).GetIndexAnchor()
		requireNoErr(t, err)

		joinedBundle, err := c.Client(joined.Index).GetIndexAnchor()
		requireNoErr(t, err)

		if founderBundle.Found && joinedBundle.Found && founderBundle.FrontierRound == joinedBundle.FrontierRound {
			if founderBundle.IndexRoot != joinedBundle.IndexRoot {
				t.Fatalf("frontier %d: founder node %d roots at %x, joined node %d roots at %x",
					founderBundle.FrontierRound, founder.Index, founderBundle.IndexRoot[:4],
					joined.Index, joinedBundle.IndexRoot[:4])
			}

			return
		}

		select {
		case <-ticker.C:
			continue
		case <-ctx.Done():
			t.Fatalf("founder node %d and joined node %d never reported a shared frontier to compare (last: founder=%d found=%v, joined=%d found=%v)",
				founder.Index, joined.Index, founderBundle.FrontierRound, founderBundle.Found,
				joinedBundle.FrontierRound, joinedBundle.Found)
			return
		}
	}
}

// testWrongCheckpointRefused spawns a node pinned to a checkpoint naming no
// committee this cluster has (a syntactically valid epoch:root pair that
// matches nothing) and asserts the fail-closed gate refuses it: the node
// records node.stopping reason=sync_unverified, never sync.completed, and
// exits on its own without ever joining the convergence set — so the rest of
// the cluster's alive count is unaffected and WithoutInvariants is not
// needed for this scenario.
func testWrongCheckpointRefused(t *testing.T, c *harness.Cluster) {
	t.Helper()

	before := len(c.Alive())

	wrong := fmt.Sprintf("%d:%s", wrongCheckpointEpoch, strings.Repeat("ab", 32))
	refused := c.SpawnWithCheckpoint(wrong)

	ctx := stepCtx(t)

	if _, err := refused.WaitEvent(ctx, "node.stopping", harness.Attr("reason", "sync_unverified")); err != nil {
		t.Fatalf("a wrong checkpoint must abort the join with node.stopping reason=sync_unverified: %v", err)
	}

	if completed := refused.Journal().Events("sync.completed"); len(completed) != 0 {
		t.Fatalf("node %d reported sync.completed despite a wrong checkpoint: it went live on state it never verified", refused.Index)
	}

	// The event and the process exiting are two different observables;
	// close the small race between them before checking the alive set below.
	waitProcessExited(ctx, t, refused)

	if got := len(c.Alive()); got != before {
		t.Fatalf("the cluster's alive set changed after a refused join: had %d, now %d", before, got)
	}

	testRefusedJoinRestartRetakesGate(t, c, refused)
}

// testRefusedJoinRestartRetakesGate restarts the node the gate just refused,
// same data directory, same wrong checkpoint it was spawned with (Restart
// reuses the last stored args unless SetTrustCheckpoint changes them). That
// directory holds a cursor and a live validator set from the snapshot
// performSync already applied — the residue a refused join always leaves
// behind, since the sync path writes the applied state and runs the commit
// loop over it BEFORE verifySyncedState decides anything (see
// internal/consensus/adopted.go). This is exactly the shape a routing bug
// could launder into permanently-adopted state on a second start; it must
// not: the restart takes the SAME gate again (node.stopping
// reason=sync_unverified, no sync.completed), and the directory must never
// resume — zero node.resumed events, across either generation.
func testRefusedJoinRestartRetakesGate(t *testing.T, c *harness.Cluster, refused *harness.Node) {
	t.Helper()

	var source *harness.Node
	for _, n := range c.Alive() {
		if n.Index != refused.Index {
			source = n
			break
		}
	}
	if source == nil {
		t.Fatalf("no alive node left to restart the refused node against")
	}

	newSegment := inSegmentAfter(refused)

	requireNoErr(t, refused.Restart(source.QUICAddr))

	ctx := stepCtx(t)

	if _, err := refused.WaitEvent(ctx, "node.stopping", newSegment, harness.Attr("reason", "sync_unverified")); err != nil {
		t.Fatalf("restarting the refused node's poisoned directory must retake the gate (reason=sync_unverified): %v", err)
	}

	if completed := refused.Journal().Events("sync.completed", newSegment); len(completed) != 0 {
		t.Fatalf("node %d reported sync.completed on restart despite the still-wrong checkpoint", refused.Index)
	}

	if resumed := refused.Journal().Events("node.resumed"); len(resumed) != 0 {
		t.Fatalf("node %d recorded %d node.resumed event(s) across its generations: a directory the gate refused must never resume from local state", refused.Index, len(resumed))
	}

	waitProcessExited(ctx, t, refused)
}

// waitProcessExited polls until n's process is no longer running, bounded by
// ctx.
func waitProcessExited(ctx context.Context, t *testing.T, n *harness.Node) {
	t.Helper()

	ticker := time.NewTicker(eventPollInterval)
	defer ticker.Stop()

	for {
		if !n.Alive() {
			return
		}

		select {
		case <-ticker.C:
			continue
		case <-ctx.Done():
			t.Fatalf("node %d never exited after refusing its join", n.Index)
			return
		}
	}
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
