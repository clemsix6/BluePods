package client

import (
	"errors"
	"fmt"

	"BluePods/internal/index"
	"BluePods/internal/network"
)

// ErrUnanchored reports an answer the caller's move is to retry. It covers two
// cases under the same contract: the live-unproven-read case, an answer taken
// against tree state no committed frontier has recorded yet (the serving node
// sits between a mutation and the commit that closes its batch, so no bundle
// for that root can exist — the spec §5 live unproven read); and the
// stale-answer case, an answer whose root the newest attested bundle still
// does not match after waiting for a frontier at or past it (the index moved
// again during the wait). Both name the same retry contract, so LightClient
// wraps rather than minting a second sentinel for the stale case. Every
// LightClient method that reads a proved answer can return it.
var ErrUnanchored = errors.New("answer is not anchored: the node's index sits ahead of its last committed frontier")

// Component names which tree a per-key proof folds to. A proof authenticates
// a key against ONE component root; the combination of all four index trees
// (domain, parent, children, and the validator tree besides) is what a quorum
// signs, which is why a component root alone proves nothing. The validator
// tree carries no Component of its own: a light client never asks it for a
// per-key proof, only for its whole leaf set, authenticated by rebuilding the
// set and comparing the root (VerifyValidatorSet) — the 2/3 test needs the
// committee's full membership and total weight, which one key's inclusion
// proof cannot give it.
type Component int

const (
	// DomainComponent is the domain tree: names to leases.
	DomainComponent Component = iota

	// ParentComponent is the parent tree: an object to its parent edge.
	ParentComponent

	// ChildrenComponent is the children top tree: a parent to its subtree root.
	ChildrenComponent
)

// VerifyProof ties one proved answer to this attested root, in the two steps
// that are only worth anything together:
//
//  1. the answer's four component roots must combine (index.CombinedRoot) to
//     the index root a stake quorum signed. Skipping this leaves the proof
//     folding to a root the serving node made up on the spot;
//  2. the proof must fold key to value under the component root the proof was
//     taken against. A nil value checks an absence proof.
//
// The value is the raw leaf the tree hashed, never a re-encoded copy of a
// decoded one: index.DecodeDomainLeaf, DecodeParentLeaf and DecodeValidatorLeaf
// read the bytes the proof covered.
func (a VerifiedAnchor) VerifyProof(anchor network.ProvedIndexAnchor, c Component, key, value, proof []byte) error {
	if err := a.bind(anchor); err != nil {
		return err
	}

	root, err := componentRoot(anchor, c)
	if err != nil {
		return err
	}

	parsed, err := index.DeserializeProof(proof)
	if err != nil {
		return fmt.Errorf("proof does not decode:\n%w", err)
	}

	if !index.Verify(root, key, value, parsed) {
		return fmt.Errorf("proof does not fold key %x to the attested component root %x", truncate(key), root[:8])
	}

	return nil
}

// bind checks that an answer's anchoring block describes the attested root:
// its four component roots must combine to it. Without this step a proof is
// tied to nothing — the serving node chooses the component roots it hands out,
// and a proof against an invented root verifies exactly as well as one against
// the real one.
func (a VerifiedAnchor) bind(anchor network.ProvedIndexAnchor) error {
	if !anchor.Anchored {
		return ErrUnanchored
	}

	combined := index.CombinedRoot(anchor.DomainRoot, anchor.ParentRoot, anchor.ChildrenRoot, anchor.ValidatorRoot)
	if combined != a.IndexRoot {
		return fmt.Errorf("answer's component roots combine to %x, not the attested index root %x", combined[:8], a.IndexRoot[:8])
	}

	return nil
}

// componentRoot picks the component root a proof for c folds to.
func componentRoot(anchor network.ProvedIndexAnchor, c Component) ([32]byte, error) {
	switch c {
	case DomainComponent:
		return anchor.DomainRoot, nil
	case ParentComponent:
		return anchor.ParentRoot, nil
	case ChildrenComponent:
		return anchor.ChildrenRoot, nil
	default:
		return [32]byte{}, fmt.Errorf("unknown index component %d", c)
	}
}

// truncate shortens a key for an error message.
func truncate(key []byte) []byte {
	if len(key) > 8 {
		return key[:8]
	}

	return key
}
