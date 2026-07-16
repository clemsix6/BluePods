package events

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

// TestCatalogNamesUniqueAndDotted asserts every catalog name is unique and dotted.
func TestCatalogNamesUniqueAndDotted(t *testing.T) {
	seen := map[string]bool{}
	for _, n := range Names {
		if seen[n] {
			t.Fatalf("duplicate event name %q", n)
		}
		seen[n] = true
		if !strings.Contains(n, ".") {
			t.Fatalf("event name %q is not dotted", n)
		}
	}
	if len(Names) < 30 {
		t.Fatalf("catalog too small: %d", len(Names))
	}
}

// TestEmitCarriesReservedKey asserts a constructor emits one record with the
// reserved key, the name as message, and its typed attributes.
func TestEmitCarriesReservedKey(t *testing.T) {
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	defer slog.SetDefault(old)

	TxCommitted([32]byte{0xAA}, [32]byte{0xBB}, 7, false, "version_conflict")

	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatalf("not JSON: %v", err)
	}
	if rec[Key] != "tx.committed" || rec["msg"] != "tx.committed" {
		t.Fatalf("bad record: %v", rec)
	}
	if rec["round"] != float64(7) || rec["success"] != false || rec["reason"] != "version_conflict" {
		t.Fatalf("bad attrs: %v", rec)
	}
	if !strings.HasPrefix(rec["tx"].(string), "aa000000") {
		t.Fatalf("tx not lowercase hex: %v", rec["tx"])
	}
	_ = context.Background()
}
