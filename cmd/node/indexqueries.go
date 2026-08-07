package main

import (
	"fmt"

	"BluePods/internal/index"
	"BluePods/internal/network"
)

// handleDomainResolve resolves a domain name and proves the answer. The
// resolution itself (Found, ObjectID) comes from state, expiry applied, and is
// unchanged; the leaf and its proof come from the authenticated domain tree, so
// a client can check the name against a quorum-attested root instead of taking
// this node's word for it. A name with no leaf at all comes back with an
// absence proof, which is just as verifiable as an inclusion. The authenticated
// answer is the leaf and its proof; Found and ObjectID are the legacy,
// unproven path, read outside the lock the leaf is read under, so a domain op
// committing between the two reads can leave them describing a different state
// than the proof does.
func (n *Node) handleDomainResolve(data []byte) ([]byte, error) {
	req, err := network.DecodeDomainResolve(data)
	if err != nil {
		return nil, err
	}

	if n.state == nil {
		return nil, fmt.Errorf("domain resolution not available")
	}

	objectID, found := n.state.ResolveDomain(req.Name)

	resp := &network.DomainResolveResponse{Found: found, ObjectID: objectID}

	if n.idxManager != nil {
		answer := n.idxManager.ResolveDomain(req.Name)
		resp.Anchor = provedAnchor(answer.Anchor)
		resp.Leaf = answer.Value
		resp.Proof = answer.Proof.Serialize()
	}

	return network.EncodeDomainResolveResp(resp), nil
}

// handleListChildren enumerates a parent's children — an owner key or an
// object ID — with the top-tree proof of that parent's subtree root. The raw
// leaves ride along unauthenticated at every set size: the client rebuilds the
// subtree from them and checks its root against the proven one, which is what
// makes a withheld leaf detectable without per-chunk range proofs.
func (n *Node) handleListChildren(data []byte) ([]byte, error) {
	req, err := network.DecodeListChildren(data)
	if err != nil {
		return nil, err
	}

	if n.idxManager == nil {
		return nil, fmt.Errorf("children enumeration not available")
	}

	answer := n.idxManager.ListChildren(req.ParentID)

	return network.EncodeListChildrenResp(&network.ListChildrenResponse{
		Anchor:      provedAnchor(answer.Anchor),
		Found:       answer.Found,
		SubtreeRoot: answer.SubtreeRoot,
		Proof:       answer.Proof.Serialize(),
		Children:    answer.Children,
	}), nil
}

// handleGetAncestors walks an object's parent edges upward, one inclusion
// proof per hop, and stops at the terminal KeyRoot. The walk is provably
// complete because each hop's kind and parent live inside the leaf the proof
// covers: a node withholding the next edge cannot make the last one it served
// look like a terminus.
func (n *Node) handleGetAncestors(data []byte) ([]byte, error) {
	req, err := network.DecodeGetAncestors(data)
	if err != nil {
		return nil, err
	}

	if n.idxManager == nil {
		return nil, fmt.Errorf("ancestor walk not available")
	}

	answer := n.idxManager.Ancestors(req.ObjectID)

	return network.EncodeGetAncestorsResp(&network.GetAncestorsResponse{
		Anchor: provedAnchor(answer.Anchor),
		Edges:  ancestorEdges(answer.Edges),
	}), nil
}

// ancestorEdges converts the index package's proved hops into their wire form,
// serializing each proof through the index package's own pinned contract.
func ancestorEdges(edges []index.AncestorEdge) []network.AncestorEdge {
	out := make([]network.AncestorEdge, len(edges))
	for i, e := range edges {
		out[i] = network.AncestorEdge{
			ChildID: e.ChildID,
			Leaf:    e.Value,
			Proof:   e.Proof.Serialize(),
		}
	}

	return out
}

// provedAnchor converts an index anchor into the wire block every proved
// response opens with. The four component roots travel because a proof folds
// to one of them and a verifier needs all four to recompute the combined root
// a quorum bundle attests.
func provedAnchor(a index.Anchor) network.ProvedIndexAnchor {
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
