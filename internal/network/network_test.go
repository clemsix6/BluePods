package network

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// generateTestKey generates a random ed25519 key pair for testing.
func generateTestKey(t *testing.T) ed25519.PrivateKey {
	t.Helper()

	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	return priv
}

// TestNodeStartStop tests starting and stopping a node.
func TestNodeStartStop(t *testing.T) {
	node, err := NewNode(Config{
		PrivateKey: generateTestKey(t),
		ListenAddr: "127.0.0.1:0",
	})
	if err != nil {
		t.Fatalf("create node: %v", err)
	}

	if err := node.Start(); err != nil {
		t.Fatalf("start node: %v", err)
	}

	if err := node.Close(); err != nil {
		t.Fatalf("close node: %v", err)
	}
}

// TestNodeConnect tests connecting two nodes.
func TestNodeConnect(t *testing.T) {
	// Create and start server node
	serverKey := generateTestKey(t)
	server, err := NewNode(Config{
		PrivateKey: serverKey,
		ListenAddr: "127.0.0.1:0",
	})
	if err != nil {
		t.Fatalf("create server: %v", err)
	}

	if err := server.Start(); err != nil {
		t.Fatalf("start server: %v", err)
	}
	defer server.Close()

	serverAddr := server.Addr()

	// Track connections on server
	var serverConnected atomic.Bool
	server.OnConnect(func(p *Peer) {
		serverConnected.Store(true)
	})

	// Create and start client node
	client, err := NewNode(Config{
		PrivateKey: generateTestKey(t),
		ListenAddr: "127.0.0.1:0",
	})
	if err != nil {
		t.Fatalf("create client: %v", err)
	}

	if err := client.Start(); err != nil {
		t.Fatalf("start client: %v", err)
	}
	defer client.Close()

	// Connect client to server
	peer, err := client.Connect(serverAddr)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}

	// Verify peer public key matches server
	if !bytes.Equal(peer.PublicKey(), serverKey.Public().(ed25519.PublicKey)) {
		t.Error("peer public key mismatch")
	}

	// Wait for server to register connection
	time.Sleep(100 * time.Millisecond)

	if !serverConnected.Load() {
		t.Error("server did not receive connection")
	}

	// Verify peer count
	if len(client.Peers()) != 1 {
		t.Errorf("client peer count: got %d, want 1", len(client.Peers()))
	}

	if len(server.Peers()) != 1 {
		t.Errorf("server peer count: got %d, want 1", len(server.Peers()))
	}
}

// TestNodeSendMessage tests sending messages between nodes.
func TestNodeSendMessage(t *testing.T) {
	// Create and start server node
	server, err := NewNode(Config{
		PrivateKey: generateTestKey(t),
		ListenAddr: "127.0.0.1:0",
	})
	if err != nil {
		t.Fatalf("create server: %v", err)
	}

	if err := server.Start(); err != nil {
		t.Fatalf("start server: %v", err)
	}
	defer server.Close()

	serverAddr := server.Addr()

	// Track received messages
	var receivedMsg []byte
	var receivedMu sync.Mutex
	msgReceived := make(chan struct{})

	server.OnMessage(func(p *Peer, data []byte) {
		receivedMu.Lock()
		receivedMsg = data
		receivedMu.Unlock()
		close(msgReceived)
	})

	// Create and start client node
	client, err := NewNode(Config{
		PrivateKey: generateTestKey(t),
		ListenAddr: "127.0.0.1:0",
	})
	if err != nil {
		t.Fatalf("create client: %v", err)
	}

	if err := client.Start(); err != nil {
		t.Fatalf("start client: %v", err)
	}
	defer client.Close()

	// Connect and send message
	peer, err := client.Connect(serverAddr)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}

	testMsg := []byte("hello, bluepods!")

	if err := peer.Send(testMsg); err != nil {
		t.Fatalf("send: %v", err)
	}

	// Wait for message
	select {
	case <-msgReceived:
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for message")
	}

	receivedMu.Lock()
	if !bytes.Equal(receivedMsg, testMsg) {
		t.Errorf("message mismatch: got %q, want %q", receivedMsg, testMsg)
	}
	receivedMu.Unlock()
}

