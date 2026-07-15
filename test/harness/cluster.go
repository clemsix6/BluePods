package harness

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/zeebo/blake3"

	"BluePods/pkg/client"
	"BluePods/pkg/daemon"
)

const (
	// sequentialStartupMax is the largest cluster size started one node at a
	// time (matching production registration flow); larger clusters start
	// their validators in one parallel batch.
	sequentialStartupMax = 10

	// bigClusterTransition is the cluster size at and above which the
	// transition window is widened: parallel registration lands validators at
	// different times, and the default window is too tight to avoid a quorum
	// deadlock.
	bigClusterTransition = 10

	// nodeReadyTimeout bounds how long NewCluster waits for a single node to
	// report node.ready.
	nodeReadyTimeout = 60 * time.Second

	// roundAdvanceTimeout bounds how long NewCluster waits for the whole
	// cluster to reach round 1 once every node is individually ready.
	roundAdvanceTimeout = 60 * time.Second

	// defaultInitialMint is the bootstrap mint used when WithInitialMint is
	// not given. It is generously large relative to bondFeeMargin so the
	// equal-stake computation in setup.go has ample precision at any
	// realistic cluster size.
	defaultInitialMint = 1_000_000_000_000
)

// Cluster manages a group of real node processes for one scenario: their
// startup sequencing, client/daemon access, kill/restart/spawn, network
// partitioning, and the teardown invariant check.
type Cluster struct {
	t *testing.T // t is the owning test

	dir         string      // dir is the cluster's base temp directory (each node gets a subdirectory)
	binaryPath  string      // binaryPath is the compiled node binary, shared across the cluster
	systemPod   string      // systemPod is the path to the system pod WASM
	systemPodID [32]byte    // systemPodID is the system pod ID (BLAKE3 of the WASM)
	opts        clusterOpts // opts is the cluster's resolved configuration

	mu    sync.Mutex // mu guards nodes (Spawn appends to it)
	nodes []*Node    // nodes is every node started so far, indexed by position
}

// NewCluster builds the node binary once, starts size real node processes
// (bootstrap plus validators), waits for the cluster to reach round 1, bonds
// an equal stake behind every non-founder validator (unless
// WithoutStakeSetup), and registers teardown: nodes are registered for
// t.Cleanup BEFORE the invariant check, so t.Cleanup's LIFO order runs the
// invariant check first, over still-alive nodes, then stops them.
func NewCluster(t *testing.T, size int, opts ...Option) *Cluster {
	t.Helper()

	if size < 1 {
		t.Fatalf("cluster size must be at least 1, got %d", size)
	}

	o := resolveOptions(size, opts)

	binPath, err := nodeBinary()
	if err != nil {
		t.Fatalf("build node binary: %v", err)
	}

	podPath, err := systemPodPath()
	if err != nil {
		t.Fatalf("locate system pod: %v", err)
	}

	podBytes, err := os.ReadFile(podPath)
	if err != nil {
		t.Fatalf("read system pod: %v", err)
	}

	c := &Cluster{
		t:           t,
		dir:         t.TempDir(),
		binaryPath:  binPath,
		systemPod:   podPath,
		systemPodID: blake3.Sum256(podBytes),
		opts:        o,
	}

	c.startCluster(size)

	// Registration order matters: t.Cleanup runs LIFO, so registering Stop
	// first and the invariant check second makes the check run BEFORE nodes
	// stop, over still-alive nodes.
	t.Cleanup(c.stopAll)
	if !o.withoutInvariants {
		t.Cleanup(func() { c.CheckInvariants(t) })
	}

	if !o.withoutStakeSetup {
		c.setupStakes()
	}

	return c
}

// resolveOptions applies opts over size-dependent defaults, matching the
// tuning test/integration/helpers.go used for large clusters.
func resolveOptions(size int, opts []Option) clusterOpts {
	o := clusterOpts{initialMint: defaultInitialMint}
	for _, opt := range opts {
		opt(&o)
	}

	if o.minValidators == 0 {
		o.minValidators = size
	}
	if size >= bigClusterTransition {
		if o.transitionGrace == 0 {
			o.transitionGrace = 100
		}
		if o.transitionBuffer == 0 {
			o.transitionBuffer = 100
		}
	}
	if o.gossipFanout == 0 && size > sequentialStartupMax {
		o.gossipFanout = size
	}

	return o
}

