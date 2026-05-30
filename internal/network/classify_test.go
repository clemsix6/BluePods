package network

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"sync/atomic"
	"testing"
	"time"
)

// startTestNode creates and starts a node, registering cleanup.
func startTestNode(t *testing.T) *Node {
	t.Helper()

	n, err := NewNode(Config{
		PrivateKey: generateTestKey(t),
		ListenAddr: "127.0.0.1:0",
	})
	if err != nil {
		t.Fatalf("create node: %v", err)
	}

	if err := n.Start(); err != nil {
		t.Fatalf("start node: %v", err)
	}

	t.Cleanup(func() { n.Close() })

	return n
}

// TestValidatorConnectionJoinsMesh verifies that a connection whose certificate
// public key satisfies the injected predicate is admitted to the trusted mesh
// (OnConnect fires) and that mesh request/response still works under
// RequestClientCert.
func TestValidatorConnectionJoinsMesh(t *testing.T) {
	server := startTestNode(t)
	validator := startTestNode(t)

	// Only the validator's key is accepted as a mesh peer.
	validatorKey := validator.PublicKey()
	server.SetValidatorPredicate(func(pk ed25519.PublicKey) bool {
		return bytes.Equal(pk, validatorKey)
	})

	server.OnRequest(func(_ *Peer, data []byte) ([]byte, error) {
		return append([]byte("echo:"), data...), nil
	})

	var connected atomic.Bool
	server.OnConnect(func(_ *Peer) { connected.Store(true) })

	peer, err := validator.Connect(server.Addr())
	if err != nil {
		t.Fatalf("validator connect: %v", err)
	}

	time.Sleep(150 * time.Millisecond)

	if !connected.Load() {
		t.Fatal("OnConnect did not fire for an accepted validator connection")
	}

	if server.GetPeer(validatorKey) == nil {
		t.Fatal("validator was not added to the peer mesh")
	}

	resp, err := peer.Request(context.Background(), []byte("hi"))
	if err != nil {
		t.Fatalf("mesh request: %v", err)
	}
	if !bytes.Equal(resp, []byte("echo:hi")) {
		t.Fatalf("mesh response mismatch: %q", resp)
	}
}

// TestClientConnectionUsesEphemeralTier verifies that a connection rejected by
// the predicate is served in the ephemeral client tier: it never joins the peer
// mesh (OnConnect does not fire), yet a request still reaches the handler.
func TestClientConnectionUsesEphemeralTier(t *testing.T) {
	server := startTestNode(t)
	client := startTestNode(t)

	// No key is a validator: every incoming connection is an ephemeral client.
	server.SetValidatorPredicate(func(ed25519.PublicKey) bool { return false })

	server.OnRequest(func(_ *Peer, data []byte) ([]byte, error) {
		return append([]byte("echo:"), data...), nil
	})

	var connected atomic.Bool
	server.OnConnect(func(_ *Peer) { connected.Store(true) })

	peer, err := client.Connect(server.Addr())
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}

	resp, err := peer.Request(context.Background(), []byte("ping"))
	if err != nil {
		t.Fatalf("client request: %v", err)
	}
	if !bytes.Equal(resp, []byte("echo:ping")) {
		t.Fatalf("client response mismatch: %q", resp)
	}

	time.Sleep(150 * time.Millisecond)

	if connected.Load() {
		t.Fatal("OnConnect fired for an ephemeral client connection")
	}

	if server.GetPeer(client.PublicKey()) != nil {
		t.Fatal("ephemeral client was added to the peer mesh")
	}
}
