package client

import (
	"fmt"

	"BluePods/internal/index"
	"BluePods/internal/network"
)

// VerifyDomain checks a proved resolution against this attested root and
// returns the leaf the proof covered. found is false when the answer is an
// absence proof, which is as verifiable as an inclusion: the name provably has
// no leaf at all.
//
// The lease's expiry is the caller's to judge against the current epoch. A
// name past its expiry keeps its leaf until the sweep removes it, so the
// authenticated answer is the leaf, never a resolved/not-resolved flag the
// serving node computed.
func (a VerifiedAnchor) VerifyDomain(resp *network.DomainResolveResponse, name string) (index.DomainLeaf, bool, error) {
	if resp == nil {
		return index.DomainLeaf{}, false, fmt.Errorf("no domain answer")
	}

	value := nilIfEmpty(resp.Leaf)

	if err := a.VerifyProof(resp.Anchor, DomainComponent, []byte(name), value, resp.Proof); err != nil {
		return index.DomainLeaf{}, false, fmt.Errorf("domain %q:\n%w", name, err)
	}

	if value == nil {
		return index.DomainLeaf{}, false, nil
	}

	leaf, ok := index.DecodeDomainLeaf(value)
	if !ok {
		return index.DomainLeaf{}, false, fmt.Errorf("domain %q: proved leaf does not decode", name)
	}

	// The name rides inside the leaf, so a server cannot answer one name with
	// another name's genuinely proved leaf.
	if leaf.Name != name {
		return index.DomainLeaf{}, false, fmt.Errorf("domain %q: proved leaf names %q", name, leaf.Name)
	}

	return leaf, true, nil
}

// VerifyChildren checks a proved enumeration against this attested root and
// returns the complete child set. Completeness is what the check buys: the
// streamed leaves are unauthenticated on their own, so they are folded back
// into a subtree root (index.ChildrenSubtreeRoot) and compared to the root the
// top-tree proof binds to the attested children root. A withheld or invented
// leaf changes that root, at every set size, and a duplicate ID is refused
// outright, which is what makes the returned length an authenticated count.
func (a VerifiedAnchor) VerifyChildren(resp *network.ListChildrenResponse, parent [32]byte) ([][32]byte, error) {
	if resp == nil {
		return nil, fmt.Errorf("no children answer")
	}

	var value []byte
	if resp.Found {
		value = resp.SubtreeRoot[:]
	}

	if err := a.VerifyProof(resp.Anchor, ChildrenComponent, parent[:], value, resp.Proof); err != nil {
		return nil, fmt.Errorf("children of %x:\n%w", parent[:8], err)
	}

	if !resp.Found {
		if len(resp.Children) != 0 {
			return nil, fmt.Errorf("children of %x: %d leaves streamed under a proof of no children", parent[:8], len(resp.Children))
		}

		return nil, nil
	}

	rebuilt, ok := index.ChildrenSubtreeRoot(resp.Children)
	if !ok {
		return nil, fmt.Errorf("children of %x: the streamed leaves repeat an ID", parent[:8])
	}

	if rebuilt != resp.SubtreeRoot {
		return nil, fmt.Errorf("children of %x: streamed leaves rebuild %x, the proven subtree root is %x",
			parent[:8], rebuilt[:8], resp.SubtreeRoot[:8])
	}

	return resp.Children, nil
}

// VerifyAncestry checks a proved ancestry walk against this attested root and
// returns its hops, the queried object's own edge first.
//
// Per-edge proofs are not enough, and checking only those is the mistake this
// function exists to prevent: a proof shows that a leaf sits at some object's
// position, and says nothing about whether the next edge continues the walk
// the previous one started. Each hop's own parent reference is therefore
// chained into the next edge's key, which is what makes the result one
// continuous chain rather than a bundle of independently true but unrelated
// edges (the requirement stated on network.GetAncestorsResponse).
//
// The walk must end where the authenticated leaves say it ends: on a KeyRoot,
// or on an object with no edge at all (an absence proof, so a withheld edge is
// not confusable with a missing one). A walk stopping on an object-parented
// edge is refused as truncated.
func (a VerifiedAnchor) VerifyAncestry(resp *network.GetAncestorsResponse, object [32]byte) ([]index.ParentLeaf, error) {
	if resp == nil || len(resp.Edges) == 0 {
		return nil, fmt.Errorf("ancestry of %x: empty walk", object[:8])
	}

	chain := make([]index.ParentLeaf, 0, len(resp.Edges))
	expected := object

	for i, edge := range resp.Edges {
		leaf, terminal, err := a.verifyEdge(resp.Anchor, edge, expected)
		if err != nil {
			return nil, fmt.Errorf("ancestry of %x, hop %d:\n%w", object[:8], i, err)
		}

		if terminal {
			return chain, nil
		}

		chain = append(chain, leaf)
		expected = leaf.Parent

		if leaf.ParentKind == index.KeyRootKind {
			return chain, nil
		}
	}

	return nil, fmt.Errorf("ancestry of %x: the walk ends on an object-parented edge, so its tail was withheld", object[:8])
}

