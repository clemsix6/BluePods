package consensus

import (
	"crypto/ed25519"
	"encoding/hex"
	"fmt"

	"BluePods/internal/logger"
	"BluePods/internal/types"
)

// validateVertex performs full validation of a vertex before accepting it.
// This is the single entry point for all vertex validation (external and local).
func (d *DAG) validateVertex(v *types.Vertex, data []byte) error {
	// 1. Producer must be a known validator
	if err := d.validateProducer(v); err != nil {
		return err
	}

	// 2. Signature must be valid
	if err := d.validateSignature(v); err != nil {
		return err
	}

	// 3. Epoch must match current epoch
	if err := d.validateEpoch(v); err != nil {
		return err
	}

	// 4. Parents must exist and form quorum.
	// Use the vertex's round (not the node's current round) to determine if
	// validation should be relaxed. A vertex produced during the transition/buffer
	// window must always be accepted, even if it arrives via gossip after the
	// node's current round has moved past the buffer.
	if !d.isRoundInTransitionOrBuffer(v.Round()) {
		if err := d.validateParents(v); err != nil {
			return err
		}
	}

	// 5. Parents must represent quorum of validators from round-1
	if err := d.validateParentsQuorum(v); err != nil {
		return err
	}

	return nil
}

// validateEpoch checks the vertex epoch matches current epoch.
func (d *DAG) validateEpoch(v *types.Vertex) error {
	if v.Epoch() != d.epoch {
		return fmt.Errorf("epoch mismatch: expected %d, got %d", d.epoch, v.Epoch())
	}
	return nil
}

// validateProducer checks the producer is in the validator set.
// During init phase (before minValidators), accepts any producer to allow
// observing the bootstrap chain and learning about new validators.
func (d *DAG) validateProducer(v *types.Vertex) error {
	producer := extractProducer(v)

	// During init, accept vertices from any producer.
	// This allows nodes to observe bootstrap's chain and commit registrations.
	if d.minValidators > 0 && d.validators.Len() < d.minValidators {
		return nil
	}

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
	parentProducer := extractLinkProducer(link)
	parent := d.store.get(parentHash)

	if parent == nil {
		logger.Debug("missing parent",
			"parentHash", hex.EncodeToString(parentHash[:8]),
			"parentProducer", hex.EncodeToString(parentProducer[:8]),
			"forRound", round,
		)
		return fmt.Errorf("parent not found: %x", parentHash)
	}

	if parent.Round() != round-1 {
		return fmt.Errorf("parent round mismatch: expected %d, got %d", round-1, parent.Round())
	}

	return nil
}

// validateParentsQuorum ensures parents reference at least 67% of validators.
// This prevents a vertex from being built on a minority chain.
func (d *DAG) validateParentsQuorum(v *types.Vertex) error {
	round := v.Round()

	// Round 0 has no parents requirement
	if round == 0 {
		return nil
	}

	// During init phase, skip quorum check.
	// Only bootstrap produces, so we can't have quorum yet.
	if d.minValidators > 0 && d.validators.Len() < d.minValidators {
		return nil
	}

	// Collect unique validator producers from parents
	parentProducers := make(map[Hash]bool)
	var link types.VertexLink

	for i := 0; i < v.ParentsLength(); i++ {
		if !v.Parents(&link, i) {
			continue
		}

		// Get the producer from the parent link
		producer := extractLinkProducer(&link)

		// Only count if producer is a known validator
		if d.validators.Contains(producer) {
			parentProducers[producer] = true
		}
	}

	quorumSize := d.validators.QuorumSize()

	// Transition + buffer period: relax quorum after minValidators is reached.
	// Use the vertex's round to determine if quorum should be relaxed.
	// Vertices produced during the transition/buffer window always get relaxed quorum.
	if d.isRoundInTransitionOrBuffer(round) {
		if len(parentProducers) >= 1 {
			return nil
		}
		return fmt.Errorf("transition: need at least 1 parent, got %d", len(parentProducers))
	}

	// Normal operation: need full quorum
	if len(parentProducers) < quorumSize {
		return fmt.Errorf("insufficient parent quorum: got %d, need %d",
			len(parentProducers), quorumSize)
	}

	return nil
}

// extractLinkProducer extracts the producer hash from a vertex link.
func extractLinkProducer(link *types.VertexLink) Hash {
	var h Hash
	if b := link.ProducerBytes(); len(b) == 32 {
		copy(h[:], b)
	}
	return h
}
