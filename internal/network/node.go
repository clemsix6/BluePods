package network

import (
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"encoding/hex"
	"fmt"
	"math/rand"
	"sync"
	"time"

	"github.com/quic-go/quic-go"
)

const (
	// defaultReconnectDelay is the default delay between reconnection attempts.
	defaultReconnectDelay = 5 * time.Second

	// maxReconnectDelay is the maximum delay between reconnection attempts.
	maxReconnectDelay = 60 * time.Second

	// alpnProtocol is the ALPN protocol identifier.
	alpnProtocol = "bluepods/1"
)

// Config holds the configuration for a Node.
type Config struct {
	PrivateKey     ed25519.PrivateKey // PrivateKey is the node's ed25519 private key
	ListenAddr     string             // ListenAddr is the address to listen on (e.g., ":9000")
	ReconnectDelay time.Duration      // ReconnectDelay is the initial delay between reconnection attempts
}

// Node represents a network node that can accept and initiate connections.
type Node struct {
	privateKey ed25519.PrivateKey // privateKey is the node's ed25519 private key
	publicKey  ed25519.PublicKey  // publicKey is the node's ed25519 public key
	listenAddr string             // listenAddr is the address to listen on
	tlsConfig  *tls.Config        // tlsConfig is the TLS configuration
	quicConfig *quic.Config       // quicConfig is the QUIC configuration

	listener *quic.Listener // listener is the QUIC listener

	peers   map[string]*Peer // peers maps public key hex to peer
	peersMu sync.RWMutex     // peersMu protects peers map

	knownAddrs   map[string]string // knownAddrs maps public key hex to address (for reconnection)
	knownAddrsMu sync.RWMutex      // knownAddrsMu protects knownAddrs map

	reconnectDelay time.Duration // reconnectDelay is the initial reconnection delay

	dedup *Dedup // dedup tracks seen messages to prevent duplicate processing

	onConnect    func(*Peer)                          // onConnect is called when a peer connects
	onMessage    func(*Peer, []byte)                 // onMessage is called when a message is received
	onDisconnect func(*Peer)                         // onDisconnect is called when a peer disconnects
	onRequest    func(*Peer, []byte) ([]byte, error) // onRequest handles bidirectional request/response
	handlersMu   sync.RWMutex                        // handlersMu protects event handlers

	ctx    context.Context    // ctx is the node's context
	cancel context.CancelFunc // cancel cancels the node's context
	wg     sync.WaitGroup     // wg waits for goroutines to finish
}

// NewNode creates a new network node.
func NewNode(cfg Config) (*Node, error) {
	if cfg.PrivateKey == nil {
		return nil, fmt.Errorf("private key is required")
	}

	if cfg.ListenAddr == "" {
		return nil, fmt.Errorf("listen address is required")
	}

	reconnectDelay := cfg.ReconnectDelay
	if reconnectDelay == 0 {
		reconnectDelay = defaultReconnectDelay
	}

	cert, err := generateCertificate(cfg.PrivateKey)
	if err != nil {
		return nil, fmt.Errorf("generate certificate: %w", err)
	}

	tlsConfig := &tls.Config{
		Certificates:       []tls.Certificate{cert},
		ClientAuth:         tls.RequireAnyClientCert,
		InsecureSkipVerify: true, // We verify the public key manually
		NextProtos:         []string{alpnProtocol},
	}

	quicConfig := &quic.Config{
		MaxIdleTimeout:  30 * time.Second,
		KeepAlivePeriod: 10 * time.Second,
	}

	ctx, cancel := context.WithCancel(context.Background())

	return &Node{
		privateKey:     cfg.PrivateKey,
		publicKey:      cfg.PrivateKey.Public().(ed25519.PublicKey),
		listenAddr:     cfg.ListenAddr,
		tlsConfig:      tlsConfig,
		quicConfig:     quicConfig,
		peers:          make(map[string]*Peer),
		knownAddrs:     make(map[string]string),
		reconnectDelay: reconnectDelay,
		dedup:          NewDedup(),
		ctx:            ctx,
		cancel:         cancel,
	}, nil
}

// PublicKey returns the node's public key.
func (n *Node) PublicKey() ed25519.PublicKey {
	return n.publicKey
}

