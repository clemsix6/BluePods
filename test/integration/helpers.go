package integration

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"BluePods/client"
)

// safeBuffer wraps bytes.Buffer with a mutex for concurrent read/write.
type safeBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

// Write appends data to the buffer (implements io.Writer).
func (sb *safeBuffer) Write(p []byte) (int, error) {
	sb.mu.Lock()
	defer sb.mu.Unlock()

	return sb.buf.Write(p)
}

// String returns the buffer contents as a string.
func (sb *safeBuffer) String() string {
	sb.mu.Lock()
	defer sb.mu.Unlock()

	return sb.buf.String()
}

// Node represents a running BluePods node process.
type Node struct {
	index    int               // index is the node's position in the cluster
	cmd      *exec.Cmd         // cmd is the running process
	httpAddr string            // httpAddr is the HTTP API address
	quicAddr string            // quicAddr is the QUIC network address
	dataDir  string            // dataDir is the node's data directory
	keyPath  string            // keyPath is the node's private key file
	stdout   *safeBuffer       // stdout captures process output
	stderr   *safeBuffer       // stderr captures process errors
	cancel   context.CancelFunc // cancel stops the process
}

// HTTPAddr returns the node's HTTP address.
func (n *Node) HTTPAddr() string { return n.httpAddr }

// IsRunning checks if the node process is alive and started successfully.
func (n *Node) IsRunning() bool {
	if n.cmd == nil || n.cmd.Process == nil {
		return false
	}

	if !strings.Contains(n.stdout.String(), "starting BluePods node") {
		return false
	}

	// Check if process exited (ProcessState is set after waitDone goroutine runs)
	if n.cmd.ProcessState != nil {
		return false
	}

	return true
}

// Logs returns the node's stdout output.
func (n *Node) Logs() string { return n.stdout.String() }

// LogContains checks if the node's logs contain a substring.
func (n *Node) LogContains(s string) bool {
	return strings.Contains(n.stdout.String(), s)
}

// LogCount counts occurrences of a substring in the node's logs.
func (n *Node) LogCount(s string) int {
	return strings.Count(n.stdout.String(), s)
}

// Stop terminates the node process.
func (n *Node) Stop() {
	if n.cancel != nil {
		n.cancel()
	}

	if n.cmd != nil && n.cmd.Process != nil {
		n.cmd.Process.Kill()
		// Wait is already called by the background goroutine in startNode.
		// Give it a moment to complete after kill.
		time.Sleep(100 * time.Millisecond)
	}
}

// KeyPath returns the path to the node's private key file.
func (n *Node) KeyPath() string { return n.keyPath }

// clusterOpts holds configuration for a Cluster.
type clusterOpts struct {
	httpBase      int    // httpBase is the starting HTTP port
	quicBase      int    // quicBase is the starting QUIC port
	minValidators int    // minValidators is the threshold for consensus
	epochLength   uint64 // epochLength is rounds per epoch (0 = disabled)
	maxChurn      int    // maxChurn is max validator changes per epoch
	syncBuffer    int    // syncBuffer is the sync buffer in seconds
	initialMint   uint64 // initialMint is the bootstrap mint amount
	gossipFanout     int // gossipFanout is the number of peers per vertex gossip (0 = default 40)
	transitionGrace  int // transitionGrace is the transition grace rounds (0 = default 20)
	transitionBuffer int // transitionBuffer is the transition buffer rounds (0 = default 10)
}

// ClusterOption configures cluster behavior.
type ClusterOption func(*clusterOpts)

// WithHTTPBase sets the starting HTTP port.
func WithHTTPBase(port int) ClusterOption { return func(o *clusterOpts) { o.httpBase = port } }

// WithQUICBase sets the starting QUIC port.
func WithQUICBase(port int) ClusterOption { return func(o *clusterOpts) { o.quicBase = port } }

// WithMinValidators sets the consensus threshold.
func WithMinValidators(n int) ClusterOption { return func(o *clusterOpts) { o.minValidators = n } }

