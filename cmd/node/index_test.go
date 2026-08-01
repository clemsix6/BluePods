package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"testing"

	"BluePods/internal/consensus"
	"BluePods/internal/network"
	"BluePods/internal/state"
	"BluePods/internal/storage"
)

// TestInitIndex_RestartRebuildMatchesNeverRestarted simulates a node restart:
// session 1 seeds genesis (tracking the reserve coin) through the index wired
// at construction, then tracks one more object through the same
// DAG.TrackObject -> indexer.ApplyEdge hook, capturing the resulting root
// without ever restarting. Session 2 reopens the same data directory fresh;
// its construction-time backfill, reading only what session 1 persisted to
// Pebble, must reproduce the identical combined root — otherwise a restarted
// node anchors a wrong root and is silently excluded by its peers.
func TestInitIndex_RestartRebuildMatchesNeverRestarted(t *testing.T) {
	dir := t.TempDir()
	_, privKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	// Session 1: seed genesis, then track one more object so both the genesis
	// feed and a live ApplyEdge contribute to the root session 2 must
	// reproduce. The direct TrackObject runs with the loops stopped: on the
	// commit path every index feed is serialized by the commit loop itself, and
	// a test calling it while that loop records frontiers would be the one
	// unsynchronized writer.
	n1, db1 := bootstrapTestNode(t, dir, privKey)
	n1.seedGenesisState()
	n1.dag.Close()

	owner := deriveOwner(privKey)
	var extra consensus.Hash
	extra[0] = 0xAB
	n1.dag.TrackObject(extra, 1, 0, 0, 0, owner)

	wantRoot := n1.idxManager.Root()
	if wantRoot == ([32]byte{}) {
		t.Fatal("test misconfigured: session 1's root is the empty root")
	}

	if err := db1.Close(); err != nil {
		t.Fatalf("close first session storage: %v", err)
	}

	// Session 2: fresh storage/state/dag over the same data directory. No live
	// TrackObject call — the root depends entirely on the construction backfill.
	n2, db2 := bootstrapTestNode(t, dir, privKey)
	defer db2.Close()
	defer n2.dag.Close()

	n2.seedGenesisState() // guarded re-seed: restores the founder, does not re-track

	if got := n2.idxManager.Root(); got != wantRoot {
		t.Errorf("restarted index root = %x, want %x (session 1, never restarted)", got[:4], wantRoot[:4])
	}
}

// TestInitIndex_BootSeedDoesNotStealCursorRound is the regression for the
// boot-frontier seed colliding with SetFrontier's non-advancing guard. The
// commit cursor is the NEXT round to decide, not the last committed
// (advanceCommitCursor sets it to round+1), so seeding the boot frontier AT
// the cursor records a pre-batch root under the cursor round's key; the
// resumed commit loop's own SetFrontier for that round is then ignored by
// the guard, and the restarted node serves a RootAt(cursor) a
// never-restarted twin — which recorded that round's root AFTER its batch
// applied — disagrees with: an anchored fork. The seed must target cursor-1,
// the round the backfilled state actually corresponds to. The cursor round's
// decision is driven synchronously here (commit loop stopped) so the test is
// deterministic.
func TestInitIndex_BootSeedDoesNotStealCursorRound(t *testing.T) {
	dir := t.TempDir()
	_, privKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	// Session 1: seed genesis and let the live commit loop decide at least
	// one round, so the restart below restores a nonzero commit cursor.
	n1, db1 := bootstrapTestNode(t, dir, privKey)
	n1.seedGenesisState()
	waitForCommit(t, n1.dag, n1.dag.LastCommittedRound(), 1)
	n1.dag.Close()
	if err := db1.Close(); err != nil {
		t.Fatalf("close first session storage: %v", err)
	}

	// Session 2: reopen the same data directory — the backfill and its frontier
	// seed run inside New — then STOP the commit loop immediately so the cursor
	// round's decision below is this test's own synchronous call, never raced by
	// the background ticker.
	n2, db2 := bootstrapTestNode(t, dir, privKey)
	defer db2.Close()
	n2.dag.Close()

	cursor := n2.dag.LastCommittedRound() // next round to decide: 0..cursor-1 are done
	if cursor == 0 {
		t.Fatal("test misconfigured: session 1 persisted no decided round")
	}

	n2.seedGenesisState()
	bootRoot := n2.idxManager.Root()

	// A mutation lands in the cursor round's batch: committed state changes
	// between the boot backfill and the cursor round's own frontier record.
	var extra consensus.Hash
	extra[0] = 0xEE
	n2.dag.TrackObject(extra, 1, 0, 0, 0, deriveOwner(privKey))

	// The commit loop decides the cursor round: commitNextRound ends with
	// setIndexFrontier(round), which is exactly this call.
	n2.idxManager.SetFrontier(cursor)

	// A never-restarted twin fed the same committed stream holds the
	// post-batch root under the cursor round's key; Root() is a pure
	// function of fed state, so the manager's current root IS that value.
	wantRoot := n2.idxManager.Root()
	if wantRoot == bootRoot {
		t.Fatal("test misconfigured: the cursor-round mutation did not change the root")
	}

	got, ok := n2.idxManager.RootAt(cursor)
	if !ok {
		t.Fatalf("RootAt(%d) unavailable after the cursor round was decided", cursor)
	}
	if got != wantRoot {
		t.Errorf("RootAt(%d) = %x, want %x: the boot seed stole the cursor round's key, anchoring a pre-batch root a never-restarted twin disagrees with", cursor, got[:4], wantRoot[:4])
	}

	// The seeded round itself: the backfilled boot state is the state after
	// round cursor-1, so that is the key it must be recorded under.
	if seeded, ok := n2.idxManager.RootAt(cursor - 1); !ok || seeded != bootRoot {
		t.Errorf("RootAt(%d) ok=%v root=%x, want the boot backfill root %x under the last DECIDED round", cursor-1, ok, seeded[:4], bootRoot[:4])
	}
}

