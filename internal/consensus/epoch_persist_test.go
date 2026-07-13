package consensus

import (
	"bytes"
	"crypto/ed25519"
	"os"
	"sort"
	"testing"

	"BluePods/internal/storage"
)

// seededValidatorSet builds n validators from fixed ed25519 seeds. Fixed seeds
// make the pubkeys, the anchor designation, and every vertex hash deterministic
// across runs, so an ordered-log comparison is reproducible instead of hinging on
// the random keys newTestValidatorSet would produce.
func seededValidatorSet(n int) ([]testValidator, *ValidatorSet) {
	vals := make([]testValidator, n)
	pubkeys := make([]Hash, n)

	for i := range vals {
		seed := make([]byte, ed25519.SeedSize)
		seed[0] = byte(i + 1)
		priv := ed25519.NewKeyFromSeed(seed)

		var pub Hash
		copy(pub[:], priv.Public().(ed25519.PublicKey))

		vals[i] = testValidator{privKey: priv, pubKey: pub}
		pubkeys[i] = pub
	}

	return vals, NewValidatorSet(pubkeys)
}

// newPersistDAG builds a listener-mode DAG (no production, so the committed log is
// driven only by the fed vertices) with equal stake and tx-auth disabled, over the
// given storage, for the restart persistence tests.
func newPersistDAG(t *testing.T, db *storage.Storage, vals []testValidator, vs *ValidatorSet, opts ...Option) *DAG {
	t.Helper()

	all := append([]Option{WithListenerMode()}, opts...)
	dag := New(db, vs, nil, testSystemPod, 0, vals[0].privKey, nil, all...)
	setEqualStake(dag, vals, 25)
	disableTxAuth(dag)

	return dag
}

// roundIndices returns the spine indices for rounds [from, to) given n producers
// per round, so a test can feed a contiguous span of rounds.
func roundIndices(from, to, n int) []int {
	var idx []int
	for i := from * n; i < to*n; i++ {
		idx = append(idx, i)
	}
	return idx
}

// TestRestartOrderMatchesTwin_ByRoundRebuild is the restart-order regression. A
// node commits some rounds, crashes with a later round still undecided but present
// in its store, then reopens and is fed the continuation. Its full committed log
// (pre-crash ++ post-crash) must match, in ORDER, that of an uninterrupted twin fed
// the identical vertices. Without the boot-time byRound rebuild the reopened node's
// byRound is empty for the pre-crash undecided round, so getByRoundProducer reports
// its producer absent, the round is spuriously blamed and skipped, and its vertices
// ride a later batch in a different order — the committed set converges but the
// order forks. Fixed seeds make the divergence deterministic.
func TestRestartOrderMatchesTwin_ByRoundRebuild(t *testing.T) {
	seededVals, _ := seededValidatorSet(4)
	n := len(seededVals)

	const cut = 2      // rounds 0..cut arrive before the crash; cut is undecided then.
	const maxRound = 4 // rounds cut+1..maxRound arrive after the reopen.

	spine := buildTaggedSpine(t, seededVals, maxRound)
	pre := roundIndices(0, cut+1, n)          // rounds 0..cut
	post := roundIndices(cut+1, maxRound+1, n) // rounds cut+1..maxRound

	// Uninterrupted twin: same phased feed, never restarted.
	_, vsTwin := reuseSet(seededVals)
	twin := newPersistDAG(t, newTestStorage(t), seededVals, vsTwin)
	ingest(twin, spine, pre)
	twin.checkCommits()
	ingest(twin, spine, post)
	twin.checkCommits()
	twinLog := drainCommitted(twin)
	twin.Close()

	if len(twinLog) == 0 {
		t.Fatal("twin committed nothing; scenario did not exercise the commit path")
	}

	// Restarted node over persistent storage.
	dir, err := os.MkdirTemp("", "consensus_restart_order_*")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	defer os.RemoveAll(dir)

	db1, err := storage.New(dir)
	if err != nil {
		t.Fatalf("open storage: %v", err)
	}
	_, vs1 := reuseSet(seededVals)
	dag1 := newPersistDAG(t, db1, seededVals, vs1)
	ingest(dag1, spine, pre)
	dag1.checkCommits()
	preLog := drainCommitted(dag1)
	dag1.Close()
	db1.Close()

	db2, err := storage.New(dir)
	if err != nil {
		t.Fatalf("reopen storage: %v", err)
	}
	defer db2.Close()
	_, vs2 := reuseSet(seededVals)
	dag2 := newPersistDAG(t, db2, seededVals, vs2)
	ingest(dag2, spine, post)
	dag2.checkCommits()
	postLog := drainCommitted(dag2)
	dag2.Close()

	restartLog := append(append([]string{}, preLog...), postLog...)

	if !equalLog(twinLog, restartLog) {
		t.Fatalf("restart forked the committed ORDER from the twin:\n twin=%v\n restart=%v", twinLog, restartLog)
	}
}

