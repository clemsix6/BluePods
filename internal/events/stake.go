package events

import "log/slog"

// StakeBonded marks validator raising its self-stake by amount debited from
// coin.
func StakeBonded(validator, coin [32]byte, amount uint64) {
	emit(EvStakeBonded, hexAttr("validator", validator), hexAttr("coin", coin), slog.Uint64("amount", amount))
}

// StakeUnbonded marks validator lowering its self-stake by amount credited
// back to coin.
func StakeUnbonded(validator, coin [32]byte, amount uint64) {
	emit(EvStakeUnbonded, hexAttr("validator", validator), hexAttr("coin", coin), slog.Uint64("amount", amount))
}

// StakeDelegated marks a new delegation position opened for validator.
func StakeDelegated(validator, position [32]byte, amount uint64) {
	emit(EvStakeDelegated, hexAttr("validator", validator), hexAttr("position", position), slog.Uint64("amount", amount))
}

// StakeUndelegated marks a delegation position closed, its principal credited
// back to its owner's coin.
func StakeUndelegated(validator, position [32]byte, amount uint64) {
	emit(EvStakeUndelegated, hexAttr("validator", validator), hexAttr("position", position), slog.Uint64("amount", amount))
}

// StakeReleased marks a deregistered validator's self-stake principal returned to
// its reward coin as the epoch boundary removes it from the active set.
func StakeReleased(validator, coin [32]byte, amount uint64) {
	emit(EvStakeReleased, hexAttr("validator", validator), hexAttr("coin", coin), slog.Uint64("amount", amount))
}
