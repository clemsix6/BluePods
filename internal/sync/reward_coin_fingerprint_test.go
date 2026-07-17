package sync

import (
	"testing"

	"github.com/zeebo/blake3"

	"BluePods/internal/consensus"
)

// TestHashValidators_RewardCoinDistinguishes verifies the reward-coin designation
// is part of the per-validator convergence digest: two validator sets identical in
// every other hashed field but the reward coin must hash differently. Without this,
// a live/replay divergence in the designation stays invisible in the fingerprint and
// only surfaces later, as diverging coin bytes at the next epoch credit.
func TestHashValidators_RewardCoinDistinguishes(t *testing.T) {
	base := consensus.ValidatorInfo{
		Pubkey:    consensus.Hash{0x01},
		SelfStake: 1000,
	}

	withCoinA := base
	withCoinA.RewardCoin = consensus.Hash{0xAA}

	withCoinB := base
	withCoinB.RewardCoin = consensus.Hash{0xBB}

	sumA := hashValidatorsSum(&withCoinA)
	sumB := hashValidatorsSum(&withCoinB)

	if sumA == sumB {
		t.Fatal("hashValidators ignores RewardCoin: two distinct designations hash identically")
	}
}

// hashValidatorsSum returns the BLAKE3 digest of hashValidators over a single
// validator, for comparing two designations.
func hashValidatorsSum(v *consensus.ValidatorInfo) [32]byte {
	h := blake3.New()
	hashValidators(h, []*consensus.ValidatorInfo{v})

	var sum [32]byte
	h.Sum(sum[:0])

	return sum
}