// startCluster starts the bootstrap node, then every validator, then waits
// for the whole cluster to reach round 1.
func (c *Cluster) startCluster(size int) {
	c.t.Helper()

	bootstrap := c.startOne(0, true, "")
	c.nodes = []*Node{bootstrap}
	c.waitNodeReady(bootstrap)

	if size > 1 {
		if size <= sequentialStartupMax {
			c.startValidatorsSequential(size, bootstrap.QUICAddr)
		} else {
			c.startValidatorsParallel(size, bootstrap.QUICAddr)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), roundAdvanceTimeout)
	defer cancel()

	if err := c.WaitAll(ctx, "consensus.round.advanced", AttrGE("round", 1)); err != nil {
		c.Dump(c.t)
		c.t.Fatalf("cluster did not reach round 1: %v", err)
	}
}

// startValidatorsSequential starts validators one at a time, waiting for
// each to become ready before starting the next (matches production
// registration flow).
func (c *Cluster) startValidatorsSequential(size int, bootstrapAddr string) {
	c.t.Helper()

	for i := 1; i < size; i++ {
		n := c.startOne(i, false, bootstrapAddr)
		c.nodes = append(c.nodes, n)
		c.waitNodeReady(n)
	}
}

// startValidatorsParallel starts every validator concurrently, then waits
// for all of them, for clusters large enough that sequential startup would
// be too slow.
func (c *Cluster) startValidatorsParallel(size int, bootstrapAddr string) {
	c.t.Helper()

	c.nodes = append(c.nodes, make([]*Node, size-1)...)

	var wg sync.WaitGroup
	for i := 1; i < size; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			c.nodes[idx] = c.startOne(idx, false, bootstrapAddr)
		}(i)
	}
	wg.Wait()

	for i := 1; i < size; i++ {
		c.waitNodeReady(c.nodes[i])
	}
}

// startOne allocates a port, creates a node under the cluster's directory,
// and starts it with the cluster's tuning.
func (c *Cluster) startOne(index int, isBootstrap bool, bootstrapAddr string) *Node {
	c.t.Helper()

	port, err := allocatePort()
	if err != nil {
		c.t.Fatalf("allocate port for node %d: %v", index, err)
	}

	dir := filepath.Join(c.dir, fmt.Sprintf("node-%d", index))
	quicAddr := fmt.Sprintf("127.0.0.1:%d", port)

	n, err := newNode(index, dir, c.binaryPath, quicAddr)
	if err != nil {
		c.t.Fatalf("create node %d: %v", index, err)
	}

	args := NodeArgs{
		Bootstrap:        isBootstrap,
		BootstrapAddr:    bootstrapAddr,
		SystemPod:        c.systemPod,
		MinValidators:    c.opts.minValidators,
		SyncBuffer:       c.opts.syncBuffer,
		EpochLength:      c.opts.epochLength,
		MaxChurn:         c.opts.maxChurn,
		GossipFanout:     c.opts.gossipFanout,
		TransitionGrace:  c.opts.transitionGrace,
		TransitionBuffer: c.opts.transitionBuffer,
		InitialMint:      c.opts.initialMint,
	}

	if err := n.Start(args); err != nil {
		c.t.Fatalf("start node %d: %v", index, err)
	}

	return n
}

// waitNodeReady blocks until n reports node.ready, dumping diagnostics and
// failing the test on timeout.
func (c *Cluster) waitNodeReady(n *Node) {
	c.t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), nodeReadyTimeout)
	defer cancel()

	if _, err := n.WaitEvent(ctx, "node.ready"); err != nil {
		c.Dump(c.t)
		c.t.Fatalf("node %d did not become ready: %v", n.Index, err)
	}
}

// stopAll gracefully stops every node, in parallel. Registered as the first
// (innermost-running, since t.Cleanup is LIFO) teardown step.
func (c *Cluster) stopAll() {
	c.mu.Lock()
	nodes := append([]*Node{}, c.nodes...)
	c.mu.Unlock()

	var wg sync.WaitGroup
	for _, n := range nodes {
		if n == nil {
			continue
		}
		wg.Add(1)
		go func(n *Node) {
			defer wg.Done()
			n.Stop()
		}(n)
	}
	wg.Wait()
}

// Node returns a node by index.
func (c *Cluster) Node(i int) *Node {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.nodes[i]
}

