package consensus

import (
	"os"
	"testing"

	"BluePods/internal/storage"
)

// admitCommittedMember adds a validator to the live set with stake and records its
// registration as committed at the given round, driving the genesis refreeze the
// production commit path performs.
func admitCommittedMember(dag *DAG, v testValidator, stake uint64, atRound uint64) {
	dag.commitMu.Lock()
	defer dag.commitMu.Unlock()

	dag.validators.AddWithStake(v.pubKey, "", [48]byte{}, stake, 0, false)
	dag.recordCommittedMember(v.pubKey, atRound)
}

// markProduced records a member's first committed vertex into the live produced set.
func markProduced(dag *DAG, v testValidator) {
	dag.commitMu.Lock()
	defer dag.commitMu.Unlock()

	dag.recordProducedMember(v.pubKey)
}

// designationsOver collects the designated producer for every round in [from, to),
// failing when any round does not resolve.
func designationsOver(t *testing.T, dag *DAG, from, to uint64) map[Hash]bool {
	t.Helper()

	seen := make(map[Hash]bool)
	for round := from; round < to; round++ {
		producer, ok := dag.anchorProducerFor(round)
		if !ok {
			t.Fatalf("round %d: no anchor producer designated", round)
		}
		seen[producer] = true
	}

	return seen
}

// TestEligibilityExcludesFrozenMemberWithoutCommittedVertex is the wedge regression
// (Phase-1 variant shape): a member whose registration is committed and frozen into
// the genesis snapshot BEFORE it has any committed vertex is never designated; the
// rounds go to members with committed vertices. Its own first committed vertex does
// NOT make it designatable (eligibility changes only at freezes); the next freeze
// that observes its production does.
func TestEligibilityExcludesFrozenMemberWithoutCommittedVertex(t *testing.T) {
	vals, _ := newTestValidatorSet(6)
	dag := New(newTestStorage(t), NewValidatorSet(nil), nil, testSystemPod, 0, vals[0].privKey, nil)
	t.Cleanup(func() { dag.Close() })

	// Three members registered (committed) and already producing.
	for i := 0; i < 3; i++ {
		admitCommittedMember(dag, vals[i], 25, uint64(i))
		markProduced(dag, vals[i])
	}

	// The laggard registers; its registration commit refreezes the genesis snapshot.
	// It has NO committed vertex at that freeze.
	laggard := vals[3]
	admitCommittedMember(dag, laggard, 25, 10)

	holders, ok := dag.HoldersForEpoch(0)
	if !ok || holders.Len() != 4 {
		t.Fatalf("expected 4 frozen holders, got ok=%v len=%d", ok, holderLen(holders))
	}

	seen := designationsOver(t, dag, 0, 200)
	if seen[laggard.pubKey] {
		t.Fatal("a member with no committed vertex at the freeze must not be designated")
	}
	if len(seen) < 2 {
		t.Fatalf("designation pinned to %d producer(s), expected spread over the eligible", len(seen))
	}

	// The laggard's first committed vertex updates the LIVE produced set only:
	// eligibility for the current snapshot must not change.
	markProduced(dag, laggard)
	if seen := designationsOver(t, dag, 0, 200); seen[laggard.pubKey] {
		t.Fatal("a produced vertex must not change eligibility before the next freeze")
	}

	// The next freeze (a fifth member's committed registration) observes the
	// laggard's production: the laggard becomes designatable, the fifth does not.
	fifth := vals[4]
	admitCommittedMember(dag, fifth, 25, 20)

	seen = designationsOver(t, dag, 0, 400)
	if !seen[laggard.pubKey] {
		t.Fatal("the laggard must become designatable at the freeze that observes its production")
	}
	if seen[fifth.pubKey] {
		t.Fatal("the freshly frozen fifth member has no committed vertex and must not be designated")
	}
}

