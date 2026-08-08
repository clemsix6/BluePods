package consensus

import (
	"testing"

	"BluePods/internal/index"
	"BluePods/internal/types"
)

// rebuiltIndex builds an index.Manager over one tracker edge and commits it at
// receiverFrontier, standing in for the trees a joining node rebuilds from a
// snapshot. flip inverts one byte of the edge's parent reference, which is
// exactly the shape of the plan's tampered snapshot: the joiner's recomputed
// root then differs from the one the network attests, at that frontier and at
// every later one, because the tracker leaf never becomes correct again.
func rebuiltIndex(t *testing.T, flip bool) (*index.Manager, Hash) {
	t.Helper()

	parent := [32]byte{0x01}
	if flip {
		parent[0] ^= 0xFF
	}

	mgr := index.NewManager()
	mgr.ApplyEdge([32]byte{0x11}, index.KeyRootKind, parent)
	mgr.SetFrontier(receiverFrontier)

	root, ok := mgr.RootAt(receiverFrontier)
	if !ok {
		t.Fatalf("joiner fixture: no root retained at frontier %d", receiverFrontier)
	}

	return mgr, root
}

// newJoinerDAG builds the four-validator DAG a joining node verifies against:
// equal stake, a frozen genesis-epoch committee, and the rebuilt index wired
// as its own recomputation of the snapshot's state.
func newJoinerDAG(t *testing.T, mgr *index.Manager) ([]testValidator, *DAG) {
	t.Helper()

	vals, vs := newTestValidatorSet(4)
	dag := New(newTestStorage(t), vs, nil, testSystemPod, 0, vals[0].privKey, nil)
	t.Cleanup(func() { dag.Close() })
	setEqualStake(dag, vals, 25)

	if mgr != nil {
		dag.SetIndexer(mgr)
	}

	return vals, dag
}

// TestAnchorQuorumSince_HonestSnapshotReachesQuorum is the fail-closed gate's
// pass case: three of four validators attest, each with its own signature, the
// root the joiner recomputed for itself at a frontier at or above the one its
// rebuilt state describes. That is the whole claim a joiner needs before going
// live, and the only condition under which it may.
func TestAnchorQuorumSince_HonestSnapshotReachesQuorum(t *testing.T) {
	mgr, root := rebuiltIndex(t, false)
	vals, dag := newJoinerDAG(t, mgr)

	for _, v := range vals[:3] {
		storeAnchoredVertex(t, dag, v, receiverFrontier+2, receiverFrontier, root)
	}

	bundle, ok := dag.AnchorQuorumSince(receiverFrontier, dag.EpochHolders())
	if !ok {
		t.Fatal("three of four validators attesting the joiner's own recomputed root must reach quorum")
	}

	if bundle.FrontierRound != receiverFrontier {
		t.Errorf("quorum frontier = %d, want %d", bundle.FrontierRound, receiverFrontier)
	}
	if bundle.IndexRoot != root {
		t.Errorf("quorum root = %x, want the joiner's own recomputed root %x", bundle.IndexRoot[:4], root[:4])
	}
	if len(bundle.Headers) != 3 {
		t.Fatalf("quorum carries %d headers, want 3", len(bundle.Headers))
	}
}

// TestAnchorQuorumSince_TamperedSnapshotNeverMatches is the plan's core
// adversarial case: the bootstrap hands the joiner a snapshot with ONE flipped
// tracker parent. The network's honest attestations are unchanged and still
// carry a stake quorum — they just attest a root the joiner cannot reproduce,
// so no frontier ever matches and the joiner must refuse to go live rather
// than serve state no validator ever saw.
func TestAnchorQuorumSince_TamperedSnapshotNeverMatches(t *testing.T) {
	_, honestRoot := rebuiltIndex(t, false)

	tampered, tamperedRoot := rebuiltIndex(t, true)
	if tamperedRoot == honestRoot {
		t.Fatal("fixture: the flipped tracker parent rebuilt the same root, so the case is vacuous")
	}

	vals, dag := newJoinerDAG(t, tampered)

	for _, v := range vals {
		storeAnchoredVertex(t, dag, v, receiverFrontier+2, receiverFrontier, honestRoot)
	}

	if _, ok := dag.AnchorQuorumSince(receiverFrontier, dag.EpochHolders()); ok {
		t.Fatal("a snapshot with a flipped tracker parent must never reach quorum: the joiner would go live on state no validator attested")
	}
}

