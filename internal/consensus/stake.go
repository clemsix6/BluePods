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