// TestEligibilityBootstrapFallback verifies the bootstrap fallback: while NO member
// of the snapshot has a committed vertex, designation rotates over the FULL
// snapshot; and an eligible set holding only non-members (all removed) also falls
// back to the full snapshot.
func TestEligibilityBootstrapFallback(t *testing.T) {
	vals, vs := newTestValidatorSet(4)
	dag := New(newTestStorage(t), vs, nil, testSystemPod, 0, vals[0].privKey, nil)
	t.Cleanup(func() { dag.Close() })
	setEqualStake(dag, vals, 25) // freezes genesis; nobody has produced

	seen := designationsOver(t, dag, 0, 200)
	if len(seen) < 2 {
		t.Fatalf("empty eligible set must fall back to the full snapshot, got %d producers", len(seen))
	}

	// Eligible set of only non-members: intersection with the holders is empty,
	// so the full snapshot stays designatable.
	stranger := newTestValidator()
	dag.commitMu.Lock()
	dag.eligibleHolders = map[Hash]bool{stranger.pubKey: true}
	dag.commitMu.Unlock()

	seen = designationsOver(t, dag, 0, 200)
	if len(seen) < 2 || seen[stranger.pubKey] {
		t.Fatalf("non-member eligible set must fall back to the full snapshot, got %d producers", len(seen))
	}
}

// TestEligibilityFreezesAtEpochBoundary is the removal-variant wedge regression: a
// member frozen into an epoch's holder snapshot with no committed vertex at that
// boundary is ineligible for the WHOLE epoch (its later production does not change
// the frozen set), and becomes designatable at the next boundary that observes it.
func TestEligibilityFreezesAtEpochBoundary(t *testing.T) {
	const epochLen = 50

	vals, vs := newTestValidatorSet(4)
	dag := New(newTestStorage(t), vs, nil, testSystemPod, 0, vals[0].privKey, nil, WithEpochLength(epochLen))
	t.Cleanup(func() { dag.Close() })
	setEqualStake(dag, vals, 25)

	// Two members have committed vertices before the first boundary.
	markProduced(dag, vals[0])
	markProduced(dag, vals[1])

	dag.commitMu.Lock()
	dag.transitionEpoch(epochLen)
	dag.commitMu.Unlock()

	// Every epoch-1 round designates one of the two producers frozen eligible at the
	// boundary; the two laggards are in the holder snapshot but never designated.
	for round := uint64(epochLen + 1); round <= 2*epochLen; round++ {
		producer, ok := dag.anchorProducerFor(round)
		if !ok {
			t.Fatalf("round %d: no designation", round)
		}
		if producer != vals[0].pubKey && producer != vals[1].pubKey {
			t.Fatalf("round %d designated a member with no committed vertex at the boundary: %x", round, producer[:4])
		}
	}

	// A laggard's first committed vertex mid-epoch changes nothing for this epoch.
	markProduced(dag, vals[2])
	for round := uint64(epochLen + 1); round <= 2*epochLen; round++ {
		if producer, _ := dag.anchorProducerFor(round); producer == vals[2].pubKey {
			t.Fatalf("round %d: mid-epoch production must not change the frozen eligible set", round)
		}
	}

	// The next boundary freeze observes it: epoch-2 rounds may designate it.
	dag.commitMu.Lock()
	dag.transitionEpoch(2 * epochLen)
	dag.commitMu.Unlock()

	seen := designationsOver(t, dag, 2*epochLen+1, 3*epochLen)
	if !seen[vals[2].pubKey] {
		t.Fatal("the member must become designatable at the boundary that observes its production")
	}
	if seen[vals[3].pubKey] {
		t.Fatal("a member still without committed vertices must stay ineligible")
	}
}