// syncDomains are the leases the live node registers before its snapshot is
// cut: the DOMAIN leg of the twin below. Owner and expiry are both hashed into
// the domain tree's leaves, so a rebuild that dropped either computes a root no
// live node agrees with.
var syncDomains = []state.DomainEntry{
	{Name: "alice.bp", ObjectID: [32]byte{0xD1}, Owner: [32]byte{0xA0, 0x01}, ExpiryEpoch: 17},
	{Name: "bob.bp", ObjectID: [32]byte{0xD2}, Owner: [32]byte{0xB0, 0x02}, ExpiryEpoch: 42},
	{Name: "carol.bp", ObjectID: [32]byte{0xD3}, Owner: [32]byte{0xC0, 0x03}, ExpiryEpoch: 9},
}

// syncSource is one live node's snapshot cut: the node that produced it, the
// result a joiner applies, the committed frontier that state describes, and
// the root the live node holds over it. Tests that only rebuild use result,
// frontier and root; tests that must contrast the joiner with the source it
// synced from (the trusted-checkpoint gate) also need live.
type syncSource struct {
	live     *Node           // live is the node the snapshot was cut from, its DAG stopped
	result   *snapshotResult // result is what requestAndApplySnapshot would hand the joiner
	frontier uint64          // frontier is the last decided round the cut state describes
	root     [32]byte        // root is the live node's combined index root over that state
}

// syncSnapshotFromLiveNode runs a bootstrap node through a short history —
// genesis seeded, rounds decided by the real commit loop, then further
// committed activity (one tracked object and three registered domain leases) —
// and returns the snapshot a joining node would be served from it, together
// with that node's own committed frontier and the root it holds over that
// state. A joiner rebuilding from this snapshot must reproduce the identical
// pair: that is what makes its vertices verifiable by every peer that followed
// the same history.
//
// The post-genesis activity is applied with the DAG stopped. Feeding it while
// the commit loop runs would put a second writer on the index trees, which
// every feed point on the commit path is serialized against — the loop is the
// only writer in production, and a test that broke that would trip -race
// rather than prove anything about rebuilds.
func syncSnapshotFromLiveNode(t *testing.T) syncSource {
	t.Helper()

	dir := t.TempDir()
	_, privKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	live, db := bootstrapTestNode(t, dir, privKey)
	t.Cleanup(func() { db.Close() })

	live.seedGenesisState()

	waitForCommit(t, live.dag, live.dag.LastCommittedRound(), 1)
	live.dag.Close()

	// Post-genesis committed activity, so the exported state is more than the
	// genesis seed: a real hierarchy AND a populated domain registry for the
	// joiner to rebuild.
	var extra consensus.Hash
	extra[0] = 0xAB
	live.dag.TrackObject(extra, 1, 0, 0, 0, deriveOwner(privKey))

	for _, d := range syncDomains {
		// Exactly what the commit path's writeDomainLeaf does for every
		// registration, renewal, repoint and transfer: the registry and the
		// authenticated tree, in lockstep.
		live.state.SetDomainLeaf(d.Name, d.ObjectID, d.Owner, d.ExpiryEpoch)
		live.idxManager.ApplyDomain(d.Name, d.ObjectID, d.Owner, d.ExpiryEpoch)
	}

	cut := live.dag.ExportConsistentCut(100)
	defer cut.DBSnapshot.Close()

	if cut.Cursor == 0 {
		t.Fatal("test misconfigured: the live node decided no round")
	}

	// The last DECIDED round: what internal/sync exports as a snapshot's
	// lastCommittedRound (cursor-1), and the round a rebuild over the exported
	// state anchors itself at.
	frontier := cut.Cursor - 1

	root := live.idxManager.Root()
	if root == ([32]byte{}) {
		t.Fatal("test misconfigured: the live node anchors the empty root")
	}

	domains := live.state.ExportDomains()
	if len(domains) != len(syncDomains) {
		t.Fatalf("test misconfigured: exported %d domains, want %d", len(domains), len(syncDomains))
	}

	result := &snapshotResult{
		lastCommittedRound: frontier,
		validators:         cut.Validators,
		vertices:           cut.Vertices,
		trackerEntries:     cut.TrackerEntries,
		domainEntries:      domains,
		issuanceRateMicro:  cut.IssuanceRate,
		regimeState:        cut.Regime,
	}

	return syncSource{live: live, result: result, frontier: frontier, root: root}
}

