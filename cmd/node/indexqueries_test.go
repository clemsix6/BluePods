package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"testing"
	"time"

	"BluePods/internal/genesis"
	"BluePods/internal/index"
	"BluePods/internal/network"
)

// provedPass is one pass of the four client round-trips a light client makes:
// the anchor bundle it verifies everything against, and the three proved
// queries whose roots must agree with it.
type provedPass struct {
	bundle    *network.GetIndexAnchorResponse
	resolve   *network.DomainResolveResponse
	children  *network.ListChildrenResponse
	ancestors *network.GetAncestorsResponse
}

// clientRoundTrip routes one encoded request through the node exactly as an
// inbound QUIC client message, checking the tag classifies as one — a handler
// reachable only by direct call is a handler no client can ever reach.
func clientRoundTrip(t *testing.T, n *Node, req []byte) []byte {
	t.Helper()

	if !network.IsClientMessage(req) {
		t.Fatalf("request tag 0x%02x does not classify as a client message, so it never routes", req[0])
	}

	resp, err := n.handleClientMessage(req)
	if err != nil {
		t.Fatalf("handleClientMessage(0x%02x): %v", req[0], err)
	}

	return resp
}

// queryProved runs the three proved queries plus the anchor bundle against n.
func queryProved(t *testing.T, n *Node, name string, parent, object [32]byte) provedPass {
	t.Helper()

	bundle, err := network.DecodeGetIndexAnchorResp(clientRoundTrip(t, n, network.EncodeGetIndexAnchor()))
	if err != nil {
		t.Fatalf("decode anchor bundle: %v", err)
	}

	resolve, err := network.DecodeDomainResolveResp(clientRoundTrip(t, n,
		network.EncodeDomainResolve(&network.DomainResolveRequest{Name: name})))
	if err != nil {
		t.Fatalf("decode domain resolve: %v", err)
	}

	children, err := network.DecodeListChildrenResp(clientRoundTrip(t, n,
		network.EncodeListChildren(&network.ListChildrenRequest{ParentID: parent})))
	if err != nil {
		t.Fatalf("decode list children: %v", err)
	}

	ancestors, err := network.DecodeGetAncestorsResp(clientRoundTrip(t, n,
		network.EncodeGetAncestors(&network.GetAncestorsRequest{ObjectID: object})))
	if err != nil {
		t.Fatalf("decode get ancestors: %v", err)
	}

	return provedPass{bundle: bundle, resolve: resolve, children: children, ancestors: ancestors}
}

// agreesWithBundle reports whether every answer in the pass is anchored and
// carries the exact root the quorum bundle attests. A live node commits while
// the pass runs, so the caller retries until one pass lines up rather than
// pinning a frontier a background commit loop makes unpredictable.
func (p provedPass) agreesWithBundle() bool {
	if !p.bundle.Found {
		return false
	}

	for _, a := range []network.ProvedIndexAnchor{p.resolve.Anchor, p.children.Anchor, p.ancestors.Anchor} {
		if !a.Anchored || a.IndexRoot != p.bundle.IndexRoot {
			return false
		}
	}

	return true
}

// awaitProvedPass retries the four round-trips until every proved answer
// agrees with the quorum bundle's root.
func awaitProvedPass(t *testing.T, n *Node, name string, parent, object [32]byte) provedPass {
	t.Helper()

	deadline := time.Now().Add(15 * time.Second)
	for {
		pass := queryProved(t, n, name, parent, object)
		if pass.agreesWithBundle() {
			return pass
		}

		if time.Now().After(deadline) {
			t.Fatalf("no pass agreed with the anchor bundle: bundle found=%v root=%x, resolve anchored=%v root=%x",
				pass.bundle.Found, pass.bundle.IndexRoot[:4], pass.resolve.Anchor.Anchored, pass.resolve.Anchor.IndexRoot[:4])
		}

		time.Sleep(20 * time.Millisecond)
	}
}

// assertAnchorCombines checks the anchoring block is self-consistent: the four
// component roots a verifier is handed must combine into the anchored index
// root, or the proofs — each folding to one component root — can never be tied
// to what the quorum signed.
func assertAnchorCombines(t *testing.T, a network.ProvedIndexAnchor) {
	t.Helper()

	combined := index.CombinedRoot(a.DomainRoot, a.ParentRoot, a.ChildrenRoot, a.ValidatorRoot)
	if combined != a.IndexRoot {
		t.Fatalf("component roots combine to %x, not the served index root %x", combined[:4], a.IndexRoot[:4])
	}
}

