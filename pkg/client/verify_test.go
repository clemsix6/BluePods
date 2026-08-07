package client

import (
	"encoding/binary"
	"encoding/hex"
	"errors"
	"testing"

	"BluePods/internal/network"
)

// The fixture this file's tests script (testValidator, newCommittee, fixture,
// checkpointOf, and friends) lives in fixture_test.go, shared with
// lightclient_test.go.

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

// TestVerifyAnchor_EpochIsWhatItsOwnQuorumSays is the fix for the label
// attack: the bundle's attested epoch must not be the served node's word, nor
// the mere maximum among whichever headers happened to count. Three of a
// four-member committee sign a GENUINE (frontier, root) pair at epoch N; the
// fourth member alone signs the SAME genuine pair claiming epoch N+1. Every
// header is individually valid and the overall bundle clears quorum, but the
// N+1 subset does not carry the committee's quorum on its own — only the N
// subset does. The attested epoch must therefore be N: one byzantine header
// must not be able to relabel a genuine N-quorum as N+1 and drag the
// checkpoint's epoch walk forward with it (see
// TestLightClient_OneByzantineHeaderDoesNotForceTheEpochWalk for the
// consequence at the LightClient seat).
func TestVerifyAnchor_EpochIsWhatItsOwnQuorumSays(t *testing.T) {
	committee := newCommittee(t, 4, 100)
	f := newFixture(t, committee)

	round, root := f.mgr.CommittedFrontier()
	set := ValidatorSet{Epoch: f.epoch, Leaves: leavesOf(committee)}

	records := make([][]byte, 0, 4)
	for _, v := range committee[:3] {
		records = append(records, headerRecord(v, round+2, f.epoch, round, root))
	}
	records = append(records, headerRecord(committee[3], round+2, f.epoch+1, round, root))

	bundle := &network.GetIndexAnchorResponse{
		Found:         true,
		FrontierRound: round,
		IndexRoot:     root,
		Epoch:         f.epoch + 1, // the serving node's own unauthenticated label
		Headers:       records,
	}

	attested, err := VerifyAnchor(bundle, set)
	if err != nil {
		t.Fatalf("a quorate bundle mixing an honest N-majority with one byzantine N+1 header was refused outright: %v", err)
	}

	if attested.Epoch != f.epoch {
		t.Fatalf("attested epoch = %d, want %d: the N+1 subset (one header) does not carry the committee's own quorum", attested.Epoch, f.epoch)
	}
}

// TestVerifyAnchor_SplitQuorumAcrossTheHandoffWindowStillAttestsTheRoot is a
// split quorum, not a byzantine minority: half the committee signs the
// GENUINE (frontier, root) pair labeling it epoch N, the other half signs the
// very same pair labeling it N+1. Both labels are protocol-legal (a producer
// picks its own epoch at production time and churn is capped, so the two
// committees overlap), and neither bucket carries the committee's own quorum
// alone. The pair itself is still attested by the full committee once the
// buckets are taken together, so it must not be refused outright — the
// attribution falls back to N, since only N+1's own bucket may ever advance
// the epoch (see TestVerifyAnchor_EpochIsWhatItsOwnQuorumSays).
func TestVerifyAnchor_SplitQuorumAcrossTheHandoffWindowStillAttestsTheRoot(t *testing.T) {
	committee := newCommittee(t, 4, 100)
	f := newFixture(t, committee)

	round, root := f.mgr.CommittedFrontier()
	set := ValidatorSet{Epoch: f.epoch, Leaves: leavesOf(committee)}

	records := make([][]byte, 0, 4)
	for _, v := range committee[:2] {
		records = append(records, headerRecord(v, round+2, f.epoch, round, root))
	}
	for _, v := range committee[2:] {
		records = append(records, headerRecord(v, round+2, f.epoch+1, round, root))
	}

	bundle := &network.GetIndexAnchorResponse{
		Found:         true,
		FrontierRound: round,
		IndexRoot:     root,
		Epoch:         f.epoch + 1, // the serving node's own unauthenticated label
		Headers:       records,
	}

	attested, err := VerifyAnchor(bundle, set)
	if err != nil {
		t.Fatalf("a quorum split evenly across the handoff window's two legal labels, on one genuine (frontier, root) pair, was refused outright: %v", err)
	}

	if attested.Epoch != f.epoch {
		t.Fatalf("attested epoch = %d, want %d: neither label's own bucket carries the committee's quorum alone, so the union attests at the lower epoch", attested.Epoch, f.epoch)
	}
}

