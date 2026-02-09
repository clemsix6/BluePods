package main

import (
	"crypto/ed25519"
	"encoding/hex"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"BluePods/internal/aggregation"
	"BluePods/internal/api"
	"BluePods/internal/consensus"
	"BluePods/internal/logger"
	"BluePods/internal/network"
	"BluePods/internal/podvm"
	"BluePods/internal/state"
	"BluePods/internal/storage"
	"BluePods/internal/sync"
)

const (
	// defaultSyncBufferSec is the default sync buffer duration in seconds.
	defaultSyncBufferSec = 12
)

// Node represents a running BluePods node.
type Node struct {
	cfg         *Config
	storage     *storage.Storage
	podPool     *podvm.Pool
	state       *state.State
	network     *network.Node
	dag         *consensus.DAG
	api         *api.Server
	snapManager *sync.SnapshotManager
	syncBuffer  *sync.VertexBuffer // syncBuffer holds vertices during sync
	systemPod   [32]byte

	// Aggregation components
	aggregator *aggregation.Aggregator  // aggregator orchestrates attestation collection
	blsKey     *aggregation.BLSKeyPair  // blsKey is the BLS key for signing attestations
	attHandler *aggregation.Handler     // attHandler responds to attestation requests
	rendezvous *aggregation.Rendezvous  // rendezvous computes object-holder mappings
}

// NewNode creates and initializes a new node.
func NewNode(cfg *Config) (*Node, error) {
	n := &Node{cfg: cfg}

	// Listener mode needs storage for snapshot and network
	if cfg.Listener {
		if cfg.BootstrapAddr == "" {
			return nil, fmt.Errorf("listener mode requires --bootstrap-addr")
		}

		if err := n.initStorage(); err != nil {
			return nil, err
		}

		if err := n.initNetwork(); err != nil {
			n.Close()
			return nil, err
		}

		return n, nil
	}

	// Full node initialization
	if err := n.initStorage(); err != nil {
		return nil, err
	}

	if err := n.initPodVM(); err != nil {
		n.Close()
		return nil, err
	}

	if err := n.initState(); err != nil {
		n.Close()
		return nil, err
	}

	if err := n.initNetwork(); err != nil {
		n.Close()
		return nil, err
	}

	// Skip DAG creation for validators that will sync.
	// They will create their DAG after receiving the snapshot.
	if cfg.BootstrapAddr == "" {
		if err := n.initConsensus(); err != nil {
			n.Close()
			return nil, err
		}
	}

	return n, nil
}

// Run starts the node and blocks until shutdown signal.
func (n *Node) Run() error {
	// Listener mode: sync then observe
	if n.cfg.Listener {
		return n.runListener()
	}

	// Validator mode: sync then participate (not bootstrap, has bootstrap-addr)
	if !n.cfg.Bootstrap && n.cfg.BootstrapAddr != "" {
		return n.runValidator()
	}

	// Bootstrap mode: start fresh
	return n.runBootstrap()
}

// runBootstrap starts the node in bootstrap mode (genesis validator).
func (n *Node) runBootstrap() error {
	if err := n.network.Start(); err != nil {
		return fmt.Errorf("start network:\n%w", err)
	}

	n.setupMessageHandlers()
	n.setupRequestHandlers()

	// Start HTTP API
	n.api = api.New(n.cfg.HTTPAddress, n.dag, nil, n.dag, n.state, n.faucetConfig(), n.aggregator, n.newHolderRouter(), n.state)
	if err := n.api.Start(); err != nil {
		return fmt.Errorf("start api:\n%w", err)
	}

	// Start snapshot manager for bootstrap nodes
	n.snapManager = sync.NewSnapshotManager(n.storage, n.dag)
	n.snapManager.SetDomainExporter(n.state)
	n.snapManager.Start()

	go n.processCommitted()

	return n.waitForShutdown()
}

// processCommitted handles committed transactions from the DAG.
func (n *Node) processCommitted() {
	for tx := range n.dag.Committed() {
		hash := hex.EncodeToString(tx.Hash[:8])

		if tx.Success {
			logger.Info("committed tx", "hash", hash, "func", tx.Function)
		} else {
			logger.Warn("conflicted tx", "hash", hash)
		}
	}
}

// waitForShutdown blocks until SIGINT or SIGTERM is received.
func (n *Node) waitForShutdown() error {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	sig := <-sigCh
	logger.Info("shutting down", "signal", sig.String())

	return n.Close()
}

// myPubkey returns this node's public key as a consensus.Hash.
func (n *Node) myPubkey() consensus.Hash {
	var pk consensus.Hash
	copy(pk[:], n.cfg.PrivateKey.Public().(ed25519.PublicKey))
	return pk
}

// faucetConfig returns the faucet configuration for the API server.
func (n *Node) faucetConfig() *api.FaucetConfig {
	return &api.FaucetConfig{
		PrivKey:   n.cfg.PrivateKey,
		SystemPod: n.systemPod,
	}
}

// Close shuts down all node components gracefully.
func (n *Node) Close() error {
	if n.api != nil {
		n.api.Stop()
	}

	if n.snapManager != nil {
		n.snapManager.Stop()
	}

	if n.dag != nil {
		n.dag.Close()
	}

	if n.network != nil {
		n.network.Close()
	}

	if n.podPool != nil {
		n.podPool.Close()
	}

	if n.storage != nil {
		n.storage.Close()
	}

	return nil
}
