package events

import "log/slog"

// TxCommitted marks a transaction decided at commit, success or typed failure.
// reason is empty on success and one of the fixed set otherwise: version_conflict,
// fee_rejected, ownership, proof_failed, authenticity_failed, duplicate,
// expired_sponsorship, execution_error.
func TxCommitted(tx, vertex [32]byte, round uint64, success bool, reason string) {
	emit(EvTxCommitted,
		hexAttr("tx", tx),
		hexAttr("vertex", vertex),
		slog.Uint64("round", round),
		slog.Bool("success", success),
		slog.String("reason", reason))
}

// TxExecuted marks the executor call for tx returning, independent of the
// commit-level outcome recorded by TxCommitted. errorCode is empty on success.
func TxExecuted(tx [32]byte, success bool, errorCode string) {
	emit(EvTxExecuted, hexAttr("tx", tx), slog.Bool("success", success), slog.String("error_code", errorCode))
}
