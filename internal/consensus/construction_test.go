package consensus

import (
	"testing"

	"BluePods/internal/index"
)

// TestWithIndexer_BackfillsAndSeedsBeforeTheLoopStarts is the construction
// contract batch 5 replaces the post-construction SetIndexer wire with. The
// index must be complete AND anchored at the right round by the time New
// returns, because New starts the commit loop and the production loop: an
// index wired a moment later loses every root the loop recorded in between
// (the decide-before-wire window), and a d.indexer field written after those
// goroutines started is read by them with no happens-before edge at all.
func TestWithIndexer_BackfillsAndSeedsBeforeTheLoopStarts(t *testing.T) {
	validators, vs := newTestValidatorSet(3)

	domains := newMockDomainStore()
	domains.SetDomainLeaf("alice.bp", [32]byte{0xAA}, [32]byte{0xBB}, 42)
	domains.SetDomainLeaf("bob.bp", [32]byte{0xCC}, [32]byte{0xDD}, 77)

	tracked := []ObjectTrackerEntry{
		{ID: Hash{0x11}, ParentKind: keyRootKind, Parent: Hash{0x01}},
		{ID: Hash{0x22}, ParentKind: objectParentKind, Parent: Hash{0x11}},
	}

	params := DefaultFeeParams()
	live := index.NewManager()

	dag := New(newTestStorage(t), vs, nil, testSystemPod, 0, validators[0].privKey, nil,
		WithLastCommittedRound(9),
		WithImportData(nil, tracked),
		WithDomainStore(domains),
		WithFeeParams(&params),
		WithIndexer(live),
	)
	t.Cleanup(func() { dag.Close() })

	// The pair the node anchors in the first vertex it produces. The seed is
	// the LAST DECIDED round: WithLastCommittedRound(9) sets the cursor — the
	// next round to decide — to 10, and the imported state describes round 9.
	round, root := live.CommittedFrontier()
	if round != 9 {
		t.Errorf("CommittedFrontier round = %d, want 9 (cursor 10 minus one): a seed at the cursor steals the round the commit loop is about to record", round)
	}

	// The root itself: an independent rebuild over the same committed state.
	// Equality proves the backfill read every leg — tracker, domains, and the
	// validator snapshot — not merely that something was recorded.
	want := index.NewManager()
	want.BuildFromState(
		[]index.TrackerEntry{
			{ID: [32]byte{0x11}, ParentKind: keyRootKind, Parent: [32]byte{0x01}},
			{ID: [32]byte{0x22}, ParentKind: objectParentKind, Parent: [32]byte{0x11}},
		},
		[]index.DomainLeaf{
			{Name: "alice.bp", ObjectID: [32]byte{0xAA}, Owner: [32]byte{0xBB}, ExpiryEpoch: 42},
			{Name: "bob.bp", ObjectID: [32]byte{0xCC}, Owner: [32]byte{0xDD}, ExpiryEpoch: 77},
		},
		dag.ValidatorLeaves(dag.EpochHolders().All()),
	)

	wantRoot := want.Root()
	if root != wantRoot {
		t.Errorf("seeded root = %x, want %x: the construction backfill missed a leg of the committed state", root[:4], wantRoot[:4])
	}

	if got, ok := live.RootAt(9); !ok || got != root {
		t.Errorf("RootAt(9) ok=%v root=%x, want the seeded root %x recorded under the last decided round", ok, got[:4], root[:4])
	}
}

// TestWithIndexer_FreshChainSeedsNoRound covers the fresh-chain skip: a DAG
// whose cursor is 0 has decided nothing, so seeding would record a root under
// round 0 — the round the commit loop is about to decide — and SetFrontier's
// non-advancing guard would then drop that round's real root, forking RootAt
// against every node that never restarted.
func TestWithIndexer_FreshChainSeedsNoRound(t *testing.T) {
	validators, vs := newTestValidatorSet(3)
	live := index.NewManager()

	dag := New(newTestStorage(t), vs, nil, testSystemPod, 0, validators[0].privKey, nil,
		WithIndexer(live),
	)
	t.Cleanup(func() { dag.Close() })

	if _, ok := live.RootAt(0); ok {
		t.Error("a fresh chain recorded a root at round 0: the commit loop is the sole frontier writer from round 0 on")
	}
}

// TestMaxTermEpochs_UnwiredFeeParamsPanics pins the fail-loud rule that
// replaced the 256/8 accessor fallbacks. A DAG that applies domain leases
// against unwired parameters is not a degraded node but a forked one: it caps
// terms and sweeps expiries by numbers no other node uses, and every domain
// leaf it writes (or refuses to write) diverges the anchored index root
// permanently. Stopping is the only outcome that cannot silently fork.
func TestMaxTermEpochs_UnwiredFeeParamsPanics(t *testing.T) {
	validators, vs := newTestValidatorSet(3)
	dag := New(newTestStorage(t), vs, nil, testSystemPod, 0, validators[0].privKey, nil)
	t.Cleanup(func() { dag.Close() })

	assertPanics(t, "maxTermEpochs", func() { _ = dag.maxTermEpochs() })
	assertPanics(t, "graceEpochs", func() { _ = dag.graceEpochs() })
}

// TestFeeParams_WiredAtConstruction covers the wire that keeps the panic above
// unreachable in production: every construction path installs the governed
// parameters as an Option, before New starts a goroutine that could read them.
func TestFeeParams_WiredAtConstruction(t *testing.T) {
	validators, vs := newTestValidatorSet(3)

	params := DefaultFeeParams()
	params.MaxTermEpochs = 12
	params.GraceEpochs = 3

	dag := New(newTestStorage(t), vs, nil, testSystemPod, 0, validators[0].privKey, nil, WithFeeParams(&params))
	t.Cleanup(func() { dag.Close() })

	if got := dag.maxTermEpochs(); got != 12 {
		t.Errorf("maxTermEpochs() = %d, want the governed 12", got)
	}
	if got := dag.graceEpochs(); got != 3 {
		t.Errorf("graceEpochs() = %d, want the governed 3", got)
	}
}

// assertPanics runs fn and fails the test when it returns without panicking.
func assertPanics(t *testing.T, name string, fn func()) {
	t.Helper()

	defer func() {
		if recover() == nil {
			t.Errorf("%s returned instead of panicking on an unwired fee system: a silent fallback forks the anchored root", name)
		}
	}()

	fn()
}
