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

var (
	defaultLogger *slog.Logger
	once          sync.Once
)

// Init initializes the global logger with timestamp precision to milliseconds.
func Init() {
	once.Do(func() {
		handler := NewHandler(os.Stdout)
		defaultLogger = slog.New(handler)
		slog.SetDefault(defaultLogger)
	})
}

// Handler is a custom slog handler with precise timestamps.
type Handler struct {
	out io.Writer
	mu  sync.Mutex
}

// NewHandler creates a new handler writing to the given writer.
func NewHandler(out io.Writer) *Handler {
	return &Handler{out: out}
}

// Enabled returns true for all levels.
func (h *Handler) Enabled(_ context.Context, _ slog.Level) bool {
	return true
}

// Handle formats and writes a log record.
func (h *Handler) Handle(_ context.Context, r slog.Record) error {
	// Format: 2024-01-15 14:30:45.123 [INFO] message key=value
	ts := r.Time.Format("2006-01-02 15:04:05.000")
	level := levelString(r.Level)

	h.mu.Lock()
	defer h.mu.Unlock()

	// Write timestamp and level
	fmt.Fprintf(h.out, "%s [%s] %s", ts, level, r.Message)

	// Write attributes
	r.Attrs(func(a slog.Attr) bool {
		fmt.Fprintf(h.out, " %s=%v", a.Key, a.Value)
		return true
	})

	fmt.Fprintln(h.out)

	return nil
}

// WithAttrs returns a new handler with the given attributes.
func (h *Handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return h
}

// WithGroup returns a new handler with the given group.
func (h *Handler) WithGroup(name string) slog.Handler {
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
