package network

import (
	"crypto/ed25519"
	"encoding/hex"
	"sync"
)

// blocklist is the set of remote public keys whose mesh traffic this node
// drops in both directions: inbound (uni and bidi streams) and outbound (Send
// and Request). It is empty by default, so a node with no test-hooks
// intervention never drops anything. The inbound check runs before the dedup
// cache marks a message seen, so gossip re-delivered after healing is not lost
// to deduplication (SetBlocklist/ClearBlocklist are test-only operations
// installed over the --test-hooks QUIC surface).
type blocklist struct {
	blocked map[string]bool // blocked maps a hex-encoded public key to membership
	mu      sync.RWMutex    // mu guards blocked
}

// newBlocklist creates an empty blocklist.
func newBlocklist() *blocklist {
	return &blocklist{blocked: make(map[string]bool)}
}

// SetBlocklist replaces the node's blocklist with the given public keys,
// dropping mesh traffic to and from every one of them until cleared.
func (n *Node) SetBlocklist(pubkeys []ed25519.PublicKey) {
	blocked := make(map[string]bool, len(pubkeys))
	for _, pk := range pubkeys {
		blocked[hex.EncodeToString(pk)] = true
	}

	n.blocklist.mu.Lock()
	n.blocklist.blocked = blocked
	n.blocklist.mu.Unlock()
}

// ClearBlocklist empties the node's blocklist, restoring normal traffic to and
// from every peer.
func (n *Node) ClearBlocklist() {
	n.blocklist.mu.Lock()
	n.blocklist.blocked = make(map[string]bool)
	n.blocklist.mu.Unlock()
}

// isBlocked reports whether pk is currently on the node's blocklist.
func (n *Node) isBlocked(pk ed25519.PublicKey) bool {
	keyHex := hex.EncodeToString(pk)

	n.blocklist.mu.RLock()
	defer n.blocklist.mu.RUnlock()

	return n.blocklist.blocked[keyHex]
}
