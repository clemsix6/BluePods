package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"testing"

	"BluePods/internal/consensus"
	"BluePods/internal/storage"
)

// committedDataDir returns a data directory holding real committed state and
// nothing else: a bootstrap session seeds genesis and commits a round, which
// persists the commit cursor and the live validator set, then closes.
//
// It is also, exactly, the residue a REFUSED join leaves behind: the sync path
// applies the source's snapshot to local storage and runs the commit loop over
// it before the verification gate decides anything, so committed state on disk
// is no evidence at all that the node owns it.
func committedDataDir(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()

	_, privKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	n, db := bootstrapTestNode(t, dir, privKey)
	n.seedGenesisState()

	waitForCommit(t, n.dag, n.dag.LastCommittedRound(), 1)

	n.dag.Close()
	if err := db.Close(); err != nil {
		t.Fatalf("close seeding session: %v", err)
	}

	return dir
}

// resumeTestNode builds the minimum a routing decision reads — the flags and an
// open data directory — over dir.
func resumeTestNode(t *testing.T, cfg *Config, dir string) (*Node, *storage.Storage) {
	t.Helper()

	db, err := storage.New(dir)
	if err != nil {
		t.Fatalf("storage: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	return &Node{cfg: cfg, storage: db}, db
}

// syncingConfig is the flag shape every non-genesis node is started with, join
// and restart alike: an upstream and a pinned checkpoint. The routing decision
// cannot come from these, which is the whole point.
func syncingConfig() *Config {
	return &Config{BootstrapAddr: "10.0.0.1:9000", TrustCheckpoint: "0:ab"}
}

// TestResumesFromLocalState_EmptyDirectory is the joiner's shape: nothing
// local, so everything this node will hold comes from a peer and must go
// through the verification gate.
func TestResumesFromLocalState_EmptyDirectory(t *testing.T) {
	n, _ := resumeTestNode(t, syncingConfig(), t.TempDir())

	if n.resumesFromLocalState() {
		t.Fatal("a node with an empty data directory resumed locally: it would go live having synced nothing")
	}
}

// TestResumesFromLocalState_UnverifiedSyncResidue is the security case at the
// routing seam: committed state alone must never route a start away from the
// gate, or a join the gate refused would be resumed as the node's own on the
// next restart — a supervisor loop walking around the verification one crash at
// a time.
func TestResumesFromLocalState_UnverifiedSyncResidue(t *testing.T) {
	n, _ := resumeTestNode(t, syncingConfig(), committedDataDir(t))

	if n.resumesFromLocalState() {
		t.Fatal("committed state alone routed the start away from the gate: a refused join's snapshot would be adopted unverified")
	}
}

// TestResumesFromLocalState_OwnState is the restart this routing exists for: a
// node that owns the committed state in its directory boots from it instead of
// re-adopting it from a peer — which is what lets a cluster recover from full
// extinction, where the first node back can never see a live stake quorum to
// attest anything.
func TestResumesFromLocalState_OwnState(t *testing.T) {
	dir := committedDataDir(t)

	n, db := resumeTestNode(t, syncingConfig(), dir)
	if err := consensus.MarkStateAdopted(db); err != nil {
		t.Fatalf("mark state adopted: %v", err)
	}

	if !n.resumesFromLocalState() {
		t.Fatal("a node did not resume from state it owns: it would re-adopt its own history as foreign state and demand a quorum attest it")
	}
}

// TestResumesFromLocalState_Roles covers the two identities that never take the
// resume branch even holding adopted state: a genesis bootstrap and a node with
// no upstream both already build their DAG locally through runBootstrap.
func TestResumesFromLocalState_Roles(t *testing.T) {
	cases := []struct {
		name string
		cfg  *Config
	}{
		{"bootstrap", &Config{Bootstrap: true, BootstrapAddr: "10.0.0.1:9000"}},
		{"no upstream", &Config{}},
	}

	dir := committedDataDir(t)

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			n, db := resumeTestNode(t, tc.cfg, dir)
			if err := consensus.MarkStateAdopted(db); err != nil {
				t.Fatalf("mark state adopted: %v", err)
			}

			if n.resumesFromLocalState() {
				t.Fatalf("%s took the resume branch: only a node handed an upstream it does not need routes through it", tc.name)
			}
		})
	}
}