// TestNodeBroadcast tests broadcasting messages to all peers.
func TestNodeBroadcast(t *testing.T) {
	// Create and start broadcaster node
	broadcaster, err := NewNode(Config{
		PrivateKey: generateTestKey(t),
		ListenAddr: "127.0.0.1:0",
	})
	if err != nil {
		t.Fatalf("create broadcaster: %v", err)
	}

	if err := broadcaster.Start(); err != nil {
		t.Fatalf("start broadcaster: %v", err)
	}
	defer broadcaster.Close()

	broadcasterAddr := broadcaster.Addr()

	// Create receivers
	const numReceivers = 3
	var receivers []*Node
	var receivedCount atomic.Int32
	var wg sync.WaitGroup

	for i := 0; i < numReceivers; i++ {
		receiver, err := NewNode(Config{
			PrivateKey: generateTestKey(t),
			ListenAddr: "127.0.0.1:0",
		})
		if err != nil {
			t.Fatalf("create receiver %d: %v", i, err)
		}

		if err := receiver.Start(); err != nil {
			t.Fatalf("start receiver %d: %v", i, err)
		}
		defer receiver.Close()

		receiver.OnMessage(func(p *Peer, data []byte) {
			receivedCount.Add(1)
			wg.Done()
		})

		receivers = append(receivers, receiver)
	}

	// Connect all receivers to broadcaster
	for i, receiver := range receivers {
		if _, err := receiver.Connect(broadcasterAddr); err != nil {
			t.Fatalf("connect receiver %d: %v", i, err)
		}
	}

	// Wait for connections to establish
	time.Sleep(100 * time.Millisecond)

	// Broadcast message
	wg.Add(numReceivers)

	if err := broadcaster.Broadcast([]byte("broadcast test")); err != nil {
		t.Fatalf("broadcast: %v", err)
	}

	// Wait for all receivers
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("timeout: received %d/%d messages", receivedCount.Load(), numReceivers)
	}

	if receivedCount.Load() != numReceivers {
		t.Errorf("received count: got %d, want %d", receivedCount.Load(), numReceivers)
	}
}

// TestNodeDisconnect tests disconnect handling.
func TestNodeDisconnect(t *testing.T) {
	// Create and start server node
	server, err := NewNode(Config{
		PrivateKey: generateTestKey(t),
		ListenAddr: "127.0.0.1:0",
	})
	if err != nil {
		t.Fatalf("create server: %v", err)
	}

	if err := server.Start(); err != nil {
		t.Fatalf("start server: %v", err)
	}
	defer server.Close()

	serverAddr := server.Addr()

	// Track disconnections
	disconnected := make(chan struct{})
	server.OnDisconnect(func(p *Peer) {
		close(disconnected)
	})

	// Create and start client node
	client, err := NewNode(Config{
		PrivateKey: generateTestKey(t),
		ListenAddr: "127.0.0.1:0",
	})
	if err != nil {
		t.Fatalf("create client: %v", err)
	}

	if err := client.Start(); err != nil {
		t.Fatalf("start client: %v", err)
	}

	// Connect then close client
	if _, err := client.Connect(serverAddr); err != nil {
		t.Fatalf("connect: %v", err)
	}

	time.Sleep(100 * time.Millisecond)

	client.Close()

	// Wait for disconnect callback
	select {
	case <-disconnected:
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for disconnect")
	}

	// Server should have no peers
	time.Sleep(100 * time.Millisecond)

	if len(server.Peers()) != 0 {
		t.Errorf("server peer count: got %d, want 0", len(server.Peers()))
	}
}

