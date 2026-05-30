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
		n.handleGossipMessage(peer, data)
	})
}

// handleGossipMessage dispatches a message received on the one-way gossip stream.
// The stream carries two kinds of payload: a gossiped transaction (tagged with
// MsgGossipTx) and a vertex (an untagged FlatBuffer). A tagged transaction is
// added to the local pending set so a producer includes it in its next vertex,
// then re-gossiped; anything else is treated as a vertex. Without this split a
// forwarded transaction is parsed as a vertex, fails validation, and is dropped,
// which makes GossipTx a no-op and silently strands transactions submitted to a
// non-producing peer.
func (n *Node) handleGossipMessage(peer *network.Peer, data []byte) {
	if body, ok := network.DecodeGossipTx(data); ok {
		n.ingestGossipedTx(body, data)
		return
	}

	n.handleVertexMessage(peer, data)
}

// ingestGossipedTx adds a gossiped transaction to the pending set and forwards it.
// The original tagged bytes are re-gossiped so the transaction reaches producers
// that this peer does not connect to directly. The network layer's dedup filter
// stops the forward from looping.
func (n *Node) ingestGossipedTx(body, tagged []byte) {
	if n.dag == nil {
		return
	}

	n.dag.SubmitTx(body)

	if n.network != nil {
		_ = n.network.Gossip(tagged, 10)
	}
}

// handleVertexMessage processes a vertex received over gossip and relays it on
// first sight so vertices propagate across the mesh even without full connectivity.
func (n *Node) handleVertexMessage(peer *network.Peer, data []byte) {
	v := types.GetRootAsVertex(data, 0)
	logger.Debug("received vertex",
		"round", v.Round(),
		"producer", hex.EncodeToString(v.ProducerBytes()[:8]),
		"from", peer.Address(),
	)

	if n.dag.AddVertex(data) {
		n.relayVertex(data)
	}
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

// GossipTx gossips a transaction (raw or ATX) to mesh peers so a producer that
// did not receive the submission directly still includes it in a vertex. The
// transaction is tagged (MsgGossipTx) so a receiving peer adds it to its pending
// set instead of misparsing the bytes as a vertex.
func (n *Node) GossipTx(tx []byte) {
	if n.network != nil {
		_ = n.network.Broadcast(network.EncodeGossipTx(tx))
	}
}

// setupRequestHandlers configures bidirectional request handlers.
// Dispatch order is load-bearing. Attestation tags (0x01-0x03) come first, then
// known client request tags (an exact set, not a range), then the snapshot
// heuristic, which has no tag and matches a FlatBuffer with RequestId() > 0. The
// client classification matches exact request tags so a snapshot request, whose
// FlatBuffer root offset (0x0c) lands in the client-tag range, is not misrouted.
func (n *Node) setupRequestHandlers() {
	n.setupValidatorPredicate()

	n.network.OnRequest(func(peer *network.Peer, data []byte) ([]byte, error) {
		// Handle attestation requests (tags 0x01-0x03)
		if aggregation.IsAttestationRequest(data) {
			if n.attHandler == nil {
				return nil, fmt.Errorf("no attestation handler")
			}
			return n.attHandler.HandleRequest(peer, data)
		}

		// Handle client-facing messages (explicit tags >= 0x04)
		if network.IsClientMessage(data) {
			return n.handleClientMessage(data)
		}

		// Handle snapshot requests (untagged FlatBuffers, RequestId() > 0)
		if sync.IsSnapshotRequest(data) {
			if n.snapManager == nil {
				return nil, fmt.Errorf("no snapshot manager")
			}
			return sync.HandleSnapshotRequest(data, n.snapManager)
		}

		return nil, fmt.Errorf("unknown request type")
	})
}

// setupValidatorPredicate injects the connection-classification test into the
// network layer. Any peer that presents a certificate (derived from its Ed25519
// key) joins the trusted mesh; a certless connection is an anonymous client
// served in the ephemeral, rate-limited tier. Mesh admission is by certificate
// presence, not validator-set membership: a non-validator's vertices carry no
// consensus weight (the DAG gates participation on validator-set membership), so
// admitting a cert-presenting peer is safe, and it avoids the chicken-and-egg
// where a registering validator is not yet in the committed set and would
// otherwise be denied the gossip it needs to become registered.
func (n *Node) setupValidatorPredicate() {
	n.network.SetValidatorPredicate(func(ed25519.PublicKey) bool {
		return true
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
