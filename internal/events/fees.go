package events

import "log/slog"

// FeesDeducted marks a fee taken from coin for tx. covered is false when the
// coin's balance could not cover the full fee; the partial amount taken is
// still pooled into the epoch fee pool, never lost.
func FeesDeducted(tx, coin [32]byte, amount uint64, covered bool) {
	emit(EvFeesDeducted, hexAttr("tx", tx), hexAttr("coin", coin), slog.Uint64("amount", amount), slog.Bool("covered", covered))
}

// DepositLocked marks a storage deposit stamped onto a newly tracked object.
func DepositLocked(object [32]byte, amount uint64) {
	emit(EvDepositLocked, hexAttr("object", object), slog.Uint64("amount", amount))
}

// DepositRefunded marks a locked deposit credited back to coin on deletion.
func DepositRefunded(object, coin [32]byte, amount uint64) {
	emit(EvDepositRefunded, hexAttr("object", object), hexAttr("coin", coin), slog.Uint64("amount", amount))
}
