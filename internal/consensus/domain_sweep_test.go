package consensus

import (
	"reflect"
	"testing"

	"BluePods/internal/events"
	"BluePods/internal/index"
)

// =============================================================================
// sweepExpiredDomains: store + tree + event
// =============================================================================

// TestSweepExpiredDomains_RemovesPastGraceFromStoreAndTree asserts a lease
// past its grace window is removed from BOTH the registry and the
// authenticated domain tree on the boundary, and emits state.domain.deleted
// with reason "expired". A live lease is untouched.
func TestSweepExpiredDomains_RemovesPastGraceFromStoreAndTree(t *testing.T) {
	env := newDomainEnv(t)
	defer env.dag.Close()

	params := DefaultFeeParams()
	params.GraceEpochs = 2
	env.dag.SetFeeSystem(nil, &params, nil)

	env.domains.SetDomainLeaf("gone", Hash{0x01}, domainAlice, 5) // expiry 5, grace 2 -> swept once newEpoch > 7
	env.domains.SetDomainLeaf("alive", Hash{0x02}, domainBob, 10) // expiry 10, grace 2 -> not swept until newEpoch > 12

	buf := captureEvents(t)
	env.dag.sweepExpiredDomains(8) // 8 > 5+2, 8 <= 10+2

	if _, _, _, ok := env.domains.DomainLeaf("gone"); ok {
		t.Fatal("expired-beyond-grace name must be removed from the store")
	}
	if _, _, _, ok := env.domains.DomainLeaf("alive"); !ok {
		t.Fatal("a lease still inside its grace window must remain in the store")
	}

	if len(env.idx.removed) != 1 || env.idx.removed[0] != "gone" {
		t.Fatalf("index removals = %v, want exactly [gone]", env.idx.removed)
	}

	recs := eventsNamed(t, buf, events.EvDomainDeleted)
	if len(recs) != 1 {
		t.Fatalf("want 1 %s event, got %d: %v", events.EvDomainDeleted, len(recs), recs)
	}
	if recs[0]["name"] != "gone" {
		t.Errorf("name = %v, want gone", recs[0]["name"])
	}
	if recs[0]["reason"] != "expired" {
		t.Errorf("reason = %v, want expired", recs[0]["reason"])
	}
}

// TestSweepExpiredDomains_BoundaryEdge_NotYetPastGrace asserts a lease
// exactly AT expiry+grace is not swept: the rule is newEpoch > expiry+grace,
// strictly greater.
func TestSweepExpiredDomains_BoundaryEdge_NotYetPastGrace(t *testing.T) {
	env := newDomainEnv(t)
	defer env.dag.Close()

	params := DefaultFeeParams()
	params.GraceEpochs = 2
	env.dag.SetFeeSystem(nil, &params, nil)

	env.domains.SetDomainLeaf("edge", Hash{0x01}, domainAlice, 5) // expiry+grace = 7

	env.dag.sweepExpiredDomains(7) // 7 > 7 is false: must not sweep

	if _, _, _, ok := env.domains.DomainLeaf("edge"); !ok {
		t.Fatal("a lease exactly at its grace boundary must not be swept yet")
	}
	if len(env.idx.removed) != 0 {
		t.Fatalf("index removals = %v, want none", env.idx.removed)
	}
}

// TestSweepExpiredDomains_WithinGraceOwnerStillRenews asserts a lease inside
// its grace window survives the sweep and its owner can still renew it — the
// one right grace preserves.
func TestSweepExpiredDomains_WithinGraceOwnerStillRenews(t *testing.T) {
	env := newDomainEnv(t)
	defer env.dag.Close()

	params := DefaultFeeParams()
	params.GraceEpochs = 2
	env.dag.SetFeeSystem(nil, &params, nil)

	env.domains.SetDomainLeaf("soon", Hash{0x01}, domainAlice, 5)

	env.dag.sweepExpiredDomains(6) // 6 <= 5+2: still in grace, not swept

	if !env.apply(domainAlice, 6, renewOp("soon", 4)) {
		t.Fatal("the owner must still be able to renew a lease inside its grace window")
	}
	if got := env.leaf(t, "soon").expiry; got != 10 {
		t.Errorf("expiry after renewal = %d, want 10 (max(5,6)+4)", got)
	}
}

// =============================================================================
// sweepExpiredDomains: root only moves when something is swept
// =============================================================================

// TestSweepExpiredDomains_NothingExpired_RootUnchanged asserts the sweep
// touches only expired leaves: with nothing past its grace window, the
// combined index root is bit-for-bit identical before and after.
func TestSweepExpiredDomains_NothingExpired_RootUnchanged(t *testing.T) {
	dag, domains, mgr := sweepManagerTestDAG(t, 2)
	defer dag.Close()

	domains.SetDomainLeaf("alive", Hash{0x01}, domainAlice, 10)
	mgr.ApplyDomain("alive", Hash{0x01}, domainAlice, 10)

	before := mgr.Root()
	dag.sweepExpiredDomains(8) // 8 <= 10+2: nothing swept
	after := mgr.Root()

	if before != after {
		t.Fatal("the index root must not move when the sweep finds nothing to remove")
	}
}