// TestVerifyAnchor_HeadersOutsideTheWindowReportTheWeakSubjectivityBoundary
// verifies the epoch-window exhaustion is reported as its own distinct
// condition rather than folded into the generic "short of two thirds"
// message a stalled chain would also produce: when every header names an
// epoch outside {set.Epoch, set.Epoch+1}, VerifyAnchor wraps
// ErrCheckpointBehind, which tells the caller to obtain a fresh checkpoint
// instead of retrying a poll that can never succeed.
func TestVerifyAnchor_HeadersOutsideTheWindowReportTheWeakSubjectivityBoundary(t *testing.T) {
	committee := newCommittee(t, 4, 100)
	f := newFixture(t, committee)
	f.headers = f.epoch + 5

	set := ValidatorSet{Epoch: f.epoch, Leaves: leavesOf(committee)}

	_, err := VerifyAnchor(bundleFrom(t, f, committee), set)
	if err == nil {
		t.Fatal("headers five epochs ahead were weighed by a stale committee")
	}

	if !errors.Is(err, ErrCheckpointBehind) {
		t.Fatalf("error does not wrap ErrCheckpointBehind: %v", err)
	}
}

// TestVerifyAnchor_BelowQuorumInsideTheWindowIsNotReportedAsCheckpointBehind
// is the negative case beside it: a bundle whose headers DO fall inside the
// window but simply do not carry a quorum is an ordinary shortfall, not the
// weak-subjectivity boundary, and must not be misreported as one.
func TestVerifyAnchor_BelowQuorumInsideTheWindowIsNotReportedAsCheckpointBehind(t *testing.T) {
	committee := newCommittee(t, 4, 100)
	f := newFixture(t, committee)

	set := ValidatorSet{Epoch: f.epoch, Leaves: leavesOf(committee)}

	_, err := VerifyAnchor(bundleFrom(t, f, committee[:2]), set)
	if err == nil {
		t.Fatal("two of four validators reached quorum")
	}

	if errors.Is(err, ErrCheckpointBehind) {
		t.Fatalf("an ordinary quorum shortfall inside the handoff window was reported as the checkpoint falling behind: %v", err)
	}
}

// TestVerifyAnchor_AllRecordsUnparsableIsNotReportedAsCheckpointBehind is the
// TDD witness for the case allHeadersOutsideWindow must not fold into "behind
// the checkpoint": a bundle whose every record fails to parse — here, a
// corrupted signature on each one — names no epoch at all. It is not "every
// header outside the handoff window", which is what ErrCheckpointBehind
// exists to report and what tells a caller to re-bootstrap out of band rather
// than poll again. A single malformed or malicious record must not be able to
// evict a light client's otherwise-valid checkpoint.
func TestVerifyAnchor_AllRecordsUnparsableIsNotReportedAsCheckpointBehind(t *testing.T) {
	committee := newCommittee(t, 4, 100)
	f := newFixture(t, committee)

	bundle := bundleFrom(t, f, committee)
	for i, record := range bundle.Headers {
		tampered := append([]byte(nil), record...)
		tampered[anchorHeaderSize-1] ^= 0xFF
		bundle.Headers[i] = tampered
	}

	set := ValidatorSet{Epoch: f.epoch, Leaves: leavesOf(committee)}

	_, err := VerifyAnchor(bundle, set)
	if err == nil {
		t.Fatal("a bundle whose every record fails to parse was accepted as attested")
	}

	if errors.Is(err, ErrCheckpointBehind) {
		t.Fatalf("a bundle with not one parsable header was reported as the checkpoint falling behind: %v", err)
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