// TestNodeReconnect tests automatic reconnection.
func TestNodeReconnect(t *testing.T) {
	// Create and start server node
	serverKey := generateTestKey(t)
	server, err := NewNode(Config{
		PrivateKey: serverKey,
		ListenAddr: "127.0.0.1:0",
	})
	if err != nil {
		t.Fatalf("create server: %v", err)
	}

	if err := server.Start(); err != nil {
		t.Fatalf("start server: %v", err)
	}

	serverAddr := server.Addr()

	// Create client with short reconnect delay
	client, err := NewNode(Config{
		PrivateKey:     generateTestKey(t),
		ListenAddr:     "127.0.0.1:0",
		ReconnectDelay: 200 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("create client: %v", err)
	}

	if err := client.Start(); err != nil {
		t.Fatalf("start client: %v", err)
	}
	defer client.Close()

	// Connect client to server
	if _, err := client.Connect(serverAddr); err != nil {
		t.Fatalf("connect: %v", err)
	}

	time.Sleep(100 * time.Millisecond)

	// Close server (simulating disconnect)
	server.Close()

	// Create new server on a DIFFERENT port (simulates server restart on new port)
	server2, err := NewNode(Config{
		PrivateKey: serverKey,
		ListenAddr: "127.0.0.1:0",
	})
	if err != nil {
		t.Fatalf("create server2: %v", err)
	}

	if err := server2.Start(); err != nil {
		t.Fatalf("start server2: %v", err)
	}
	defer server2.Close()

	newServerAddr := server2.Addr()

	reconnected := make(chan struct{})
	server2.OnConnect(func(p *Peer) {
		close(reconnected)
	})

	// Update the known address in client (simulating address discovery)
	client.knownAddrsMu.Lock()
	for k := range client.knownAddrs {
		client.knownAddrs[k] = newServerAddr
	}
	client.knownAddrsMu.Unlock()

	// Wait for reconnection
	select {
	case <-reconnected:
	case <-time.After(10 * time.Second):
		t.Fatal("timeout waiting for reconnection")
	}
}

// TestLargeMessage tests sending large messages.
func TestLargeMessage(t *testing.T) {
	// Create and start server node
	server, err := NewNode(Config{
		PrivateKey: generateTestKey(t),
		ListenAddr: "127.0.0.1:0",
	})
	if err != nil {
		t.Fatalf("create server: %v", err)
	}

	if err := server.Start(); err != nil {
		t.Fatalf("start server: %v", err)
	}
	defer server.Close()

	serverAddr := server.Addr()

	// Track received message
	var receivedMsg []byte
	msgReceived := make(chan struct{})

	server.OnMessage(func(p *Peer, data []byte) {
		receivedMsg = data
		close(msgReceived)
	})

	// Create client
	client, err := NewNode(Config{
		PrivateKey: generateTestKey(t),
		ListenAddr: "127.0.0.1:0",
	})
	if err != nil {
		t.Fatalf("create client: %v", err)
	}

	if err := client.Start(); err != nil {
		t.Fatalf("start client: %v", err)
	}
	defer client.Close()

	// Connect and send large message (1 MB)
	peer, err := client.Connect(serverAddr)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}

	largeMsg := make([]byte, 1<<20) // 1 MB
	for i := range largeMsg {
		largeMsg[i] = byte(i % 256)
	}

	if err := peer.Send(largeMsg); err != nil {
		t.Fatalf("send: %v", err)
	}

	// Wait for message
	select {
	case <-msgReceived:
	case <-time.After(10 * time.Second):
		t.Fatal("timeout waiting for message")
	}

	if !bytes.Equal(receivedMsg, largeMsg) {
		t.Error("large message mismatch")
	}
}

// TestConcurrentSend tests sending multiple messages concurrently.
func TestConcurrentSend(t *testing.T) {
	// Create and start server node
	server, err := NewNode(Config{
		PrivateKey: generateTestKey(t),
		ListenAddr: "127.0.0.1:0",
	})
	if err != nil {
		t.Fatalf("create server: %v", err)
	}

	if err := server.Start(); err != nil {
		t.Fatalf("start server: %v", err)
	}
	defer server.Close()

	serverAddr := server.Addr()

	// Track received messages
	const numMessages = 100
	var receivedCount atomic.Int32

	server.OnMessage(func(p *Peer, data []byte) {
		receivedCount.Add(1)
	})

	// Create client
	client, err := NewNode(Config{
		PrivateKey: generateTestKey(t),
		ListenAddr: "127.0.0.1:0",
	})
	if err != nil {
		t.Fatalf("create client: %v", err)
	}

	if err := client.Start(); err != nil {
		t.Fatalf("start client: %v", err)
	}
	defer client.Close()

	// Connect
	peer, err := client.Connect(serverAddr)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}

	// Send messages concurrently
	var wg sync.WaitGroup
	wg.Add(numMessages)

	for i := 0; i < numMessages; i++ {
		go func(i int) {
			defer wg.Done()

			msg := []byte{byte(i)}
			if err := peer.Send(msg); err != nil {
				t.Errorf("send %d: %v", i, err)
			}
		}(i)
	}

	wg.Wait()

	// Wait for all messages to arrive
	time.Sleep(500 * time.Millisecond)

	if receivedCount.Load() != numMessages {
		t.Errorf("received count: got %d, want %d", receivedCount.Load(), numMessages)
	}
}

