package network

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"os"
	"sync/atomic"
	"time"

	"github.com/quic-go/quic-go"

	"BluePods/internal/logger"
)

const (
	// defaultRequestTimeout is the default timeout for Request calls.
	defaultRequestTimeout = 30 * time.Second

	// sendStreamTimeout bounds one Send: opening the unidirectional stream plus
	// writing the message onto it. A peer that keeps up answers in microseconds
	// on a local mesh, so the bound is only ever reached by a peer that has
	// stopped returning stream credit or reading, and Send's contract for such a
	// peer is to drop the message rather than wait.
	sendStreamTimeout = 2 * time.Second

	// uniStreamReadTimeout bounds how long handleUniStream waits for a gossip
	// message to finish arriving. Gossip messages are small, so a peer that
	// keeps up finishes in microseconds; the bound is only ever reached by a
	// peer that opened a stream and then stopped writing, and it exists so that
	// goroutine and stream slot are freed rather than parked on it forever.
	uniStreamReadTimeout = 30 * time.Second

	// streamAbortCode is the QUIC application error code used to abort a
	// stream that has stalled past its deadline, on either side: CancelWrite
	// (RESET_STREAM) on a send that timed out, CancelRead (STOP_SENDING) on a
	// receive that timed out. Neither is gated on the stream's own
	// flow-control credit the way a clean Close (FIN) is, so both complete the
	// stream immediately even when the peer is not reading or not writing;
	// Close on a write that failed for that reason would instead queue the FIN
	// behind the very data that is blocked, so the stream never finishes and
	// its slot is never reclaimed.
	streamAbortCode quic.StreamErrorCode = 1
)

// Peer represents a connection to a remote node.
type Peer struct {
	publicKey ed25519.PublicKey // publicKey is the remote node's ed25519 public key
	address   string            // address is the remote address (for reconnection)
	conn      *quic.Conn        // conn is the underlying QUIC connection
	node      *Node             // node is the parent node
	closed    atomic.Bool       // closed indicates if the peer is closed
	sendSlot  chan struct{}     // sendSlot serialises Send, one holder at a time, acquired under the send deadline
}

// PublicKey returns the remote node's ed25519 public key.
func (p *Peer) PublicKey() ed25519.PublicKey {
	return p.publicKey
}

// Address returns the remote address.
func (p *Peer) Address() string {
	return p.address
}

// Send sends a message to the peer using a new unidirectional stream. A
// blocked peer is silently dropped (returns nil): from the application's
// perspective a partitioned peer looks unreachable, not erroring.
//
// The WHOLE send is bounded by sendStreamTimeout — waiting for the peer's send
// slot included, not only the stream operations once it is held. The wait this
// replaces was unbounded, and it was taken while holding the slot: a peer that
// stops returning unidirectional-stream credit, or stops reading, parked the
// caller and every other sender to that peer for good. The callers are the
// consensus liveness loop, which gossips its own vertex while holding roundMu,
// and the relay path, which gossips from inside the receive handler that would
// have freed the credit — so one starved peer stopped the node producing,
// committing and answering client submissions, and a cycle of such peers stopped
// the mesh.
//
// The slot must be under the same deadline as the stream work, or the bound
// covers one send and not the wait for it: a stalled peer collects a queue (a
// broadcast, a liveness gossip every 500ms, a relay per received vertex), and the
// n-th waiter would pay n times the timeout before its own attempt even starts.
// Under one deadline a caller behind a busy slot gives up instead of queueing.
//
// Dropping is the safe side: gossip is re-announced (the frontier leaf is
// rebroadcast on every liveness tick) and a peer that cannot take a message is
// exactly the unreachable peer this contract already describes.
func (p *Peer) Send(data []byte) error {
	if p.closed.Load() {
		return fmt.Errorf("peer is closed")
	}

	if p.node.isBlocked(p.publicKey) {
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), sendStreamTimeout)
	defer cancel()

	if !p.acquireSend(ctx) {
		return fmt.Errorf("send slot busy after %v", sendStreamTimeout)
	}
	defer p.releaseSend()

	stream, err := p.conn.OpenUniStreamSync(ctx)
	if err != nil {
		return fmt.Errorf("open stream: %w", err)
	}

	// The write shares the same deadline: an open that used most of the budget
	// must not then hand an unbounded write to a peer that is not reading.
	if deadline, ok := ctx.Deadline(); ok {
		stream.SetWriteDeadline(deadline)
	}

	if err := writeMessage(stream, data); err != nil {
		// CancelWrite (RESET_STREAM), not Close (FIN): a write that failed on
		// the deadline failed because the receiver is not reading and has
		// stopped granting flow-control window, and Close would queue the FIN
		// behind that same blocked data — the stream never completes and its
		// slot in maxIncomingUniStreams is never reclaimed on either side.
		// CancelWrite is not gated on stream data credit, so it completes the
		// stream immediately regardless.
		stream.CancelWrite(streamAbortCode)
		return fmt.Errorf("write message: %w", err)
	}

	// Close (FIN) here is verified non-blocking: writeMessage already
	// succeeded, so the receiver has already granted (and is consuming) the
	// window this stream needed.
	return stream.Close()
}

