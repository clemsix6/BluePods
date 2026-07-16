package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"BluePods/internal/network"
	"BluePods/internal/sync"
)

// TestHandleStateFingerprint_DisabledRefusal verifies that without
// --test-hooks, the fingerprint handler returns the typed refusal and touches
// neither the dag nor the state (both left nil here, which would otherwise
// panic if the handler reached past the guard).
func TestHandleStateFingerprint_DisabledRefusal(t *testing.T) {
	n := &Node{cfg: &Config{TestHooks: false}}

	respBytes, err := n.handleStateFingerprint()
	if err != nil {
		t.Fatalf("handleStateFingerprint: %v", err)
	}

	resp, err := network.DecodeStateFingerprintResp(respBytes)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}

	if resp.Err != testHooksDisabledErr {
		t.Fatalf("err = %q, want %q", resp.Err, testHooksDisabledErr)
	}

	if resp.Round != 0 || resp.TotalSupply != 0 || resp.Checksum != ([32]byte{}) {
		t.Fatalf("refusal response should be zeroed besides Err: %+v", resp)
	}
}

// TestHandleStateFingerprint_Success verifies that with --test-hooks enabled,
// the handler returns the same fingerprint sync.ComputeFingerprint would, over
// a genesis-seeded node.
func TestHandleStateFingerprint_Success(t *testing.T) {
	_, privKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	n, db := bootstrapTestNode(t, t.TempDir(), privKey)
	defer db.Close()
	defer n.dag.Close()

	n.cfg.TestHooks = true
	n.seedGenesisState()

	want := sync.ComputeFingerprint(n.dag, n.state)

	respBytes, err := n.handleStateFingerprint()
	if err != nil {
		t.Fatalf("handleStateFingerprint: %v", err)
	}

	got, err := network.DecodeStateFingerprintResp(respBytes)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}

	if got.Err != "" {
		t.Fatalf("unexpected refusal: %q", got.Err)
	}

	if got.Round != want.Round || got.Checksum != want.Checksum || got.TotalSupply != want.TotalSupply ||
		got.CoinsTotal != want.CoinsTotal || got.TotalBonded != want.TotalBonded ||
		got.Deposits != want.Deposits || got.FeesInFlight != want.FeesInFlight {
		t.Fatalf("handler response = %+v, want %+v", got, want)
	}
}

// TestHandleTestControl_DisabledRefusal verifies that without --test-hooks,
// both operations return the typed refusal and emit no event.
func TestHandleTestControl_DisabledRefusal(t *testing.T) {
	n := &Node{cfg: &Config{TestHooks: false}}

	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	defer slog.SetDefault(old)

	req := network.EncodeTestControl(&network.TestControlRequest{Op: network.TestControlOpSetPartition})

	respBytes, err := n.handleTestControl(req)
	if err != nil {
		t.Fatalf("handleTestControl: %v", err)
	}

	resp, err := network.DecodeTestControlResp(respBytes)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}

	if resp.Err != testHooksDisabledErr {
		t.Fatalf("err = %q, want %q", resp.Err, testHooksDisabledErr)
	}

	if buf.Len() != 0 {
		t.Fatalf("expected no event emitted while disabled, got: %s", buf.String())
	}
}

// TestHandleTestControl_SetAndClearEmitEvents verifies that with --test-hooks
// enabled, the set and clear operations succeed and each emits its typed
// partition event.
func TestHandleTestControl_SetAndClearEmitEvents(t *testing.T) {
	_, privKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	netNode, err := network.NewNode(network.Config{PrivateKey: privKey, ListenAddr: "127.0.0.1:0"})
	if err != nil {
		t.Fatalf("create network node: %v", err)
	}
	defer netNode.Close()

	n := &Node{cfg: &Config{TestHooks: true}, network: netNode}

	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	defer slog.SetDefault(old)

	var a, b [32]byte
	a[0] = 0x11
	b[0] = 0x22

	setReq := network.EncodeTestControl(&network.TestControlRequest{
		Op:      network.TestControlOpSetPartition,
		Pubkeys: [][32]byte{a, b},
	})

	respBytes, err := n.handleTestControl(setReq)
	if err != nil {
		t.Fatalf("handleTestControl set: %v", err)
	}

	resp, err := network.DecodeTestControlResp(respBytes)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Err != "" {
		t.Fatalf("unexpected refusal: %q", resp.Err)
	}

	assertEventEmitted(t, &buf, "net.partition.applied")

	clearReq := network.EncodeTestControl(&network.TestControlRequest{Op: network.TestControlOpClearPartition})

	respBytes, err = n.handleTestControl(clearReq)
	if err != nil {
		t.Fatalf("handleTestControl clear: %v", err)
	}

	resp, err = network.DecodeTestControlResp(respBytes)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Err != "" {
		t.Fatalf("unexpected refusal: %q", resp.Err)
	}

	assertEventEmitted(t, &buf, "net.partition.cleared")
}

// TestHandleTestControl_UnknownOp verifies an unrecognized op is refused with
// a descriptive error instead of silently doing nothing.
func TestHandleTestControl_UnknownOp(t *testing.T) {
	_, privKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	netNode, err := network.NewNode(network.Config{PrivateKey: privKey, ListenAddr: "127.0.0.1:0"})
	if err != nil {
		t.Fatalf("create network node: %v", err)
	}
	defer netNode.Close()

	n := &Node{cfg: &Config{TestHooks: true}, network: netNode}

	req := network.EncodeTestControl(&network.TestControlRequest{Op: 0x7F})

	respBytes, err := n.handleTestControl(req)
	if err != nil {
		t.Fatalf("handleTestControl: %v", err)
	}

	resp, err := network.DecodeTestControlResp(respBytes)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}

	if resp.Err == "" || !strings.Contains(resp.Err, "unknown test-control op") {
		t.Fatalf("err = %q, want a message naming the unknown op", resp.Err)
	}
}

// assertEventEmitted scans buf's newline-delimited JSON records for one whose
// "event" attribute equals name, failing the test if none matches.
func assertEventEmitted(t *testing.T, buf *bytes.Buffer, name string) {
	t.Helper()

	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}

		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			continue
		}

		if rec["event"] == name {
			return
		}
	}

	t.Fatalf("event %q not found in log output: %s", name, buf.String())
}
