package consensus

// EffectiveStake returns a validator's consensus/reward weight: self plus
// delegated stake. A jailed (or nil) validator contributes zero.
func EffectiveStake(v *ValidatorInfo) uint64 {
	if v == nil || v.Jailed {
		return 0
	}

	return safeAdd(v.SelfStake, v.DelegatedTotal)
}

// totalBonded sums effective stake over the active validator set (O(validators)).
func (d *DAG) totalBonded() uint64 {
	var total uint64
	for _, v := range d.validators.All() {
		total = safeAdd(total, EffectiveStake(v))
	}

	return total
}

// cappedWeight returns a validator's voting weight: effective stake capped at the
// per-validator ceiling. The ceiling is the larger of the configured fraction of
// total stake (per-mille) and an equal share, so a small set keeps a reachable
// 2/3 quorum. Uses safeMul to avoid overflow on total*capMille.
func cappedWeight(effective, total, capMille uint64, setSize int) uint64 {
	if setSize <= 0 || total == 0 {
		return effective
	}

	ceiling := safeMul(total, capMille) / 1000
	if equal := total / uint64(setSize); ceiling < equal {
		ceiling = equal
	}

	if effective > ceiling {
		return ceiling
	}

	return effective
}

// quorumReached reports whether a capped-stake sum meets the 2/3 BFT threshold,
// using exact integer arithmetic. A zero total (no stake yet) is NOT quorum:
// without this guard 3*0 >= 2*0 would read as "always quorum" for a zero-stake
// set, defeating Sybil resistance.
func quorumReached(cappedSum, total uint64) bool {
	if total == 0 {
		return false
	}

	return safeMul(3, cappedSum) >= safeMul(2, total)
}

// cappedStakeOf sums the capped voting weight of the given producers within a
// holder set and returns the set's uncapped total stake. The cap and equal-share
// floor are computed against the set's uncapped total and size, so the same
// snapshot yields the same weights on every node. Jailed validators contribute
// zero (via EffectiveStake).
func (d *DAG) cappedStakeOf(set *ValidatorSet, producers map[Hash]bool) (cappedSum, total uint64) {
	all := set.All()
	setSize := len(all)

	for _, v := range all {
		total = safeAdd(total, EffectiveStake(v))
	}

	for _, v := range all {
		if !producers[v.Pubkey] {
			continue
		}

		weight := cappedWeight(EffectiveStake(v), total, d.votingCapMille, setSize)
		cappedSum = safeAdd(cappedSum, weight)
	}

	return cappedSum, total
}