// acquireSend takes this peer's single send slot, giving up when ctx ends. It
// reports whether the slot was taken; the caller releases it with releaseSend.
func (p *Peer) acquireSend(ctx context.Context) bool {
	select {
	case p.sendSlot <- struct{}{}:
		return true
	case <-ctx.Done():
		return false
	}
}

// releaseSend frees this peer's send slot.
func (p *Peer) releaseSend() {
	<-p.sendSlot
}

// Close closes the peer connection.
func (p *Peer) Close() error {
	if p.closed.Swap(true) {
		return nil // Already closed
	}

	return p.conn.CloseWithError(0, "closed")
}

// Request sends data and waits for response via bidirectional stream.
// Uses the provided context for timeout/cancellation. A blocked peer errors
// immediately rather than opening a stream that will never be served.
func (p *Peer) Request(ctx context.Context, data []byte) ([]byte, error) {
	if p.closed.Load() {
		return nil, fmt.Errorf("peer is closed")
	}

	if p.node.isBlocked(p.publicKey) {
		return nil, fmt.Errorf("peer blocked")
	}

	stream, err := p.conn.OpenStreamSync(ctx)
	if err != nil {
		return nil, fmt.Errorf("open stream:\n%w", err)
	}
	defer stream.Close()

	// Set deadline from context
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(defaultRequestTimeout)
	}
	stream.SetDeadline(deadline)

	// Write request
	if err := writeMessage(stream, data); err != nil {
		return nil, fmt.Errorf("write request:\n%w", err)
	}

	// Read response (server knows request is complete via length-prefixed protocol)
	response, err := readMessage(stream)
	if err != nil {
		return nil, fmt.Errorf("read response:\n%w", err)
	}

	return response, nil
}

// receiveLoop accepts incoming streams and processes messages.
func (p *Peer) receiveLoop() {
	// Accept both unidirectional and bidirectional streams concurrently
	go p.acceptBidiStreams(context.Background())

	uniCount := 0
	for {
		// Use timeout to detect stuck connections
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		stream, err := p.conn.AcceptUniStream(ctx)
		cancel()

		if err != nil {
			if ctx.Err() == context.DeadlineExceeded {
				logger.Debug("no uni streams received", "peer", p.address, "total", uniCount)
				continue // Try again
			}
			logger.Debug("receiveLoop ended", "peer", p.address, "error", err, "uniStreams", uniCount)
			break
		}

		uniCount++
		go p.handleUniStream(stream)
	}

	p.handleDisconnect()
}

// acceptBidiStreams accepts bidirectional streams for request/response.
func (p *Peer) acceptBidiStreams(ctx context.Context) {
	for {
		stream, err := p.conn.AcceptStream(ctx)
		if err != nil {
			return
		}

		go p.handleBidiStream(stream)
	}
}

// handleBidiStream handles a bidirectional request/response stream. A blocked
// peer is dropped without reading or responding: the requester simply sees no
// response and times out, matching how an unreachable peer behaves.
func (p *Peer) handleBidiStream(stream *quic.Stream) {
	defer stream.Close()

	if p.node.isBlocked(p.publicKey) {
		return
	}

	// Read request
	data, err := readMessage(stream)
	if err != nil {
		return
	}

	// Call handler
	response, err := p.node.callOnRequest(p, data)
	if err != nil {
		return
	}

	// Write response
	writeMessage(stream, response)
}

// handleUniStream reads a message from a unidirectional stream.
//
// The read is bounded by uniStreamReadTimeout: without a deadline, a peer that
// opens a stream and then never finishes writing parks this goroutine and its
// maxIncomingUniStreams slot forever, and the mesh tier grants that budget per
// connection, so a single half-writing peer can hold up to 4096 of them open.
func (p *Peer) handleUniStream(stream *quic.ReceiveStream) {
	stream.SetReadDeadline(time.Now().Add(uniStreamReadTimeout))

	data, err := readMessage(stream)
	if err != nil {
		if errors.Is(err, os.ErrDeadlineExceeded) {
			// CancelRead reclaims the stream and its slot immediately; without
			// it the half-written stream stays open (and counted) past the
			// deadline this was meant to bound.
			stream.CancelRead(streamAbortCode)
		}
		logger.Debug("stream read error", "peer", p.address, "error", err)
		return
	}

	logger.Debug("uni data received", "peer", p.address, "bytes", len(data))

	// Blocked-peer drop MUST run before the dedup check: marking a blocked
	// message as seen would poison the dedup cache, so identical traffic sent
	// again after the block is lifted would be silently dropped as a
	// duplicate instead of delivered.
	if p.node.isBlocked(p.publicKey) {
		return
	}

	// Check for duplicate message
	if !p.node.dedup.Check(data) {
		logger.Debug("dedup filtered", "peer", p.address, "bytes", len(data))
		return
	}

	p.node.callOnMessage(p, data)
}

// handleDisconnect handles peer disconnection.
func (p *Peer) handleDisconnect() {
	if p.closed.Swap(true) {
		return // Already closed
	}

	p.node.handlePeerDisconnect(p)
}
