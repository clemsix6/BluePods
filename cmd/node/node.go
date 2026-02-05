package main

import (
	"crypto/ed25519"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/zeebo/blake3"

	"BluePods/internal/consensus"
	"BluePods/internal/genesis"
	"BluePods/internal/network"
	"BluePods/internal/podvm"
	"BluePods/internal/state"
	"BluePods/internal/storage"
)

// Node represents a running BluePods node.
type Node struct {
	cfg       *Config
	storage   *storage.Storage
	podPool   *podvm.Pool
	state     *state.State
	network   *network.Node
	dag       *consensus.DAG
	systemPod [32]byte
}

// NewNode creates and initializes a new node.
func NewNode(cfg *Config) (*Node, error) {
	n := &Node{cfg: cfg}

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

	if err := n.initConsensus(); err != nil {
		n.Close()
		return nil, err
	}

	return n, nil
}

// initStorage initializes the Pebble storage.
func (n *Node) initStorage() error {
	dbPath := n.cfg.DataPath + "/db"

	if err := os.MkdirAll(n.cfg.DataPath, 0755); err != nil {
		return fmt.Errorf("create data directory:\n%w", err)
	}

	db, err := storage.New(dbPath)
	if err != nil {
		return fmt.Errorf("init storage:\n%w", err)
	}

	n.storage = db

	return nil
}

// initPodVM initializes the WASM runtime pool with the system pod.
func (n *Node) initPodVM() error {
	pool := podvm.New()

	wasmBytes, err := os.ReadFile(n.cfg.SystemPodPath)
	if err != nil {
		pool.Close()
		return fmt.Errorf("read system pod WASM:\n%w", err)
	}

	// Use blake3 hash as system pod ID
	n.systemPod = blake3.Sum256(wasmBytes)

	if _, err := pool.Load(wasmBytes, &n.systemPod); err != nil {
		pool.Close()
		return fmt.Errorf("load system pod:\n%w", err)
	}

	n.podPool = pool

	return nil
}

// initState initializes the transaction executor.
func (n *Node) initState() error {
	n.state = state.New(n.storage, n.podPool)
	return nil
}

// initNetwork initializes the P2P network node.
func (n *Node) initNetwork() error {
	netCfg := network.Config{
		PrivateKey: n.cfg.PrivateKey,
		ListenAddr: n.cfg.QUICAddress,
	}

	node, err := network.NewNode(netCfg)
	if err != nil {
		return fmt.Errorf("init network:\n%w", err)
	}

	n.network = node

	return nil
}

// initConsensus initializes the DAG consensus engine.
func (n *Node) initConsensus() error {
	validators := n.buildValidatorSet()
	opts := n.buildConsensusOpts()

	n.dag = consensus.New(
		n.storage,
		validators,
		n.network,
		n.systemPod,
		0, // epoch
		n.cfg.PrivateKey,
		n.state,
		opts...,
	)

	return nil
}

// buildValidatorSet creates the initial validator set.
func (n *Node) buildValidatorSet() *consensus.ValidatorSet {
	pubKey := n.cfg.PrivateKey.Public().(ed25519.PublicKey)

	var hash consensus.Hash
	copy(hash[:], pubKey)

	if n.cfg.Bootstrap {
		// Bootstrap validator must be in set BEFORE first vertex for validation
		return consensus.NewValidatorSet([]consensus.Hash{hash})
	}

	// TODO: Load validator set from state for non-bootstrap nodes
	return consensus.NewValidatorSet(nil)
}

// buildConsensusOpts creates consensus options including genesis transactions.
func (n *Node) buildConsensusOpts() []consensus.Option {
	if !n.cfg.Bootstrap {
		return nil
	}

	genesisCfg := genesis.Config{
		PrivateKey:  n.cfg.PrivateKey,
		InitialMint: n.cfg.InitialMint,
		HTTPAddress: n.cfg.HTTPAddress,
		QUICAddress: n.cfg.QUICAddress,
		SystemPodID: n.systemPod,
	}

	txs, err := genesis.BuildTransactions(genesisCfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: failed to build genesis txs: %v\n", err)
		return nil
	}

	return []consensus.Option{consensus.WithGenesisTxs(txs)}
}

// Run starts the node and blocks until shutdown signal.
func (n *Node) Run() error {
	if err := n.network.Start(); err != nil {
		return fmt.Errorf("start network:\n%w", err)
	}

	n.setupMessageHandlers()

	go n.processCommitted()

	return n.waitForShutdown()
}

// setupMessageHandlers configures network message handlers.
func (n *Node) setupMessageHandlers() {
	n.network.OnMessage(func(peer *network.Peer, data []byte) {
		// Handle incoming vertices from peers
		n.dag.AddVertex(data)
	})
}

// processCommitted handles committed transactions from the DAG.
func (n *Node) processCommitted() {
	for tx := range n.dag.Committed() {
		if tx.Success {
			fmt.Printf("[DAG] committed tx: %x\n", tx.Hash[:8])
		} else {
			fmt.Printf("[DAG] conflicted tx: %x\n", tx.Hash[:8])
		}
	}
}

// waitForShutdown blocks until SIGINT or SIGTERM is received.
func (n *Node) waitForShutdown() error {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	sig := <-sigCh
	fmt.Printf("\nReceived %s, shutting down...\n", sig)

	return n.Close()
}

// Close shuts down all node components gracefully.
func (n *Node) Close() error {
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
