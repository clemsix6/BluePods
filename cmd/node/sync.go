package main

import (
	"bytes"
	"crypto/ed25519"
	"encoding/hex"
	"fmt"
	"time"

	"BluePods/internal/api"
	"BluePods/internal/consensus"
	"BluePods/internal/logger"
	"BluePods/internal/network"
	"BluePods/internal/state"
	"BluePods/internal/sync"
	"BluePods/internal/types"
)

// snapshotResult holds the result of applying a snapshot.
type snapshotResult struct {
	lastCommittedRound uint64                         // lastCommittedRound is the round of the last committed vertex
	validators         []*consensus.ValidatorInfo      // validators is the set of validators from the snapshot
	vertices           []consensus.VertexEntry         // vertices is the set of vertices from the snapshot
	trackerEntries     []consensus.ObjectTrackerEntry  // trackerEntries is the set of object tracker entries
	domainEntries      []state.DomainEntry             // domainEntries is the set of domain mappings from the snapshot
}

// runValidator runs the node as a new validator: sync then participate.
func (n *Node) runValidator() error {
	// Create buffer to collect vertices during sync
	n.syncBuffer.Store(sync.NewVertexBuffer())

	// Set up handler to buffer vertices
	n.network.OnMessage(func(peer *network.Peer, data []byte) {
		logger.Debug("buffering vertex", "from", peer.Address(), "len", len(data))
		if buf := n.syncBuffer.Load(); buf != nil {
			buf.Add(data)
		}
	})

	// Connect to bootstrap
	peer, err := n.network.Connect(n.cfg.BootstrapAddr)
	if err != nil {
		return fmt.Errorf("connect to bootstrap:\n%w", err)
	}

	logger.Info("connected to bootstrap", "addr", peer.Address())

	// Perform full sync (as validator, not listener)
	// Note: performSync sets up message handlers before returning
	if err := n.performSync(peer, true); err != nil {
		return fmt.Errorf("sync failed:\n%w", err)
	}

	// Set up request handlers for snapshot serving and attestations
	n.setupRequestHandlers()

	// Start network listener early so other validators can connect to us.
	// With the shared transport, the listener uses the same UDP socket as outgoing connections.
	if err := n.network.Start(); err != nil {
		return fmt.Errorf("start network:\n%w", err)
	}

	// Connect to existing validators from snapshot to form mesh network
	n.connectToExistingValidators()

	// Start processing committed transactions
	go n.processCommitted()

	// Register as validator by sending raw tx to bootstrap
	if err := n.registerAsValidator(); err != nil {
		return fmt.Errorf("register validator:\n%w", err)
	}

	// Start HTTP API
	n.api = api.New(n.cfg.HTTPAddress, n.dag, nil, n.dag, n.state, n.faucetConfig(), n.aggregator, n.newHolderRouter(), n.state)
	if err := n.api.Start(); err != nil {
		return fmt.Errorf("start api:\n%w", err)
	}

	// Start snapshot manager
	n.snapManager = sync.NewSnapshotManager(n.storage, n.dag)
	n.snapManager.SetDomainExporter(n.state)
	n.snapManager.Start()

	logger.Info("validator mode active", "round", n.dag.Round())

	return n.waitForShutdown()
}

// runListener runs the node in listener mode (observe only).
func (n *Node) runListener() error {
	// Create buffer to collect vertices during sync
	n.syncBuffer.Store(sync.NewVertexBuffer())

	// Set up handler to buffer vertices
	n.network.OnMessage(func(peer *network.Peer, data []byte) {
		if buf := n.syncBuffer.Load(); buf != nil {
			buf.Add(data)
		}
	})

	// Connect to bootstrap
	peer, err := n.network.Connect(n.cfg.BootstrapAddr)
	if err != nil {
		return fmt.Errorf("connect to bootstrap:\n%w", err)
	}

	logger.Info("connected to bootstrap", "addr", peer.Address())

	// Perform full sync (listener mode)
	// Note: performSync sets up message handlers before returning
	if err := n.performSync(peer, false); err != nil {
		return fmt.Errorf("sync failed:\n%w", err)
	}

	// Process committed transactions
	go n.processCommitted()

	return n.waitForShutdown()
}

