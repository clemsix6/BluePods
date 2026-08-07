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
)

const (
	// sequentialStartupMax is the largest cluster size started one node at a
	// time (matching production registration flow); larger clusters start
	// their validators in one parallel batch.
	sequentialStartupMax = 10

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

	clientsMu sync.Mutex             // clientsMu guards clients
	clients   map[int]*client.Client // clients caches one client.Client per node index, keyed by Node.Index
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

// resolveOptions applies opts over size-dependent defaults: minValidators
// defaults to the cluster size, and a large cluster fans gossip out to every
// peer. The transition grace/buffer keep the node defaults — the strict latch
// arms on committed stake, so the bootstrap window no longer needs widening.
func resolveOptions(size int, opts []Option) clusterOpts {
	o := clusterOpts{initialMint: defaultInitialMint}
	for _, opt := range opts {
		opt(&o)
	}

	if o.minValidators == 0 {
		o.minValidators = size
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

	bootstrap := c.startOne(0, true, "", "", false)
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
		// A founding member: joins before the cluster has anything stable to
		// pin a checkpoint to, so it takes the insecure hatch explicitly
		// (never inferred from an absent checkpoint — see startOneErr).
		n := c.startOne(i, false, bootstrapAddr, "", true)
		c.nodes = append(c.nodes, n)
		c.waitNodeReady(n)
	}
}

// startValidatorsParallel starts every validator concurrently, then waits
// for all of them, for clusters large enough that sequential startup would
// be too slow. The spawned goroutines report failures over a channel and
// never call c.t.Fatalf themselves: FailNow (which Fatalf calls) must run on
// the goroutine running the test, never one the test spawns.
func (c *Cluster) startValidatorsParallel(size int, bootstrapAddr string) {
	c.t.Helper()

	c.nodes = append(c.nodes, make([]*Node, size-1)...)

	type result struct {
		idx int
		n   *Node
		err error
	}

	results := make(chan result, size-1)
	for i := 1; i < size; i++ {
		go func(idx int) {
			// Founding members again: see startValidatorsSequential.
			n, err := c.startOneErr(idx, false, bootstrapAddr, "", true)
			results <- result{idx: idx, n: n, err: err}
		}(i)
	}

	for i := 1; i < size; i++ {
		r := <-results
		if r.err != nil {
			c.t.Fatalf("start node %d: %v", r.idx, r.err)
		}
		c.nodes[r.idx] = r.n
	}

	for i := 1; i < size; i++ {
		c.waitNodeReady(c.nodes[i])
	}
}

// startOne allocates a port, creates a node under the cluster's directory,
// and starts it with the cluster's tuning, failing the test on error.
// insecure is explicit at every call site on purpose (see startOneErr): the
// hatch is never taken by a caller merely forgetting to pass a checkpoint.
func (c *Cluster) startOne(index int, isBootstrap bool, bootstrapAddr, checkpoint string, insecure bool) *Node {
	c.t.Helper()

	n, err := c.startOneErr(index, isBootstrap, bootstrapAddr, checkpoint, insecure)
	if err != nil {
		c.t.Fatalf("%v", err)
	}

	return n
}

// startOneErr is startOne's non-fatal core: it never touches *testing.T, so
// it is safe to call from a goroutine startValidatorsParallel spawns (only
// the test's own goroutine may call t.Fatalf/FailNow).
//
// insecure selects the --insecure-bootstrap escape hatch and is always
// passed explicitly by the caller (true only for the cluster's founding
// members, which join before anything stable exists to checkpoint against);
// it used to be inferred from checkpoint == "", which made the hatch a
// silent default for any future caller that forgot to derive a checkpoint.
// A non-empty checkpoint always wins in buildArgs regardless of this flag
// (see node.go), so passing insecure=true alongside a real checkpoint is
// harmless, but every current call site still states its intent plainly.
func (c *Cluster) startOneErr(index int, isBootstrap bool, bootstrapAddr, checkpoint string, insecure bool) (*Node, error) {
	port, err := allocatePort()
	if err != nil {
		return nil, fmt.Errorf("allocate port for node %d:\n%w", index, err)
	}

	dir := filepath.Join(c.dir, fmt.Sprintf("node-%d", index))
	quicAddr := fmt.Sprintf("127.0.0.1:%d", port)

	n, err := newNode(index, dir, c.binaryPath, quicAddr)
	if err != nil {
		return nil, fmt.Errorf("create node %d:\n%w", index, err)
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
		TrustCheckpoint:  checkpoint,

		// Only the cluster's FOUNDING members start unverified, and only
		// because there is nothing stable to pin yet: the genesis committee is
		// refrozen on every committed registration until the strict latch
		// arms, so a checkpoint read before a founder's snapshot is cut is
		// routinely stale by the time that snapshot arrives. Production has the
		// same shape — a founding set is provisioned out of band. Every join
		// AFTER the cluster is up (Spawn, Restart) pins a real checkpoint and
		// takes the verified path. The caller states this explicitly through
		// insecure rather than it being inferred here from an absent
		// checkpoint.
		InsecureBootstrap: insecure,
	}

	if err := n.Start(args); err != nil {
		return nil, fmt.Errorf("start node %d:\n%w", index, err)
	}

	return n, nil
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

// newClientFor returns a client.Client connected to n without failing the
// test on error, creating and caching one the first time n is asked for.
// NewClient pays a connect-and-status round trip; QUICTransport dials fresh
// per RPC rather than holding a persistent connection, and a node's
// QUICAddr never changes across restarts, so one cached client per node
// index is safe to reuse for the cluster's lifetime — sparing repeated
// callers (fingerprint polling chief among them) that round trip on every
// call.
func (c *Cluster) newClientFor(n *Node) (*client.Client, error) {
	c.clientsMu.Lock()
	defer c.clientsMu.Unlock()

	if cli, ok := c.clients[n.Index]; ok {
		return cli, nil
	}

	cli, err := client.NewClient(n.QUICAddr, c.systemPodID)
	if err != nil {
		return nil, err
	}

	if c.clients == nil {
		c.clients = make(map[int]*client.Client)
	}
	c.clients[n.Index] = cli

	return cli, nil
}

// Kill hard-kills node i.
func (c *Cluster) Kill(i int) {
	c.Node(i).Kill()
}

// Restart restarts node i with the same key, data directory and port, and
// waits for it to become ready again in its new journal segment. A node that
// has been running resumes from the committed state in that directory: it
// syncs nothing and verifies nothing, because there is no foreign state to
// judge. The upstream and the checkpoint are still passed (the node binary
// requires an anchor from anything holding a --bootstrap-addr, and a directory
// that turns out to be empty would need one), they simply go unused.
func (c *Cluster) Restart(i int) {
	c.t.Helper()

	n := c.Node(i)
	nextSeg := n.Journal().currentSegment() + 1

	if err := c.restartNode(n); err != nil {
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

// restartNode restarts n with the flags its identity dictates: the cluster's
// bootstrap comes back with --bootstrap and no upstream, every other node with
// the upstream and checkpoint a join would need. Which path the binary then
// takes is ITS decision, read off the data directory — a restart over adopted
// state resumes; only a node holding nothing of its own syncs and verifies.
func (c *Cluster) restartNode(n *Node) error {
	c.t.Helper()

	if n.isBootstrap() {
		return n.Restart("")
	}

	source := c.firstAlive(n.Index)
	if source == nil {
		return fmt.Errorf("no alive node to sync from")
	}

	n.SetTrustCheckpoint(c.trustCheckpointFrom(source))

	return n.Restart(source.QUICAddr)
}

// Spawn starts a brand-new node that registers and syncs against the
// cluster, waiting for it to report sync.completed before returning. The
// newcomer pins a real checkpoint read off the node it syncs from, so
// sync.completed here means the verified path completed — a spawned node that
// cannot prove its snapshot never reaches it.
func (c *Cluster) Spawn() *Node {
	c.t.Helper()

	c.mu.Lock()
	idx := len(c.nodes)
	c.mu.Unlock()

	source := c.firstAlive(-1)
	if source == nil {
		c.t.Fatalf("spawn: no alive node to sync from")
	}

	n := c.startOne(idx, false, source.QUICAddr, c.trustCheckpointFrom(source), false)

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

// SpawnWithCheckpoint starts a brand-new node against the cluster, exactly
// like Spawn, except the caller supplies the checkpoint value directly
// instead of it being derived from an alive node's own published root — the
// hook a scenario needs to exercise the refusal path with a checkpoint that
// names no committee the cluster actually has. Unlike Spawn it does not wait
// for sync.completed: a wrong checkpoint means that event never arrives (the
// node's own fail-closed gate exits it first), so the caller asserts on the
// returned node's journal instead.
func (c *Cluster) SpawnWithCheckpoint(checkpoint string) *Node {
	c.t.Helper()

	c.mu.Lock()
	idx := len(c.nodes)
	c.mu.Unlock()

	source := c.firstAlive(-1)
	if source == nil {
		c.t.Fatalf("spawn: no alive node to sync from")
	}

	n := c.startOne(idx, false, source.QUICAddr, checkpoint, false)

	c.mu.Lock()
	c.nodes = append(c.nodes, n)
	c.mu.Unlock()

	return n
}

// firstAlive returns the first alive node other than exclude, or nil if none
// is alive. It is both the sync source and the checkpoint source for a join:
// the same node answers for the state and for the set that will judge it, and
// a checkpoint read anywhere else in the cluster would be the same value
// anyway (the frozen set is network-wide).
func (c *Cluster) firstAlive(exclude int) *Node {
	for _, n := range c.Nodes() {
		if n == nil || n.Index == exclude || !n.Alive() {
			continue
		}

		return n
	}

	return nil
}

// trustCheckpointFrom reads the checkpoint an operator would publish from a
// running node: the newest epoch.validators.frozen event it emitted, which
// carries the epoch and that epoch's validator-set root. Newest, not first:
// the root is republished on every freeze, and an older one names a committee
// the joiner may no longer hold.
func (c *Cluster) trustCheckpointFrom(source *Node) string {
	c.t.Helper()

	published := source.Journal().Events("epoch.validators.frozen")
	if len(published) == 0 {
		c.Dump(c.t)
		c.t.Fatalf("node %d published no epoch.validators.frozen event: nothing to pin a verified join to", source.Index)
	}

	last := published[len(published)-1]

	epoch, ok := toFloat64(last.Attrs["epoch"])
	root, isString := last.Attrs["root"].(string)
	if !ok || !isString || root == "" {
		c.t.Fatalf("node %d published a malformed checkpoint event: %v", source.Index, last.Attrs)
	}

	return fmt.Sprintf("%d:%s", uint64(epoch), root)
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
