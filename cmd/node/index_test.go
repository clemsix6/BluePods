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
