package events

import "log/slog"

// EpochTransitioned marks the epoch boundary landing on epoch, carrying the
// hex pubkeys added to and removed from the committed validator set.
func EpochTransitioned(epoch uint64, added, removed []string) {
	emit(EvEpochTransitioned, slog.Uint64("epoch", epoch), slog.Any("added", added), slog.Any("removed", removed))
}

// ValidatorRegistered marks validator (re-)registering with quicAddr.
func ValidatorRegistered(validator [32]byte, quicAddr string) {
	emit(EvValidatorRegistered, hexAttr("validator", validator), slog.String("quic_addr", quicAddr))
}

// ValidatorDeregistered marks validator scheduled for removal at the next
// epoch boundary.
func ValidatorDeregistered(validator [32]byte) {
	emit(EvValidatorDeregistered, hexAttr("validator", validator))
}

// RewardsDistributed marks an epoch's issuance pool credited across
// validators active validators.
func RewardsDistributed(epoch, pool uint64, validators int) {
	emit(EvRewardsDistributed, slog.Uint64("epoch", epoch), slog.Uint64("pool", pool), slog.Int("validators", validators))
}
