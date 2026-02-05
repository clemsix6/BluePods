package consensus

import (
	"crypto/ed25519"
	"fmt"

	"BluePods/internal/types"
)

// validateVertex checks signature, producer, and parents.
func (d *DAG) validateVertex(v *types.Vertex, data []byte) error {
	if err := d.validateProducer(v); err != nil {
		return err
	}

	if err := d.validateSignature(v); err != nil {
		return err
	}

	if err := d.validateParents(v); err != nil {
		return err
	}

	return nil
}

// validateProducer checks the producer is in the validator set.
func (d *DAG) validateProducer(v *types.Vertex) error {
	producer := extractProducer(v)

	if !d.validators.Contains(producer) {
		return fmt.Errorf("unknown producer: %x", producer)
	}

	return nil
}

// validateSignature verifies the Ed25519 signature.
func (d *DAG) validateSignature(v *types.Vertex) error {
	sig := v.SignatureBytes()
	if len(sig) != ed25519.SignatureSize {
		return fmt.Errorf("invalid signature size: %d", len(sig))
	}

	pubkey := v.ProducerBytes()
	if len(pubkey) != ed25519.PublicKeySize {
		return fmt.Errorf("invalid pubkey size: %d", len(pubkey))
	}

	hashBytes := v.HashBytes()
	if len(hashBytes) != 32 {
		return fmt.Errorf("invalid hash size: %d", len(hashBytes))
	}

	if !ed25519.Verify(pubkey, hashBytes, sig) {
		return fmt.Errorf("invalid signature")
	}

	return nil
}

// validateParents checks all parents exist and are from round N-1.
func (d *DAG) validateParents(v *types.Vertex) error {
	round := v.Round()

	if round == 0 {
		return nil
	}

	parentCount := v.ParentsLength()
	if parentCount == 0 {
		return fmt.Errorf("no parents for round %d", round)
	}

	var link types.VertexLink
	for i := 0; i < parentCount; i++ {
		if !v.Parents(&link, i) {
			return fmt.Errorf("failed to read parent %d", i)
		}

		if err := d.validateParentLink(&link, round); err != nil {
			return err
		}
	}

	return nil
}

// validateParentLink checks a single parent link.
func (d *DAG) validateParentLink(link *types.VertexLink, round uint64) error {
	parentHash := extractLinkHash(link)
	parent := d.store.get(parentHash)

	if parent == nil {
		return fmt.Errorf("parent not found: %x", parentHash)
	}

	if parent.Round() != round-1 {
		return fmt.Errorf("parent round mismatch: expected %d, got %d", round-1, parent.Round())
	}

	return nil
}
