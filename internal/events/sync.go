package events

import "log/slog"

// SnapshotCreated marks a state snapshot exported at round.
func SnapshotCreated(round uint64, checksum [32]byte) {
	emit(EvSnapshotCreated, slog.Uint64("round", round), hexAttr("checksum", checksum))
}

// SnapshotApplied marks a received snapshot applied to local state, restoring
// objects objects.
func SnapshotApplied(round uint64, checksum [32]byte, objects int) {
	emit(EvSnapshotApplied, slog.Uint64("round", round), hexAttr("checksum", checksum), slog.Int("objects", objects))
}

// SyncCompleted marks a node finishing catch-up sync at round.
func SyncCompleted(round uint64) {
	emit(EvSyncCompleted, slog.Uint64("round", round))
}