// decodeProof deserializes a wire proof through the index package's own
// contract.
func decodeProof(t *testing.T, data []byte) index.Proof {
	t.Helper()

	p, err := index.DeserializeProof(data)
	if err != nil {
		t.Fatalf("deserialize proof: %v", err)
	}

	return p
}

// TestProvedIndexQueries_VerifyAgainstAnchorBundle is the round-trip the spec
// §5 read path is made of: a client asks one node to resolve a name, list a
// key's children and walk an object's ancestry, and verifies all three against
// the GetIndexAnchor bundle alone — the proofs fold to the component roots, the
// component roots combine to the bundle's attested index root, and nothing the
// serving node said is taken on trust. The name asked for is unregistered, so
// the resolution leg is the ABSENCE proof.
func TestProvedIndexQueries_VerifyAgainstAnchorBundle(t *testing.T) {
	dir := t.TempDir()
	_, privKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	n, db := bootstrapTestNode(t, dir, privKey)
	t.Cleanup(func() { n.dag.Close(); db.Close() })

	n.seedGenesisState()
	waitForCommit(t, n.dag, n.dag.LastCommittedRound(), 2)

	owner := deriveOwner(privKey)
	coinID := genesis.GenesisCoinID(owner)

	const unregistered = "never-registered.bp"
	pass := awaitProvedPass(t, n, unregistered, owner, coinID)

	assertAnchorCombines(t, pass.resolve.Anchor)

	// Absence: no leaf, and the proof binds the name's empty position to the
	// domain root the bundle attests.
	if pass.resolve.Found || len(pass.resolve.Leaf) != 0 {
		t.Fatalf("unregistered name resolved: found=%v leaf=%d bytes", pass.resolve.Found, len(pass.resolve.Leaf))
	}

	if !index.Verify(pass.resolve.Anchor.DomainRoot, []byte(unregistered), nil, decodeProof(t, pass.resolve.Proof)) {
		t.Fatal("absence proof for an unregistered name does not verify against the attested domain root")
	}

	// Enumeration: the genesis reserve coin hangs off the founder's key, and
	// the subtree the top tree proves commits to exactly the streamed leaves.
	assertChildrenProve(t, pass.children, owner, coinID)

	// Ancestry: the coin's single edge terminates at the founder's KeyRoot.
	assertAncestryProves(t, pass.ancestors, coinID, owner)
}

// assertChildrenProve checks a ListChildren answer: the top-tree proof binds
// the parent's subtree root to the attested children root, the streamed leaves
// rebuild exactly that subtree root, and want is among them.
func assertChildrenProve(t *testing.T, resp *network.ListChildrenResponse, parent, want [32]byte) {
	t.Helper()

	if !resp.Found {
		t.Fatalf("parent %x has no children entry", parent[:4])
	}

	if !index.Verify(resp.Anchor.ChildrenRoot, parent[:], resp.SubtreeRoot[:], decodeProof(t, resp.Proof)) {
		t.Fatal("top-tree proof does not bind the subtree root to the attested children root")
	}

	if got := index.ChildrenSubtreeRoot(resp.Children); got != resp.SubtreeRoot {
		t.Fatalf("streamed leaves rebuild subtree root %x, want the proven %x", got[:4], resp.SubtreeRoot[:4])
	}

	for _, c := range resp.Children {
		if c == want {
			return
		}
	}

	t.Fatalf("child %x absent from the %d streamed leaves", want[:4], len(resp.Children))
}

// assertAncestryProves checks a GetAncestors answer: every edge's leaf is
// proven against the attested parent root, each hop's leaf names the child it
// was asked for, and the walk terminates at the expected KeyRoot rather than
// stopping wherever the server chose.
func assertAncestryProves(t *testing.T, resp *network.GetAncestorsResponse, object, wantKeyRoot [32]byte) {
	t.Helper()

	if len(resp.Edges) == 0 {
		t.Fatal("ancestry walk returned no edge")
	}

	next := object
	for i, edge := range resp.Edges {
		if edge.ChildID != next {
			t.Fatalf("edge %d is for %x, want the walk's next hop %x", i, edge.ChildID[:4], next[:4])
		}

		if !index.Verify(resp.Anchor.ParentRoot, edge.ChildID[:], edge.Leaf, decodeProof(t, edge.Proof)) {
			t.Fatalf("edge %d does not verify against the attested parent root", i)
		}

		leaf, ok := index.DecodeParentLeaf(edge.Leaf)
		if !ok {
			t.Fatalf("edge %d carries an undecodable parent leaf", i)
		}

		if leaf.ChildID != edge.ChildID {
			t.Fatalf("edge %d leaf names child %x, not %x", i, leaf.ChildID[:4], edge.ChildID[:4])
		}

		if leaf.ParentKind == index.KeyRootKind {
			if i != len(resp.Edges)-1 {
				t.Fatalf("edge %d terminates at a KeyRoot but %d edges follow", i, len(resp.Edges)-1-i)
			}

			if leaf.Parent != wantKeyRoot {
				t.Fatalf("walk terminates at key %x, want %x", leaf.Parent[:4], wantKeyRoot[:4])
			}

			return
		}

		next = leaf.Parent
	}

	t.Fatal("walk never reached a KeyRoot: the server withheld the terminal edge")
}