// performSync executes the full sync process: buffer, snapshot, replay.
// If asValidator is true, initializes for active participation instead of listener mode.
func (n *Node) performSync(peer *network.Peer, asValidator bool) error {
	// Wait to buffer enough vertices (covers snapshot interval)
	bufferDuration := time.Duration(n.cfg.SyncBufferSec) * time.Second
	logger.Info("buffering vertices", "duration", bufferDuration)
	time.Sleep(bufferDuration)

	logger.Info("buffer status",
		"vertices", n.syncBuffer.Load().Len(),
		"minRound", n.syncBuffer.Load().MinRound(),
		"maxRound", n.syncBuffer.Load().MaxRound(),
	)

	// Request and apply snapshot
	result, err := n.requestAndApplySnapshot(peer)
	if err != nil {
		return fmt.Errorf("apply snapshot:\n%w", err)
	}

	// Initialize full node components
	if err := n.initPodVM(); err != nil {
		return fmt.Errorf("init podvm:\n%w", err)
	}

	if err := n.initState(); err != nil {
		return fmt.Errorf("init state:\n%w", err)
	}

	// Initialize consensus with appropriate mode
	if asValidator {
		if err := n.initConsensusForValidator(result); err != nil {
			return fmt.Errorf("init consensus:\n%w", err)
		}
	} else {
		if err := n.initConsensusForListener(result); err != nil {
			return fmt.Errorf("init consensus:\n%w", err)
		}
	}

	// Switch message handler BEFORE replay to avoid losing vertices.
	// New vertices will go directly to DAG while we replay the buffer.
	// Relay new vertices to other peers for full mesh propagation.
	n.network.OnMessage(func(peer *network.Peer, data []byte) {
		logger.Debug("gossip received", "from", peer.Address(), "len", len(data))
		if n.dag.AddVertex(data) {
			n.relayVertex(data)
		}
	})

	// Replay buffered vertices through DAG
	if err := n.replayBufferedVertices(result.lastCommittedRound); err != nil {
		return fmt.Errorf("replay vertices:\n%w", err)
	}

	// Purge pending vertices whose parents predate the snapshot
	minRound := snapshotMinRound(result.vertices)
	if minRound > 0 {
		n.dag.PurgePendingBeforeRound(minRound)
	}

	// Clear buffer
	if buf := n.syncBuffer.Swap(nil); buf != nil {
		buf.Clear()
	}

	logger.Info("sync complete", "round", n.dag.Round())

	return nil
}

// requestAndApplySnapshot requests a snapshot from a peer and applies it locally.
func (n *Node) requestAndApplySnapshot(peer *network.Peer) (*snapshotResult, error) {
	logger.Info("requesting snapshot")

	data, err := sync.RequestSnapshot(peer)
	if err != nil {
		return nil, fmt.Errorf("request snapshot:\n%w", err)
	}

	snapshot, err := sync.ApplySnapshot(n.storage, data)
	if err != nil {
		return nil, fmt.Errorf("apply snapshot:\n%w", err)
	}

	result := &snapshotResult{
		lastCommittedRound: snapshot.LastCommittedRound(),
		validators:         sync.ExtractValidators(snapshot),
		vertices:           sync.ExtractVertices(snapshot),
		trackerEntries:     sync.ExtractTrackerEntries(snapshot),
		domainEntries:      sync.ExtractDomains(snapshot),
	}

	// Import domain entries into state
	if len(result.domainEntries) > 0 {
		n.state.ImportDomains(result.domainEntries)
	}

	logger.Info("snapshot applied",
		"round", result.lastCommittedRound,
		"objects", snapshot.ObjectsLength(),
		"validators", len(result.validators),
		"vertices", len(result.vertices),
		"trackerEntries", len(result.trackerEntries),
		"domains", len(result.domainEntries),
	)

	return result, nil
}

// initConsensusForListener initializes the DAG in listener mode (observe only).
func (n *Node) initConsensusForListener(result *snapshotResult) error {
	validators := n.buildValidatorSetFromSnapshot(result.validators)

	opts := []consensus.Option{
		consensus.WithLastCommittedRound(result.lastCommittedRound),
		consensus.WithListenerMode(),
		consensus.WithImportData(result.vertices, result.trackerEntries),
	}

	if n.cfg.EpochLength > 0 {
		opts = append(opts, consensus.WithEpochLength(n.cfg.EpochLength))
	}

	n.dag = consensus.New(
		n.storage,
		validators,
		nil, // no broadcaster for listener
		n.systemPod,
		0, // epoch
		n.cfg.PrivateKey,
		n.state,
		opts...,
	)

	n.setupValidatorCallback()

	return nil
}

