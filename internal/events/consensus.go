package events

import "log/slog"

// VertexProduced marks a vertex signed and stored by this node's producer,
// carrying its transaction count.
func VertexProduced(vertex [32]byte, round uint64, txs int) {
	emit(EvVertexProduced, hexAttr("vertex", vertex), slog.Uint64("round", round), slog.Int("txs", txs))
}

// VertexReceived marks a remote vertex accepted into the DAG.
func VertexReceived(vertex, producer [32]byte, round uint64) {
	emit(EvVertexReceived, hexAttr("vertex", vertex), hexAttr("producer", producer), slog.Uint64("round", round))
}

// VertexRejected marks a vertex terminally rejected during validation: it is
// not stored and it is not relayed. reason is one of the fixed validation
// failure codes (for example "bad_signature", "wrong_epoch", "parent_round",
// "parent_quorum", "fee_summary").
func VertexRejected(vertex [32]byte, reason string) {
	emit(EvVertexRejected, hexAttr("vertex", vertex), slog.String("reason", reason))
}

// VertexQuarantined marks a vertex whose anchored index root this node
// disproved against its own committed history at frontier. Unlike a rejection
// it is STORED and served on request, so any causal batch containing it can
// still complete; it is never relayed and never referenced, and the commit path
// convicts its producer if it reaches committed history.
func VertexQuarantined(vertex, producer [32]byte, round, frontier uint64) {
	emit(EvVertexQuarantined,
		hexAttr("vertex", vertex),
		hexAttr("producer", producer),
		slog.Uint64("round", round),
		slog.Uint64("frontier", frontier))
}

// RoundAdvanced marks the production round observed from the commit loop (NOT
// the commit cursor) advancing to round, carrying the round's designated
// anchor producer so scenarios can target it.
func RoundAdvanced(round uint64, designated [32]byte) {
	emit(EvRoundAdvanced, slog.Uint64("round", round), hexAttr("designated", designated))
}

// AnchorCommitted marks an anchor decided at round, covering vertices
// vertices produced by producer.
func AnchorCommitted(round uint64, anchor, producer [32]byte, vertices int) {
	emit(EvAnchorCommitted,
		slog.Uint64("round", round),
		hexAttr("anchor", anchor),
		hexAttr("producer", producer),
		slog.Int("vertices", vertices))
}

// RoundSkipped marks a round decided with no anchor.
func RoundSkipped(round uint64) {
	emit(EvRoundSkipped, slog.Uint64("round", round))
}

// AnchorFault marks a committed vertex whose anchored index root contradicts
// the emitting node's own deterministic recomputation at the same frontier:
// producer signed claimed, this node derived computed. It is emitted once per
// lying vertex, beside the persisted evidence that proves it.
func AnchorFault(producer [32]byte, round uint64, claimed, computed [32]byte) {
	emit(EvAnchorFault,
		hexAttr("producer", producer),
		slog.Uint64("round", round),
		hexAttr("claimed", claimed),
		hexAttr("computed", computed))
}
