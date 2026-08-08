package harness

import (
	"context"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"BluePods/pkg/client"
)

// TestClusterBasics starts a 3-node cluster, asserts the default stake setup
// completed (every node's journal shows both non-founder bonds, since
// consensus replicates the commit stream to every node), drives a coin
// split through Client(0) to commitment on every node, and round-trips a
// partition and heal.
//
// WithoutInvariants: this test is about orchestration mechanics, not full
// invariant validation (that is CheckInvariants' own test). A known,
// reproducible project bug (not a harness defect; see the Task 17 report)
// makes cross-node fingerprints diverge whenever 2+ validators register
// within the same cluster, so the automatic convergence/supply check would
// fail here through no fault of the mechanics under test.
func TestClusterBasics(t *testing.T) {
	c := NewCluster(t, 3, WithoutInvariants())

	for _, n := range c.Nodes() {
		got := n.Journal().Events("stake.bonded")
		if len(got) != 2 {
			t.Fatalf("node %d: expected 2 stake.bonded events (one per non-founder), got %d", n.Index, len(got))
		}
	}

	w := client.NewWallet()
	cli0 := c.Client(0)

	coinID, faucetHash, err := cli0.Faucet(w.Pubkey(), 1_000_000)
	if err != nil {
		t.Fatalf("faucet: %v", err)
	}

	waitCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := c.WaitAll(waitCtx, "tx.committed", Attr("tx", hex.EncodeToString(faucetHash[:]))); err != nil {
		t.Fatalf("wait faucet commit on every node: %v", err)
	}

	if err := w.RefreshCoin(cli0, coinID); err != nil {
		t.Fatalf("refresh coin: %v", err)
	}

	recipient := client.NewWallet().Pubkey()

	_, splitHash, err := w.Split(cli0, coinID, 100, recipient)
	if err != nil {
		t.Fatalf("split: %v", err)
	}

	splitCtx, cancel2 := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel2()

	splitPreds := []Pred{Attr("tx", hex.EncodeToString(splitHash[:])), Attr("success", true)}
	if err := c.WaitAll(splitCtx, "tx.committed", splitPreds...); err != nil {
		t.Fatalf("wait split commit on every node: %v", err)
	}

	c.Partition([]int{0}, []int{1, 2})
	c.Heal()
}

