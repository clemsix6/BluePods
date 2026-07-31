package consensus

import (
	"errors"
	"testing"

	"BluePods/internal/index"
	"BluePods/internal/types"
)

// receiverFrontier is the committed round every receiver in this file has
// decided: a vertex anchoring it is verifiable, anything above it is not.
const receiverFrontier = 3

// anchorIndexer is the index seam stand-in a PRODUCER DAG is wired to, so a
// test can make it anchor any (frontier_round, index_root) pair — including
// the pairs no honest producer would ever emit, which is the whole point of
// stage 1. Only CommittedFrontier is consulted on the production path; the
// other methods exist to satisfy the seam.
type anchorIndexer struct {
	frontier uint64 // frontier is the round CommittedFrontier reports
	root     Hash   // root is the index root CommittedFrontier reports
}

func (a *anchorIndexer) ApplyEdge(child [32]byte, kind byte, parent [32]byte) {}

func (a *anchorIndexer) RemoveObject(child [32]byte) {}

func (a *anchorIndexer) RebuildValidators(entries []index.ValidatorLeaf) {}

func (a *anchorIndexer) SetFrontier(round uint64) {}

func (a *anchorIndexer) CommittedFrontier() (uint64, [32]byte) { return a.frontier, a.root }

func (a *anchorIndexer) RootAt(round uint64) ([32]byte, bool) { return [32]byte{}, false }

// newAnchoredProducer returns a function that produces genuinely signed
// vertices carrying an arbitrary anchor, by driving the real production path
// (buildVertex) over a controllable seam. Building the flatbuffer by hand
// instead would let a test drift from what a producer actually emits.
func newAnchoredProducer(t *testing.T, producer testValidator) func(round, frontier uint64, root Hash) *types.Vertex {
	t.Helper()

	seam := &anchorIndexer{}

	dag := New(newTestStorage(t), NewValidatorSet(nil), nil, testSystemPod, 0, producer.privKey, nil)
	t.Cleanup(func() { dag.Close() })
	dag.SetIndexer(seam)

	return func(round, frontier uint64, root Hash) *types.Vertex {
		seam.frontier, seam.root = frontier, root

		return types.GetRootAsVertex(dag.buildVertex(round, nil, nil), 0)
	}
}

// newAnchorReceiver returns a DAG wired to a real index Manager that has
// committed receiverFrontier, together with the root it retains for that
// round.
func newAnchorReceiver(t *testing.T, vs *ValidatorSet, key testValidator, opts ...Option) (*DAG, Hash) {
	t.Helper()

	mgr := index.NewManager()
	mgr.ApplyEdge([32]byte{0x11}, index.KeyRootKind, [32]byte{0x01})
	mgr.SetFrontier(receiverFrontier)

	dag := New(newTestStorage(t), vs, nil, testSystemPod, 0, key.privKey, nil, opts...)
	t.Cleanup(func() { dag.Close() })
	dag.SetIndexer(mgr)

	root, ok := mgr.RootAt(receiverFrontier)
	if !ok {
		t.Fatalf("receiver has no root at its own committed frontier %d", receiverFrontier)
	}

	if root == (Hash{}) {
		t.Fatal("receiver's committed root is the zero root, which would make the genesis-tolerance cases vacuous")
	}

	return dag, root
}

// TestValidateIndexAnchor_WrongRootAtCommittedFrontier is the core of stage 1:
// a vertex anchoring a frontier the receiver has itself committed is checked
// against the receiver's retained root, and a mismatch is a terminal rejection
// carrying the index_root reason — never a fault record (that is the commit
// path's job) and never a buffer.
func TestValidateIndexAnchor_WrongRootAtCommittedFrontier(t *testing.T) {
	validators, vs := newTestValidatorSet(2)
	receiver, committed := newAnchorReceiver(t, vs, validators[0])
	produce := newAnchoredProducer(t, validators[1])

	honest := produce(5, receiverFrontier, committed)
	if err := receiver.validateIndexAnchor(honest); err != nil {
		t.Fatalf("a vertex anchoring the receiver's own committed root must pass: %v", err)
	}

	lying := committed
	lying[0] ^= 0xFF

	err := receiver.validateIndexAnchor(produce(5, receiverFrontier, lying))
	if !errors.Is(err, errIndexRoot) {
		t.Fatalf("a wrong root at a committed frontier must be rejected with errIndexRoot, got: %v", err)
	}

	if got := classifyRejection(err); got != "index_root" {
		t.Fatalf("classifyRejection = %q, want %q", got, "index_root")
	}
}

