package index

import (
	"sync"
	"sync/atomic"
	"testing"
)

// TestManager_StreamVsBuildFromState_IdenticalRoot applies a synthetic
// committed stream (creates, a reparent, a delete, domain register/update,
// and a validator snapshot) to one Manager, then rebuilds a second Manager
// from scratch via BuildFromState with the equivalent final-state entries.
// The two must land on the identical combined root: BuildFromState is meant
// to reproduce exactly what the live commit path would have produced,
// regardless of the path taken to get there (boot backfill or restart).
func TestManager_StreamVsBuildFromState_IdenticalRoot(t *testing.T) {
	k1 := [32]byte{0x01}
	o1 := [32]byte{0x11}
	o2 := [32]byte{0x12}
	o3 := [32]byte{0x13}
	validators := []ValidatorLeaf{{Pubkey: [32]byte{0x99}, CappedStake: 42, Status: ValidatorActive}}

	stream := NewManager()
	stream.ApplyEdge(o1, KeyRootKind, k1)      // create O1 under K1
	stream.ApplyEdge(o2, ObjectParentKind, o1) // create O2 under O1
	stream.ApplyEdge(o2, KeyRootKind, k1)      // reparent O2 directly under K1
	stream.ApplyEdge(o3, KeyRootKind, k1)      // create O3 under K1
	stream.RemoveObject(o3)                    // delete O3
	stream.ApplyDomain("a.pod", o2, k1, 0)     // register a.pod -> O2
	stream.ApplyDomain("a.pod", o1, k1, 0)     // update a.pod -> O1
	stream.ApplyDomain("b.pod", o1, k1, 0)     // register b.pod -> O1
	stream.RemoveDomain("b.pod")               // then remove it
	stream.RebuildValidators(validators)

	rebuilt := NewManager()
	rebuilt.BuildFromState(
		[]TrackerEntry{
			{ID: o1, ParentKind: KeyRootKind, Parent: k1},
			{ID: o2, ParentKind: KeyRootKind, Parent: k1},
		},
		[]DomainLeaf{{Name: "a.pod", ObjectID: o1, Owner: k1}},
		validators,
	)

	if stream.Root() != rebuilt.Root() {
		t.Errorf("stream root %x != BuildFromState root %x", stream.Root(), rebuilt.Root())
	}
}

// TestManager_RestartRebuildMatchesNeverRestarted models a node restart: a
// "session 1" Manager processes a live stream and is then abandoned mid-run
// (as if the process crashed) without ever calling SetFrontier again. A
// "session 2" Manager is built fresh via BuildFromState from the exact same
// final tracker/domain/validator entries session 1 would have persisted to
// disk. Both must agree on Root(), independent of history bookkeeping.
func TestManager_RestartRebuildMatchesNeverRestarted(t *testing.T) {
	k1 := [32]byte{0xA0}
	o1 := [32]byte{0xA1}
	o2 := [32]byte{0xA2}
	validators := []ValidatorLeaf{
		{Pubkey: [32]byte{0x01}, CappedStake: 10, Status: ValidatorActive},
		{Pubkey: [32]byte{0x02}, CappedStake: 20, Status: ValidatorActive},
	}

	session1 := NewManager()
	session1.ApplyEdge(o1, KeyRootKind, k1)
	session1.ApplyEdge(o2, ObjectParentKind, o1)
	session1.ApplyDomain("live.pod", o1, k1, 100)
	session1.RebuildValidators(validators)
	session1.SetFrontier(7)
	wantRoot := session1.Root()

	// Session 2: never saw the live calls above, only the persisted final state.
	session2 := NewManager()
	session2.BuildFromState(
		[]TrackerEntry{
			{ID: o1, ParentKind: KeyRootKind, Parent: k1},
			{ID: o2, ParentKind: ObjectParentKind, Parent: o1},
		},
		[]DomainLeaf{{Name: "live.pod", ObjectID: o1, Owner: k1, ExpiryEpoch: 100}},
		validators,
	)

	if got := session2.Root(); got != wantRoot {
		t.Errorf("restarted root %x != never-restarted root %x", got, wantRoot)
	}

	// A fresh rebuild carries no history: RootAt for the restarted session
	// only knows what SetFrontier records going forward.
	if _, ok := session2.RootAt(7); ok {
		t.Error("a fresh BuildFromState should not retain the prior session's history")
	}
}

