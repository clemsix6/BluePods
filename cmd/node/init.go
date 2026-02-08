package main

import (
	"crypto/ed25519"
	"fmt"
	"os"

	"github.com/zeebo/blake3"

	"BluePods/internal/aggregation"
	"BluePods/internal/consensus"
	"BluePods/internal/genesis"
	"BluePods/internal/logger"
	"BluePods/internal/network"
	"BluePods/internal/podvm"
	"BluePods/internal/state"
	"BluePods/internal/storage"
)

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

	n.setupValidatorCallback()
	n.initAggregation(validators)

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

	// Derive BLS key early so it's available for genesis
	blsKey, err := aggregation.DeriveFromED25519(n.cfg.PrivateKey)
	if err != nil {
		logger.Warn("failed to derive BLS key for genesis", "error", err)
		return nil
	}

	n.blsKey = blsKey

	genesisCfg := genesis.Config{
		PrivateKey:  n.cfg.PrivateKey,
		InitialMint: n.cfg.InitialMint,
		HTTPAddress: n.cfg.HTTPAddress,
		QUICAddress: n.cfg.QUICAddress,
		SystemPodID: n.systemPod,
		BLSPubkey:   blsKey.PublicKeyBytes(),
	}

	txs, err := genesis.BuildTransactions(genesisCfg)
	if err != nil {
		logger.Warn("failed to build genesis txs", "error", err)
		return nil
	}

	return n.appendEpochOpts([]consensus.Option{
		consensus.WithGenesisTxs(txs),
		consensus.WithBootstrap(),
		consensus.WithMinValidators(n.cfg.MinValidators),
	})
}

// appendEpochOpts adds epoch-related options if configured.
func (n *Node) appendEpochOpts(opts []consensus.Option) []consensus.Option {
	if n.cfg.EpochLength > 0 {
		opts = append(opts, consensus.WithEpochLength(n.cfg.EpochLength))
	}

	if n.cfg.MaxChurnPerEpoch > 0 {
		opts = append(opts, consensus.WithMaxChurnPerEpoch(n.cfg.MaxChurnPerEpoch))
	}

	return opts
}
