package client

import (
	"testing"
	"time"

	"BluePods/internal/index"
	"BluePods/internal/network"
)

// The proved-read verbs (ResolveDomain, ListChildren, Ancestors) and the
// freshness primitive (WaitForFrontier). The epoch walk and checkpoint
// authentication have their own tests in lightclient_test.go.

// attestedFor returns the fixture's current committed root as a verified
// anchor, for the checks that exercise one answer rather than the whole
// client.
func attestedFor(f *fixture) VerifiedAnchor {
	round, root := f.mgr.CommittedFrontier()

	return VerifiedAnchor{FrontierRound: round, IndexRoot: root, Epoch: f.epoch}
}

// advancingSource is a fixture whose committed frontier advances while a
// client polls it, the shape a live node has.
type advancingSource struct {
	*fixture

	calls     int    // calls counts the bundle requests made so far
	advanceAt int    // advanceAt is the request that moves the frontier
	to        uint64 // to is the frontier it moves to
}

// GetIndexAnchor advances the fixture's frontier on the scripted request, then
// serves the bundle for whatever frontier is current.
func (s *advancingSource) GetIndexAnchor() (*network.GetIndexAnchorResponse, error) {
	s.calls++

	if s.calls == s.advanceAt {
		s.fixture.mgr.SetFrontier(s.to)
	}

	return s.fixture.GetIndexAnchor()
}

// TestLightClient_WaitForFrontierReturnsOnTheCoveringBundle verifies the
// freshness primitive: a client that just saw its transaction finalize at a
// round waits for a bundle attesting that round or later, and returns as soon
// as one exists rather than on a fixed delay.
func TestLightClient_WaitForFrontierReturnsOnTheCoveringBundle(t *testing.T) {
	committee := newCommittee(t, 4, 100)
	f := newFixture(t, committee)

	src := &advancingSource{fixture: f, advanceAt: 3, to: 11}
	lc := &LightClient{src: src, checkpoint: f.checkpointOf(committee)}

	// Already covered: the bundle at frontier 10 answers a wait for round 10
	// on the first poll.
	if _, err := lc.WaitForFrontier(10, time.Second); err != nil {
		t.Fatalf("wait for an already-attested frontier: %v", err)
	}

	before := src.calls

	attested, err := lc.WaitForFrontier(11, 5*time.Second)
	if err != nil {
		t.Fatalf("wait for frontier 11: %v", err)
	}

	if attested.FrontierRound < 11 {
		t.Fatalf("returned a bundle at frontier %d, want 11 or later", attested.FrontierRound)
	}

	if src.calls-before < 2 {
		t.Fatalf("returned after %d polls without the frontier having moved", src.calls-before)
	}
}

// TestLightClient_UnanchoredAnswerIsRetriedNotTrusted verifies the spec §5
// live unproven read is reported as such: between a tree mutation and the
// commit that records it, no bundle for that root can exist, and the answer
// must come back as unverifiable rather than as an answer.
func TestLightClient_UnanchoredAnswerIsRetriedNotTrusted(t *testing.T) {
	committee := newCommittee(t, 4, 100)
	f := newFixture(t, committee)

	lc := &LightClient{src: f, checkpoint: f.checkpointOf(committee)}

	// A mutation with no SetFrontier behind it: the trees are now ahead of
	// every committed round.
	f.mgr.ApplyDomain("late.config", [32]byte{0x22}, f.owner, 100)

	if _, _, err := lc.ResolveDomain("late.config"); err == nil {
		t.Fatal("an answer taken against uncommitted tree state was accepted")
	}
}

// TestLightClient_EnumerationMustBeComplete verifies the completeness half of
// a proved enumeration: the streamed leaves are unauthenticated on their own,
// so a server that withholds one must be caught by the subtree root the top
// tree proves — at any set size, with no threshold below which the stream is
// taken on trust.
func TestLightClient_EnumerationMustBeComplete(t *testing.T) {
	committee := newCommittee(t, 4, 100)
	f := newFixture(t, committee)

	lc := &LightClient{src: f, checkpoint: f.checkpointOf(committee)}

	children, err := lc.ListChildren(f.owner)
	if err != nil {
		t.Fatalf("proved enumeration: %v", err)
	}

	if len(children) != 2 {
		t.Fatalf("enumerated %d children, want 2", len(children))
	}

	resp, err := f.ListChildren(f.owner)
	if err != nil {
		t.Fatalf("children: %v", err)
	}

	resp.Children = resp.Children[:1]

	if _, err := attestedFor(f).VerifyChildren(resp, f.owner); err == nil {
		t.Fatal("a withheld child leaf was accepted as a complete enumeration")
	}
}

