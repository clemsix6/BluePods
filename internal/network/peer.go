package network

import (
	"context"
	"crypto/ed25519"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/quic-go/quic-go"

	"BluePods/internal/logger"
)

const (
	// defaultRequestTimeout is the default timeout for Request calls.
	defaultRequestTimeout = 30 * time.Second
)

// Peer represents a connection to a remote node.
type Peer struct {
	publicKey ed25519.PublicKey // publicKey is the remote node's ed25519 public key
	address   string            // address is the remote address (for reconnection)
	conn      *quic.Conn        // conn is the underlying QUIC connection
	node      *Node             // node is the parent node
	closed    atomic.Bool       // closed indicates if the peer is closed
	mu        sync.Mutex        // mu protects send operations
}

// PublicKey returns the remote node's ed25519 public key.
func (p *Peer) PublicKey() ed25519.PublicKey {
	return p.publicKey
}

// Address returns the remote address.
func (p *Peer) Address() string {
	return p.address
}

// Send sends a message to the peer using a new unidirectional stream.
func (p *Peer) Send(data []byte) error {
	if p.closed.Load() {
		return fmt.Errorf("peer is closed")
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	stream, err := p.conn.OpenUniStreamSync(context.Background())
	if err != nil {
		return fmt.Errorf("open stream: %w", err)
	}

	if err := writeMessage(stream, data); err != nil {
		stream.Close()
		return fmt.Errorf("write message: %w", err)
	}

	return stream.Close()
}

// Close closes the peer connection.
func (p *Peer) Close() error {
	if p.closed.Swap(true) {
		return nil // Already closed
	}

	return p.conn.CloseWithError(0, "closed")
}

// Request sends data and waits for response via bidirectional stream.
// Uses the provided context for timeout/cancellation.
func (p *Peer) Request(ctx context.Context, data []byte) ([]byte, error) {
	if p.closed.Load() {
		return nil, fmt.Errorf("peer is closed")
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

// handleBidiStream handles a bidirectional request/response stream.
func (p *Peer) handleBidiStream(stream *quic.Stream) {
	defer stream.Close()

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
func (p *Peer) handleUniStream(stream *quic.ReceiveStream) {
	data, err := readMessage(stream)
	if err != nil {
		logger.Debug("stream read error", "peer", p.address, "error", err)
		return
	}

	logger.Debug("uni data received", "peer", p.address, "bytes", len(data))

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
