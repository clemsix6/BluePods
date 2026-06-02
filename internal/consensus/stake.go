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