// WithEpochLength sets the rounds per epoch.
func WithEpochLength(n uint64) ClusterOption { return func(o *clusterOpts) { o.epochLength = n } }

// WithMaxChurn sets the max validator changes per epoch.
func WithMaxChurn(n int) ClusterOption { return func(o *clusterOpts) { o.maxChurn = n } }

// WithSyncBuffer sets the sync buffer in seconds.
func WithSyncBuffer(n int) ClusterOption { return func(o *clusterOpts) { o.syncBuffer = n } }

// WithInitialMint sets the bootstrap mint amount.
func WithInitialMint(n uint64) ClusterOption { return func(o *clusterOpts) { o.initialMint = n } }

// WithGossipFanout sets the gossip fanout (peers per vertex).
// Set to cluster size or higher for 100% delivery in tests.
func WithGossipFanout(n int) ClusterOption { return func(o *clusterOpts) { o.gossipFanout = n } }

// WithTransitionGrace sets the grace rounds after minValidators is reached.
func WithTransitionGrace(n int) ClusterOption { return func(o *clusterOpts) { o.transitionGrace = n } }

// WithTransitionBuffer sets the buffer rounds after the grace period.
func WithTransitionBuffer(n int) ClusterOption {
	return func(o *clusterOpts) { o.transitionBuffer = n }
}

// Cluster manages a group of nodes for a simulation.
type Cluster struct {
	t          *testing.T  // t is the test context
	nodes      []*Node     // nodes is the list of running nodes
	binaryPath string      // binaryPath is the compiled node binary
	systemPod  string      // systemPod is the path to the system pod WASM
	testDir    string      // testDir is the temporary directory for node data
	opts       clusterOpts // opts is the cluster configuration
}

// NewCluster builds the binary, starts N nodes, and registers cleanup.
func NewCluster(t *testing.T, size int, options ...ClusterOption) *Cluster {
	t.Helper()

	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	opts := clusterOpts{
		httpBase:   18000,
		quicBase:   19000,
		syncBuffer: 12,
	}
	for _, o := range options {
		o(&opts)
	}

	if opts.minValidators == 0 {
		opts.minValidators = size
	}

	// For larger clusters with parallel startup, extend the transition period
	// to prevent quorum deadlock. During parallel startup, nodes register at
	// different times and need extra rounds to synchronize before strict BFT
	// quorum kicks in. Grace=100 + Buffer=100 gives ~100 seconds of relaxed
	// quorum at ~2 rounds/sec, plenty of time for all nodes to converge.
	if size >= 10 && opts.transitionGrace == 0 {
		opts.transitionGrace = 100
	}
	if size >= 10 && opts.transitionBuffer == 0 {
		opts.transitionBuffer = 100
	}

	c := &Cluster{
		t:          t,
		binaryPath: buildBinary(t),
		systemPod:  findSystemPod(t),
		opts:       opts,
	}

	testDir, err := os.MkdirTemp("", "bluepods_sim_*")
	if err != nil {
		t.Fatalf("create temp dir: %v", err)
	}

	c.testDir = testDir
	t.Cleanup(func() { os.RemoveAll(testDir) })

	c.startNodes(size)
	t.Cleanup(func() { c.Stop() })

	return c
}

// startNodes starts the bootstrap and validator nodes.
func (c *Cluster) startNodes(size int) {
	c.t.Helper()
	c.nodes = make([]*Node, size)

	// Start bootstrap node
	c.nodes[0] = c.startNode(0, "", true)
	time.Sleep(4 * time.Second)

	if !c.nodes[0].IsRunning() {
		c.t.Fatalf("bootstrap failed:\nSTDOUT:\n%s\nSTDERR:\n%s",
			c.nodes[0].stdout.String(), c.nodes[0].stderr.String())
	}

	if size == 1 {
		return
	}

	c.startValidatorNodes(size)
}

