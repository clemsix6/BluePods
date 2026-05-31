package network

import (
	"context"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/quic-go/quic-go"

	"BluePods/internal/logger"
)

const (
	// maxClientConnsPerIP caps concurrent ephemeral connections from one IP.
	maxClientConnsPerIP = 16

	// maxClientStreamsPerIP caps concurrent in-flight request streams per IP.
	maxClientStreamsPerIP = 64

	// maxClientPendingBytesPerIP caps the unprocessed request bytes per IP.
	maxClientPendingBytesPerIP = 8 << 20 // 8 MB

	// clientRateBurst is the token-bucket burst size per IP.
	clientRateBurst = 64

	// clientRatePerSecond is the steady-state request rate per IP.
	clientRatePerSecond = 32

	// clientIdleTimeout closes an ephemeral connection after this much silence.
	clientIdleTimeout = 20 * time.Second
)

// ResourceLimitError indicates that an ingress resource cap was exceeded.
// It is returned before any handler work so abusive clients are cheap to reject.
type ResourceLimitError struct {
	Resource string // Resource names the cap that was hit
}

// Error implements the error interface.
func (e *ResourceLimitError) Error() string {
	return fmt.Sprintf("resource limit exceeded: %s", e.Resource)
}

// ipScope tracks the live resource usage and rate state for one source IP.
type ipScope struct {
	conns        int       // conns is the number of live connections from this IP
	streams      int       // streams is the number of in-flight request streams
	pendingBytes int       // pendingBytes is unprocessed request bytes in flight
	tokens       float64   // tokens is the current token-bucket allowance
	lastRefill   time.Time // lastRefill is when tokens were last replenished
}

// clientGate enforces per-IP ingress limits for ephemeral client connections.
type clientGate struct {
	mu     sync.Mutex          // mu protects the scopes map
	scopes map[string]*ipScope // scopes maps source IP to its live usage
}

// newClientGate creates a per-IP ingress gate.
func newClientGate() *clientGate {
	return &clientGate{scopes: make(map[string]*ipScope)}
}

// scopeFor returns (creating if needed) the scope for an IP. Caller holds mu.
func (g *clientGate) scopeFor(ip string) *ipScope {
	s := g.scopes[ip]
	if s == nil {
		s = &ipScope{tokens: clientRateBurst, lastRefill: time.Now()}
		g.scopes[ip] = s
	}

	return s
}

// admitConn reserves a connection slot for an IP.
// It returns a ResourceLimitError when the per-IP connection cap is reached.
func (g *clientGate) admitConn(ip string) error {
	g.mu.Lock()
	defer g.mu.Unlock()

	s := g.scopeFor(ip)
	if s.conns >= maxClientConnsPerIP {
		return &ResourceLimitError{Resource: "connections-per-ip"}
	}

	s.conns++

	return nil
}

// releaseConn frees a connection slot and drops the scope when fully idle.
func (g *clientGate) releaseConn(ip string) {
	g.mu.Lock()
	defer g.mu.Unlock()

	s := g.scopes[ip]
	if s == nil {
		return
	}

	s.conns--
	if s.conns <= 0 && s.streams <= 0 && s.pendingBytes <= 0 {
		delete(g.scopes, ip)
	}
}

// admitStream reserves a stream slot plus its pending-byte budget for an IP and
// consumes one rate-limiter token. It returns a typed ResourceLimitError when
// any cap or the rate limit is exceeded, before any handler work runs.
func (g *clientGate) admitStream(ip string, size int) error {
	g.mu.Lock()
	defer g.mu.Unlock()

	s := g.scopeFor(ip)

	if s.streams >= maxClientStreamsPerIP {
		return &ResourceLimitError{Resource: "streams-per-ip"}
	}

	if s.pendingBytes+size > maxClientPendingBytesPerIP {
		return &ResourceLimitError{Resource: "pending-bytes-per-ip"}
	}

	if !g.takeToken(s) {
		return &ResourceLimitError{Resource: "rate-limit-per-ip"}
	}

	s.streams++
	s.pendingBytes += size

	return nil
}

// releaseStream frees a stream slot and its pending-byte budget for an IP.
func (g *clientGate) releaseStream(ip string, size int) {
	g.mu.Lock()
	defer g.mu.Unlock()

	s := g.scopes[ip]
	if s == nil {
		return
	}

	s.streams--
	s.pendingBytes -= size

	if s.pendingBytes < 0 {
		s.pendingBytes = 0
	}
}

// takeToken refills the bucket by elapsed time and consumes one token.
// It returns false when the bucket is empty. Caller holds mu.
func (g *clientGate) takeToken(s *ipScope) bool {
	now := time.Now()
	elapsed := now.Sub(s.lastRefill).Seconds()
	s.lastRefill = now

	s.tokens += elapsed * clientRatePerSecond
	if s.tokens > clientRateBurst {
		s.tokens = clientRateBurst
	}

	if s.tokens < 1 {
		return false
	}

	s.tokens--

	return true
}

// handleClientConn serves an ephemeral, untrusted client connection.
// The connection is never added to the peer mesh, never reconnected, and is
// closed when idle. Each request stream passes the per-IP gates before any
// handler work. The Retry round-trip (VerifySourceAddress on the transport)
// has already validated the source address by the time this runs.
func (n *Node) handleClientConn(conn *quic.Conn) {
	ip := clientIP(conn.RemoteAddr())

	if err := n.clientGate.admitConn(ip); err != nil {
		conn.CloseWithError(2, err.Error())
		return
	}
	defer n.clientGate.releaseConn(ip)

	logger.Debug("client connection accepted", "ip", ip)

	for {
		ctx, cancel := context.WithTimeout(n.ctx, clientIdleTimeout)
		stream, err := conn.AcceptStream(ctx)
		cancel()

		if err != nil {
			conn.CloseWithError(0, "idle")
			return
		}

		go n.serveClientStream(ip, stream)
	}
}

// serveClientStream reads one request, enforces the per-IP gates, dispatches it
// through the request handler, and writes the response.
func (n *Node) serveClientStream(ip string, stream *quic.Stream) {
	defer stream.Close()

	stream.SetReadDeadline(time.Now().Add(clientIdleTimeout))

	data, err := readMessage(stream)
	if err != nil {
		return
	}

	if err := n.clientGate.admitStream(ip, len(data)); err != nil {
		logger.Debug("client request rejected", "ip", ip, "reason", err)
		return
	}
	defer n.clientGate.releaseStream(ip, len(data))

	response, err := n.callOnRequest(nil, data)
	if err != nil {
		return
	}

	stream.SetWriteDeadline(time.Now().Add(clientIdleTimeout))
	writeMessage(stream, response)
}

// clientIP extracts the IP portion of a network address for per-IP accounting.
func clientIP(addr net.Addr) string {
	host, _, err := net.SplitHostPort(addr.String())
	if err != nil {
		return addr.String()
	}

	return host
}