// TestEligibilityDeterministicAcrossViews verifies designation is a pure function of
// committed history: two nodes fed the same committed events — evaluated at different
// moments, one holding extra uncommitted vertices in its store — designate identically
// for every round, including next-epoch (forward-scan proxy) rounds.
func TestEligibilityDeterministicAcrossViews(t *testing.T) {
	vals, _ := newTestValidatorSet(5)

	build := func() *DAG {
		dag := New(newTestStorage(t), NewValidatorSet(nil), nil, testSystemPod, 0, vals[0].privKey, nil, WithEpochLength(50))
		t.Cleanup(func() { dag.Close() })
		for i := 0; i < 4; i++ {
			admitCommittedMember(dag, vals[i], 25, uint64(i))
		}
		markProduced(dag, vals[0])
		markProduced(dag, vals[1])
		admitCommittedMember(dag, vals[4], 25, 9) // refreeze observing the two producers
		return dag
	}

	dagA := build()
	dagB := build()

	// A holds extra UNCOMMITTED vertices; they must not affect designation.
	for r := uint64(0); r < 30; r++ {
		addDagVertex(t, dagA, vals[2], r, nil)
	}

	// A is evaluated now; B is evaluated after further uncommitted arrivals.
	desigA := make(map[uint64]Hash)
	for round := uint64(0); round < 100; round++ {
		producer, ok := dagA.anchorProducerFor(round)
		if !ok {
			t.Fatalf("A round %d: no designation", round)
		}
		desigA[round] = producer
	}

	addDagVertex(t, dagB, vals[3], 40, nil)

	for round := uint64(0); round < 100; round++ {
		producer, ok := dagB.anchorProducerFor(round)
		if !ok {
			t.Fatalf("B round %d: no designation", round)
		}
		if want := desigA[round]; producer != want {
			t.Fatalf("round %d: designation diverged across views: A=%x B=%x", round, want[:4], producer[:4])
		}
	}
}

// TestEligibilitySurvivesRestart verifies the produced set and the frozen eligible
// sets are persisted atomically with the cursor and restored at boot: a restarted
// node designates identically, keeps the laggard excluded, and a post-restart
// refreeze still observes the pre-restart production.
func TestEligibilitySurvivesRestart(t *testing.T) {
	dir, err := os.MkdirTemp("", "consensus_eligible_restart_*")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })

	vals, _ := newTestValidatorSet(5)

	db1, err := storage.New(dir)
	if err != nil {
		t.Fatalf("open storage: %v", err)
	}

	dag1 := New(db1, NewValidatorSet(nil), nil, testSystemPod, 0, vals[0].privKey, nil)
	for i := 0; i < 3; i++ {
		admitCommittedMember(dag1, vals[i], 25, uint64(i))
		markProduced(dag1, vals[i])
	}
	laggard := vals[3]
	admitCommittedMember(dag1, laggard, 25, 10) // frozen in, no committed vertex
	markProduced(dag1, laggard)                 // produced AFTER the freeze: live set only

	// Persist exactly as the commit path does: cursor + epoch state in one batch.
	dag1.commitMu.Lock()
	dag1.store.saveCommitCursorBatch(dag1.lastCommitted, dag1.epochStateKVs())
	dag1.commitMu.Unlock()

	before := make(map[uint64]Hash)
	for round := uint64(0); round < 100; round++ {
		producer, ok := dag1.anchorProducerFor(round)
		if !ok {
			t.Fatalf("round %d: no designation before restart", round)
		}
		before[round] = producer
	}

	dag1.Close()
	db1.Close()

	db2, err := storage.New(dir)
	if err != nil {
		t.Fatalf("reopen storage: %v", err)
	}
	t.Cleanup(func() { db2.Close() })

	dag2 := New(db2, NewValidatorSet(nil), nil, testSystemPod, 0, vals[0].privKey, nil)
	t.Cleanup(func() { dag2.Close() })

	for round := uint64(0); round < 100; round++ {
		producer, ok := dag2.anchorProducerFor(round)
		if !ok {
			t.Fatalf("round %d: no designation after restart", round)
		}
		if want := before[round]; producer != want {
			t.Fatalf("round %d: designation changed across restart: %x -> %x", round, want[:4], producer[:4])
		}
		if producer == laggard.pubKey {
			t.Fatalf("round %d: laggard designated after restart", round)
		}
	}

	// A post-restart refreeze observes the laggard's pre-restart production: the
	// restored produced set must carry it. (Re-seed the live set first: the frozen
	// snapshot persists membership, the live set is rebuilt by the node at boot.)
	dag2.commitMu.Lock()
	for i := 0; i < 4; i++ {
		dag2.validators.AddWithStake(vals[i].pubKey, "", [48]byte{}, 25, 0, false)
	}
	dag2.commitMu.Unlock()
	admitCommittedMember(dag2, vals[4], 25, 20)

	seen := designationsOver(t, dag2, 0, 400)
	if !seen[laggard.pubKey] {
		t.Fatal("restored produced set lost the laggard: post-restart refreeze must observe it")
	}
}

