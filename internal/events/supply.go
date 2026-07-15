package events

import "log/slog"

// SupplyIssued marks new supply minted into the epoch reward pool, at rate
// (the thermostat's issuance rate, in micro-units per unit stake).
func SupplyIssued(epoch, amount, rate uint64) {
	emit(EvSupplyIssued, slog.Uint64("epoch", epoch), slog.Uint64("amount", amount), slog.Uint64("rate", rate))
}

// SupplyBurned marks supply permanently removed, carrying its cause (for
// example "deletion").
func SupplyBurned(amount uint64, reason string) {
	emit(EvSupplyBurned, slog.Uint64("amount", amount), slog.String("reason", reason))
}
