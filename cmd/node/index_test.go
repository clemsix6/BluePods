package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"testing"

	"BluePods/internal/consensus"
)

// TestInitIndex_RestartRebuildMatchesNeverRestarted simulates a node restart:
// session 1 seeds genesis (tracking the reserve coin), wires the index via
// initIndex, then tracks one more object live through the wired
// DAG.TrackObject -> indexer.ApplyEdge hook, capturing the resulting root
// without ever restarting. Session 2 reopens the same data directory fresh
// and calls initIndex alone. Its BuildFromState backfill, reading only what
// session 1 persisted to Pebble, must reproduce the identical combined root —
// otherwise a restarted node would anchor a wrong root and be silently
// excluded by peers the moment anchoring lands.
func TestInitIndex_RestartRebuildMatchesNeverRestarted(t *testing.T) {
	dir := t.TempDir()
	_, privKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	// Session 1: seed genesis, wire the index, then track one more object
	// live so both the backfill path and the live ApplyEdge hook contribute
	// to the final root session 2 must reproduce.
	n1, db1 := bootstrapTestNode(t, dir, privKey)
	n1.seedGenesisState()
	n1.initIndex()

	owner := deriveOwner(privKey)
	var extra consensus.Hash
	extra[0] = 0xAB
	n1.dag.TrackObject(extra, 1, 0, 0, 0, owner)

	wantRoot := n1.idxManager.Root()
	if wantRoot == ([32]byte{}) {
		t.Fatal("test misconfigured: session 1's root is the empty root")
	}

	n1.dag.Close()
	if err := db1.Close(); err != nil {
		t.Fatalf("close first session storage: %v", err)
	}

	// Session 2: fresh storage/state/dag over the same data directory. Only
	// initIndex runs — no live TrackObject call — so its root depends
	// entirely on the BuildFromState backfill.
	n2, db2 := bootstrapTestNode(t, dir, privKey)
	defer db2.Close()
	defer n2.dag.Close()

	n2.seedGenesisState() // guarded re-seed: restores the founder, does not re-track
	n2.initIndex()

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
// applied — disagrees with: an anchored fork the moment batch 3 anchors
// roots. The seed must target cursor-1, the round the backfilled state
// actually corresponds to. The cursor round's decision is driven
// synchronously here (commit loop stopped) so the test is deterministic.
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

	// Session 2: reopen the same data directory, then STOP the commit loop
	// immediately so the cursor round's decision below is this test's own
	// synchronous call, never raced by the background ticker.
	n2, db2 := bootstrapTestNode(t, dir, privKey)
	defer db2.Close()
	n2.dag.Close()

	cursor := n2.dag.LastCommittedRound() // next round to decide: 0..cursor-1 are done
	if cursor == 0 {
		t.Fatal("test misconfigured: session 1 persisted no decided round")
	}

	n2.seedGenesisState()
	n2.initIndex()
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

// TestInitIndex_FreshBootBuildsNonEmptyRoot verifies initIndex backfills the
// genesis-seeded reserve coin into the index on an ordinary (non-restart)
// boot, so a bootstrap node never anchors an empty root once genesis exists.
func TestInitIndex_FreshBootBuildsNonEmptyRoot(t *testing.T) {
	dir := t.TempDir()
	_, privKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	n, db := bootstrapTestNode(t, dir, privKey)
	defer db.Close()
	defer n.dag.Close()

	n.seedGenesisState()
	n.initIndex()

	if n.idxManager == nil {
		t.Fatal("initIndex did not construct the manager")
	}
	if n.idxManager.Root() == ([32]byte{}) {
		t.Error("index root is empty after genesis seeding; the reserve coin was not backfilled")
	}

	entries := n.dag.ExportTrackerEntries()
	if len(entries) == 0 {
		t.Fatal("test misconfigured: no tracker entries after genesis seeding")
	}
}