// TestRestartResumesPastEpochBoundary is the past-boundary resume regression. A
// node drives commits through two epoch boundaries (currentEpoch 2), crashes with a
// later round undecided, and reopens. Its epoch state must be restored so it serves
// HoldersForEpoch for its decided epochs and keeps committing. Without the restore
// currentEpoch resets to 0: HoldersForEpoch(2) returns false, anchorProducerFor
// yields nothing, and the commit loop wedges on WAIT forever.
func TestRestartResumesPastEpochBoundary(t *testing.T) {
	vals, _ := seededValidatorSet(4)
	n := len(vals)
	const epochLen = 4

	// Rounds 0..9 cross boundaries at 4 (epoch 1) and 8 (epoch 2); round 10 certifies
	// round 9 after the reopen.
	spine := buildTaggedSpine(t, vals, 10)
	preRounds := roundIndices(0, 10, n) // rounds 0..9
	postRound := roundIndices(10, 11, n) // round 10

	dir, err := os.MkdirTemp("", "consensus_restart_epoch_*")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	defer os.RemoveAll(dir)

	db1, err := storage.New(dir)
	if err != nil {
		t.Fatalf("open storage: %v", err)
	}
	_, vs1 := reuseSet(vals)
	dag1 := newPersistDAG(t, db1, vals, vs1, WithEpochLength(epochLen))
	ingest(dag1, spine, preRounds)
	dag1.checkCommits()

	if dag1.Epoch() != 2 {
		t.Fatalf("pre-crash node did not cross two boundaries: epoch=%d", dag1.Epoch())
	}
	persistedCursor := dag1.LastCommittedRound()
	if persistedCursor <= 8 {
		t.Fatalf("pre-crash cursor did not advance past the second boundary: %d", persistedCursor)
	}
	dag1.Close()
	db1.Close()

	db2, err := storage.New(dir)
	if err != nil {
		t.Fatalf("reopen storage: %v", err)
	}
	defer db2.Close()
	_, vs2 := reuseSet(vals)
	dag2 := newPersistDAG(t, db2, vals, vs2, WithEpochLength(epochLen))

	// Epoch state restored: no wedge, no genesis fallback on the restored path.
	if dag2.Epoch() != 2 {
		t.Fatalf("epoch not restored on reopen: got %d, want 2", dag2.Epoch())
	}
	if dag2.epochHolders == nil || dag2.epochHolders.Len() != n {
		t.Fatalf("epoch holders not restored on reopen: %+v", dag2.epochHolders)
	}
	if _, ok := dag2.HoldersForEpoch(2); !ok {
		t.Fatal("HoldersForEpoch(2) unresolved after restore — restart would wedge")
	}
	if dag2.LastCommittedRound() != persistedCursor {
		t.Fatalf("cursor not restored: got %d, want %d", dag2.LastCommittedRound(), persistedCursor)
	}

	// The reopened node itself resumes deciding: fed the certifier round it advances
	// the cursor and applies a batch, rather than wedging on WAIT.
	ingest(dag2, spine, postRound)
	dag2.checkCommits()

	if dag2.LastCommittedRound() <= persistedCursor {
		t.Fatalf("restart wedged: cursor stuck at %d", dag2.LastCommittedRound())
	}
	if committed := drainCommitted(dag2); len(committed) == 0 {
		t.Fatal("restart advanced the cursor but committed nothing")
	}
	dag2.Close()
}

