package events

import "log/slog"

// IngressTxReceived marks a submitted transaction accepted into the mempool
// path, before commit. kind is one of "raw", "attested" or "faucet".
func IngressTxReceived(tx [32]byte, kind string) {
	emit(EvIngressTxReceived, hexAttr("tx", tx), slog.String("kind", kind))
}

// IngressTxRejected marks a submission rejected before it reached consensus,
// carrying a short reason code and a human-readable detail.
func IngressTxRejected(reason, detail string) {
	emit(EvIngressTxRejected, slog.String("reason", reason), slog.String("detail", detail))
}
