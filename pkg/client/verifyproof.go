package client

import (
	"errors"
	"fmt"

	"BluePods/internal/index"
	"BluePods/internal/network"
)

// errUnanchored reports an answer taken against tree state no committed
// frontier has recorded yet: the serving node sits between a mutation and the
// commit that closes its batch, so no bundle for that root can exist. It is
// the spec §5 live unproven read, and the caller's move is to retry.
var errUnanchored = errors.New("answer is not anchored: the node's index sits ahead of its last committed frontier")

// Component names which of the four trees a proof folds to. A proof authenticates
// a key against ONE component root; the combination of all four is what a
// quorum signs, which is why a component root alone proves nothing.
type Component int

const (
	// DomainComponent is the domain tree: names to leases.
	DomainComponent Component = iota

	// ParentComponent is the parent tree: an object to its parent edge.
	ParentComponent

	// ChildrenComponent is the children top tree: a parent to its subtree root.
	ChildrenComponent

	// ValidatorComponent is the validator tree: an epoch's committee.
	ValidatorComponent
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
		return errUnanchored
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
	case ValidatorComponent:
		return anchor.ValidatorRoot, nil
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