// TestManager_RootAt_WindowAndEpochCheckpoint drives 1500 committed rounds
// through SetFrontier, marking round 500 as an epoch checkpoint via a
// preceding RebuildValidators call. It verifies the bounded 1000-round
// window evicts old, non-checkpointed rounds, while the checkpointed round
// survives indefinitely.
func TestManager_RootAt_WindowAndEpochCheckpoint(t *testing.T) {
	m := NewManager()

	for round := uint64(1); round <= 1500; round++ {
		// Vary the state slightly each round so successive roots are not
		// trivially identical.
		m.ApplyDomain("round.pod", [32]byte{byte(round)}, [32]byte{0x01}, round)

		if round == 500 {
			m.RebuildValidators([]ValidatorLeaf{{Pubkey: [32]byte{0x01}, CappedStake: round, Status: ValidatorActive}})
		}

		m.SetFrontier(round)
	}

	if _, ok := m.RootAt(2000); ok {
		t.Error("RootAt(2000) should be false: never set")
	}
	if _, ok := m.RootAt(1500); !ok {
		t.Error("RootAt(1500) should be true: the most recent round")
	}
	if _, ok := m.RootAt(501); !ok {
		t.Error("RootAt(501) should be true: inside the 1000-round window")
	}
	if _, ok := m.RootAt(499); ok {
		t.Error("RootAt(499) should be false: outside the window and not checkpointed")
	}
	if _, ok := m.RootAt(100); ok {
		t.Error("RootAt(100) should be false: evicted long ago")
	}
	if _, ok := m.RootAt(500); !ok {
		t.Error("RootAt(500) should be true: an epoch checkpoint survives eviction past the window")
	}
}

// TestManager_SetFrontier_IgnoresNonAdvancingRound verifies SetFrontier drops a
// round at or before the last recorded one, keeping the FIFO order strictly
// monotonic and a round's first-recorded root authoritative, and that a
// pending checkpoint from a RebuildValidators call preceding the ignored call
// is retained for the next round that does get recorded.
func TestManager_SetFrontier_IgnoresNonAdvancingRound(t *testing.T) {
	m := NewManager()

	m.ApplyDomain("round.pod", [32]byte{0x01}, [32]byte{0x01}, 0)
	m.SetFrontier(0)
	firstRoot := m.Root()

	// A duplicate call for the same round: state changes underneath it, but the
	// call must be ignored, not overwrite history[0] with the new root.
	m.ApplyDomain("round.pod", [32]byte{0x02}, [32]byte{0x01}, 0)
	m.SetFrontier(0)

	if got, _ := m.RootAt(0); got != firstRoot {
		t.Errorf("RootAt(0) = %x, want the first-recorded root %x (duplicate SetFrontier(0) must not overwrite it)", got, firstRoot)
	}

	// An out-of-order call (round behind the last recorded one) is ignored too.
	m.SetFrontier(1)
	afterOne := m.Root()
	m.ApplyDomain("round.pod", [32]byte{0x03}, [32]byte{0x01}, 0)
	m.SetFrontier(0)

	if got, ok := m.RootAt(1); !ok || got != afterOne {
		t.Errorf("RootAt(1) = %x, ok=%v; an out-of-order SetFrontier(0) must not disturb round 1", got, ok)
	}

	// A pending checkpoint set right before an ignored call must survive to the
	// next round that actually gets recorded, not be silently dropped.
	m.RebuildValidators([]ValidatorLeaf{{Pubkey: [32]byte{0x09}, CappedStake: 1, Status: ValidatorActive}})
	m.SetFrontier(1) // ignored: 1 <= last recorded (1)
	m.SetFrontier(2)

	if _, ok := m.RootAt(2); !ok {
		t.Error("RootAt(2) should be true: the pending checkpoint from the ignored call must attach to the next recorded round")
	}
}

// TestManager_ApplyDomainRemoveDomainRoundTrip verifies the domain feed
// points reach the underlying tree.
func TestManager_ApplyDomainRemoveDomainRoundTrip(t *testing.T) {
	m := NewManager()
	m.ApplyDomain("x.pod", [32]byte{0x01}, [32]byte{0x02}, 5)

	before := m.Root()
	m.RemoveDomain("x.pod")

	if m.Root() == before {
		t.Error("root unchanged after removing the only domain leaf")
	}
}

// TestManager_NewManagerRootIsEmpty verifies a fresh Manager's root is
// deterministic and reproducible (two independent empty managers agree).
func TestManager_NewManagerRootIsEmpty(t *testing.T) {
	if NewManager().Root() != NewManager().Root() {
		t.Error("two fresh managers disagree on the empty root")
	}
}

