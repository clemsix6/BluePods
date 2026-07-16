package events

import (
	"context"
	"encoding/hex"
	"log/slog"
)

// Key is the reserved attribute name that marks a record as a typed event.
const Key = "event"

// emit writes one event record at Info level. The record message is the event
// name and the reserved Key attribute carries it for machine filtering.
func emit(name string, attrs ...slog.Attr) {
	all := make([]slog.Attr, 0, len(attrs)+1)
	all = append(all, slog.String(Key, name))
	all = append(all, attrs...)
	slog.LogAttrs(context.Background(), slog.LevelInfo, name, all...)
}

// hexAttr encodes a 32-byte identifier as a lowercase hex attribute.
func hexAttr(key string, id [32]byte) slog.Attr {
	return slog.String(key, hex.EncodeToString(id[:]))
}
