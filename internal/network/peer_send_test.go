package network

import (
	"context"
	"sync"
	"testing"
	"time"
)

// maxSaturatingStreams caps the streams the stall tests open while looking for
// the credit wall. It is derived from the budget rather than fixed, so raising
// maxIncomingUniStreams cannot silently turn these tests into no-ops.
const maxSaturatingStreams = maxIncomingUniStreams + 64

// TestPeerSendDoesNotBlockOnStalledReceiver asserts Peer.Send returns within a
// bounded time once a peer has stopped returning unidirectional-stream credit.
//
// QUIC grants a sender a finite unidirectional-stream credit and returns it only
// as the receiver consumes each stream. A receiver whose readers are stuck
// therefore exhausts that credit, and an unbounded OpenUniStreamSync then waits
// for credit that never comes.
//
// That wait is not a slow send, it is a permanent one, and Send takes it while
// holding the per-peer mutex, so every other sender to that peer queues behind
// it. In the node it wedges the mesh: the consensus liveness loop gossips its own
// vertex while holding roundMu, and the relay path gossips from inside the very
// receive handler that would have freed the credit, so a cycle of nodes each
// waiting on the next one's credit stops producing, committing and answering
// submissions for good.
//
// Send's contract is that a peer it cannot reach looks unreachable rather than
// erroring, so the bound is the fix: the message is dropped and the caller keeps
// running.
func TestPeerSendDoesNotBlockOnStalledReceiver(t *testing.T) {
	receiver, err := NewNode(Config{PrivateKey: generateTestKey(t), ListenAddr: "127.0.0.1:0"})
	if err != nil {
		t.Fatalf("create receiver: %v", err)
	}

	if err := receiver.Start(); err != nil {
		t.Fatalf("start receiver: %v", err)
	}
	defer receiver.Close()

	sender, err := NewNode(Config{PrivateKey: generateTestKey(t), ListenAddr: "127.0.0.1:0"})
	if err != nil {
		t.Fatalf("create sender: %v", err)
	}

	if err := sender.Start(); err != nil {
		t.Fatalf("start sender: %v", err)
	}
	defer sender.Close()

	peer, err := sender.Connect(receiver.Addr())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}

	saturateStreamCredit(t, peer)

	// Concurrent sends, off the test goroutine: an unbounded one never comes
	// back, and the test must report that rather than hang to the package timeout.
	//
	// They run together because one caller is not the real case. Broadcast fans
	// one message out to every peer, the liveness loop gossips every 500ms and the
	// relay path gossips per received vertex, so a stalled peer collects a queue.
	// If each waiter first waits out the whole queue ahead of it and only then
	// starts its own bounded attempt, the bound is per-send but the wait is
	// per-queue, and the client submission at the back of it still times out.
	const senders = 6

	done := make(chan struct{})
	go func() {
		defer close(done)

		var wg sync.WaitGroup
		for i := 0; i < senders; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_ = peer.Send([]byte("vertex"))
			}()
		}
		wg.Wait()
	}()

	bound := sendStreamTimeout + 5*time.Second

	select {
	case <-done:
	case <-time.After(bound):
		t.Fatalf("%d concurrent sends to a peer that stopped returning stream credit did not all return within %v",
			senders, bound)
	}
}

// TestBroadcastCostsOneTimeoutNotOnePerStalledPeer asserts a fan-out to several
// stalled peers costs about one send timeout, not one per peer.
//
// Each peer's Send is bounded on its own, but a serial fan-out adds those bounds
// up, so the caller pays sendStreamTimeout times the number of peers that are not
// keeping up. The mesh calls this from the client's own request goroutine
// (handleSubmitTx gossips the transaction inline), where the sum overran the
// client's request deadline while each individual send was well inside its bound.
// The peers are independent, so the fan-out is bounded by the slowest of them.
func TestBroadcastCostsOneTimeoutNotOnePerStalledPeer(t *testing.T) {
	const stalledPeers = 3

	sender, err := NewNode(Config{PrivateKey: generateTestKey(t), ListenAddr: "127.0.0.1:0"})
	if err != nil {
		t.Fatalf("create sender: %v", err)
	}

	if err := sender.Start(); err != nil {
		t.Fatalf("start sender: %v", err)
	}
	defer sender.Close()

	for i := 0; i < stalledPeers; i++ {
		receiver, err := NewNode(Config{PrivateKey: generateTestKey(t), ListenAddr: "127.0.0.1:0"})
		if err != nil {
			t.Fatalf("create receiver %d: %v", i, err)
		}

		if err := receiver.Start(); err != nil {
			t.Fatalf("start receiver %d: %v", i, err)
		}
		defer receiver.Close()

		peer, err := sender.Connect(receiver.Addr())
		if err != nil {
			t.Fatalf("connect receiver %d: %v", i, err)
		}

		saturateStreamCredit(t, peer)
	}

	done := make(chan time.Duration, 1)
	go func() {
		start := time.Now()
		_ = sender.Broadcast([]byte("vertex"))
		done <- time.Since(start)
	}()

	// One timeout plus slack: two would already mean the bounds are being added up.
	bound := sendStreamTimeout + sendStreamTimeout/2

	select {
	case d := <-done:
		if d > bound {
			t.Fatalf("broadcast over %d stalled peers took %v, want at most %v (one send timeout, not one per peer)",
				stalledPeers, d, bound)
		}

	case <-time.After(sendStreamTimeout*stalledPeers + 5*time.Second):
		t.Fatalf("broadcast over %d stalled peers never returned", stalledPeers)
	}
}

// saturateStreamCredit opens unidirectional streams to peer until the peer's
// credit runs out, leaving every one of them unconsumed: each carries a length
// prefix promising more bytes than it delivers, so the receiver's reader blocks
// inside the frame and never releases the stream. That is the node's own stalled
// state, where each receive handler is itself waiting on the mesh.
func saturateStreamCredit(t *testing.T, peer *Peer) {
	t.Helper()

	// A prefix promising 255 bytes, followed by one.
	stuck := []byte{0, 0, 0, 255, 'x'}

	for i := 0; i < maxSaturatingStreams; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		stream, err := peer.conn.OpenUniStreamSync(ctx)
		cancel()

		if err != nil {
			return // credit exhausted: the wall the test needs
		}

		if _, err := stream.Write(stuck); err != nil {
			t.Fatalf("write saturating stream %d: %v", i, err)
		}
	}

	t.Fatalf("%d streams never exhausted the peer's credit; the test no longer exercises the block", maxSaturatingStreams)
}
