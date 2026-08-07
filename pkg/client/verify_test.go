package client

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"testing"

	"BluePods/internal/index"
	"BluePods/internal/network"
)

// =============================================================================
// Fixture: a scripted node backed by a real index manager
// =============================================================================

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
	for _, v := range f.signers {
		records = append(records, headerRecord(v, round+2, f.headers, round, root))
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

// =============================================================================
// The header wire contract
// =============================================================================

// TestAnchorHeader_GoldenLayout pins this package's reimplementation of the
// vertex header to the SAME fixed vector internal/consensus pins its own
// encoding to (TestVertexHeader_GoldenLayout). A light client that reorders a
// field or drops the domain tag does not fail loudly — it silently disagrees
// with every node on the network, so the two vectors must be the same bytes.
func TestAnchorHeader_GoldenLayout(t *testing.T) {
	var producer, indexRoot, bodyHash [32]byte
	for i := range producer {
		producer[i] = 0x01
		indexRoot[i] = 0x02
		bodyHash[i] = 0x03
	}

	header := make([]byte, 0, anchorHeaderSize)
	header = append(header, producer[:]...)
	header = binary.BigEndian.AppendUint64(header, 1234)
	header = binary.BigEndian.AppendUint64(header, 5)
	header = binary.BigEndian.AppendUint64(header, 1200)
	header = append(header, indexRoot[:]...)
	header = append(header, bodyHash[:]...)

	const wantBytes = "" +
		"0101010101010101010101010101010101010101010101010101010101010101" +
		"00000000000004d2" +
		"0000000000000005" +
		"00000000000004b0" +
		"0202020202020202020202020202020202020202020202020202020202020202" +
		"0303030303030303030303030303030303030303030303030303030303030303"

	if got := hex.EncodeToString(header); got != wantBytes {
		t.Fatalf("header encoding =\n%s\nwant\n%s", got, wantBytes)
	}

	if len(header) != anchorHeaderSize {
		t.Fatalf("header is %d bytes, want %d", len(header), anchorHeaderSize)
	}

	const wantIdentity = "369460b53e5d185da3b58be53018407b0683c7498b893c6ad73709a950c89f77"

	identity := headerIdentity(header)
	if got := hex.EncodeToString(identity[:]); got != wantIdentity {
		t.Fatalf("vertex identity = %s, want %s", got, wantIdentity)
	}
}

// TestParseAnchorRecord_ReadsTheSignedFields verifies the parser reads back
// every field the producer signed, and refuses a record whose signature does
// not cover the header it travels with.
func TestParseAnchorRecord_ReadsTheSignedFields(t *testing.T) {
	v := newCommittee(t, 1, 100)[0]
	root := [32]byte{0x77, 0x88}

	record := headerRecord(v, 42, 7, 40, root)

	header, err := parseAnchorRecord(record)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	if header.Epoch != 7 || header.FrontierRound != 40 || header.IndexRoot != root {
		t.Fatalf("fields lost: %+v", header)
	}

	if header.Producer != v.leaf.Pubkey {
		t.Fatalf("producer = %x, want %x", header.Producer[:4], v.leaf.Pubkey[:4])
	}

	tampered := append([]byte(nil), record...)
	tampered[anchorHeaderSize-1] ^= 0xFF

	if _, err := parseAnchorRecord(tampered); err == nil {
		t.Fatal("a record whose header was edited after signing parsed cleanly")
	}
}

// =============================================================================
// VerifyAnchor: the quorum weighing
// =============================================================================

// bundleFrom returns the fixture's bundle attested by the given signers.
func bundleFrom(t *testing.T, f *fixture, signers []testValidator) *network.GetIndexAnchorResponse {
	t.Helper()

	saved := f.signers
	f.signers = signers

	bundle, err := f.GetIndexAnchor()
	if err != nil {
		t.Fatalf("bundle: %v", err)
	}

	f.signers = saved

	return bundle
}

// TestVerifyAnchor_QuorumAttestsTheRoot verifies a bundle carrying two thirds
// of the committee's capped stake yields the attested (frontier, root) pair.
func TestVerifyAnchor_QuorumAttestsTheRoot(t *testing.T) {
	committee := newCommittee(t, 4, 100)
	f := newFixture(t, committee)

	round, root := f.mgr.CommittedFrontier()
	set := ValidatorSet{Epoch: f.epoch, Leaves: leavesOf(committee)}

	attested, err := VerifyAnchor(bundleFrom(t, f, committee[:3]), set)
	if err != nil {
		t.Fatalf("three of four validators did not reach quorum: %v", err)
	}

	if attested.FrontierRound != round || attested.IndexRoot != root {
		t.Fatalf("attested %d/%x, want %d/%x", attested.FrontierRound, attested.IndexRoot[:4], round, root[:4])
	}

	if attested.Epoch != f.headers {
		t.Fatalf("attested epoch = %d, want %d", attested.Epoch, f.headers)
	}
}

// TestVerifyAnchor_BelowQuorumIsRefused is the check that separates a verifier
// from a credulous decoder: two of four equal-weight validators sign a bundle
// whose headers are genuine, individually valid and all agree. Everything
// about it verifies except the one thing that matters — they carry half the
// committee's capped stake, not two thirds. An implementation that counts
// headers, or takes the first valid one, accepts this bundle; a minority is
// then free to attest any root it likes to every light client.
func TestVerifyAnchor_BelowQuorumIsRefused(t *testing.T) {
	committee := newCommittee(t, 4, 100)
	f := newFixture(t, committee)

	set := ValidatorSet{Epoch: f.epoch, Leaves: leavesOf(committee)}

	if _, err := VerifyAnchor(bundleFrom(t, f, committee[:2]), set); err == nil {
		t.Fatal("a bundle carrying half the committee's capped stake was accepted as attested")
	}
}

// TestVerifyAnchor_DuplicateProducerCountsOnce verifies a producer's weight is
// counted once however many records it contributes: padding a sub-quorum
// bundle with copies of one member's header is the cheapest possible forgery.
func TestVerifyAnchor_DuplicateProducerCountsOnce(t *testing.T) {
	committee := newCommittee(t, 4, 100)
	f := newFixture(t, committee)
	f.duplicate = true

	set := ValidatorSet{Epoch: f.epoch, Leaves: leavesOf(committee)}

	if _, err := VerifyAnchor(bundleFrom(t, f, committee[:2]), set); err == nil {
		t.Fatal("a bundle padded with a repeated producer was accepted as attested")
	}
}

// TestVerifyAnchor_OutsiderSignaturesCarryNoWeight verifies a bundle signed by
// keys outside the committee is refused however many of them there are: the
// weight comes from the authenticated committee, never from the count of valid
// signatures.
func TestVerifyAnchor_OutsiderSignaturesCarryNoWeight(t *testing.T) {
	committee := newCommittee(t, 4, 100)
	f := newFixture(t, committee)

	set := ValidatorSet{Epoch: f.epoch, Leaves: leavesOf(committee)}

	if _, err := VerifyAnchor(bundleFrom(t, f, newCommittee(t, 9, 100)), set); err == nil {
		t.Fatal("nine outsiders were accepted as a quorum of a four-member committee")
	}
}

// TestVerifyAnchor_HeaderMustRepeatTheBundleClaim verifies the serving node's
// own summary fields are worth nothing: a bundle whose headers attest one root
// while the response claims another is refused, since only what a producer
// signed is evidence.
func TestVerifyAnchor_HeaderMustRepeatTheBundleClaim(t *testing.T) {
	committee := newCommittee(t, 4, 100)
	f := newFixture(t, committee)

	bundle := bundleFrom(t, f, committee)
	bundle.IndexRoot = [32]byte{0xDE, 0xAD}

	set := ValidatorSet{Epoch: f.epoch, Leaves: leavesOf(committee)}

	if _, err := VerifyAnchor(bundle, set); err == nil {
		t.Fatal("a bundle claiming a root none of its headers signed was accepted")
	}
}

// TestVerifyAnchor_EpochWindowIsTheHandoffRule verifies the spec §5 window: a
// committee at epoch N weighs headers at N and at N+1 (churn is capped, the
// sets overlap by construction) and nothing further out, which is the boundary
// where a stale checkpoint must be refused rather than stretched.
func TestVerifyAnchor_EpochWindowIsTheHandoffRule(t *testing.T) {
	committee := newCommittee(t, 4, 100)
	f := newFixture(t, committee)

	set := ValidatorSet{Epoch: f.epoch, Leaves: leavesOf(committee)}

	f.headers = f.epoch + 1
	attested, err := VerifyAnchor(bundleFrom(t, f, committee), set)
	if err != nil {
		t.Fatalf("headers one epoch ahead were not weighed by the current committee: %v", err)
	}

	if attested.Epoch != f.epoch+1 {
		t.Fatalf("attested epoch = %d, want %d", attested.Epoch, f.epoch+1)
	}

	f.headers = f.epoch + 2
	if _, err := VerifyAnchor(bundleFrom(t, f, committee), set); err == nil {
		t.Fatal("headers two epochs ahead were weighed by a stale committee")
	}
}

// =============================================================================
// VerifyProof: binding a proof to what the quorum signed
// =============================================================================

// TestVerifyProof_BindsTheComponentRootsToTheAttestedRoot is the second half of
// what makes a proof evidence. A proof folds a key to ONE component root, and
// the serving node picks the component roots it hands out — so a proof checked
// against them alone is a proof against a number the node invented. Here the
// answer is internally perfect (its proof folds to its own domain root) but its
// component roots do not combine to the root the quorum signed, and it must be
// refused.
func TestVerifyProof_BindsTheComponentRootsToTheAttestedRoot(t *testing.T) {
	committee := newCommittee(t, 4, 100)
	f := newFixture(t, committee)

	resp, err := f.ResolveDomainProved(f.name)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	attested := VerifiedAnchor{FrontierRound: 10, IndexRoot: [32]byte{0xC0, 0xFF, 0xEE}}

	if err := attested.VerifyProof(resp.Anchor, DomainComponent, []byte(f.name), resp.Leaf, resp.Proof); err == nil {
		t.Fatal("a proof folding to component roots that combine to another index root was accepted")
	}
}

// TestVerifyProof_AcceptsAnAnswerUnderTheAttestedRoot verifies the same answer
// passes once the attested root is the one its components combine to, and that
// the leaf it authenticates decodes to what was registered.
func TestVerifyProof_AcceptsAnAnswerUnderTheAttestedRoot(t *testing.T) {
	committee := newCommittee(t, 4, 100)
	f := newFixture(t, committee)

	resp, err := f.ResolveDomainProved(f.name)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	_, root := f.mgr.CommittedFrontier()
	attested := VerifiedAnchor{FrontierRound: 10, IndexRoot: root}

	leaf, found, err := attested.VerifyDomain(resp, f.name)
	if err != nil || !found {
		t.Fatalf("proved resolution rejected: found=%v err=%v", found, err)
	}

	if leaf.ObjectID != ([32]byte{0x11}) || leaf.Owner != f.owner {
		t.Fatalf("proved leaf: %+v", leaf)
	}
}

// TestVerifyProof_AbsenceIsAsVerifiableAsInclusion verifies an unregistered
// name comes back provably absent rather than merely unanswered.
func TestVerifyProof_AbsenceIsAsVerifiableAsInclusion(t *testing.T) {
	committee := newCommittee(t, 4, 100)
	f := newFixture(t, committee)

	resp, err := f.ResolveDomainProved("never-registered.bp")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	_, root := f.mgr.CommittedFrontier()
	attested := VerifiedAnchor{FrontierRound: 10, IndexRoot: root}

	leaf, found, err := attested.VerifyDomain(resp, "never-registered.bp")
	if err != nil {
		t.Fatalf("absence proof rejected: %v", err)
	}

	if found || leaf.Name != "" {
		t.Fatalf("unregistered name resolved: %+v", leaf)
	}
}
