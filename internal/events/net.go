package events

import "log/slog"

// PeerConnected marks a QUIC peer connection established with peer (its hex
// pubkey).
func PeerConnected(peer string) {
	emit(EvPeerConnected, slog.String("peer", peer))
}

// PeerDisconnected marks a QUIC peer connection torn down.
func PeerDisconnected(peer string) {
	emit(EvPeerDisconnected, slog.String("peer", peer))
}

// PartitionApplied marks the local blocklist populated with blocked (hex
// pubkeys), a test-hooks-only operation.
func PartitionApplied(blocked []string) {
	emit(EvPartitionApplied, slog.Any("blocked", blocked))
}

// PartitionCleared marks the local blocklist emptied, a test-hooks-only
// operation.
func PartitionCleared() {
	emit(EvPartitionCleared)
}