// Addr returns the listener's address. Returns empty string if not started.
func (n *Node) Addr() string {
	if n.listener == nil {
		return ""
	}

	return n.listener.Addr().String()
}

// Start starts the node and begins accepting connections.
func (n *Node) Start() error {
	listener, err := quic.ListenAddr(n.listenAddr, n.tlsConfig, n.quicConfig)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}

	n.listener = listener

	n.wg.Add(1)
	go n.acceptLoop()

	return nil
}

// Connect connects to a remote node at the given address.
func (n *Node) Connect(addr string) (*Peer, error) {
	conn, err := quic.DialAddr(n.ctx, addr, n.tlsConfig, n.quicConfig)
	if err != nil {
		return nil, fmt.Errorf("dial: %w", err)
	}

	peer, err := n.setupPeer(conn, addr)
	if err != nil {
		conn.CloseWithError(1, "setup failed")
		return nil, err
	}

	return peer, nil
}

// Broadcast sends a message to all connected peers.
func (n *Node) Broadcast(data []byte) error {
	n.peersMu.RLock()
	peers := make([]*Peer, 0, len(n.peers))
	for _, p := range n.peers {
		peers = append(peers, p)
	}
	n.peersMu.RUnlock()

	var lastErr error

	for _, p := range peers {
		if err := p.Send(data); err != nil {
			lastErr = err
		}
	}

	return lastErr
}

// Gossip sends data to a random subset of connected peers.
// If fanout >= peer count, sends to all peers (same as Broadcast).
func (n *Node) Gossip(data []byte, fanout int) error {
	n.peersMu.RLock()
	peers := make([]*Peer, 0, len(n.peers))
	for _, p := range n.peers {
		peers = append(peers, p)
	}
	n.peersMu.RUnlock()

	selected := selectRandomPeers(peers, fanout)

	var lastErr error

	for _, p := range selected {
		if err := p.Send(data); err != nil {
			lastErr = err
		}
	}

	return lastErr
}

// selectRandomPeers returns up to n random peers from the slice.
// If n >= len(peers), returns all peers.
func selectRandomPeers(peers []*Peer, n int) []*Peer {
	if n >= len(peers) {
		return peers
	}

	indices := rand.Perm(len(peers))[:n]
	selected := make([]*Peer, n)

	for i, idx := range indices {
		selected[i] = peers[idx]
	}

	return selected
}

// Peers returns a list of all connected peers.
func (n *Node) Peers() []*Peer {
	n.peersMu.RLock()
	defer n.peersMu.RUnlock()

	peers := make([]*Peer, 0, len(n.peers))
	for _, p := range n.peers {
		peers = append(peers, p)
	}

	return peers
}

// GetPeer returns the peer for the given public key, or nil if not connected.
func (n *Node) GetPeer(pubkey ed25519.PublicKey) *Peer {
	keyHex := hex.EncodeToString(pubkey)

	n.peersMu.RLock()
	defer n.peersMu.RUnlock()

	return n.peers[keyHex]
}

// OnConnect sets the handler called when a peer connects.
func (n *Node) OnConnect(fn func(*Peer)) {
	n.handlersMu.Lock()
	n.onConnect = fn
	n.handlersMu.Unlock()
}

// OnMessage sets the handler called when a message is received.
func (n *Node) OnMessage(fn func(*Peer, []byte)) {
	n.handlersMu.Lock()
	n.onMessage = fn
	n.handlersMu.Unlock()
}

// OnDisconnect sets the handler called when a peer disconnects.
func (n *Node) OnDisconnect(fn func(*Peer)) {
	n.handlersMu.Lock()
	n.onDisconnect = fn
	n.handlersMu.Unlock()
}

// OnRequest sets the handler for incoming bidirectional requests.
// The handler receives request data and returns response data.
func (n *Node) OnRequest(fn func(*Peer, []byte) ([]byte, error)) {
	n.handlersMu.Lock()
	n.onRequest = fn
	n.handlersMu.Unlock()
}

// Close stops the node and closes all connections.
func (n *Node) Close() error {
	n.cancel()

	if n.listener != nil {
		n.listener.Close()
	}

	n.peersMu.Lock()
	for _, p := range n.peers {
		p.Close()
	}
	n.peers = make(map[string]*Peer)
	n.peersMu.Unlock()

	n.dedup.Close()
	n.wg.Wait()

	return nil
}

