package aggregation

import (
	"bytes"
	"sort"

	"BluePods/internal/consensus"

	"github.com/zeebo/blake3"
)

// Rendezvous computes object-to-holder mappings using rendezvous hashing.
type Rendezvous struct {
	vs *consensus.ValidatorSet // vs is the live validator set
}

// scoredValidator pairs a validator with its computed score.
type scoredValidator struct {
	pubkey consensus.Hash // pubkey is the validator's public key
	score  [32]byte       // score is the computed rendezvous score
}

// NewRendezvous creates a new Rendezvous from the validator set.
func NewRendezvous(vs *consensus.ValidatorSet) *Rendezvous {
	return &Rendezvous{vs: vs}
}

// ComputeHolders returns the N validators responsible for holding the object.
// The validators are ordered by their rendezvous score (highest first).
func (r *Rendezvous) ComputeHolders(objectID [32]byte, replication int) []consensus.Hash {
	validators := r.vs.Validators()

	if replication <= 0 {
		return nil
	}

	if replication > len(validators) {
		replication = len(validators)
	}

	// Score all validators
	scored := make([]scoredValidator, len(validators))

	for i, v := range validators {
		scored[i] = scoredValidator{
			pubkey: v,
			score:  r.computeScore(objectID, v),
		}
	}

	// Sort by score descending (highest score = responsible for object)
	sort.Slice(scored, func(i, j int) bool {
		return bytes.Compare(scored[i].score[:], scored[j].score[:]) > 0
	})

	// Return top N
	result := make([]consensus.Hash, replication)
	for i := 0; i < replication; i++ {
		result[i] = scored[i].pubkey
	}

	return result
}

// computeScore calculates the rendezvous score for an object-validator pair.
// Score = BLAKE3(objectID || validatorPubkey)
func (r *Rendezvous) computeScore(objectID, validator [32]byte) [32]byte {
	h := blake3.New()
	h.Write(objectID[:])
	h.Write(validator[:])

	var result [32]byte
	h.Sum(result[:0])

	return result
}

// QuorumSize returns the minimum number of attestations required for quorum.
// Uses 67% threshold (2f+1 for f = n/3).
func QuorumSize(replication int) int {
	return (replication*67 + 99) / 100
}