// Nodes returns every node started so far.
func (c *Cluster) Nodes() []*Node {
	c.mu.Lock()
	defer c.mu.Unlock()

	return append([]*Node{}, c.nodes...)
}

// Alive returns every currently-alive node.
func (c *Cluster) Alive() []*Node {
	var out []*Node
	for _, n := range c.Nodes() {
		if n != nil && n.Alive() {
			out = append(out, n)
		}
	}

	return out
}

// SystemPod returns the system pod ID every client and daemon in the
// cluster is configured with.
func (c *Cluster) SystemPod() [32]byte { return c.systemPodID }

// Client creates a client.Client connected to node i.
func (c *Cluster) Client(i int) *client.Client {
	c.t.Helper()

	return c.clientFor(c.Node(i))
}

// clientFor creates a client.Client connected to n, failing the test on
// error. Callers that must keep going or degrade gracefully when a node is
// unreachable (fingerprint polling, diagnostics) use newClientFor instead.
func (c *Cluster) clientFor(n *Node) *client.Client {
	c.t.Helper()

	cli, err := c.newClientFor(n)
	if err != nil {
		c.t.Fatalf("client for node %d: %v", n.Index, err)
	}

	return cli
}

// newClientFor creates a client.Client connected to n without failing the
// test on error.
func (c *Cluster) newClientFor(n *Node) (*client.Client, error) {
	return client.NewClient(n.QUICAddr, c.systemPodID)
}

// Daemon creates a daemon.Daemon connected to node i.
func (c *Cluster) Daemon(i int) *daemon.Daemon {
	c.t.Helper()

	n := c.Node(i)

	d, err := daemon.New([]string{n.QUICAddr})
	if err != nil {
		c.t.Fatalf("daemon for node %d: %v", i, err)
	}

	return d
}

// Kill hard-kills node i.
func (c *Cluster) Kill(i int) {
	c.Node(i).Kill()
}

// Restart restarts node i, syncing from the first other alive node, and
// waits for it to become ready again in its new journal segment.
func (c *Cluster) Restart(i int) {
	c.t.Helper()

	n := c.Node(i)
	nextSeg := n.Journal().currentSegment() + 1
	syncFrom := c.firstAliveAddr(i)

	if err := n.Restart(syncFrom); err != nil {
		c.t.Fatalf("restart node %d: %v", i, err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), nodeReadyTimeout)
	defer cancel()

	inNewSegment := func(e Event) bool { return e.Seg >= nextSeg }
	if _, err := n.WaitEvent(ctx, "node.ready", inNewSegment); err != nil {
		c.Dump(c.t)
		c.t.Fatalf("node %d did not become ready after restart: %v", i, err)
	}
}

// Spawn starts a brand-new node that registers and syncs against the
// cluster, waiting for it to report sync.completed before returning.
func (c *Cluster) Spawn() *Node {
	c.t.Helper()

	c.mu.Lock()
	idx := len(c.nodes)
	c.mu.Unlock()

	bootstrapAddr := c.firstAliveAddr(-1)
	if bootstrapAddr == "" {
		c.t.Fatalf("spawn: no alive node to sync from")
	}

	n := c.startOne(idx, false, bootstrapAddr)

	c.mu.Lock()
	c.nodes = append(c.nodes, n)
	c.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), nodeReadyTimeout)
	defer cancel()

	if _, err := n.WaitEvent(ctx, "sync.completed"); err != nil {
		c.Dump(c.t)
		c.t.Fatalf("spawned node %d did not sync: %v", idx, err)
	}

	return n
}

// firstAliveAddr returns the QUIC address of the first alive node other than
// exclude, or "" if none is alive.
func (c *Cluster) firstAliveAddr(exclude int) string {
	for _, n := range c.Nodes() {
		if n == nil || n.Index == exclude || !n.Alive() {
			continue
		}

		return n.QUICAddr
	}

	return ""
}

// WaitAll blocks until every alive node has recorded an event matching name
// and preds, or ctx ends.
func (c *Cluster) WaitAll(ctx context.Context, name string, preds ...Pred) error {
	for _, n := range c.Alive() {
		if _, err := n.WaitEvent(ctx, name, preds...); err != nil {
			return fmt.Errorf("node %d: wait %q:\n%w", n.Index, name, err)
		}
	}

	return nil
}