// TestValidateIndexAnchor_UnverifiableFrontierPasses covers the liveness half
// of the rule: a receiver that cannot check an anchor accepts the vertex
// outright. Three ways to be unverifiable — a frontier above the receiver's
// own committed frontier, a frontier that has fallen out of the retained
// window, and no index wired at all — and all three PASS with a root that
// would be rejected outright at a verifiable frontier. Blocking any of them
// would couple vertex acceptance to commit lag.
func TestValidateIndexAnchor_UnverifiableFrontierPasses(t *testing.T) {
	validators, vs := newTestValidatorSet(2)
	produce := newAnchoredProducer(t, validators[1])

	garbage := Hash{0xAB, 0xCD}

	t.Run("future frontier", func(t *testing.T) {
		receiver, _ := newAnchorReceiver(t, vs, validators[0])

		if err := receiver.validateIndexAnchor(produce(5, receiverFrontier+6, garbage)); err != nil {
			t.Fatalf("a vertex anchoring a frontier this node has not committed must pass: %v", err)
		}
	})

	t.Run("outside the retention window", func(t *testing.T) {
		mgr := index.NewManager()
		for round := uint64(1); round <= 1200; round++ {
			mgr.ApplyEdge([32]byte{byte(round), byte(round >> 8)}, index.KeyRootKind, [32]byte{0x01})
			mgr.SetFrontier(round)
		}

		if _, ok := mgr.RootAt(1); ok {
			t.Fatal("round 1 is still retained after 1200 committed rounds: the test no longer covers eviction")
		}

		receiver := New(newTestStorage(t), vs, nil, testSystemPod, 0, validators[0].privKey, nil)
		t.Cleanup(func() { receiver.Close() })
		receiver.SetIndexer(mgr)

		if err := receiver.validateIndexAnchor(produce(1300, 1, garbage)); err != nil {
			t.Fatalf("a frontier older than the retained window must be unverifiable and pass: %v", err)
		}
	})

	t.Run("no indexer wired", func(t *testing.T) {
		receiver := New(newTestStorage(t), vs, nil, testSystemPod, 0, validators[0].privKey, nil)
		t.Cleanup(func() { receiver.Close() })

		if err := receiver.validateIndexAnchor(produce(5, receiverFrontier, garbage)); err != nil {
			t.Fatalf("a node with no index wired can verify nothing and must pass: %v", err)
		}
	})
}

// TestValidateIndexAnchor_ZeroRootGenesisEpochOnly pins the spec §5 tolerance:
// a zero anchor is accepted during the genesis epoch, where no index exists
// yet, and is rejected exactly like a wrong root from the first epoch boundary
// on.
//
// The tolerance keys on the VERTEX's own round, never on the receiver's
// current epoch. The receiver's epoch is not network-uniform at any instant —
// a node that crashed, joined late or stalled trails the live epoch by however
// far its commit cursor trails — so a receiver-relative rule would terminally
// reject a genesis vertex that reaches a further-along node through deep-gap
// recovery or late gossip, and accept a post-boundary liar on a lagging one.
// The two sweeps below hold the vertices fixed and move the receiver's epoch
// underneath them: the verdicts must not move.
func TestValidateIndexAnchor_ZeroRootGenesisEpochOnly(t *testing.T) {
	const epochLength = 10

	validators, vs := newTestValidatorSet(2)
	receiver, _ := newAnchorReceiver(t, vs, validators[0], WithEpochLength(epochLength))
	produce := newAnchoredProducer(t, validators[1])

	// Round 10 is the last round the genesis epoch commits (a nonzero multiple
	// of epochLength commits before the transition), round 11 the first round
	// past the boundary.
	if got := receiver.commitEpochForRound(10); got != 0 {
		t.Fatalf("round 10 commits in epoch %d, want the genesis epoch: the boundary this test straddles moved", got)
	}
	if got := receiver.commitEpochForRound(11); got != 1 {
		t.Fatalf("round 11 commits in epoch %d, want 1: the boundary this test straddles moved", got)
	}

	genesisSide := produce(10, receiverFrontier, Hash{})
	pastBoundary := produce(11, receiverFrontier, Hash{})

	for _, receiverEpoch := range []uint64{0, 1, 5} {
		receiver.commitMu.Lock()
		receiver.setCurrentEpoch(receiverEpoch)
		receiver.commitMu.Unlock()

		if err := receiver.validateIndexAnchor(genesisSide); err != nil {
			t.Fatalf("a zero anchor on a genesis-epoch round must pass on a receiver in epoch %d: %v", receiverEpoch, err)
		}

		err := receiver.validateIndexAnchor(pastBoundary)
		if !errors.Is(err, errIndexRoot) {
			t.Fatalf("a zero anchor on a round past the first boundary must be rejected on a receiver in epoch %d, got: %v", receiverEpoch, err)
		}
	}
}

// TestValidateVertex_WiresIndexAnchorCheck verifies the check is reachable
// from the single validation entry point, not merely callable in isolation,
// and that it runs BEFORE the parent checks: a lying vertex is rejected on its
// root even when its parents would have failed (or buffered) first.
func TestValidateVertex_WiresIndexAnchorCheck(t *testing.T) {
	validators, vs := newTestValidatorSet(2)
	receiver, committed := newAnchorReceiver(t, vs, validators[0])
	produce := newAnchoredProducer(t, validators[1])

	honest := produce(0, receiverFrontier, committed)
	if err := receiver.validateVertex(honest, nil); err != nil {
		t.Fatalf("an honest anchored vertex must pass full validation: %v", err)
	}

	lying := committed
	lying[1] ^= 0xFF

	if err := receiver.validateVertex(produce(0, receiverFrontier, lying), nil); !errors.Is(err, errIndexRoot) {
		t.Fatalf("validateVertex must reject a wrong anchor, got: %v", err)
	}

	// Round 5 with no parents fails the parent checks too; the anchor verdict
	// must be the one that comes back, so a liar can never be buffered and
	// retried instead of rejected.
	if err := receiver.validateVertex(produce(5, receiverFrontier, lying), nil); !errors.Is(err, errIndexRoot) {
		t.Fatalf("the anchor check must precede the parent checks, got: %v", err)
	}
}
