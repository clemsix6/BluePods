package client

import (
	"errors"
	"testing"

	"BluePods/internal/index"
	"BluePods/internal/network"
)

// The epoch walk and checkpoint authentication. The proved-read verbs
// (ResolveDomain, ListChildren, Ancestors) and the freshness primitive
// (WaitForFrontier) have their own tests in lightclient_reads_test.go.

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

// failingValidatorTreeSource wraps a fixture and turns GetValidatorTree into
// a plain transport failure at one scripted epoch, leaving every other epoch
// (and every other call) to the underlying fixture.
type failingValidatorTreeSource struct {
	*fixture

	failAt uint64 // failAt is the epoch whose request errors
}

// GetValidatorTree fails for failAt and delegates otherwise.
func (s *failingValidatorTreeSource) GetValidatorTree(epoch uint64) (*network.GetValidatorTreeResponse, error) {
	if epoch == s.failAt {
		return nil, errors.New("dial validator tree: connection refused")
	}

	return s.fixture.GetValidatorTree(epoch)
}

// TestLightClient_AdvanceErrorDistinguishesTransportFailureFromNoNeed
// verifies the two states AdvanceError exists to tell apart. Right after
// construction, before Anchor has ever run, there is nothing to report: nil.
// At the checkpoint's own epoch, Anchor has no epoch to walk to: also nil —
// "didn't need to". Once a newer epoch is attested and the walk's own
// transport call fails, AdvanceError must turn non-nil — "could not" — and
// Anchor itself must still succeed, since declining to advance costs nothing
// immediately (see Anchor), and the checkpoint must stay exactly where it was.
func TestLightClient_AdvanceErrorDistinguishesTransportFailureFromNoNeed(t *testing.T) {
	epochN := newCommittee(t, 4, 100)
	f := newFixture(t, epochN)

	pinned := f.checkpointOf(epochN)
	src := &failingValidatorTreeSource{fixture: f}
	lc := &LightClient{src: src, checkpoint: pinned}

	if err := lc.AdvanceError(); err != nil {
		t.Fatalf("AdvanceError before any call: %v, want nil", err)
	}

	if _, err := lc.Anchor(); err != nil {
		t.Fatalf("anchor at the checkpointed epoch: %v", err)
	}

	if err := lc.AdvanceError(); err != nil {
		t.Fatalf("AdvanceError with nothing to walk to: %v, want nil", err)
	}

	epochNext := append(append([]testValidator{}, epochN[1:]...), newCommittee(t, 1, 100)...)
	crossBoundary(f, epochNext)
	f.signers = epochN[:3]
	src.failAt = f.epoch

	attested, err := lc.Anchor()
	if err != nil {
		t.Fatalf("anchor still verifies despite the walk's own transport failure: %v", err)
	}

	if attested.Epoch != f.epoch {
		t.Fatalf("attested epoch = %d, want %d", attested.Epoch, f.epoch)
	}

	if err := lc.AdvanceError(); err == nil {
		t.Fatal("AdvanceError is nil after the walk's transport call failed")
	}

	if walked := lc.Checkpoint(); walked != pinned {
		t.Fatalf("checkpoint moved to %+v despite the walk failing, want it to stay pinned at %+v", walked, pinned)
	}
}

// TestLightClient_OneByzantineHeaderDoesNotForceTheEpochWalk is
// TestVerifyAnchor_EpochIsWhatItsOwnQuorumSays carried through to the walk it
// would otherwise trigger: three of the four checkpointed committee members
// sign the genuine (frontier, root) pair at epoch N, and the fourth alone
// signs the SAME pair claiming N+1. The bundle still verifies — the N subset
// alone already carries the committee's quorum — but the epoch walk must not
// follow the lone byzantine label. A client that attributed the anchor to
// N+1 here would pin Checkpoint{Epoch: N+1} to a committee it never
// authenticated and PERSIST it, sliding the handoff window to {N+1, N+2} and
// making every subsequent genuine N header stop counting.
func TestLightClient_OneByzantineHeaderDoesNotForceTheEpochWalk(t *testing.T) {
	committee := newCommittee(t, 4, 100)
	f := newFixture(t, committee)
	f.stragglerEpoch = f.epoch + 1

	pinned := f.checkpointOf(committee)
	lc := &LightClient{src: f, checkpoint: pinned}

	attested, err := lc.Anchor()
	if err != nil {
		t.Fatalf("a quorate bundle mixing an honest N-majority with one byzantine N+1 header was refused outright: %v", err)
	}

	if attested.Epoch != f.epoch {
		t.Fatalf("attested epoch = %d, want %d", attested.Epoch, f.epoch)
	}

	if walked := lc.Checkpoint(); walked != pinned {
		t.Fatalf("checkpoint moved to %+v off one byzantine header, want it to stay pinned at %+v", walked, pinned)
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

// TestLightClient_FallbackRejectsAWrongCommitteeEvenWhenItSelfAttests is the
// missing witness for authenticate's fallback link (verify.go, the
// ValidatorSetHash branch below the index-root strong link). The sibling
// test above happens to fail for an unrelated reason: its fixture keeps
// signing the bundle with the OLD committee, so VerifyAnchor's own membership
// check would refuse it downstream even if the fallback accepted the served
// committee outright — the fallback itself is never exercised as the thing
// that blocks the substitution.
//
// Here the served committee is entirely self-consistent: it is also the one
// that signs the bundle, at a quorum. Nothing downstream of authenticate
// could tell it apart from a real handoff. The node's index has also moved
// past the checkpointed root (SetFrontier past the pinned round), which
// forces authenticate past the strong link and into the fallback — so only
// the checkpoint's pinned ValidatorSetHash can catch the substitution.
func TestLightClient_FallbackRejectsAWrongCommitteeEvenWhenItSelfAttests(t *testing.T) {
	original := newCommittee(t, 4, 100)
	f := newFixture(t, original)

	pinned := f.checkpointOf(original)

	wrong := newCommittee(t, 4, 100)
	f.mgr.RebuildValidators(leavesOf(wrong))
	f.mgr.SetFrontier(11)
	f.signers = wrong

	lc := &LightClient{src: f, checkpoint: pinned}

	if _, err := lc.Anchor(); err == nil {
		t.Fatal("a wrong committee that signs its own quorate bundle was authenticated against a checkpoint pinning a different one")
	}
}