// syncedJoiner builds the cmd/node-level shape of a node that joins by sync:
// its own storage, network handle and state, with the snapshot's domains
// imported, and NO pre-existing DAG or index — NewNode skips initConsensus
// entirely whenever BootstrapAddr is set, so everything the joiner runs on is
// built by the sync-side construction paths.
func syncedJoiner(t *testing.T, result *snapshotResult) *Node {
	t.Helper()

	db, err := storage.New(t.TempDir())
	if err != nil {
		t.Fatalf("storage: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	_, privKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	net, err := network.NewNode(network.Config{PrivateKey: privKey, ListenAddr: "127.0.0.1:0"})
	if err != nil {
		t.Fatalf("network: %v", err)
	}
	t.Cleanup(func() { net.Close() })

	n := &Node{
		cfg: &Config{
			PrivateKey:    privKey,
			BootstrapAddr: "127.0.0.1:1",
			MinValidators: 1,
		},
		storage:   db,
		network:   net,
		systemPod: consensus.Hash{0x01, 0x02, 0x03},
	}

	// performSync's own order: the snapshot's domains land in state through the
	// handle that applied the snapshot, and initState then rebuilds the handle
	// over the same Pebble directory before consensus is constructed.
	if err := n.initState(); err != nil {
		t.Fatalf("init state: %v", err)
	}

	if len(result.domainEntries) > 0 {
		n.state.ImportDomains(result.domainEntries)
	}

	if err := n.initState(); err != nil {
		t.Fatalf("init state: %v", err)
	}

	return n
}

// assertSyncedIndex checks that a node built through a sync-side construction
// path carries an index anchoring the same (frontier, root) pair a node that
// followed the same history anchors over the same committed state.
func assertSyncedIndex(t *testing.T, n *Node, frontier uint64, wantRoot [32]byte) {
	t.Helper()

	if n.idxManager == nil {
		t.Fatal("a node built through the sync path has no index: it anchors (0, zero root) in every vertex it produces, which every indexed peer rejects once the vertex round is past the first epoch boundary")
	}

	// The rebuilt trees themselves: tracker hierarchy, domain leases with their
	// owners and expiries, and the validator snapshot, combined.
	if got := n.idxManager.Root(); got != wantRoot {
		t.Errorf("synced index root = %x, want %x (the root over the same committed state)", got[:4], wantRoot[:4])
	}

	// What the joiner anchors in the vertices it produces. The round may have
	// moved on if its own commit loop decided one before it was stopped, but a
	// zero root, or one below the snapshot's frontier, never becomes correct.
	gotRound, gotRoot := n.idxManager.CommittedFrontier()
	if gotRound < frontier || gotRoot == ([32]byte{}) {
		t.Errorf("synced CommittedFrontier() = (%d, %x), want the snapshot's frontier %d or later carrying a real root", gotRound, gotRoot[:4], frontier)
	}

	// The pair itself: wantRoot is not a live node's own RootAt(frontier) —
	// the fixture applies its post-snapshot writes (the extra tracked object,
	// the domain leases) directly to the trees after the live node's commit
	// loop already stopped, so those never advance any recorded frontier, and
	// the live node's own RootAt(frontier) still holds whatever root was
	// current when frontier was actually decided. wantRoot is instead
	// live.idxManager.Root() read AFTER those writes, i.e. the root the
	// exported cut describes. What this checks is that the joiner, rebuilding
	// solely from that cut, reproduces the identical root under the
	// frontier's history entry. A seed on the wrong round leaves this round
	// unrecorded, so !ok fails here too.
	if got, ok := n.idxManager.RootAt(frontier); !ok || got != wantRoot {
		t.Errorf("synced RootAt(%d) ok=%v root=%x, want %x", frontier, ok, got[:4], wantRoot[:4])
	}
}

// TestInitConsensusForValidator_RebuildsIndex is the sync-side twin of the
// restart test above, for the active-participation construction path. A node
// that joined by sync produces vertices immediately, so an unwired index there
// is not a silent gap: it anchors (0, zero root) in every vertex, and stage-1
// ingress verification makes every indexed peer reject them the moment
// commitEpochForRound(round) leaves the genesis epoch.
func TestInitConsensusForValidator_RebuildsIndex(t *testing.T) {
	src := syncSnapshotFromLiveNode(t)

	n := syncedJoiner(t, src.result)
	if err := n.initConsensusForValidator(src.result); err != nil {
		t.Fatalf("initConsensusForValidator: %v", err)
	}

	// Stop the loops before asserting: the joiner's own commit loop would
	// otherwise move the frontier past the boot seed mid-assertion.
	n.dag.Close()

	assertSyncedIndex(t, n, src.frontier, src.root)
}

// TestInitConsensusForListener_RebuildsIndex covers the listener construction
// path. A listener produces nothing, but it commits the same ordered log and
// serves snapshots, so its index must track the network's just as exactly.
func TestInitConsensusForListener_RebuildsIndex(t *testing.T) {
	src := syncSnapshotFromLiveNode(t)

	n := syncedJoiner(t, src.result)
	if err := n.initConsensusForListener(src.result); err != nil {
		t.Fatalf("initConsensusForListener: %v", err)
	}

	n.dag.Close()

	assertSyncedIndex(t, n, src.frontier, src.root)
}

// TestInitConsensusForValidator_DomainOwnerMutationChangesRoot is the domain
// leg's discriminator. A joiner whose imported registry differs from the
// source's by ONE lease owner must rebuild a different root: the owner is
// hashed into the domain leaf, so a rebuild that ignored it (or a snapshot
// that carried the name and object without the lease's holder) would agree
// with the source anyway, and a lying bootstrap could hand a joiner a registry
// whose names point at whatever keys it likes while the anchored root still
// checked out.
func TestInitConsensusForValidator_DomainOwnerMutationChangesRoot(t *testing.T) {
	src := syncSnapshotFromLiveNode(t)

	// One flipped owner byte in the imported state, nothing else touched.
	tampered := *src.result
	tampered.domainEntries = append([]state.DomainEntry(nil), src.result.domainEntries...)
	tampered.domainEntries[0].Owner[0] ^= 0xFF

	n := syncedJoiner(t, &tampered)
	if err := n.initConsensusForValidator(&tampered); err != nil {
		t.Fatalf("initConsensusForValidator: %v", err)
	}
	n.dag.Close()

	if got := n.idxManager.Root(); got == src.root {
		t.Errorf("a flipped domain owner rebuilt the same root %x: the domain leg is not covered by the anchored root", got[:4])
	}
}

// TestInitIndex_FreshBootBuildsNonEmptyRoot is a smoke test: it checks that a
// bootstrap node's index manager is wired and carries a non-empty root after
// genesis seeding, on an ordinary (non-restart) boot. CombinedRoot is never
// the zero value once any tree has a leaf, so this cannot fail on the
// genesis-coin feed specifically reaching the index — that property (the
// reserve coin's tracker entry actually landing in the tree, not just some
// root existing) is what TestInitIndex_RestartRebuildMatchesNeverRestarted
// discriminates, by comparing against a twin that took the same coin through
// the construction backfill instead of the live feed.
func TestInitIndex_FreshBootBuildsNonEmptyRoot(t *testing.T) {
	dir := t.TempDir()
	_, privKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	n, db := bootstrapTestNode(t, dir, privKey)
	defer db.Close()
	defer n.dag.Close()

	if n.idxManager == nil {
		t.Fatal("construction did not build the index manager")
	}

	n.seedGenesisState()

	if n.idxManager.Root() == ([32]byte{}) {
		t.Error("index root is empty after genesis seeding; the reserve coin never reached the index")
	}

	entries := n.dag.ExportTrackerEntries()
	if len(entries) == 0 {
		t.Fatal("test misconfigured: no tracker entries after genesis seeding")
	}
}
