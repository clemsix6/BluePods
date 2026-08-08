package client

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/binary"
	"testing"

	"BluePods/internal/index"
	"BluePods/internal/network"
)

// =============================================================================
// Fixture: a scripted node backed by a real index manager
// =============================================================================
//
// Shared by every test file in this package that needs a committee, a
// scripted node, or a checkpoint to pin — verify_test.go and
// lightclient_test.go both consume it.

// testValidator is one committee member: the key that signs vertex headers and
// the leaf the validator tree hashes for it.
type testValidator struct {
	pub  ed25519.PublicKey  // pub is the producer identity inside a header
	priv ed25519.PrivateKey // priv signs the vertex identity
	leaf index.ValidatorLeaf
}

// newCommittee returns n validators, each carrying the same capped weight, so
// a quorum is a plain member count and a test's arithmetic is obvious.
func newCommittee(t *testing.T, n int, stake uint64) []testValidator {
	t.Helper()

	out := make([]testValidator, 0, n)

	for i := 0; i < n; i++ {
		pub, priv, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatalf("generate key: %v", err)
		}

		var pubkey [32]byte
		copy(pubkey[:], pub)

		out = append(out, testValidator{
			pub:  pub,
			priv: priv,
			leaf: index.ValidatorLeaf{Pubkey: pubkey, CappedStake: stake, Status: index.ValidatorActive},
		})
	}

	return out
}

// leavesOf returns a committee's validator leaves.
func leavesOf(committee []testValidator) []index.ValidatorLeaf {
	out := make([]index.ValidatorLeaf, len(committee))
	for i, v := range committee {
		out[i] = v.leaf
	}

	return out
}

// headerRecord builds one bundle record the way a producer does: the NORMATIVE
// 120-byte header (internal/consensus/header.go) followed by the producer's
// signature over BLAKE3(0x01 || header). It is written out field by field on
// purpose — this is the external reimplementation the wire contract is
// addressed to, and TestAnchorHeader_GoldenLayout pins it to the same vector
// consensus pins itself to.
func headerRecord(v testValidator, round, epoch, frontier uint64, root [32]byte) []byte {
	var bodyHash [32]byte
	bodyHash[0] = 0xEE

	header := make([]byte, 0, anchorHeaderSize)
	header = append(header, v.pub...)
	header = binary.BigEndian.AppendUint64(header, round)
	header = binary.BigEndian.AppendUint64(header, epoch)
	header = binary.BigEndian.AppendUint64(header, frontier)
	header = append(header, root[:]...)
	header = append(header, bodyHash[:]...)

	identity := headerIdentity(header)

	return append(header, ed25519.Sign(v.priv, identity[:])...)
}

// fixture is a scripted node: a real index.Manager serving proved answers, a
// committee whose members sign the anchor bundle, and the knobs a test uses to
// script what that node says.
type fixture struct {
	mgr       *index.Manager
	epoch     uint64          // epoch is the epoch the served validator tree describes
	signers   []testValidator // signers are the producers whose headers ride in the bundle
	headers   uint64          // headers is the epoch the produced headers carry
	duplicate bool            // duplicate repeats the first signer's record

	// stragglerEpoch, when nonzero, makes the LAST signer in signers claim
	// this epoch instead of headers while still attesting the same genuine
	// (frontier, root) pair as everyone else — one committee member diverging
	// from an otherwise unanimous, quorate majority.
	stragglerEpoch uint64

	owner  [32]byte // owner is the key the fixture's object tree hangs off
	child  [32]byte // child is the object parented to owner
	other  [32]byte // other is a second object parented to owner
	nested [32]byte // nested is the object parented to child
	name   string   // name is the registered domain name
}

// newFixture builds the node's state: one registered name, a two-level object
// tree under owner, and committee A frozen as epoch 1's validator tree,
// committed at frontier 10.
func newFixture(t *testing.T, committee []testValidator) *fixture {
	t.Helper()

	f := &fixture{
		mgr:     index.NewManager(),
		epoch:   1,
		signers: committee,
		headers: 1,
		owner:   [32]byte{0x0F},
		child:   [32]byte{0xA1},
		other:   [32]byte{0xC3},
		nested:  [32]byte{0xB2},
		name:    "demo.config",
	}

	f.mgr.ApplyDomain(f.name, [32]byte{0x11}, f.owner, 100)
	f.mgr.ApplyEdge(f.child, index.KeyRootKind, f.owner)
	f.mgr.ApplyEdge(f.other, index.KeyRootKind, f.owner)
	f.mgr.ApplyEdge(f.nested, index.ObjectParentKind, f.child)
	f.mgr.RebuildValidators(leavesOf(committee))
	f.mgr.SetFrontier(10)

	return f
}

