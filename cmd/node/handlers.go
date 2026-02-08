package main

import (
	"bytes"
	"crypto/ed25519"
	"encoding/hex"
	"fmt"
	"time"

	"BluePods/internal/aggregation"
	"BluePods/internal/consensus"
	"BluePods/internal/logger"
	"BluePods/internal/network"
	"BluePods/internal/sync"
	"BluePods/internal/types"
)

// setupMessageHandlers configures network message handlers.
func (n *Node) setupMessageHandlers() {
	n.network.OnMessage(func(peer *network.Peer, data []byte) {
		// Log received vertex for debugging
		v := types.GetRootAsVertex(data, 0)
		producer := hex.EncodeToString(v.ProducerBytes()[:8])
		logger.Debug("received vertex",
			"round", v.Round(),
			"producer", producer,
			"from", peer.Address(),
		)

		// Handle incoming vertices from peers
		// If the vertex is new (not duplicate), relay it to other peers
		if n.dag.AddVertex(data) {
			n.relayVertex(data)
		}
	})
}

// relayVertex gossips a received vertex to other peers.
// This ensures vertices propagate through the mesh network even if
// nodes don't have direct connections to all validators.
func (n *Node) relayVertex(data []byte) {
	if n.network != nil {
		// Use smaller fanout for relay to avoid amplification
		_ = n.network.Gossip(data, 10)
	}
}

// GossipTx gossips a transaction to network peers.
// This allows validators to forward transactions to producers.
func (n *Node) GossipTx(tx []byte) {
	if n.network != nil {
		_ = n.network.Broadcast(tx)
	}
}

// setupRequestHandlers configures bidirectional request handlers.
func (n *Node) setupRequestHandlers() {
	n.network.OnRequest(func(peer *network.Peer, data []byte) ([]byte, error) {
		// Handle attestation requests
		if aggregation.IsAttestationRequest(data) {
			if n.attHandler == nil {
				return nil, fmt.Errorf("no attestation handler")
			}
			return n.attHandler.HandleRequest(peer, data)
		}

		// Handle snapshot requests
		if sync.IsSnapshotRequest(data) {
			if n.snapManager == nil {
				return nil, fmt.Errorf("no snapshot manager")
			}
			return sync.HandleSnapshotRequest(data, n.snapManager)
		}

		return nil, fmt.Errorf("unknown request type")
	})
}

// setupValidatorCallback configures the callback for new validator registration.
// When a new validator joins, we connect to their QUIC address.
func (n *Node) setupValidatorCallback() {
	myPubkey := n.cfg.PrivateKey.Public().(ed25519.PublicKey)

	n.dag.OnValidatorAdded(func(info *consensus.ValidatorInfo) {
		// Don't connect to self
		if bytes.Equal(info.Pubkey[:], myPubkey) {
			return
		}

		logger.Info("new validator registered",
			"pubkey", hex.EncodeToString(info.Pubkey[:8]),
			"quic", info.QUICAddr,
		)

		// Connect to the new validator if we have their address
		if info.QUICAddr != "" {
			go n.connectToValidator(info)
		}
	})
}

// connectToValidator establishes a connection to a new validator with retry logic.
// Retries are needed because the target validator's listener might not be up yet.
func (n *Node) connectToValidator(info *consensus.ValidatorInfo) {
	maxRetries := 5
	retryDelay := 2 * time.Second

	for attempt := 0; attempt < maxRetries; attempt++ {
		// Check if already connected
		if n.network.GetPeer(info.Pubkey[:]) != nil {
			return
		}

		peer, err := n.network.Connect(info.QUICAddr)
		if err == nil {
			logger.Info("connected to validator",
				"pubkey", hex.EncodeToString(info.Pubkey[:8]),
				"addr", peer.Address(),
			)
			return
		}

		if attempt < maxRetries-1 {
			logger.Debug("retrying validator connection",
				"pubkey", hex.EncodeToString(info.Pubkey[:8]),
				"attempt", attempt+1,
				"error", err,
			)
			time.Sleep(retryDelay)
		} else {
			logger.Warn("failed to connect to validator after retries",
				"pubkey", hex.EncodeToString(info.Pubkey[:8]),
				"addr", info.QUICAddr,
				"attempts", maxRetries,
			)
		}
	}
}