// acceptLoop accepts incoming connections.
func (n *Node) acceptLoop() {
	defer n.wg.Done()

	for {
		conn, err := n.listener.Accept(n.ctx)
		if err != nil {
			return // Listener closed
		}

		go n.handleIncoming(conn)
	}
}

// handleIncoming handles an incoming connection.
func (n *Node) handleIncoming(conn *quic.Conn) {
	peer, err := n.setupPeer(conn, conn.RemoteAddr().String())
	if err != nil {
		conn.CloseWithError(1, "setup failed")
		return
	}

	n.callOnConnect(peer)
}

// setupPeer creates a Peer from a QUIC connection.
func (n *Node) setupPeer(conn *quic.Conn, addr string) (*Peer, error) {
	tlsState := conn.ConnectionState().TLS

	pubKey, err := extractPublicKey(tlsState)
	if err != nil {
		return nil, fmt.Errorf("extract public key: %w", err)
	}

	keyHex := hex.EncodeToString(pubKey)

	peer := &Peer{
		publicKey: pubKey,
		address:   addr,
		conn:      conn,
		node:      n,
	}

	n.peersMu.Lock()
	n.peers[keyHex] = peer
	n.peersMu.Unlock()

	n.knownAddrsMu.Lock()
	n.knownAddrs[keyHex] = addr
	n.knownAddrsMu.Unlock()

	n.wg.Add(1)
	go func() {
		defer n.wg.Done()
		peer.receiveLoop()
	}()

	return peer, nil
}

// handlePeerDisconnect handles a peer disconnection.
func (n *Node) handlePeerDisconnect(p *Peer) {
	keyHex := hex.EncodeToString(p.publicKey)

	n.peersMu.Lock()
	delete(n.peers, keyHex)
	n.peersMu.Unlock()

	n.callOnDisconnect(p)

	// Schedule reconnection
	n.wg.Add(1)
	go func() {
		defer n.wg.Done()
		n.reconnectPeer(keyHex)
	}()
}

// reconnectPeer attempts to reconnect to a peer with exponential backoff.
func (n *Node) reconnectPeer(keyHex string) {
	delay := n.reconnectDelay

	for {
		select {
		case <-n.ctx.Done():
			return
		case <-time.After(delay):
		}

		n.knownAddrsMu.RLock()
		addr, ok := n.knownAddrs[keyHex]
		n.knownAddrsMu.RUnlock()

		if !ok {
			return // Peer removed from known addresses
		}

		// Check if already reconnected
		n.peersMu.RLock()
		_, exists := n.peers[keyHex]
		n.peersMu.RUnlock()

		if exists {
			return // Already reconnected
		}

		peer, err := n.Connect(addr)
		if err == nil {
			n.callOnConnect(peer)
			return
		}

		// Exponential backoff
		delay = delay * 2
		if delay > maxReconnectDelay {
			delay = maxReconnectDelay
		}
	}
}

// callOnConnect calls the onConnect handler if set.
func (n *Node) callOnConnect(p *Peer) {
	n.handlersMu.RLock()
	fn := n.onConnect
	n.handlersMu.RUnlock()

	if fn != nil {
		fn(p)
	}
}

// callOnMessage calls the onMessage handler if set.
func (n *Node) callOnMessage(p *Peer, data []byte) {
	n.handlersMu.RLock()
	fn := n.onMessage
	n.handlersMu.RUnlock()

	if fn != nil {
		fn(p, data)
	}
}

// callOnDisconnect calls the onDisconnect handler if set.
func (n *Node) callOnDisconnect(p *Peer) {
	n.handlersMu.RLock()
	fn := n.onDisconnect
	n.handlersMu.RUnlock()

	if fn != nil {
		fn(p)
	}
}

// callOnRequest calls the onRequest handler if set.
func (n *Node) callOnRequest(p *Peer, data []byte) ([]byte, error) {
	n.handlersMu.RLock()
	fn := n.onRequest
	n.handlersMu.RUnlock()

	if fn == nil {
		return nil, fmt.Errorf("no request handler registered")
	}

	return fn(p, data)
}
