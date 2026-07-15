package events

import "log/slog"

// NodeStarted marks a node beginning startup, before genesis seeding or sync
// runs.
func NodeStarted(pubkey [32]byte, quicAddr string) {
	emit(EvNodeStarted, hexAttr("pubkey", pubkey), slog.String("quic_addr", quicAddr))
}

// NodeReady marks a node's listener coming up: genesis seeding or catch-up
// sync has completed and the node is ready to serve traffic at round.
func NodeReady(round uint64) {
	emit(EvNodeReady, slog.Uint64("round", round))
}

// NodeStopping marks a node beginning shutdown, carrying the reason (signal
// name or close cause).
func NodeStopping(reason string) {
	emit(EvNodeStopping, slog.String("reason", reason))
}