// startValidatorNodes starts nodes 1..N-1.
// Small clusters (<=5) use sequential startup (matching production node registration flow).
// Larger clusters use parallel startup to avoid transition deadlocks.
func (c *Cluster) startValidatorNodes(size int) {
	c.t.Helper()

	bootstrapQUIC := c.nodes[0].quicAddr

	if size <= 5 {
		c.startValidatorsSequential(size, bootstrapQUIC)
	} else {
		c.startValidatorsParallel(size, bootstrapQUIC)
	}
}

// startValidatorsSequential starts validators one at a time with sync waits.
// Uses syncBuffer + 6s between nodes to ensure each node syncs and registers
// before the next starts. This matches production deployment flow.
func (c *Cluster) startValidatorsSequential(size int, bootstrapQUIC string) {
	c.t.Helper()

	// Wait syncBuffer + 6s per node (old test used 18s with default syncBuffer=12s)
	syncWait := time.Duration(c.opts.syncBuffer+6) * time.Second
	if syncWait < 18*time.Second {
		syncWait = 18 * time.Second
	}

	for i := 1; i < size; i++ {
		c.nodes[i] = c.startNode(i, bootstrapQUIC, false)

		c.t.Logf("Waiting for node %d to sync (%v)...", i, syncWait)
		time.Sleep(syncWait)

		if !c.nodes[i].IsRunning() {
			c.t.Fatalf("Node %d failed to start:\nSTDERR:\n%s",
				i, c.nodes[i].stderr.String())
		}
	}

	// Wait for network stabilization
	c.t.Logf("Waiting for stabilization (15s)...")
	time.Sleep(15 * time.Second)
}

// startValidatorsParallel starts all validators in parallel for large clusters.
// Relies on gossipFanout being set to cluster size (or higher) in tests to
// guarantee 100% vertex delivery from the bootstrap producer.
func (c *Cluster) startValidatorsParallel(size int, bootstrapQUIC string) {
	c.t.Helper()
	c.startBatch(1, size-1, bootstrapQUIC, 1)
}

// startBatch starts nodes from..to in parallel and waits for sync.
func (c *Cluster) startBatch(from, to int, bootstrapQUIC string, phase int) {
	c.t.Helper()

	count := to - from + 1
	var wg sync.WaitGroup

	for i := from; i <= to; i++ {
		wg.Add(1)

		go func(idx int) {
			defer wg.Done()
			c.nodes[idx] = c.startNode(idx, bootstrapQUIC, false)
		}(i)
	}

	wg.Wait()

	syncWait := time.Duration(c.opts.syncBuffer+8) * time.Second
	if syncWait < 20*time.Second {
		syncWait = 20 * time.Second
	}

	c.t.Logf("Phase %d: %d nodes started (%d-%d), waiting for sync (%v)...",
		phase, count, from, to, syncWait)
	time.Sleep(syncWait)

	c.verifyNodesRunning(from, to)
}

// verifyNodesRunning checks that nodes in the given range are alive.
func (c *Cluster) verifyNodesRunning(from, to int) {
	c.t.Helper()

	for i := from; i <= to; i++ {
		if c.nodes[i] == nil || !c.nodes[i].IsRunning() {
			stderr := ""
			if c.nodes[i] != nil {
				stderr = c.nodes[i].stderr.String()
			}
			c.t.Fatalf("Node %d failed to start:\nSTDERR:\n%s", i, stderr)
		}
	}
}