// TestEligibilityCarriedInSyncBlob verifies a joiner installing the sync regime blob
// derives the identical eligibility: same designations, laggard excluded, and the
// produced set carried so later freezes on the joiner observe the same production.
func TestEligibilityCarriedInSyncBlob(t *testing.T) {
	vals, _ := newTestValidatorSet(5)

	src := New(newTestStorage(t), NewValidatorSet(nil), nil, testSystemPod, 0, vals[0].privKey, nil)
	t.Cleanup(func() { src.Close() })
	for i := 0; i < 3; i++ {
		admitCommittedMember(src, vals[i], 25, uint64(i))
		markProduced(src, vals[i])
	}
	laggard := vals[3]
	admitCommittedMember(src, laggard, 25, 10)
	markProduced(src, laggard) // live produced only, after the freeze

	_, blob := src.ExportRegimeState()

	joiner := New(newTestStorage(t), NewValidatorSet(nil), nil, testSystemPod, 0, vals[0].privKey, nil,
		WithSyncedRegimeState(blob))
	t.Cleanup(func() { joiner.Close() })

	for round := uint64(0); round < 100; round++ {
		want, okS := src.anchorProducerFor(round)
		got, okJ := joiner.anchorProducerFor(round)
		if !okS || !okJ {
			t.Fatalf("round %d: designation missing (src=%v joiner=%v)", round, okS, okJ)
		}
		if got != want {
			t.Fatalf("round %d: joiner designation diverged: src=%x joiner=%x", round, want[:4], got[:4])
		}
		if got == laggard.pubKey {
			t.Fatalf("round %d: laggard designated on the joiner", round)
		}
	}

	// The joiner's next freeze must observe the carried produced set.
	joiner.commitMu.Lock()
	for i := 0; i < 4; i++ {
		joiner.validators.AddWithStake(vals[i].pubKey, "", [48]byte{}, 25, 0, false)
	}
	joiner.commitMu.Unlock()
	admitCommittedMember(joiner, vals[4], 25, 20)

	seen := designationsOver(t, joiner, 0, 400)
	if !seen[laggard.pubKey] {
		t.Fatal("sync blob lost the produced set: the joiner's next freeze must observe the laggard")
	}
}

// TestEligibilityPersistsWithCursorWhenProducedChanges verifies a produced-set change
// off any regime event is persisted atomically with the cursor advance, so a restart
// between freezes never loses a member's first committed vertex.
func TestEligibilityPersistsWithCursorWhenProducedChanges(t *testing.T) {
	vals, vs := newTestValidatorSet(4)
	dag := New(newTestStorage(t), vs, nil, testSystemPod, 0, vals[0].privKey, nil)
	t.Cleanup(func() { dag.Close() })
	setEqualStake(dag, vals, 25)

	dag.commitMu.Lock()
	dag.regimeDirty = false // isolate: only the produced change should force the batch
	dag.recordProducedMember(vals[0].pubKey)
	if !dag.producedDirty {
		dag.commitMu.Unlock()
		t.Fatal("recording a first committed vertex must mark the produced set dirty")
	}
	dag.advanceCommitCursor(7) // non-boundary advance
	persisted := decodeMemberSet(dag.store.loadMetaBytes(producedMembersKey))
	dirtyAfter := dag.producedDirty
	dag.commitMu.Unlock()

	if dirtyAfter {
		t.Fatal("the cursor advance must clear the produced-dirty flag")
	}
	if !persisted[vals[0].pubKey] {
		t.Fatal("the produced set must be persisted with the cursor batch")
	}
}
