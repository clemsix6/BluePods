package harness

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/zeebo/blake3"

	"BluePods/pkg/client"
)

// startTestBootstrap builds the node binary and system pod once, then starts
// a fresh single-node bootstrap under t.TempDir(), returning it. The caller
// is responsible for killing it (directly or via t.Cleanup).
func startTestBootstrap(t *testing.T) *Node {
	t.Helper()

	binPath, err := nodeBinary()
	if err != nil {
		t.Fatalf("build node binary: %v", err)
	}

	podPath, err := systemPodPath()
	if err != nil {
		t.Fatalf("locate system pod: %v", err)
	}

	dir := t.TempDir()

	port, err := allocatePort()
	if err != nil {
		t.Fatalf("allocate port: %v", err)
	}
	quicAddr := fmt.Sprintf("127.0.0.1:%d", port)

	n, err := newNode(0, dir, binPath, quicAddr)
	if err != nil {
		t.Fatalf("new node: %v", err)
	}

	if err := n.Start(NodeArgs{Bootstrap: true, SystemPod: podPath}); err != nil {
		t.Fatalf("start node: %v", err)
	}

	return n
}

// waitReady blocks until the node reaches node.ready, bounded by timeout.
func waitReady(t *testing.T, n *Node, timeout time.Duration) Event {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	ev, err := n.WaitEvent(ctx, "node.ready")
	if err != nil {
		t.Fatalf("wait node.ready: %v", err)
	}

	return ev
}

// TestNodeStartAndReady asserts a bootstrap node reaches node.started then
// node.ready under real process startup.
func TestNodeStartAndReady(t *testing.T) {
	n := startTestBootstrap(t)
	t.Cleanup(n.Kill)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if _, err := n.WaitEvent(ctx, "node.started"); err != nil {
		t.Fatalf("wait node.started: %v", err)
	}

	ev := waitReady(t, n, 30*time.Second)
	if ev.Seg != 0 {
		t.Fatalf("expected segment 0, got %d", ev.Seg)
	}

	if !n.Alive() {
		t.Fatal("node should report alive once ready")
	}
}

// TestNodeKillMidTraffic drives real faucet traffic against a live node and
// SIGKILLs it right after traffic starts flowing (event-driven, not a
// sleep), so the pump observes a hard kill with a write potentially in
// flight. Alive() must go false and ParseError must stay nil: a truncated
// final line is dropped structurally, never fed to the journal.
func TestNodeKillMidTraffic(t *testing.T) {
	n := startTestBootstrap(t)
	waitReady(t, n, 30*time.Second)

	podPath, err := systemPodPath()
	if err != nil {
		t.Fatalf("locate system pod: %v", err)
	}
	podBytes, err := os.ReadFile(podPath)
	if err != nil {
		t.Fatalf("read system pod: %v", err)
	}
	podID := blake3.Sum256(podBytes)

	cli, err := client.NewClient(n.QUICAddr, podID)
	if err != nil {
		t.Fatalf("client: %v", err)
	}

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		w := client.NewWallet()
		for {
			select {
			case <-stop:
				return
			default:
			}
			cli.Faucet(w.Pubkey(), 1) //nolint:errcheck // deliberately racing the kill below
		}
	}()

	waitCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if _, err := n.WaitEvent(waitCtx, "ingress.tx.received"); err != nil {
		close(stop)
		wg.Wait()
		t.Fatalf("wait for traffic to start: %v", err)
	}

	n.Kill()
	close(stop)
	wg.Wait()

	if n.Alive() {
		t.Fatal("node should not be alive after Kill")
	}
	if err := n.ParseError(); err != nil {
		t.Fatalf("unexpected schema-drift error after a hard kill: %v", err)
	}
}

// TestNodeRestartOpensNewSegment asserts Restart reuses the same identity,
// opens journal segment 1, and reaches node.ready again.
func TestNodeRestartOpensNewSegment(t *testing.T) {
	n := startTestBootstrap(t)
	waitReady(t, n, 30*time.Second)

	n.Kill()
	if n.Alive() {
		t.Fatal("node should not be alive after Kill")
	}

	if err := n.Restart(""); err != nil {
		t.Fatalf("restart: %v", err)
	}
	t.Cleanup(n.Kill)

	// A bare WaitEvent for "node.ready" would immediately match the stale
	// segment-0 event (Wait matches already-recorded events by design), so
	// the new segment is targeted explicitly through Event.Seg — an ordinary
	// Pred, since Seg is an exported field.
	inNewSegment := func(e Event) bool { return e.Seg >= 1 }

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	ev, err := n.WaitEvent(ctx, "node.ready", inNewSegment)
	if err != nil {
		t.Fatalf("wait node.ready after restart: %v", err)
	}
	if ev.Seg != 1 {
		t.Fatalf("expected the restart's node.ready in segment 1, got %d", ev.Seg)
	}

	if err := n.ParseError(); err != nil {
		t.Fatalf("unexpected schema-drift error across restart: %v", err)
	}
}
