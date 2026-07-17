package aggregation

import (
	"BluePods/internal/attest"
	"BluePods/internal/consensus"
)

// Rendezvous computes object-to-holder mappings using rendezvous hashing.
// It wraps a live validator set; the holder math lives in the attest package.
type Rendezvous struct {
	vs *consensus.ValidatorSet // vs is the live validator set
}

// NewRendezvous creates a new Rendezvous from the validator set.
func NewRendezvous(vs *consensus.ValidatorSet) *Rendezvous {
	return &Rendezvous{vs: vs}
}

// ComputeHolders returns the N validators responsible for holding the object,
// ordered by rendezvous score (highest first).
func (r *Rendezvous) ComputeHolders(objectID [32]byte, replication int) []consensus.Hash {
	return attest.ComputeHolders(r.vs, objectID, replication)
}
