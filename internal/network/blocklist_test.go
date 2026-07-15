package network

import (
	"context"
	"crypto/ed25519"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// newConnectedPair starts two nodes and connects a to b, returning both nodes
// and the peer objects each side holds for the other.
func newConnectedPair(t *testing.T) (a, b *Node, peerAtoB, peerBtoA *Peer) {
	t.Helper()

	a, err := NewNode(Config{PrivateKey: generateTestKey(t), ListenAddr: "127.0.0.1:0"})
	if err != nil {
		t.Fatalf("create a: %v", err)
	}
	if err := a.Start(); err != nil {
		t.Fatalf("start a: %v", err)
	}
	t.Cleanup(func() { a.Close() })

	b, err = NewNode(Config{PrivateKey: generateTestKey(t), ListenAddr: "127.0.0.1:0"})
	if err != nil {
		t.Fatalf("create b: %v", err)
	}
	if err := b.Start(); err != nil {
		t.Fatalf("start b: %v", err)
	}
	t.Cleanup(func() { b.Close() })

	peerAtoB, err = a.Connect(b.Addr())
	if err != nil {
		t.Fatalf("connect a->b: %v", err)
	}

	time.Sleep(100 * time.Millisecond)

	peerBtoA = b.GetPeer(a.PublicKey())
	if peerBtoA == nil {
		t.Fatal("b has no peer for a after connect")
	}

	return a, b, peerAtoB, peerBtoA
}

// TestBlockedPeerInboundDropped verifies that once A blocks B, a message B
// sends to A over the gossip (uni-stream) path is never delivered to A's
// OnMessage handler.
func TestBlockedPeerInboundDropped(t *testing.T) {
	a, _, peerAtoB, peerBtoA := newConnectedPair(t)

	var received atomic.Bool
	a.OnMessage(func(p *Peer, data []byte) { received.Store(true) })

	a.SetBlocklist([]ed25519.PublicKey{peerAtoB.PublicKey()})

	if err := peerBtoA.Send([]byte("hello from b")); err != nil {
		t.Fatalf("send: %v", err)
	}

	time.Sleep(200 * time.Millisecond)

	if received.Load() {
		t.Error("message delivered to A despite blocklist")
	}
}

// TestBlockedPeerOutboundSendSilent verifies A's Send to a blocked B delivers
// nothing yet returns nil (a partitioned peer looks unreachable, not erroring).
func TestBlockedPeerOutboundSendSilent(t *testing.T) {
	_, b, peerAtoB, _ := newConnectedPair(t)

	var received atomic.Bool
	b.OnMessage(func(p *Peer, data []byte) { received.Store(true) })

	peerAtoB.node.SetBlocklist([]ed25519.PublicKey{peerAtoB.PublicKey()})

	if err := peerAtoB.Send([]byte("hello from a")); err != nil {
		t.Errorf("send to blocked peer should return nil, got: %v", err)
	}

	time.Sleep(200 * time.Millisecond)

	if received.Load() {
		t.Error("message delivered to B despite A's blocklist")
	}
}

// TestBlockedPeerOutboundRequestErrors verifies A's Request to a blocked B
// errors immediately instead of opening a stream.
func TestBlockedPeerOutboundRequestErrors(t *testing.T) {
	_, b, peerAtoB, _ := newConnectedPair(t)

	b.OnRequest(func(p *Peer, data []byte) ([]byte, error) {
		return []byte("should never be sent"), nil
	})

	peerAtoB.node.SetBlocklist([]ed25519.PublicKey{peerAtoB.PublicKey()})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if _, err := peerAtoB.Request(ctx, []byte("ping")); err == nil {
		t.Error("expected error requesting a blocked peer")
	}
}

// TestBlockedPeerInboundRequestNoResponse verifies that when A blocks B, a
// request B sends to A gets no response (A drops it silently, so B's Request
// call times out on its context deadline).
func TestBlockedPeerInboundRequestNoResponse(t *testing.T) {
	a, _, peerAtoB, peerBtoA := newConnectedPair(t)

	a.OnRequest(func(p *Peer, data []byte) ([]byte, error) {
		return []byte("should never be sent"), nil
	})

	a.SetBlocklist([]ed25519.PublicKey{peerAtoB.PublicKey()})

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	if _, err := peerBtoA.Request(ctx, []byte("ping")); err == nil {
		t.Error("expected a timeout requesting a node that dropped the request")
	}
}

// TestBlockedPeerHealAndResendDelivers is the dedup-ordering regression test:
// the inbound drop must run BEFORE dedup marks a message seen, so after
// ClearBlocklist a re-sent message with the SAME bytes is delivered rather than
// silently eaten by the dedup cache.
func TestBlockedPeerHealAndResendDelivers(t *testing.T) {
	a, _, peerAtoB, peerBtoA := newConnectedPair(t)

	var receivedCount atomic.Int32
	a.OnMessage(func(p *Peer, data []byte) { receivedCount.Add(1) })

	a.SetBlocklist([]ed25519.PublicKey{peerAtoB.PublicKey()})

	msg := []byte("identical payload across the heal boundary")

	if err := peerBtoA.Send(msg); err != nil {
		t.Fatalf("send while blocked: %v", err)
	}

	time.Sleep(200 * time.Millisecond)

	if receivedCount.Load() != 0 {
		t.Fatalf("message delivered while blocked: count=%d", receivedCount.Load())
	}

	a.ClearBlocklist()

	if err := peerBtoA.Send(msg); err != nil {
		t.Fatalf("resend after heal: %v", err)
	}

	time.Sleep(200 * time.Millisecond)

	if receivedCount.Load() != 1 {
		t.Errorf("resent message not delivered after heal: count=%d, want 1", receivedCount.Load())
	}
}

// TestBlocklistConcurrentSetClear races SetBlocklist, ClearBlocklist and
// isBlocked reads to catch data races under -race.
func TestBlocklistConcurrentSetClear(t *testing.T) {
	n, err := NewNode(Config{PrivateKey: generateTestKey(t), ListenAddr: "127.0.0.1:0"})
	if err != nil {
		t.Fatalf("create node: %v", err)
	}
	defer n.Close()

	other := generateTestKey(t).Public().(ed25519.PublicKey)

	var wg sync.WaitGroup
	wg.Add(3)

	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			n.SetBlocklist([]ed25519.PublicKey{other})
		}
	}()

	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			n.ClearBlocklist()
		}
	}()

	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			n.isBlocked(other)
		}
	}()

	wg.Wait()
}