// startNode starts a single node process.
func (c *Cluster) startNode(index int, bootstrapQUIC string, isBootstrap bool) *Node {
	c.t.Helper()

	node := &Node{
		index:    index,
		httpAddr: fmt.Sprintf("127.0.0.1:%d", c.opts.httpBase+index),
		quicAddr: fmt.Sprintf("127.0.0.1:%d", c.opts.quicBase+index),
		dataDir:  filepath.Join(c.testDir, fmt.Sprintf("node-%d", index)),
		stdout:   &safeBuffer{},
		stderr:   &safeBuffer{},
	}
	node.keyPath = filepath.Join(node.dataDir, "key")

	if err := os.MkdirAll(node.dataDir, 0755); err != nil {
		c.t.Fatalf("create node dir %d: %v", index, err)
	}

	args := c.buildNodeArgs(node, bootstrapQUIC, isBootstrap)

	ctx, cancel := context.WithCancel(context.Background())
	node.cancel = cancel

	node.cmd = exec.CommandContext(ctx, c.binaryPath, args...)
	node.cmd.Stdout = node.stdout
	node.cmd.Stderr = node.stderr

	if err := node.cmd.Start(); err != nil {
		c.t.Fatalf("start node %d: %v", index, err)
	}

	// Wait in background so ProcessState gets set when the process exits.
	go node.cmd.Wait()

	return node
}

// buildNodeArgs constructs command-line arguments for a node.
func (c *Cluster) buildNodeArgs(node *Node, bootstrapQUIC string, isBootstrap bool) []string {
	args := []string{
		"--data", node.dataDir,
		"--http", node.httpAddr,
		"--quic", node.quicAddr,
		"--key", node.keyPath,
		"--system-pod", c.systemPod,
		"--min-validators", fmt.Sprintf("%d", c.opts.minValidators),
		"--sync-buffer", fmt.Sprintf("%d", c.opts.syncBuffer),
	}

	if c.opts.epochLength > 0 {
		args = append(args, "--epoch-length", fmt.Sprintf("%d", c.opts.epochLength))
	}

	if c.opts.maxChurn > 0 {
		args = append(args, "--max-churn", fmt.Sprintf("%d", c.opts.maxChurn))
	}

	if c.opts.gossipFanout > 0 {
		args = append(args, "--gossip-fanout", fmt.Sprintf("%d", c.opts.gossipFanout))
	}

	if c.opts.transitionGrace > 0 {
		args = append(args, "--transition-grace", fmt.Sprintf("%d", c.opts.transitionGrace))
	}

	if c.opts.transitionBuffer > 0 {
		args = append(args, "--transition-buffer", fmt.Sprintf("%d", c.opts.transitionBuffer))
	}

	if c.opts.initialMint > 0 {
		args = append(args, "--initial-mint", fmt.Sprintf("%d", c.opts.initialMint))
	}

	if isBootstrap {
		args = append(args, "--bootstrap")
	} else {
		args = append(args, "--bootstrap-addr", bootstrapQUIC)
	}

	return args
}

// Stop kills all nodes in parallel.
func (c *Cluster) Stop() {
	var wg sync.WaitGroup

	for _, node := range c.nodes {
		if node == nil {
			continue
		}

		wg.Add(1)

		go func(n *Node) {
			defer wg.Done()
			n.Stop()
		}(node)
	}

	wg.Wait()
}

// Node returns a node by index.
func (c *Cluster) Node(i int) *Node { return c.nodes[i] }

// Bootstrap returns the bootstrap node (node 0).
func (c *Cluster) Bootstrap() *Node { return c.nodes[0] }

// Size returns the number of nodes.
func (c *Cluster) Size() int { return len(c.nodes) }

// Nodes returns all nodes.
func (c *Cluster) Nodes() []*Node { return c.nodes }

// Client creates a client.Client connected to a node.
func (c *Cluster) Client(nodeIndex int) *client.Client {
	c.t.Helper()

	cli, err := client.NewClient(c.nodes[nodeIndex].httpAddr)
	if err != nil {
		c.t.Fatalf("create client for node %d: %v", nodeIndex, err)
	}

	return cli
}

// AddNodes starts additional validator nodes and appends them to the cluster.
func (c *Cluster) AddNodes(count int) []*Node {
	c.t.Helper()

	bootstrapQUIC := c.nodes[0].quicAddr
	newNodes := make([]*Node, count)

	for i := 0; i < count; i++ {
		idx := len(c.nodes) + i
		newNodes[i] = c.startNode(idx, bootstrapQUIC, false)
	}

	c.nodes = append(c.nodes, newNodes...)

	return newNodes
}