// TestManager_CommittedFrontier_ConcurrentWithApply exercises frontierMu
// under -race: a single writer goroutine hammers ApplyEdge+SetFrontier —
// mirroring the commit path's own serialization under its own lock — while
// several reader goroutines call CommittedFrontier concurrently, thousands of
// times over. A dropped or misplaced frontierMu would show up here as a data
// race even on a run where every individual assertion still happens to pass.
//
// Every pair a reader observes during the race window is only recorded here,
// and verified after the writer goroutine finishes (synchronized through the
// closed done channel plus the readers' WaitGroup): a torn read would pair
// one SetFrontier call's round with a different call's root, which
// RootAt(pair.round) == pair.root then catches. The concurrent-RootAt case
// is TestManager_RootAt_ConcurrentWithCommitPath's job.
func TestManager_CommittedFrontier_ConcurrentWithApply(t *testing.T) {
	m := NewManager()

	// Stay within historyWindow so every round this test anchors is still
	// retained by RootAt at verification time -- eviction is not what this
	// test is about.
	const rounds = historyWindow - 1

	type pair struct {
		round uint64
		root  [32]byte
	}

	var pairsMu sync.Mutex
	var pairs []pair

	done := make(chan struct{})

	go func() {
		defer close(done)
		for round := uint64(1); round <= rounds; round++ {
			m.ApplyEdge([32]byte{byte(round), byte(round >> 8)}, KeyRootKind, [32]byte{0x01})
			m.SetFrontier(round)
		}
	}()

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				round, root := m.CommittedFrontier()

				pairsMu.Lock()
				pairs = append(pairs, pair{round, root})
				pairsMu.Unlock()

				select {
				case <-done:
					return
				default:
				}
			}
		}()
	}
	wg.Wait()

	if len(pairs) == 0 {
		t.Fatal("no pairs observed")
	}

	checked := 0
	for _, p := range pairs {
		if p.round == 0 {
			continue // observed before the writer's first SetFrontier call
		}

		want, ok := m.RootAt(p.round)
		if !ok {
			t.Fatalf("RootAt(%d) not retained, expected it inside the history window", p.round)
		}
		if want != p.root {
			t.Fatalf("torn read: CommittedFrontier paired round %d with root %x, but RootAt(%d) = %x", p.round, p.root, p.round, want)
		}
		checked++
	}

	if checked == 0 {
		t.Fatal("every observed pair had round 0 -- readers never overlapped the writer")
	}
}

// TestManager_RootAt_ConcurrentWithCommitPath is the -race regression for the
// reader RootAt acquired when ingress validation started calling it: vertex
// validation runs on the gossip goroutines, concurrently with the commit
// loop's SetFrontier writes. history, order and epochCheckpoints are plain
// maps and a slice, so an unguarded RootAt against a live commit path is not
// a benign race but a fatal concurrent map read/write that takes the node
// down under load.
//
// One writer mirrors the commit path (ApplyEdge + SetFrontier, serialized by
// the DAG's commitMu in production, by being a single goroutine here) while
// several ingress-style readers call CommittedFrontier and RootAt in a tight
// loop. The readers assert INSIDE the race window, which is sound because a
// round's retained root is immutable once recorded (SetFrontier's
// non-advancing guard makes the first write authoritative and nothing but
// eviction ever removes it): whatever frontier a reader observes, RootAt of
// that same round must return that same root.
func TestManager_RootAt_ConcurrentWithCommitPath(t *testing.T) {
	m := NewManager()

	// Stay inside historyWindow so no round a reader observes can be evicted
	// between its CommittedFrontier and its RootAt call.
	const rounds = historyWindow - 1

	done := make(chan struct{})

	go func() {
		defer close(done)
		for round := uint64(1); round <= rounds; round++ {
			m.ApplyEdge([32]byte{byte(round), byte(round >> 8)}, KeyRootKind, [32]byte{0x01})

			// One checkpoint mid-run, so epochCheckpoints is written under the
			// readers too, not only history and order.
			if round == rounds/2 {
				m.RebuildValidators([]ValidatorLeaf{{Pubkey: [32]byte{0x07}, CappedStake: round, Status: ValidatorActive}})
			}

			m.SetFrontier(round)
		}
	}()

	var wg sync.WaitGroup
	var observed atomic.Int64

	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				round, root := m.CommittedFrontier()

				if got, ok := m.RootAt(round); ok && got != root {
					t.Errorf("RootAt(%d) = %x, want the frontier root %x observed for that round", round, got, root)
					return
				}

				if round > 0 {
					observed.Add(1)
				}

				select {
				case <-done:
					return
				default:
				}
			}
		}()
	}

	wg.Wait()

	if observed.Load() == 0 {
		t.Fatal("readers never observed a recorded frontier -- they did not overlap the writer")
	}
}