// TestDedupBasic tests basic deduplication functionality.
func TestDedupBasic(t *testing.T) {
	d := NewDedup()
	defer d.Close()

	msg := []byte("test message")

	// First check should return true (new message)
	if !d.Check(msg) {
		t.Error("first check should return true")
	}

	// Second check should return false (duplicate)
	if d.Check(msg) {
		t.Error("second check should return false")
	}

	// Different message should return true
	if !d.Check([]byte("different message")) {
		t.Error("different message should return true")
	}
}

// TestDedupConcurrent tests concurrent deduplication.
func TestDedupConcurrent(t *testing.T) {
	d := NewDedup()
	defer d.Close()

	const numGoroutines = 100
	msg := []byte("same message")

	var successCount atomic.Int32
	var wg sync.WaitGroup
	wg.Add(numGoroutines)

	for i := 0; i < numGoroutines; i++ {
		go func() {
			defer wg.Done()
			if d.Check(msg) {
				successCount.Add(1)
			}
		}()
	}

	wg.Wait()

	// Only one goroutine should succeed
	if successCount.Load() != 1 {
		t.Errorf("success count: got %d, want 1", successCount.Load())
	}
}

// TestDedupExpiry tests that entries expire after TTL.
func TestDedupExpiry(t *testing.T) {
	d := NewDedup()
	defer d.Close()

	// Override TTL for testing (use internal access)
	d.ttl = int64(100 * time.Millisecond)

	msg := []byte("expiring message")

	// First check
	if !d.Check(msg) {
		t.Error("first check should return true")
	}

	// Immediate second check should fail
	if d.Check(msg) {
		t.Error("immediate second check should return false")
	}

	// Wait for expiry + cleanup
	time.Sleep(200 * time.Millisecond)

	// After expiry, should return true again
	if !d.Check(msg) {
		t.Error("check after expiry should return true")
	}
}

// TestGetPeer tests lookup by public key.
func TestGetPeer(t *testing.T) {
	// Create and start server node
	serverKey := generateTestKey(t)
	server, err := NewNode(Config{
		PrivateKey: serverKey,
		ListenAddr: "127.0.0.1:0",
	})
	if err != nil {
		t.Fatalf("create server: %v", err)
	}

	if err := server.Start(); err != nil {
		t.Fatalf("start server: %v", err)
	}
	defer server.Close()

	// Create client
	clientKey := generateTestKey(t)
	client, err := NewNode(Config{
		PrivateKey: clientKey,
		ListenAddr: "127.0.0.1:0",
	})
	if err != nil {
		t.Fatalf("create client: %v", err)
	}

	if err := client.Start(); err != nil {
		t.Fatalf("start client: %v", err)
	}
	defer client.Close()

	// Before connecting, GetPeer should return nil
	if peer := client.GetPeer(serverKey.Public().(ed25519.PublicKey)); peer != nil {
		t.Error("GetPeer should return nil before connecting")
	}

	// Connect
	if _, err := client.Connect(server.Addr()); err != nil {
		t.Fatalf("connect: %v", err)
	}

	time.Sleep(100 * time.Millisecond)

	// After connecting, GetPeer should return the peer
	peer := client.GetPeer(serverKey.Public().(ed25519.PublicKey))
	if peer == nil {
		t.Fatal("GetPeer should return peer after connecting")
	}

	if !bytes.Equal(peer.PublicKey(), serverKey.Public().(ed25519.PublicKey)) {
		t.Error("peer public key mismatch")
	}

	// GetPeer with unknown key should return nil
	unknownKey := generateTestKey(t)
	if peer := client.GetPeer(unknownKey.Public().(ed25519.PublicKey)); peer != nil {
		t.Error("GetPeer should return nil for unknown key")
	}
}

// TestRequestResponse tests bidirectional stream request/response.
func TestRequestResponse(t *testing.T) {
	// Create server
	server, err := NewNode(Config{
		PrivateKey: generateTestKey(t),
		ListenAddr: "127.0.0.1:0",
	})
	if err != nil {
		t.Fatalf("create server: %v", err)
	}

	if err := server.Start(); err != nil {
		t.Fatalf("start server: %v", err)
	}
	defer server.Close()

	// Set up request handler that echoes back with prefix
	server.OnRequest(func(p *Peer, data []byte) ([]byte, error) {
		return append([]byte("echo:"), data...), nil
	})

	// Create client
	client, err := NewNode(Config{
		PrivateKey: generateTestKey(t),
		ListenAddr: "127.0.0.1:0",
	})
	if err != nil {
		t.Fatalf("create client: %v", err)
	}

	if err := client.Start(); err != nil {
		t.Fatalf("start client: %v", err)
	}
	defer client.Close()

	// Connect
	peer, err := client.Connect(server.Addr())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}

	time.Sleep(100 * time.Millisecond)

	// Send request
	ctx := context.Background()
	response, err := peer.Request(ctx, []byte("hello"))
	if err != nil {
		t.Fatalf("request: %v", err)
	}

	expected := []byte("echo:hello")
	if !bytes.Equal(response, expected) {
		t.Errorf("response mismatch: got %q, want %q", response, expected)
	}
}

