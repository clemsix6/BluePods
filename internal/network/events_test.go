package network

import (
	"bytes"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

	"BluePods/internal/events"
)

// captureEvents swaps the default slog logger for a JSON handler writing into a
// fresh buffer, restoring the previous default on test cleanup.
func captureEvents(t *testing.T) *bytes.Buffer {
	t.Helper()

	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })

	return &buf
}

// eventsNamed decodes one JSON object per captured line and returns those whose
// reserved events.Key attribute equals name, in emission order.
func eventsNamed(t *testing.T, buf *bytes.Buffer, name string) []map[string]any {
	t.Helper()

	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}

		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("captured line is not JSON: %v (line=%q)", err, line)
		}

		if rec[events.Key] == name {
			out = append(out, rec)
		}
	}

	return out
}

// TestSetupPeer_EmitsPeerConnectedWithFullKey verifies that establishing a
// connection emits net.peer.connected carrying the FULL hex pubkey (not the
// 16-char prefix the adjacent log line truncates to for readability).
func TestSetupPeer_EmitsPeerConnectedWithFullKey(t *testing.T) {
	serverKey := generateTestKey(t)
	server, err := NewNode(Config{PrivateKey: serverKey, ListenAddr: "127.0.0.1:0"})
	if err != nil {
		t.Fatalf("create server: %v", err)
	}
	if err := server.Start(); err != nil {
		t.Fatalf("start server: %v", err)
	}
	defer server.Close()

	client, err := NewNode(Config{PrivateKey: generateTestKey(t), ListenAddr: "127.0.0.1:0"})
	if err != nil {
		t.Fatalf("create client: %v", err)
	}
	if err := client.Start(); err != nil {
		t.Fatalf("start client: %v", err)
	}
	defer client.Close()

	buf := captureEvents(t)

	if _, err := client.Connect(server.Addr()); err != nil {
		t.Fatalf("connect: %v", err)
	}

	time.Sleep(100 * time.Millisecond)

	recs := eventsNamed(t, buf, events.EvPeerConnected)
	if len(recs) < 1 {
		t.Fatalf("want at least 1 %s event, got %d", events.EvPeerConnected, len(recs))
	}

	wantKey := hex.EncodeToString(serverKey.Public().(ed25519.PublicKey))
	found := false
	for _, rec := range recs {
		if rec["peer"] == wantKey {
			found = true
		}
		// The event must carry the full key, never the truncated log prefix.
		if peer, ok := rec["peer"].(string); ok && len(peer) != 64 {
			t.Errorf("peer = %q, want a full 64-char hex key", peer)
		}
	}
	if !found {
		t.Errorf("no %s event named server's full key %s, got %v", events.EvPeerConnected, wantKey, recs)
	}
}

// TestHandlePeerDisconnect_EmitsPeerDisconnected verifies a torn-down
// connection emits net.peer.disconnected.
func TestHandlePeerDisconnect_EmitsPeerDisconnected(t *testing.T) {
	server, err := NewNode(Config{PrivateKey: generateTestKey(t), ListenAddr: "127.0.0.1:0"})
	if err != nil {
		t.Fatalf("create server: %v", err)
	}
	if err := server.Start(); err != nil {
		t.Fatalf("start server: %v", err)
	}
	defer server.Close()

	disconnected := make(chan struct{})
	server.OnDisconnect(func(p *Peer) { close(disconnected) })

	client, err := NewNode(Config{PrivateKey: generateTestKey(t), ListenAddr: "127.0.0.1:0"})
	if err != nil {
		t.Fatalf("create client: %v", err)
	}
	if err := client.Start(); err != nil {
		t.Fatalf("start client: %v", err)
	}

	if _, err := client.Connect(server.Addr()); err != nil {
		t.Fatalf("connect: %v", err)
	}

	time.Sleep(100 * time.Millisecond)

	buf := captureEvents(t)

	client.Close()

	select {
	case <-disconnected:
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for disconnect")
	}

	time.Sleep(100 * time.Millisecond)

	recs := eventsNamed(t, buf, events.EvPeerDisconnected)
	if len(recs) != 1 {
		t.Fatalf("want 1 %s event, got %d: %v", events.EvPeerDisconnected, len(recs), recs)
	}
}
