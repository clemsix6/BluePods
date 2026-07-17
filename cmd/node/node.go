package main

import (
	"crypto/ed25519"
	"encoding/hex"
	"fmt"
	"os"
	"os/signal"
	gosync "sync"
	"sync/atomic"
	"syscall"

	"golang.org/x/term"

	"BluePods/cmd/node/tui"
	"BluePods/internal/aggregation"
	"BluePods/internal/consensus"
	"BluePods/internal/events"
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
	snapManager *sync.SnapshotManager
	syncBuffer  atomic.Pointer[sync.VertexBuffer] // syncBuffer holds vertices during sync
	systemPod   [32]byte

	// Observability sources, fed from the committed stream and submission ingress.
	stats   *Stats         // stats accumulates transaction-derived counters
	txIndex *txStatusIndex // txIndex maps a tx hash to its last-known status

	useTUI bool // useTUI is true when the dashboard should run instead of line logs

	// Aggregation components
	blsKey     *aggregation.BLSKeyPair                          // blsKey is the BLS key for signing attestations
	attHandler *aggregation.Handler                             // attHandler responds to attestation requests
	rendezvous *aggregation.Rendezvous                          // rendezvous computes object-holder mappings
	isHolder   func(objectID [32]byte, replication uint16) bool // isHolder reports whether this node currently holds a replicated object

	// Faucet serialization. Every faucet split mutates the single genesis reserve
	// coin, so concurrent requests would all read the same version and lose the
	// version race at commit. faucetMu serializes request handling and
	// faucetNextVersion tracks the reserve coin's next version across in-flight
	// splits so sequential requests carry V, V+1, V+2 in submission order.
	faucetMu          gosync.Mutex // faucetMu serializes faucet request handling
	faucetNextVersion uint64       // faucetNextVersion is the reserve coin version the next split will reference
}

// NewNode creates and initializes a new node.
func NewNode(cfg *Config) (*Node, error) {
	n := &Node{cfg: cfg, stats: newStats(), txIndex: newTxStatusIndex()}
	n.useTUI = !cfg.LogMode && cfg.LogFormat != "json" && term.IsTerminal(int(os.Stdout.Fd()))

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

	// Start snapshot manager for bootstrap nodes
	n.snapManager = sync.NewSnapshotManager(n.storage, n.dag)
	n.snapManager.SetDomainExporter(n.state)
	n.snapManager.Start()

	go n.processCommitted()

	events.NodeReady(n.dag.LastCommittedRound())

	return n.serve()
}

// processCommitted handles committed transactions from the DAG, feeding the
// stats source and the tx-status index before logging.
func (n *Node) processCommitted() {
	for tx := range n.dag.Committed() {
		round := n.dag.LastCommittedRound()
		n.stats.record(tx)
		n.txIndex.markCommitted(tx, round)
		n.txIndex.prune(round)

		hash := hex.EncodeToString(tx.Hash[:8])
		if tx.Success {
			logger.Info("committed tx", "hash", hash, "func", tx.Function)
		} else {
			logger.Warn("conflicted tx", "hash", hash, "reason", tx.Reason.String())
		}
	}
}

// waitForShutdown blocks until SIGINT or SIGTERM is received.
func (n *Node) waitForShutdown() error {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	sig := <-sigCh
	logger.Info("shutting down", "signal", sig.String())
	events.NodeStopping(sig.String())

	return n.Close()
}

// Snapshot assembles the dashboard view from the stats source plus live DAG and
// network reads. It implements tui.Provider.
func (n *Node) Snapshot() tui.Snapshot {
	s := tui.Snapshot{
		NodeAddr: n.cfg.QUICAddress,
		TotalTx:  n.stats.TotalTx(),
		FailedTx: n.stats.FailedTx(),
		TPS:      n.stats.TPS(),
	}

	if n.dag != nil {
		s.Round = n.dag.Round()
		s.Epoch = n.dag.Epoch()
		s.LastCommitted = n.dag.LastCommittedRound()
		s.Validators = len(n.dag.ValidatorsInfo())
	}

	if n.network != nil {
		s.ConnectedPeers = n.network.ConnectedPeers()
	}

	for _, tx := range n.stats.Recent() {
		s.Recent = append(s.Recent, tui.RecentTx{
			Hash:     hex.EncodeToString(tx.Hash[:4]),
			Function: tx.Function,
			Success:  tx.Success,
			Reason:   tx.Reason.String(),
		})
	}

	return s
}

// serve blocks until shutdown, running the dashboard on a terminal or waiting for
// a signal otherwise. Both paths end by closing the node.
func (n *Node) serve() error {
	if n.useTUI {
		if err := n.redirectLogsToDisk(); err != nil {
			logger.Warn("dashboard disabled, logging to terminal", "error", err)
			return n.waitForShutdown()
		}

		err := tui.Run(n)
		n.Close()
		return err
	}

	return n.waitForShutdown()
}

// redirectLogsToDisk sends log output to a file under the data directory so log
// lines do not corrupt the dashboard. The file is left open for the lifetime of
// the process; the OS closes it on exit.
func (n *Node) redirectLogsToDisk() error {
	path := n.cfg.DataPath + "/node.log"
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return fmt.Errorf("open log file %s:\n%w", path, err)
	}

	logger.SetOutput(f)
	return nil
}

// myPubkey returns this node's public key as a consensus.Hash.
func (n *Node) myPubkey() consensus.Hash {
	var pk consensus.Hash
	copy(pk[:], n.cfg.PrivateKey.Public().(ed25519.PublicKey))
	return pk
}

// Close shuts down all node components gracefully.
func (n *Node) Close() error {
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