// TestClusterSpawnJoinsOnTheVerifiedPath is the harness half of the
// fail-closed join: a spawned node must pin a REAL checkpoint derived from the
// node it syncs from, and complete the verification the node binary now runs
// before it reports sync.completed. Without this the whole scenario corpus
// would exercise the --insecure-bootstrap escape hatch and prove nothing about
// the gate.
//
// WithoutInvariants for the same reason as TestClusterBasics: the newcomer's
// registration is a second one in this cluster.
func TestClusterSpawnJoinsOnTheVerifiedPath(t *testing.T) {
	c := NewCluster(t, 2, WithoutInvariants())

	// Spawn itself is the assertion: it waits for sync.completed, which a node
	// that failed verification never emits (it exits instead).
	n := c.Spawn()

	if n.trustCheckpointArg() == "" {
		t.Fatal("the spawned node pinned no checkpoint: it joined on the escape hatch, not the verified path")
	}

	if refusals := n.Journal().Events("node.stopping", Attr("reason", "sync_unverified")); len(refusals) != 0 {
		t.Fatalf("the spawned node refused its own join: %v", refusals)
	}

	logs, err := os.ReadFile(filepath.Join(n.Dir, "stdout.log"))
	if err != nil {
		t.Fatalf("read spawned node log: %v", err)
	}

	if !strings.Contains(string(logs), verifiedJoinLog) {
		t.Fatal("the spawned node never reported a verified join: the gate did not run")
	}

	if strings.Contains(string(logs), "INSECURE BOOTSTRAP") {
		t.Fatal("the spawned node warned about an insecure bootstrap: the checkpoint was not honoured")
	}

	// Resuming needs state the node OWNS, and proving the join is only half of
	// that: the other half is the committed state a resume rebuilds from, the
	// commit cursor and the live validator set its own commit loop persists.
	// Spawn returns the instant sync.completed lands, before either exists, so
	// wait for the newcomer to commit rounds for itself first. A node stopped
	// with nothing of its own on disk has nothing to resume and correctly joins
	// again, which is a different claim from the one under test here.
	waitCtx, cancel := context.WithTimeout(context.Background(), nodeReadyTimeout)
	defer cancel()

	if _, err := n.WaitEvent(waitCtx, "node.ready"); err != nil {
		t.Fatalf("the spawned node never became ready: %v", err)
	}

	if _, err := n.Journal().waitCount(waitCtx, "consensus.anchor.committed", 2); err != nil {
		t.Fatalf("the spawned node committed no round of its own: %v", err)
	}

	// A restart is NOT a second join. The state in that data directory is the
	// node's own — it proved it above, before going live on it, and has been
	// committing into it since — so the restart resumes from it: no snapshot is
	// fetched, no quorum is asked to attest anything, and the gate is not re-run
	// because there is nothing foreign to judge. Restart already waited for
	// node.ready in the new segment. The stop is graceful so the durability of
	// the last committed round is not what this test turns on (node_test.go
	// owns SIGKILL's semantics, and the crash scenarios own crash recovery).
	nextSeg := n.Journal().currentSegment() + 1

	if err := n.Stop(); err != nil {
		t.Fatalf("stop the spawned node: %v", err)
	}

	c.Restart(n.Index)

	inNewSegment := func(e Event) bool { return e.Seg >= nextSeg }

	if resumed := n.Journal().Events("node.resumed", inNewSegment); len(resumed) != 1 {
		t.Fatalf("the restarted node recorded %d node.resumed events in its new segment, want 1: it re-adopted its own state as foreign", len(resumed))
	}

	if synced := n.Journal().Events("sync.completed", inNewSegment); len(synced) != 0 {
		t.Fatal("the restarted node synced: it threw away committed state it owns and took it back from a peer")
	}

	logs, err = os.ReadFile(filepath.Join(n.Dir, "stdout.log"))
	if err != nil {
		t.Fatalf("read restarted node log: %v", err)
	}

	if got := strings.Count(string(logs), verifiedJoinLog); got != 1 {
		t.Fatalf("the node reported %d verified joins, want exactly the spawn's: a restart proves nothing to anyone", got)
	}
}

// verifiedJoinLog is the line a node writes once a stake quorum has attested
// the index root it rebuilt: the observable end of the fail-closed gate.
const verifiedJoinLog = "synced state verified against the anchored root"

// TestClusterJoinRefusesAWrongCheckpoint is the negative half, through the
// real binary: a node handed a checkpoint that pins a committee the cluster
// does not have must refuse to go live and say so, rather than sync happily
// against whatever validator set its source happened to ship. The refusal is
// observable exactly where an operator and a scenario look for it —
// node.stopping with reason "sync_unverified".
func TestClusterJoinRefusesAWrongCheckpoint(t *testing.T) {
	c := NewCluster(t, 2, WithoutInvariants())

	source := c.firstAlive(-1)
	if source == nil {
		t.Fatal("no alive node to sync from")
	}

	// Same epoch, a root no committee hashes to.
	wrong := fmt.Sprintf("%d:%s", 0, strings.Repeat("ab", 32))

	refused, err := c.startOneErr(99, false, source.QUICAddr, wrong, false)
	if err != nil {
		t.Fatalf("start the refusing node: %v", err)
	}
	t.Cleanup(func() { refused.Stop() })

	ctx, cancel := context.WithTimeout(context.Background(), nodeReadyTimeout)
	defer cancel()

	if _, err := refused.WaitEvent(ctx, "node.stopping", Attr("reason", "sync_unverified")); err != nil {
		t.Fatalf("a wrong checkpoint must abort the join with node.stopping reason=sync_unverified: %v", err)
	}

	if completed := refused.Journal().Events("sync.completed"); len(completed) != 0 {
		t.Fatal("the refusing node reported sync.completed: it went live on state it could not verify")
	}
}