// TestLightClient_AncestryMustBeOneChain verifies the chaining requirement
// stated on network.GetAncestorsResponse: every edge below is individually
// proved against the attested parent root, and the walk is still a forgery,
// because the second hop belongs to an unrelated object. Verifying each edge
// on its own accepts it; chaining each hop's own parent reference into the
// next edge's key is what rejects it.
func TestLightClient_AncestryMustBeOneChain(t *testing.T) {
	committee := newCommittee(t, 4, 100)
	f := newFixture(t, committee)

	lc := &LightClient{src: f, checkpoint: f.checkpointOf(committee)}

	chain, err := lc.Ancestors(f.nested)
	if err != nil {
		t.Fatalf("proved ancestry: %v", err)
	}

	if len(chain) != 2 || chain[0].Parent != f.child || chain[1].ParentKind != index.KeyRootKind {
		t.Fatalf("walk did not terminate at the owner key: %+v", chain)
	}

	spliced, err := f.GetAncestors(f.nested)
	if err != nil {
		t.Fatalf("ancestors: %v", err)
	}

	unrelated, err := f.GetAncestors(f.other)
	if err != nil {
		t.Fatalf("ancestors: %v", err)
	}

	spliced.Edges[1] = unrelated.Edges[0]

	attested := attestedFor(f)

	// The spliced edge is genuinely proved on its own, which is exactly why a
	// per-edge check is not enough.
	if err := attested.VerifyProof(spliced.Anchor, ParentComponent,
		unrelated.Edges[0].ChildID[:], unrelated.Edges[0].Leaf, unrelated.Edges[0].Proof); err != nil {
		t.Fatalf("the spliced edge is not individually valid, so the test proves nothing: %v", err)
	}

	if _, err := attested.VerifyAncestry(spliced, f.nested); err == nil {
		t.Fatal("a walk spliced from unrelated but individually proved edges was accepted")
	}
}

// nilAnswerSource wraps a fixture but returns (nil, nil) from every proved
// read — no error, but no answer either. That is the defensive case F7 (see
// pkg/client review) exists for: a transport implementation that forgets its
// own contract must be met with an error, not a nil-pointer dereference.
type nilAnswerSource struct {
	*fixture
}

// ResolveDomainProved always answers with nothing.
func (nilAnswerSource) ResolveDomainProved(string) (*network.DomainResolveResponse, error) {
	return nil, nil
}

// ListChildren always answers with nothing.
func (nilAnswerSource) ListChildren([32]byte) (*network.ListChildrenResponse, error) {
	return nil, nil
}

// GetAncestors always answers with nothing.
func (nilAnswerSource) GetAncestors([32]byte) (*network.GetAncestorsResponse, error) {
	return nil, nil
}

// TestLightClient_NilAnswerIsAnErrorNotAPanic verifies ResolveDomain,
// ListChildren and Ancestors all guard a nil answer before touching it.
// VerifyDomain, VerifyChildren and VerifyAncestry already guard resp == nil,
// but each LightClient method dereferences resp.Anchor to call attest before
// ever reaching them — so without its own guard, the LightClient itself is
// what panics, not the verifier.
func TestLightClient_NilAnswerIsAnErrorNotAPanic(t *testing.T) {
	committee := newCommittee(t, 4, 100)
	f := newFixture(t, committee)

	lc := &LightClient{src: nilAnswerSource{fixture: f}, checkpoint: f.checkpointOf(committee)}

	if _, _, err := lc.ResolveDomain(f.name); err == nil {
		t.Fatal("a nil domain answer was accepted")
	}

	if _, err := lc.ListChildren(f.owner); err == nil {
		t.Fatal("a nil children answer was accepted")
	}

	if _, err := lc.Ancestors(f.nested); err == nil {
		t.Fatal("a nil ancestry answer was accepted")
	}
}