// TestAnchorQuorumSince_MinorityAborts covers the silent-network case: the
// headers the joiner holds agree with its own root but fall short of the
// capped-stake quorum. Fail-closed means the default outcome is refusal, so a
// minority must not be treated as good enough.
func TestAnchorQuorumSince_MinorityAborts(t *testing.T) {
	mgr, root := rebuiltIndex(t, false)
	vals, dag := newJoinerDAG(t, mgr)

	for _, v := range vals[:2] {
		storeAnchoredVertex(t, dag, v, receiverFrontier+2, receiverFrontier, root)
	}

	if _, ok := dag.AnchorQuorumSince(receiverFrontier, dag.EpochHolders()); ok {
		t.Fatal("two of four validators are below the capped-stake quorum: the joiner must abort, not go live")
	}
}

// TestAnchorQuorumSince_OutsiderStakeDoesNotCount pins the judge: attestations
// from keys outside the trusted validator set carry no weight, however many of
// them a lying bootstrap manufactures. Without this the bootstrap supplies both
// the state and the judge, the exact substitution the trusted checkpoint exists
// to prevent.
func TestAnchorQuorumSince_OutsiderStakeDoesNotCount(t *testing.T) {
	mgr, root := rebuiltIndex(t, false)
	vals, dag := newJoinerDAG(t, mgr)

	storeAnchoredVertex(t, dag, vals[0], receiverFrontier+2, receiverFrontier, root)

	for i := 0; i < 8; i++ {
		storeAnchoredVertex(t, dag, newTestValidator(), receiverFrontier+2, receiverFrontier, root)
	}

	if _, ok := dag.AnchorQuorumSince(receiverFrontier, dag.EpochHolders()); ok {
		t.Fatal("keys outside the trusted set reached quorum: a fabricated committee would pass the gate")
	}
}

// TestAnchorQuorumSince_UnsignedHeaderDoesNotCount checks the signatures are
// verified here rather than assumed from ingress. Snapshot-imported vertices
// reach the store without ever passing ingress validation, so a lying
// bootstrap can plant a record naming an honest producer over a signature that
// producer never made; counting it would let the bootstrap forge the very
// quorum the joiner weighs.
func TestAnchorQuorumSince_UnsignedHeaderDoesNotCount(t *testing.T) {
	mgr, root := rebuiltIndex(t, false)
	vals, dag := newJoinerDAG(t, mgr)

	for _, v := range vals[:2] {
		storeAnchoredVertex(t, dag, v, receiverFrontier+2, receiverFrontier, root)
	}

	// The third producer's vertex, stored with one byte of its signature
	// flipped: a structurally perfect header over a signature that verifies
	// against nothing.
	data, hash := newAnchoredVertices(t, vals[2])(receiverFrontier+2, receiverFrontier, root, nil)
	types.GetRootAsVertex(data, 0).SignatureBytes()[0] ^= 0xFF
	dag.store.add(data, hash, receiverFrontier+2, vals[2].pubKey)

	if _, ok := dag.AnchorQuorumSince(receiverFrontier, dag.EpochHolders()); ok {
		t.Fatal("a broken signature counted toward the quorum: the gate trusts headers it has not verified")
	}
}

// TestAnchorQuorumSince_BelowMinFrontierIgnored pins the freshness floor: a
// quorum at a frontier BELOW the one the joiner's rebuilt state describes
// proves nothing about that state. Accepting it would let a bootstrap satisfy
// the gate with an old, genuinely attested frontier while handing over a
// snapshot it has since diverged from.
func TestAnchorQuorumSince_BelowMinFrontierIgnored(t *testing.T) {
	mgr := index.NewManager()
	mgr.ApplyEdge([32]byte{0x22}, index.KeyRootKind, [32]byte{0x02})
	mgr.SetFrontier(receiverFrontier - 1)

	oldRoot, ok := mgr.RootAt(receiverFrontier - 1)
	if !ok {
		t.Fatal("fixture: no root retained at the earlier frontier")
	}

	mgr.ApplyEdge([32]byte{0x11}, index.KeyRootKind, [32]byte{0x01})
	mgr.SetFrontier(receiverFrontier)

	vals, dag := newJoinerDAG(t, mgr)

	for _, v := range vals {
		storeAnchoredVertex(t, dag, v, receiverFrontier+2, receiverFrontier-1, oldRoot)
	}

	if _, ok := dag.AnchorQuorumSince(receiverFrontier, dag.EpochHolders()); ok {
		t.Fatal("a quorum below the joiner's own rebuilt frontier must not satisfy the gate")
	}
}

// TestAnchorQuorumSince_NoIndexerAborts covers a DAG with no index rebuilt: it
// recomputed nothing, so it can verify nothing and must refuse.
func TestAnchorQuorumSince_NoIndexerAborts(t *testing.T) {
	_, dag := newJoinerDAG(t, nil)

	if _, ok := dag.AnchorQuorumSince(0, dag.EpochHolders()); ok {
		t.Fatal("a DAG with no index rebuilt must never report a verified quorum")
	}
}