// TestRequestTimeout tests request timeout handling.
func TestRequestTimeout(t *testing.T) {
	// Create server
	server, err := NewNode(Config{
		PrivateKey: generateTestKey(t),
		ListenAddr: "127.0.0.1:0",
	})
	if err != nil {
		t.Fatalf("create server: %v", err)
	}

	if err := server.Start(); err != nil {
		t.Fatalf("start server: %v", err)
	}
	defer server.Close()

	// Set up slow handler that delays response
	server.OnRequest(func(p *Peer, data []byte) ([]byte, error) {
		time.Sleep(500 * time.Millisecond)
		return []byte("late"), nil
	})

	// Create client
	client, err := NewNode(Config{
		PrivateKey: generateTestKey(t),
		ListenAddr: "127.0.0.1:0",
	})
	if err != nil {
		t.Fatalf("create client: %v", err)
	}

	if err := client.Start(); err != nil {
		t.Fatalf("start client: %v", err)
	}
	defer client.Close()

	// Connect
	peer, err := client.Connect(server.Addr())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}

	time.Sleep(100 * time.Millisecond)

	// Send request with short timeout
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	_, err = peer.Request(ctx, []byte("hello"))
	if err == nil {
		t.Error("expected timeout error")
	}
}

// TestDedupIntegration tests deduplication in the network layer.
func TestDedupIntegration(t *testing.T) {
	// Create server
	server, err := NewNode(Config{
		PrivateKey: generateTestKey(t),
		ListenAddr: "127.0.0.1:0",
	})
	if err != nil {
		t.Fatalf("create server: %v", err)
	}

	if err := server.Start(); err != nil {
		t.Fatalf("start server: %v", err)
	}
	defer server.Close()

	serverAddr := server.Addr()

	// Track received messages
	var receivedCount atomic.Int32
	server.OnMessage(func(p *Peer, data []byte) {
		receivedCount.Add(1)
	})

	// Create two clients
	client1, _ := NewNode(Config{
		PrivateKey: generateTestKey(t),
		ListenAddr: "127.0.0.1:0",
	})
	client1.Start()
	defer client1.Close()

	client2, _ := NewNode(Config{
		PrivateKey: generateTestKey(t),
		ListenAddr: "127.0.0.1:0",
	})
	client2.Start()
	defer client2.Close()

	peer1, _ := client1.Connect(serverAddr)
	peer2, _ := client2.Connect(serverAddr)

	// Both clients send the same message
	msg := []byte("duplicate message from different peers")
	peer1.Send(msg)
	peer2.Send(msg)

	time.Sleep(200 * time.Millisecond)

	// Only one message should be received (dedup)
	if receivedCount.Load() != 1 {
		t.Errorf("received count: got %d, want 1 (dedup failed)", receivedCount.Load())
	}
}

// TestGossipSelectsSubset tests that Gossip sends to fanout peers when peers > fanout.
func TestGossipSelectsSubset(t *testing.T) {
	// Create broadcaster node
	broadcaster, err := NewNode(Config{
		PrivateKey: generateTestKey(t),
		ListenAddr: "127.0.0.1:0",
	})
	if err != nil {
		t.Fatalf("create broadcaster: %v", err)
	}

	if err := broadcaster.Start(); err != nil {
		t.Fatalf("start broadcaster: %v", err)
	}
	defer broadcaster.Close()

	broadcasterAddr := broadcaster.Addr()

	// Create 5 receivers
	const numReceivers = 5
	const fanout = 2

	var receivers []*Node
	var receivedCount atomic.Int32

	for i := 0; i < numReceivers; i++ {
		receiver, err := NewNode(Config{
			PrivateKey: generateTestKey(t),
			ListenAddr: "127.0.0.1:0",
		})
		if err != nil {
			t.Fatalf("create receiver %d: %v", i, err)
		}

		if err := receiver.Start(); err != nil {
			t.Fatalf("start receiver %d: %v", i, err)
		}
		defer receiver.Close()

		receiver.OnMessage(func(p *Peer, data []byte) {
			receivedCount.Add(1)
		})

		receivers = append(receivers, receiver)
	}

	// Connect all receivers to broadcaster
	for i, receiver := range receivers {
		if _, err := receiver.Connect(broadcasterAddr); err != nil {
			t.Fatalf("connect receiver %d: %v", i, err)
		}
	}

	time.Sleep(100 * time.Millisecond)

	// Gossip with fanout=2 (should only reach 2 of 5 receivers)
	if err := broadcaster.Gossip([]byte("gossip test"), fanout); err != nil {
		t.Fatalf("gossip: %v", err)
	}

	time.Sleep(200 * time.Millisecond)

	// Should receive exactly fanout messages
	if receivedCount.Load() != fanout {
		t.Errorf("received count: got %d, want %d", receivedCount.Load(), fanout)
	}
}

