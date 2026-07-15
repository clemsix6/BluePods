package logger

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// withReset saves the package-level logger state before a test and restores
// it on cleanup, so tests that call UseJSON/SetNode/SetOutput never leak state
// into other tests.
func withReset(t *testing.T) {
	t.Helper()
	savedFormat, savedWriter, savedNode := format, writer, nodeAttr

	t.Cleanup(func() {
		format, writer, nodeAttr = savedFormat, savedWriter, savedNode
		rebuild()
	})
}

// TestTextHandlerWithAttrsRenders asserts that attributes installed via With
// render on every subsequent line, the WithAttrs bug this task fixes.
func TestTextHandlerWithAttrsRenders(t *testing.T) {
	withReset(t)

	var buf bytes.Buffer
	SetOutput(&buf)

	l := With("node", "abcd1234")
	l.Info("first")
	l.Info("second")

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("want 2 lines, got %d: %q", len(lines), buf.String())
	}
	for _, line := range lines {
		if !strings.Contains(line, "node=abcd1234") {
			t.Fatalf("line missing node attr: %q", line)
		}
	}
}

// TestUseJSONEmitsDebugLevelJSON asserts UseJSON installs a JSON handler at
// Debug level, so Debug lines survive and every line unmarshals as JSON.
func TestUseJSONEmitsDebugLevelJSON(t *testing.T) {
	withReset(t)

	var buf bytes.Buffer
	UseJSON(&buf)

	Debug("debug line", "k", "v")
	Info("info line")

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("want 2 lines, got %d: %q", len(lines), buf.String())
	}

	var rec map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &rec); err != nil {
		t.Fatalf("not JSON: %v", err)
	}
	if rec["level"] != "DEBUG" || rec["msg"] != "debug line" || rec["k"] != "v" {
		t.Fatalf("bad debug record: %v", rec)
	}
}

// TestSetNodeAfterUseJSONStampsRecords asserts SetNode installs a permanent
// "node" attribute that appears on records even when called after UseJSON.
func TestSetNodeAfterUseJSONStampsRecords(t *testing.T) {
	withReset(t)

	var buf bytes.Buffer
	UseJSON(&buf)
	SetNode("deadbeef")

	Info("hello")

	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatalf("not JSON: %v", err)
	}
	if rec["node"] != "deadbeef" {
		t.Fatalf("missing node attr: %v", rec)
	}
}
