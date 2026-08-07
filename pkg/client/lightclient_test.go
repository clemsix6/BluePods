package client

import (
	"testing"
	"time"

	"BluePods/internal/index"
	"BluePods/internal/network"
)

// attestedFor returns the fixture's current committed root as a verified
// anchor, for the checks that exercise one answer rather than the whole
// client.
func attestedFor(f *fixture) VerifiedAnchor {
	round, root := f.mgr.CommittedFrontier()

	return VerifiedAnchor{FrontierRound: round, IndexRoot: root, Epoch: f.epoch}
}

// crossBoundary moves the fixture's node past an epoch boundary: the new
// committee is frozen into the validator tree, the index commits at the next
// frontier, and the producers' headers start naming the new epoch — the state
// the spec §5 handoff rule describes.
func crossBoundary(f *fixture, next []testValidator) {
	f.mgr.RebuildValidators(leavesOf(next))
	f.mgr.SetFrontier(11)
	f.epoch++
	f.headers = f.epoch
}

// TestLightClient_WalksAnEpochBoundaryAndVerifiesADomainProof is the whole
// spec §5 read path from a light client's seat, in one run:
//
//	a checkpoint at epoch N
//	  -> a bundle whose headers carry epoch N+1 inside the boundary window,
//	     weighed by the epoch-N committee (the handoff rule)
//	  -> that first N+1-attested root proves the NEW committee, which the
//	     client re-pins its checkpoint to
//	  -> a domain proof verified against the bundle, under the new committee.
//
// Nothing the serving node says is taken on trust at any step: the committee
// comes out of the index root, the root comes out of the signed headers, and
// the leaf comes out of a proof folding to that root.
func TestLightClient_WalksAnEpochBoundaryAndVerifiesADomainProof(t *testing.T) {
	epochN := newCommittee(t, 4, 100)
	f := newFixture(t, epochN)

	lc := &LightClient{src: f, checkpoint: f.checkpointOf(epochN)}

	// The checkpoint's own epoch: the committee is authenticated from the
	// pinned index root before it weighs anything.
	if _, err := lc.Anchor(); err != nil {
		t.Fatalf("anchor at the checkpointed epoch: %v", err)
	}

	// The boundary. The new committee shares three members with the old one
	// (churn is capped, the sets overlap by construction) and the headers now
	// name epoch N+1 while the client still holds only the epoch-N committee.
	epochNext := append(append([]testValidator{}, epochN[1:]...), newCommittee(t, 1, 100)...)
	crossBoundary(f, epochNext)
	f.signers = epochN[:3]

	attested, err := lc.Anchor()
	if err != nil {
		t.Fatalf("the epoch-N committee did not weigh the N+1 headers: %v", err)
	}

	if attested.Epoch != f.epoch {
		t.Fatalf("attested epoch = %d, want %d", attested.Epoch, f.epoch)
	}

	// The attested root proved the new committee, so the checkpoint moved.
	walked := lc.Checkpoint()
	if walked.Epoch != f.epoch || walked.IndexRoot != attested.IndexRoot {
		t.Fatalf("checkpoint did not walk: %+v, want epoch %d root %x", walked, f.epoch, attested.IndexRoot[:4])
	}

	if walked.ValidatorSetHash != index.ValidatorRootOf(leavesOf(epochNext)) {
		t.Fatal("the walked checkpoint pins a committee the attested root does not commit to")
	}

	// A proved read under the new committee.
	f.signers = epochNext[:3]

	leaf, found, err := lc.ResolveDomain(f.name)
	if err != nil || !found {
		t.Fatalf("proved resolution after the walk: found=%v err=%v", found, err)
	}

	if leaf.Name != f.name || leaf.Owner != f.owner {
		t.Fatalf("proved leaf: %+v", leaf)
	}

	// And the teeth: the member the boundary dropped can no longer attest
	// anything, alone or otherwise. The old committee is not the judge any more.
	f.signers = epochN[:1]

	if _, _, err := lc.ResolveDomain(f.name); err == nil {
		t.Fatal("a bundle signed by a dropped validator was accepted after the walk")
	}
}

// TestLightClient_RefusesACommitteeTheCheckpointDoesNotPin verifies the
// bootstrap link: a node serving some other committee for the checkpointed
// epoch is refused before it can weigh a single quorum, which is the whole
// point of pinning something out of band.
func TestLightClient_RefusesACommitteeTheCheckpointDoesNotPin(t *testing.T) {
	epochN := newCommittee(t, 4, 100)
	f := newFixture(t, epochN)

	pinned := f.checkpointOf(epochN)

	// The node swaps the committee under the client's feet, index root and all.
	f.mgr.RebuildValidators(leavesOf(newCommittee(t, 4, 100)))
	f.mgr.SetFrontier(11)

	lc := &LightClient{src: f, checkpoint: pinned}

	if _, err := lc.Anchor(); err == nil {
		t.Fatal("a committee neither the pinned root nor the pinned hash covers was accepted")
	}
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