// provedFixture is a node whose loops are stopped over a hand-fed committed
// state: one registered lease, two objects under the founder's key alongside
// the genesis coin, and one object nested under another. Stopping the loops
// first is what makes the anchored frontier exact — the test itself plays the
// commit path's final SetFrontier — and mirrors the index rebuild tests, which
// feed the trees the same way for the same reason.
type provedFixture struct {
	node    *Node
	owner   [32]byte
	round   uint64
	name    string
	leafID  [32]byte
	top     [32]byte
	nested  [32]byte
	expiry  uint64
	objects [][32]byte
}

// buildProvedFixture assembles the stopped-loop fixture above.
func buildProvedFixture(t *testing.T) provedFixture {
	t.Helper()

	dir := t.TempDir()
	_, privKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	n, db := bootstrapTestNode(t, dir, privKey)
	t.Cleanup(func() { db.Close() })

	n.seedGenesisState()
	waitForCommit(t, n.dag, n.dag.LastCommittedRound(), 1)
	n.dag.Close()

	owner := deriveOwner(privKey)

	f := provedFixture{
		node:   n,
		owner:  owner,
		name:   "proved.bp",
		leafID: [32]byte{0xDD, 0x01},
		top:    [32]byte{0xA1},
		nested: [32]byte{0xB2},
		expiry: 1_000,
	}

	// Two more edges under the founder's key, plus one object nested under the
	// first: the enumeration leg gets a set of three, the ancestry leg a walk
	// of two hops.
	n.dag.TrackObject(f.top, 1, 0, 0, index.KeyRootKind, owner)
	n.dag.TrackObject(f.nested, 1, 0, 0, index.ObjectParentKind, f.top)

	// Exactly what the commit path's writeDomainLeaf does: the registry and the
	// authenticated tree, in lockstep.
	n.state.SetDomainLeaf(f.name, f.leafID, owner, f.expiry)
	n.idxManager.ApplyDomain(f.name, f.leafID, owner, f.expiry)

	// The commit cursor names the NEXT round to decide, so it is the round the
	// batch just fed above would be recorded under: this is commitNextRound's
	// closing setIndexFrontier, played by hand with the loop stopped.
	f.round = n.dag.LastCommittedRound()
	if f.round == 0 {
		t.Fatal("test misconfigured: the node decided no round")
	}

	n.idxManager.SetFrontier(f.round)

	f.objects = [][32]byte{genesis.GenesisCoinID(owner), f.top}

	return f
}

// TestProvedIndexQueries_InclusionOverFedState proves the inclusion legs
// against a frontier the test itself anchors: a registered lease resolves with
// a self-describing leaf, the founder's key enumerates every object under it,
// and a nested object's ancestry walks two hops to the KeyRoot. Every answer
// carries the exact root the manager retained for that frontier, which is what
// a quorum bundle at that frontier attests.
func TestProvedIndexQueries_InclusionOverFedState(t *testing.T) {
	f := buildProvedFixture(t)

	pass := queryProved(t, f.node, f.name, f.owner, f.nested)

	anchor := pass.resolve.Anchor
	if !anchor.Anchored || anchor.FrontierRound != f.round {
		t.Fatalf("resolve anchored=%v at round %d, want round %d", anchor.Anchored, anchor.FrontierRound, f.round)
	}

	assertAnchorCombines(t, anchor)

	retained, ok := f.node.idxManager.RootAt(f.round)
	if !ok || retained != anchor.IndexRoot {
		t.Fatalf("served root %x is not the root retained for frontier %d (ok=%v, %x)",
			anchor.IndexRoot[:4], f.round, ok, retained[:4])
	}

	// Resolution: the leaf travels as the exact bytes the tree hashed, so the
	// proof folds it without the client re-encoding anything.
	if !pass.resolve.Found || pass.resolve.ObjectID != f.leafID {
		t.Fatalf("resolve(%s) = (%x, %v), want (%x, true)", f.name, pass.resolve.ObjectID[:4], pass.resolve.Found, f.leafID[:4])
	}

	if !index.Verify(anchor.DomainRoot, []byte(f.name), pass.resolve.Leaf, decodeProof(t, pass.resolve.Proof)) {
		t.Fatal("inclusion proof does not verify against the attested domain root")
	}

	leaf, ok := index.DecodeDomainLeaf(pass.resolve.Leaf)
	if !ok {
		t.Fatal("served domain leaf does not decode")
	}

	if leaf.Name != f.name || leaf.ObjectID != f.leafID || leaf.Owner != f.owner || leaf.ExpiryEpoch != f.expiry {
		t.Fatalf("served leaf %+v does not describe the registered lease", leaf)
	}

	assertChildrenProve(t, pass.children, f.owner, f.top)

	if len(pass.children.Children) != len(f.objects) {
		t.Fatalf("enumerated %d children under the founder key, want %d", len(pass.children.Children), len(f.objects))
	}

	assertAncestryProves(t, pass.ancestors, f.nested, f.owner)

	if len(pass.ancestors.Edges) != 2 {
		t.Fatalf("ancestry of a nested object returned %d edges, want 2 (nested -> parent -> KeyRoot)", len(pass.ancestors.Edges))
	}
}