// TestGossipFallsBackToBroadcast tests that Gossip sends to all peers when fanout >= peers.
func TestGossipFallsBackToBroadcast(t *testing.T) {
	// Create broadcaster node
	broadcaster, err := NewNode(Config{
		PrivateKey: generateTestKey(t),
		ListenAddr: "127.0.0.1:0",
	})
	if err != nil {
		t.Fatalf("create broadcaster: %v", err)
	}

	if err := broadcaster.Start(); err != nil {
		t.Fatalf("start broadcaster: %v", err)
	}
	defer broadcaster.Close()

	broadcasterAddr := broadcaster.Addr()

	// Create 3 receivers
	const numReceivers = 3
	const fanout = 10 // Larger than numReceivers

	var receivers []*Node
	var receivedCount atomic.Int32
	var wg sync.WaitGroup

	for i := 0; i < numReceivers; i++ {
		receiver, err := NewNode(Config{
			PrivateKey: generateTestKey(t),
			ListenAddr: "127.0.0.1:0",
		})
		if err != nil {
			t.Fatalf("create receiver %d: %v", i, err)
		}

		if err := receiver.Start(); err != nil {
			t.Fatalf("start receiver %d: %v", i, err)
		}
		defer receiver.Close()

		receiver.OnMessage(func(p *Peer, data []byte) {
			receivedCount.Add(1)
			wg.Done()
		})

		receivers = append(receivers, receiver)
	}

	// Connect all receivers to broadcaster
	for i, receiver := range receivers {
		if _, err := receiver.Connect(broadcasterAddr); err != nil {
			t.Fatalf("connect receiver %d: %v", i, err)
		}
	}

	time.Sleep(100 * time.Millisecond)

	// Gossip with fanout > numReceivers (should reach all)
	wg.Add(numReceivers)

	if err := broadcaster.Gossip([]byte("gossip fallback test"), fanout); err != nil {
		t.Fatalf("gossip: %v", err)
	}

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("timeout: received %d/%d messages", receivedCount.Load(), numReceivers)
	}

	// Should receive all messages
	if receivedCount.Load() != numReceivers {
		t.Errorf("received count: got %d, want %d", receivedCount.Load(), numReceivers)
	}
}

// TestSelectRandomPeers tests the peer selection helper.
func TestSelectRandomPeers(t *testing.T) {
	// Create mock peers
	peers := make([]*Peer, 10)
	for i := range peers {
		peers[i] = &Peer{}
	}

	// Test n > len(peers) returns all
	selected := selectRandomPeers(peers, 20)
	if len(selected) != 10 {
		t.Errorf("n > len: got %d, want 10", len(selected))
	}

	// Test n == len(peers) returns all
	selected = selectRandomPeers(peers, 10)
	if len(selected) != 10 {
		t.Errorf("n == len: got %d, want 10", len(selected))
	}

	// Test n < len(peers) returns n
	selected = selectRandomPeers(peers, 3)
	if len(selected) != 3 {
		t.Errorf("n < len: got %d, want 3", len(selected))
	}

	// Test randomness (run multiple times, check we don't always get same result)
	results := make(map[*Peer]int)
	for i := 0; i < 100; i++ {
		selected = selectRandomPeers(peers, 1)
		results[selected[0]]++
	}

	// With 10 peers and 100 iterations, each should be selected ~10 times
	// Check that at least 5 different peers were selected (very unlikely to fail)
	if len(results) < 5 {
		t.Errorf("randomness check: only %d different peers selected", len(results))
	}
}