// initConsensusForValidator initializes the DAG for active participation.
func (n *Node) initConsensusForValidator(result *snapshotResult) error {
	logger.Info("snapshot validators received", "count", len(result.validators))
	validators := n.buildValidatorSetFromSnapshot(result.validators)
	logger.Info("validator set created", "size", validators.Len())

	opts := []consensus.Option{
		consensus.WithLastCommittedRound(result.lastCommittedRound),
		consensus.WithMinValidators(n.cfg.MinValidators),
		consensus.WithImportData(result.vertices, result.trackerEntries),
	}

	opts = n.appendEpochOpts(opts)

	n.dag = consensus.New(
		n.storage,
		validators,
		n.network, // broadcaster for active participation
		n.systemPod,
		0, // epoch
		n.cfg.PrivateKey,
		n.state,
		opts...,
	)

	logger.Info("DAG created for validator mode",
		"validators", n.dag.ValidatorsInfo(),
		"round", n.dag.Round(),
		"minValidators", n.cfg.MinValidators,
	)

	n.setupValidatorCallback()
	n.initAggregation(validators)

	return nil
}

// buildValidatorSetFromSnapshot creates a ValidatorSet from snapshot validators.
// It preserves both pubkeys and network addresses from the snapshot.
func (n *Node) buildValidatorSetFromSnapshot(validators []*consensus.ValidatorInfo) *consensus.ValidatorSet {
	// Create empty validator set
	vs := consensus.NewValidatorSet(nil)

	logger.Info("building validator set from snapshot",
		"count", len(validators),
	)

	// Add each validator with their full info (pubkey + addresses + BLS key)
	for _, v := range validators {
		vs.Add(v.Pubkey, v.HTTPAddr, v.QUICAddr, v.BLSPubkey)
		logger.Debug("added validator from snapshot",
			"pubkey", hex.EncodeToString(v.Pubkey[:8]),
			"http", v.HTTPAddr,
			"quic", v.QUICAddr,
		)
	}

	return vs
}

// connectToExistingValidators connects to all validators from the snapshot.
// This is called after sync to form the mesh network with existing validators.
func (n *Node) connectToExistingValidators() {
	myPubkey := n.cfg.PrivateKey.Public().(ed25519.PublicKey)
	validators := n.dag.ValidatorsInfo()

	for _, info := range validators {
		// Skip self
		if bytes.Equal(info.Pubkey[:], myPubkey) {
			continue
		}

		// Skip validators without QUIC address
		if info.QUICAddr == "" {
			continue
		}

		go n.connectToValidator(info)
	}
}

// replayBufferedVertices injects buffered vertices into the DAG.
func (n *Node) replayBufferedVertices(lastCommittedRound uint64) error {
	// Replay ALL buffered vertices, not just those after lastCommittedRound.
	// The snapshot may not include all historical vertices, and the buffer
	// contains vertices received during sync that complete the chain.
	// The DAG will handle duplicates and out-of-order vertices gracefully.
	vertices := n.syncBuffer.Load().GetAll()

	// Count unique producers in buffer to debug sync issues
	producers := make(map[string]int)
	for _, data := range vertices {
		v := types.GetRootAsVertex(data, 0)
		prod := hex.EncodeToString(v.ProducerBytes()[:8])
		producers[prod]++
	}

	logger.Info("replaying vertices",
		"count", len(vertices),
		"fromSnapshot", lastCommittedRound,
		"bufferMinRound", n.syncBuffer.Load().MinRound(),
		"bufferMaxRound", n.syncBuffer.Load().MaxRound(),
		"dagRoundAfterImport", n.dag.Round(),
		"producersInBuffer", producers,
	)

	added := 0
	for _, data := range vertices {
		if n.dag.AddVertex(data) {
			added++
		}
	}

	logger.Info("replay complete",
		"added", added,
		"total", len(vertices),
		"dagRoundNow", n.dag.Round(),
	)

	return nil
}

// snapshotMinRound returns the minimum round from snapshot vertices.
// Returns 0 if no vertices are present.
func snapshotMinRound(vertices []consensus.VertexEntry) uint64 {
	if len(vertices) == 0 {
		return 0
	}

	minRound := vertices[0].Round
	for _, v := range vertices[1:] {
		if v.Round < minRound {
			minRound = v.Round
		}
	}

	return minRound
}