// TestProvedIndexQueries_TruncatedLeafStreamFailsSubtreeCheck is the
// completeness guarantee: a serving node that streams a child set with one
// leaf held back cannot make the rebuilt subtree root match the one its own
// top-tree proof commits to. This is the ONLY mechanism at any set size — there
// is no threshold below which the client trusts the stream.
func TestProvedIndexQueries_TruncatedLeafStreamFailsSubtreeCheck(t *testing.T) {
	f := buildProvedFixture(t)

	resp, err := network.DecodeListChildrenResp(clientRoundTrip(t, f.node,
		network.EncodeListChildren(&network.ListChildrenRequest{ParentID: f.owner})))
	if err != nil {
		t.Fatalf("decode list children: %v", err)
	}

	if len(resp.Children) < 2 {
		t.Fatalf("test misconfigured: %d children streamed, need at least 2 to truncate", len(resp.Children))
	}

	if !index.Verify(resp.Anchor.ChildrenRoot, f.owner[:], resp.SubtreeRoot[:], decodeProof(t, resp.Proof)) {
		t.Fatal("the honest answer does not verify, so the truncation below proves nothing")
	}

	if got := index.ChildrenSubtreeRoot(resp.Children); got != resp.SubtreeRoot {
		t.Fatalf("the honest stream rebuilds %x, not the proven %x", got[:4], resp.SubtreeRoot[:4])
	}

	truncated := resp.Children[:len(resp.Children)-1]
	if got := index.ChildrenSubtreeRoot(truncated); got == resp.SubtreeRoot {
		t.Fatal("a truncated leaf stream rebuilt the proven subtree root: enumeration completeness is not enforced")
	}
}

// TestProvedIndexQueries_AbsentParentAndUnknownObject covers the empty
// answers: a parent with no children and an object with no parent edge both
// come back with an absence proof against the attested root, not with an
// unproven empty list a serving node could fake.
func TestProvedIndexQueries_AbsentParentAndUnknownObject(t *testing.T) {
	f := buildProvedFixture(t)

	var stranger [32]byte
	stranger[0] = 0x77

	children, err := network.DecodeListChildrenResp(clientRoundTrip(t, f.node,
		network.EncodeListChildren(&network.ListChildrenRequest{ParentID: stranger})))
	if err != nil {
		t.Fatalf("decode list children: %v", err)
	}

	if children.Found || len(children.Children) != 0 {
		t.Fatalf("a parent with no children answered found=%v with %d leaves", children.Found, len(children.Children))
	}

	if !index.Verify(children.Anchor.ChildrenRoot, stranger[:], nil, decodeProof(t, children.Proof)) {
		t.Fatal("absence proof for a childless parent does not verify against the attested children root")
	}

	ancestors, err := network.DecodeGetAncestorsResp(clientRoundTrip(t, f.node,
		network.EncodeGetAncestors(&network.GetAncestorsRequest{ObjectID: stranger})))
	if err != nil {
		t.Fatalf("decode get ancestors: %v", err)
	}

	if len(ancestors.Edges) != 1 || len(ancestors.Edges[0].Leaf) != 0 {
		t.Fatalf("unknown object walked %d edges, want exactly one absence edge", len(ancestors.Edges))
	}

	if !index.Verify(ancestors.Anchor.ParentRoot, stranger[:], nil, decodeProof(t, ancestors.Edges[0].Proof)) {
		t.Fatal("absence proof for an unknown object does not verify against the attested parent root")
	}
}