// WaitReady polls until all nodes see the full cluster validator count,
// waits for round convergence, then submits a warm-up faucet.
func (c *Cluster) WaitReady(timeout time.Duration) {
	c.t.Helper()
	c.WaitForValidators(len(c.nodes), timeout)

	// Wait for nodes to converge on similar rounds before running tests.
	// During init phase, bootstrap runs far ahead; other nodes catch up
	// by replaying committed rounds from received vertices.
	if len(c.nodes) > 1 {
		c.waitForRoundConvergence(60 * time.Second)
		c.warmupNetwork()
	}
}

// waitForRoundConvergence polls until all sampled nodes are within 10 rounds
// of each other. This ensures the cluster is synchronized before tests start.
func (c *Cluster) waitForRoundConvergence(timeout time.Duration) {
	c.t.Helper()

	checkNodes := c.selectCheckNodes()
	deadline := time.Now().Add(timeout)

	for time.Now().Before(deadline) {
		minRound := uint64(1<<63 - 1)
		maxRound := uint64(0)
		allResponded := true

		for _, i := range checkNodes {
			status := QueryStatusSafe(c.nodes[i].httpAddr)
			if status == nil {
				allResponded = false
				break
			}

			if status.Round < minRound {
				minRound = status.Round
			}
			if status.Round > maxRound {
				maxRound = status.Round
			}
		}

		if allResponded && maxRound-minRound <= 10 {
			c.t.Logf("Round convergence: [%d, %d]", minRound, maxRound)
			return
		}

		time.Sleep(2 * time.Second)
	}

	c.t.Logf("Warning: round convergence not reached within %v, continuing", timeout)
}

// warmupNetwork submits a throwaway faucet tx and waits for it to commit.
// This primes the consensus commit pipeline after the transition period.
func (c *Cluster) warmupNetwork() {
	c.t.Helper()

	cli := c.Client(0)
	w := client.NewWallet()
	pk := w.Pubkey()

	coinID, err := cli.Faucet(pk, 1)
	if err != nil {
		c.t.Logf("warmup faucet failed: %v (continuing)", err)
		return
	}

	// Wait up to 60s for the warmup tx to commit
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		_, err := cli.GetObject(coinID)
		if err == nil {
			c.t.Logf("Warmup tx committed")
			return
		}
		time.Sleep(time.Second)
	}

	c.t.Logf("warmup tx did not commit within 60s (continuing)")
}

// WaitForValidators polls /status until ALL nodes see the expected validator count.
// For small clusters, all nodes are checked. For large clusters (>10), a representative
// sample of nodes is checked to avoid excessive HTTP requests.
func (c *Cluster) WaitForValidators(expected int, timeout time.Duration) {
	c.t.Helper()

	// Determine which nodes to check
	checkNodes := c.selectCheckNodes()
	deadline := time.Now().Add(timeout)
	lastLog := time.Time{}

	for time.Now().Before(deadline) {
		converged := c.countConvergedNodes(checkNodes, expected)

		if converged == len(checkNodes) {
			c.t.Logf("Validators reached: %d (expected %d)", expected, expected)
			return
		}

		// Log progress every 10s
		if time.Since(lastLog) >= 10*time.Second {
			c.t.Logf("WaitForValidators: %d/%d checked nodes converged (expected %d validators)",
				converged, len(checkNodes), expected)
			lastLog = time.Now()
		}

		time.Sleep(2 * time.Second)
	}

	c.logNodeStates()
	c.t.Fatalf("timeout waiting for %d validators on all nodes", expected)
}