// verifyEdge proves one hop against the attested parent root and reports the
// leaf it authenticated. terminal is true for an absence proof: the object
// provably has no edge, which ends the walk.
func (a VerifiedAnchor) verifyEdge(anchor network.ProvedIndexAnchor, edge network.AncestorEdge, expected [32]byte) (index.ParentLeaf, bool, error) {
	if edge.ChildID != expected {
		return index.ParentLeaf{}, false, fmt.Errorf("edge is for %x, the previous hop's parent is %x", edge.ChildID[:8], expected[:8])
	}

	value := nilIfEmpty(edge.Leaf)

	if err := a.VerifyProof(anchor, ParentComponent, edge.ChildID[:], value, edge.Proof); err != nil {
		return index.ParentLeaf{}, false, err
	}

	if value == nil {
		return index.ParentLeaf{}, true, nil
	}

	leaf, ok := index.DecodeParentLeaf(value)
	if !ok {
		return index.ParentLeaf{}, false, fmt.Errorf("proved leaf does not decode")
	}

	// The child ID rides inside the leaf, so a leaf genuinely proved at
	// another object's position cannot be spliced in here.
	if leaf.ChildID != edge.ChildID {
		return index.ParentLeaf{}, false, fmt.Errorf("proved leaf belongs to %x", leaf.ChildID[:8])
	}

	return leaf, false, nil
}

// VerifyValidatorSet checks a served validator set against this attested root
// and returns it as the authority for weighing quorums.
//
// The whole set is rebuilt and its root compared, rather than one inclusion
// proof per member: the 2/3 test divides a capped-stake sum by the set's
// capped TOTAL, and an inclusion proof authenticates a member while saying
// nothing about how many others exist. Rebuilding is what authenticates the
// count, the membership and every weight at once.
func (a VerifiedAnchor) VerifyValidatorSet(resp *network.GetValidatorTreeResponse) (ValidatorSet, error) {
	if resp == nil || !resp.Found {
		return ValidatorSet{}, fmt.Errorf("node serves no validator tree for the requested epoch")
	}

	if err := a.bind(resp.Anchor); err != nil {
		return ValidatorSet{}, fmt.Errorf("validator set of epoch %d:\n%w", resp.Epoch, err)
	}

	leaves, err := decodeValidatorLeaves(resp.Leaves)
	if err != nil {
		return ValidatorSet{}, fmt.Errorf("validator set of epoch %d:\n%w", resp.Epoch, err)
	}

	if root := index.ValidatorRootOf(leaves); root != resp.Anchor.ValidatorRoot {
		return ValidatorSet{}, fmt.Errorf("validator set of epoch %d rebuilds root %x, the attested validator root is %x",
			resp.Epoch, root[:8], resp.Anchor.ValidatorRoot[:8])
	}

	return ValidatorSet{Epoch: resp.Epoch, Leaves: leaves}, nil
}

// decodeValidatorLeaves reads the raw leaf values a validator-tree answer
// carries, exactly as the tree hashed them.
func decodeValidatorLeaves(values [][]byte) ([]index.ValidatorLeaf, error) {
	leaves := make([]index.ValidatorLeaf, 0, len(values))

	for i, v := range values {
		leaf, ok := index.DecodeValidatorLeaf(v)
		if !ok {
			return nil, fmt.Errorf("leaf %d does not decode", i)
		}

		leaves = append(leaves, leaf)
	}

	return leaves, nil
}

// nilIfEmpty maps an empty slice to nil, the value index.Verify reads as "prove
// this key absent".
func nilIfEmpty(b []byte) []byte {
	if len(b) == 0 {
		return nil
	}

	return b
}