// TestEpochBoundaryPersistsCursorAndEpochAtomically asserts the boundary write is
// one batch carrying BOTH the commit cursor and the epoch state. After a commit that
// crosses an epoch boundary, both keys must be present on disk, and a reopen must
// restore a consistent (cursor, epoch) pair. Because both keys ride a single
// SetBatch (atomic: all or none), a restart can never load a cursor past the
// boundary beside a stale epoch — the wedge C2 closes.
func TestEpochBoundaryPersistsCursorAndEpochAtomically(t *testing.T) {
	vals, _ := seededValidatorSet(4)
	n := len(vals)
	const epochLen = 4

	dir, err := os.MkdirTemp("", "consensus_boundary_atomic_*")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	defer os.RemoveAll(dir)

	db, err := storage.New(dir)
	if err != nil {
		t.Fatalf("open storage: %v", err)
	}

	_, vs := reuseSet(vals)
	dag := newPersistDAG(t, db, vals, vs, WithEpochLength(epochLen))

	spine := buildTaggedSpine(t, vals, 5) // crosses the round-4 boundary
	ingest(dag, spine, roundIndices(0, 6, n))
	dag.checkCommits()

	if dag.Epoch() != 1 {
		t.Fatalf("boundary not crossed via commit path: epoch=%d", dag.Epoch())
	}
	persistedCursor := dag.LastCommittedRound()

	// Both keys must be persisted together at the boundary.
	cursorRaw, _ := db.Get(commitCursorKey)
	epochRaw, _ := db.Get(currentEpochKey)
	holdersRaw, _ := db.Get(epochHoldersKey)

	if len(cursorRaw) < 8 {
		t.Fatal("commit cursor key not persisted at the boundary")
	}
	if len(epochRaw) < 8 {
		t.Fatal("currentEpoch key not persisted at the boundary (partial batch)")
	}
	if len(holdersRaw) == 0 {
		t.Fatal("epochHolders snapshot not persisted at the boundary (partial batch)")
	}
	dag.Close()
	db.Close()

	// Reopen: the (cursor, epoch) pair is restored consistently.
	db2, err := storage.New(dir)
	if err != nil {
		t.Fatalf("reopen storage: %v", err)
	}
	defer db2.Close()
	_, vs2 := reuseSet(vals)
	dag2 := newPersistDAG(t, db2, vals, vs2, WithEpochLength(epochLen))

	if dag2.Epoch() != 1 || dag2.LastCommittedRound() != persistedCursor {
		t.Fatalf("inconsistent restore: epoch=%d cursor=%d, want epoch=1 cursor=%d",
			dag2.Epoch(), dag2.LastCommittedRound(), persistedCursor)
	}
	dag2.Close()
}

// TestByRoundRebuiltOnReopen verifies the store rebuilds its byRound index from the
// persisted round-index entries at boot, covering every round including those above
// any commit cursor. Without the rebuild byRound is empty after a reopen, and a
// re-gossiped pre-crash vertex — rejected by add as already stored — is never
// re-indexed, permanently amputating the round.
func TestByRoundRebuiltOnReopen(t *testing.T) {
	vals, _ := newTestValidatorSet(3)

	dir, err := os.MkdirTemp("", "consensus_byround_rebuild_*")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	defer os.RemoveAll(dir)

	db1, err := storage.New(dir)
	if err != nil {
		t.Fatalf("open storage: %v", err)
	}
	s1 := newStore(db1)

	// Vertices across several rounds, including an equivocation in round 2.
	for r := uint64(0); r <= 3; r++ {
		for _, v := range vals {
			addRoundVertex(t, s1, v, r, []Hash{{byte(r)}})
		}
	}
	addRoundVertex(t, s1, vals[0], 2, []Hash{{0xEE}}) // equivocating second vertex

	before := byRoundSnapshot(s1)
	db1.Close()

	db2, err := storage.New(dir)
	if err != nil {
		t.Fatalf("reopen storage: %v", err)
	}
	defer db2.Close()
	s2 := newStore(db2)

	after := byRoundSnapshot(s2)

	if len(after) != len(before) {
		t.Fatalf("round count differs after reopen: before=%d after=%d", len(before), len(after))
	}
	for round, hashesBefore := range before {
		hashesAfter, ok := after[round]
		if !ok {
			t.Fatalf("round %d absent from rebuilt byRound", round)
		}
		if !equalHashes(hashesBefore, hashesAfter) {
			t.Fatalf("round %d byRound differs after reopen:\n before=%x\n after=%x", round, hashesBefore, hashesAfter)
		}
	}
}

// byRoundSnapshot returns a copy of the store's byRound index with each round's
// hashes sorted, so two snapshots compare independently of insertion order (Pebble
// restores round entries in key order, not arrival order).
func byRoundSnapshot(s *store) map[uint64][]Hash {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make(map[uint64][]Hash, len(s.byRound))
	for round, hashes := range s.byRound {
		cp := append([]Hash{}, hashes...)
		sort.Slice(cp, func(i, j int) bool { return bytes.Compare(cp[i][:], cp[j][:]) < 0 })
		out[round] = cp
	}

	return out
}

// equalHashes reports whether two hash slices are equal element-wise.
func equalHashes(a, b []Hash) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