// selectCheckNodes picks which nodes to monitor during convergence.
// For small clusters, all nodes. For large clusters, node 0 + evenly spaced sample.
func (c *Cluster) selectCheckNodes() []int {
	if len(c.nodes) <= 10 {
		indices := make([]int, len(c.nodes))
		for i := range c.nodes {
			indices[i] = i
		}
		return indices
	}

	// For large clusters: node 0 + every 5th node + last node
	indices := []int{0}
	for i := 5; i < len(c.nodes); i += 5 {
		indices = append(indices, i)
	}

	last := len(c.nodes) - 1
	if indices[len(indices)-1] != last {
		indices = append(indices, last)
	}

	return indices
}

// countConvergedNodes counts how many of the given nodes see >= expected validators.
func (c *Cluster) countConvergedNodes(indices []int, expected int) int {
	converged := 0

	for _, i := range indices {
		node := c.nodes[i]
		if node == nil {
			continue
		}

		status := QueryStatusSafe(node.httpAddr)
		if status != nil && status.Validators >= expected {
			converged++
		}
	}

	return converged
}

// logNodeStates logs the status of all nodes for debugging convergence failures.
func (c *Cluster) logNodeStates() {
	for i, node := range c.nodes {
		if node == nil {
			continue
		}

		status := QueryStatusSafe(node.httpAddr)
		if status != nil {
			c.t.Logf("Node %d: round=%d validators=%d", i, status.Round, status.Validators)
		} else {
			c.t.Logf("Node %d: running=%v, no status response", i, node.IsRunning())
		}
	}
}

// WaitForRound polls /status until the target round is reached.
func (c *Cluster) WaitForRound(round uint64, timeout time.Duration) {
	c.t.Helper()

	deadline := time.Now().Add(timeout)

	for time.Now().Before(deadline) {
		status := QueryStatusSafe(c.nodes[0].httpAddr)

		if status != nil && status.Round >= round {
			c.t.Logf("Round reached: %d (target %d)", status.Round, round)
			return
		}

		time.Sleep(2 * time.Second)
	}

	c.t.Fatalf("timeout waiting for round %d", round)
}

// WaitForEpoch polls /status until the target epoch is reached.
func (c *Cluster) WaitForEpoch(epoch uint64, timeout time.Duration) {
	c.t.Helper()

	deadline := time.Now().Add(timeout)

	for time.Now().Before(deadline) {
		status := QueryStatusSafe(c.nodes[0].httpAddr)

		if status != nil && status.Epoch >= epoch {
			c.t.Logf("Epoch reached: %d (target %d)", status.Epoch, epoch)
			return
		}

		time.Sleep(2 * time.Second)
	}

	c.t.Fatalf("timeout waiting for epoch %d", epoch)
}

// buildBinary compiles the node binary.
// Uses a unique temp file per test to avoid races when running simulations in parallel.
func buildBinary(t *testing.T) string {
	t.Helper()

	tmpFile, err := os.CreateTemp("", "bluepods_test_*")
	if err != nil {
		t.Fatalf("create temp binary file: %v", err)
	}

	binary := tmpFile.Name()
	tmpFile.Close()

	cmd := exec.Command("go", "build", "-o", binary, "./cmd/node")
	cmd.Dir = getProjectRoot(t)

	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("build failed: %v\n%s", err, output)
	}

	t.Cleanup(func() { os.Remove(binary) })

	return binary
}

// findSystemPod locates the system pod WASM file.
func findSystemPod(t *testing.T) string {
	t.Helper()

	root := getProjectRoot(t)
	path := filepath.Join(root, "pods", "pod-system", "build", "pod.wasm")

	if _, err := os.Stat(path); err != nil {
		t.Fatalf("system pod not found at %s: %v\nRun: cd pods/pod-system && make", path, err)
	}

	return path
}

// getProjectRoot returns the project root directory (containing go.mod).
func getProjectRoot(t *testing.T) string {
	t.Helper()

	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("get working dir: %v", err)
	}

	dir := wd
	for i := 0; i < 5; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}

		dir = filepath.Dir(dir)
	}

	t.Fatalf("could not find project root from %s", wd)

	return ""
}
