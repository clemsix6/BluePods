package attest

import (
	"bytes"
	"sort"

	"BluePods/internal/validators"

	"github.com/zeebo/blake3"
)

// scoredValidator pairs a validator with its computed rendezvous score.
type scoredValidator struct {
	pubkey validators.Hash // pubkey is the validator's public key
	score  [32]byte        // score is the computed rendezvous score
}

// ComputeHolders returns the N validators responsible for holding the object.
// The validators are ordered by their rendezvous score (highest first).
// It returns nil for a non-positive replication factor.
func ComputeHolders(vs *validators.ValidatorSet, objectID [32]byte, replication int) []validators.Hash {
	pubkeys := vs.Validators()

	if replication <= 0 {
		return nil
	}

	if replication > len(pubkeys) {
		replication = len(pubkeys)
	}

	scored := make([]scoredValidator, len(pubkeys))
	for i, v := range pubkeys {
		scored[i] = scoredValidator{
			pubkey: v,
			score:  computeScore(objectID, v),
		}
	}

	// Sort by score descending (highest score = responsible for object)
	sort.Slice(scored, func(i, j int) bool {
		return bytes.Compare(scored[i].score[:], scored[j].score[:]) > 0
	})

	result := make([]validators.Hash, replication)
	for i := 0; i < replication; i++ {
		result[i] = scored[i].pubkey
	}

	return result
}

// computeScore calculates the rendezvous score for an object-validator pair.
// Score = BLAKE3(objectID || validatorPubkey).
func computeScore(objectID, validator [32]byte) [32]byte {
	h := blake3.New()
	h.Write(objectID[:])
	h.Write(validator[:])

	var result [32]byte
	h.Sum(result[:0])

	return result
}

// QuorumSize returns the minimum number of attestations required for a
// per-object quorum. It uses a 67% threshold (2f+1 for f = n/3). This is
// distinct from the BFT consensus quorum on ValidatorSet.
func QuorumSize(replication int) int {
	return (replication*67 + 99) / 100
}