// TestSweepExpiredDomains_SweptLeaf_RootChanges asserts the combined index
// root DOES move once the sweep actually removes a leaf, and the removal
// reaches the tree, not just the store.
func TestSweepExpiredDomains_SweptLeaf_RootChanges(t *testing.T) {
	dag, domains, mgr := sweepManagerTestDAG(t, 2)
	defer dag.Close()

	domains.SetDomainLeaf("gone", Hash{0x01}, domainAlice, 5)
	domains.SetDomainLeaf("alive", Hash{0x02}, domainBob, 10)
	mgr.ApplyDomain("gone", Hash{0x01}, domainAlice, 5)
	mgr.ApplyDomain("alive", Hash{0x02}, domainBob, 10)

	before := mgr.Root()
	dag.sweepExpiredDomains(8) // sweeps "gone" only
	after := mgr.Root()

	if before == after {
		t.Fatal("the index root must change once the sweep removes a leaf")
	}

	// The surviving leaf alone must reproduce the post-sweep root: proves the
	// sweep dropped exactly "gone" from the tree, not something else.
	solo := index.NewManager()
	solo.ApplyDomain("alive", Hash{0x02}, domainBob, 10)
	if solo.Root() != after {
		t.Fatal("post-sweep root must equal a tree built from the surviving leaf alone")
	}
}

// =============================================================================
// sweepExpiredDomains: determinism
// =============================================================================

// TestSweepExpiredDomains_DeterministicOrder asserts swept names are removed
// in sorted order regardless of the store's (randomized Go map) iteration
// order, so two independently-built registries with the same expired names
// sweep them in byte-identical sequence.
func TestSweepExpiredDomains_DeterministicOrder(t *testing.T) {
	names := []string{"zeta", "alpha", "mu", "beta", "omega", "delta"}
	want := []string{"alpha", "beta", "delta", "mu", "omega", "zeta"}

	for run := 0; run < 3; run++ {
		env := newDomainEnv(t)

		for i, n := range names {
			env.domains.SetDomainLeaf(n, Hash{byte(i + 1)}, domainAlice, 1) // all expired
		}

		env.dag.sweepExpiredDomains(100) // grace default: everything is long past it

		if !reflect.DeepEqual(env.idx.removed, want) {
			t.Fatalf("run %d: removed order = %v, want %v (sorted)", run, env.idx.removed, want)
		}

		env.dag.Close()
	}
}

// =============================================================================
// sweepExpiredDomains: wiring and nil safety
// =============================================================================

// TestTransitionEpoch_SweepsExpiredDomains asserts the epoch-boundary hook is
// actually wired: transitionEpoch itself removes a lease past its grace
// window, using the epoch it is transitioning INTO (currentEpoch+1).
func TestTransitionEpoch_SweepsExpiredDomains(t *testing.T) {
	env := newDomainEnv(t)
	defer env.dag.Close()

	env.dag.epochLength = 10

	params := DefaultFeeParams()
	params.GraceEpochs = 0
	env.dag.SetFeeSystem(nil, &params, nil)

	// currentEpoch is 0 here; transitionEpoch moves it to 1. A lease expiring
	// at epoch 0 with zero grace is past grace once newEpoch (1) > 0.
	env.domains.SetDomainLeaf("expiring", Hash{0x01}, domainAlice, 0)

	env.dag.transitionEpoch(10)

	if _, _, _, ok := env.domains.DomainLeaf("expiring"); ok {
		t.Fatal("transitionEpoch must sweep a lease past its grace window")
	}
}

// TestSweepExpiredDomains_NoDomainStore_NoPanic asserts a DAG with no domain
// store wired sweeps nothing rather than panicking, the same fail-closed
// posture every other domain feed point takes when unset.
func TestSweepExpiredDomains_NoDomainStore_NoPanic(t *testing.T) {
	dag := opsTestDAG(t)
	defer dag.Close()

	dag.sweepExpiredDomains(1_000_000) // no domain store wired: must not panic
}

// =============================================================================
// Test helpers
// =============================================================================

// sweepManagerTestDAG builds a DAG wired to an in-memory domain store and a
// REAL index.Manager (not the recording mock), so a test can assert the
// combined index root actually moves — and moves only — when the sweep
// removes a leaf.
func sweepManagerTestDAG(t *testing.T, grace uint64) (*DAG, *mockDomainStore, *index.Manager) {
	t.Helper()

	validators, vs := newTestValidatorSet(3)
	dag := New(newTestStorage(t), vs, &mockBroadcaster{}, testSystemPod, 0, validators[0].privKey, nil)
	disableTxAuth(dag)

	domains := newMockDomainStore()
	dag.SetDomainStore(domains)

	mgr := index.NewManager()
	dag.SetIndexer(mgr)

	params := DefaultFeeParams()
	params.GraceEpochs = grace
	dag.SetFeeSystem(nil, &params, nil)

	return dag, domains, mgr
}