// GetIndexAnchor serves the bundle: the manager's committed frontier and root,
// attested by the scripted signers at the scripted header epoch.
func (f *fixture) GetIndexAnchor() (*network.GetIndexAnchorResponse, error) {
	round, root := f.mgr.CommittedFrontier()

	records := make([][]byte, 0, len(f.signers)+1)
	for i, v := range f.signers {
		epoch := f.headers
		if f.stragglerEpoch != 0 && i == len(f.signers)-1 {
			epoch = f.stragglerEpoch
		}

		records = append(records, headerRecord(v, round+2, epoch, round, root))
	}

	if f.duplicate && len(records) > 0 {
		records = append(records, records[0])
	}

	return &network.GetIndexAnchorResponse{
		Found:         true,
		FrontierRound: round,
		IndexRoot:     root,
		Epoch:         f.headers,
		Headers:       records,
	}, nil
}

// GetValidatorTree serves the current tree, and only for the epoch it
// describes — the manager keeps no versioned validator trees.
func (f *fixture) GetValidatorTree(epoch uint64) (*network.GetValidatorTreeResponse, error) {
	answer := f.mgr.ValidatorSet()

	resp := &network.GetValidatorTreeResponse{Anchor: wireAnchor(answer.Anchor), Epoch: f.epoch}
	if epoch == f.epoch {
		resp.Found = true
		resp.Leaves = answer.Values
	}

	return resp, nil
}

// ResolveDomainProved serves a proved resolution.
func (f *fixture) ResolveDomainProved(name string) (*network.DomainResolveResponse, error) {
	answer := f.mgr.ResolveDomain(name)

	return &network.DomainResolveResponse{
		Anchor: wireAnchor(answer.Anchor),
		Found:  len(answer.Value) != 0,
		Leaf:   answer.Value,
		Proof:  answer.Proof.Serialize(),
	}, nil
}

// ListChildren serves a proved enumeration.
func (f *fixture) ListChildren(parent [32]byte) (*network.ListChildrenResponse, error) {
	answer := f.mgr.ListChildren(parent)

	return &network.ListChildrenResponse{
		Anchor:      wireAnchor(answer.Anchor),
		Found:       answer.Found,
		SubtreeRoot: answer.SubtreeRoot,
		Proof:       answer.Proof.Serialize(),
		Children:    answer.Children,
	}, nil
}

// GetAncestors serves a proved ancestry walk.
func (f *fixture) GetAncestors(object [32]byte) (*network.GetAncestorsResponse, error) {
	answer := f.mgr.Ancestors(object)

	edges := make([]network.AncestorEdge, len(answer.Edges))
	for i, e := range answer.Edges {
		edges[i] = network.AncestorEdge{ChildID: e.ChildID, Leaf: e.Value, Proof: e.Proof.Serialize()}
	}

	return &network.GetAncestorsResponse{Anchor: wireAnchor(answer.Anchor), Edges: edges}, nil
}

// wireAnchor converts an index anchor into the block every proved response
// opens with, exactly as cmd/node does.
func wireAnchor(a index.Anchor) network.ProvedIndexAnchor {
	return network.ProvedIndexAnchor{
		Anchored:      a.Anchored,
		FrontierRound: a.Round,
		IndexRoot:     a.Roots.Combined,
		DomainRoot:    a.Roots.Domain,
		ParentRoot:    a.Roots.Parent,
		ChildrenRoot:  a.Roots.Children,
		ValidatorRoot: a.Roots.Validator,
	}
}

// checkpointOf pins the fixture's current state as a light client's trust
// anchor.
func (f *fixture) checkpointOf(committee []testValidator) Checkpoint {
	_, root := f.mgr.CommittedFrontier()

	return Checkpoint{
		Epoch:            f.epoch,
		IndexRoot:        root,
		ValidatorSetHash: index.ValidatorRootOf(leavesOf(committee)),
	}
}
