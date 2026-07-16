package logger

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"
	"time"
)

// format selects which slog.Handler backs the default logger: "text" (human
// readable, the default) or "json" (the standard slog.JSONHandler at Debug
// level, one JSON object per line).
var format = "text"

// writer is the current default logger's output destination.
var writer io.Writer = os.Stdout

// nodeAttr is a permanent attribute installed on every record once SetNode is
// called. Its zero value (empty Key) means no node identity is installed yet.
var nodeAttr slog.Attr

var (
	defaultLogger *slog.Logger
	once          sync.Once
)

// Init initializes the global logger with timestamp precision to milliseconds.
func Init() {
	once.Do(rebuild)
}

// SetOutput redirects all subsequent log output to w. It replaces the default
// logger handler and is safe to call after Init. Pass io.Discard to silence logs
// or a file writer to capture them without polluting the terminal.
func SetOutput(w io.Writer) {
	writer = w
	rebuild()
}

// UseJSON switches the default logger to the standard slog.JSONHandler at
// Debug level, writing to w. This is the format the scenario harness and any
// production log collector consume; human Debug logs are preserved in it.
func UseJSON(w io.Writer) {
	format = "json"
	writer = w
	rebuild()
}

// SetNode installs a permanent "node" attribute (a short identity prefix) on
// every subsequent record, for both output formats. Safe to call before or
// after UseJSON/SetOutput; it always rebuilds from the current state.
func SetNode(prefix string) {
	nodeAttr = slog.String("node", prefix)
	rebuild()
}

// rebuild reconstructs the default logger from the package-level format,
// writer and nodeAttr state, so every setter leaves a fully consistent logger
// behind regardless of call order.
func rebuild() {
	var handler slog.Handler
	if format == "json" {
		handler = slog.NewJSONHandler(writer, &slog.HandlerOptions{Level: slog.LevelDebug})
	} else {
		handler = NewHandler(writer)
	}

	if nodeAttr.Key != "" {
		handler = handler.WithAttrs([]slog.Attr{nodeAttr})
	}

	defaultLogger = slog.New(handler)
	slog.SetDefault(defaultLogger)
}

// Handler is a custom slog handler with precise timestamps.
type Handler struct {
	shared *handlerState // shared holds the output and its lock, common to every WithAttrs copy
	attrs  []slog.Attr   // attrs are logger-level attributes installed via WithAttrs, rendered before record attrs
}

// handlerState is the mutable state shared by a Handler and every copy
// produced by WithAttrs, so writes from all of them serialize on one lock.
type handlerState struct {
	out io.Writer  // out is the destination for formatted log lines
	mu  sync.Mutex // mu serializes writes to out
}

// NewHandler creates a new handler writing to the given writer.
func NewHandler(out io.Writer) *Handler {
	return &Handler{shared: &handlerState{out: out}}
}

// Enabled returns true for all levels.
func (h *Handler) Enabled(_ context.Context, _ slog.Level) bool {
	return true
}

// Handle formats and writes a log record, rendering the handler's own
// logger-level attributes (from WithAttrs) before the record's own attributes.
func (h *Handler) Handle(_ context.Context, r slog.Record) error {
	// Format: 2024-01-15 14:30:45.123 [INFO] message key=value
	ts := r.Time.Format("2006-01-02 15:04:05.000")
	level := levelString(r.Level)

	h.shared.mu.Lock()
	defer h.shared.mu.Unlock()

	fmt.Fprintf(h.shared.out, "%s [%s] %s", ts, level, r.Message)

	for _, a := range h.attrs {
		fmt.Fprintf(h.shared.out, " %s=%v", a.Key, a.Value)
	}

	r.Attrs(func(a slog.Attr) bool {
		fmt.Fprintf(h.shared.out, " %s=%v", a.Key, a.Value)
		return true
	})

	fmt.Fprintln(h.shared.out)

	return nil
}

// WithAttrs returns a copy of the handler with attrs appended to its
// logger-level attribute set. The copy shares the underlying writer and lock
// with h, so both keep writing to the same serialized stream.
func (h *Handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	next := make([]slog.Attr, 0, len(h.attrs)+len(attrs))
	next = append(next, h.attrs...)
	next = append(next, attrs...)

	return &Handler{shared: h.shared, attrs: next}
}

// WithGroup returns a new handler with the given group.
func (h *Handler) WithGroup(_ string) slog.Handler {
	return h
}

// levelString returns a short string for the log level.
func levelString(l slog.Level) string {
	switch l {
	case slog.LevelDebug:
		return "DBG"
	case slog.LevelInfo:
		return "INF"
	case slog.LevelWarn:
		return "WRN"
	case slog.LevelError:
		return "ERR"
	default:
		return "???"
	}
}

// Info logs at INFO level.
func Info(msg string, args ...any) {
	slog.Info(msg, args...)
}

// Debug logs at DEBUG level.
func Debug(msg string, args ...any) {
	slog.Debug(msg, args...)
}

// Warn logs at WARN level.
func Warn(msg string, args ...any) {
	slog.Warn(msg, args...)
}

// Error logs at ERROR level.
func Error(msg string, args ...any) {
	slog.Error(msg, args...)
}

// With returns a logger with the given attributes.
func With(args ...any) *slog.Logger {
	return slog.Default().With(args...)
}

// Timed returns elapsed time since start for logging duration.
func Timed(start time.Time) slog.Attr {
	return slog.Duration("elapsed", time.Since(start))
}
