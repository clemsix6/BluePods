package main

import (
	"bytes"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	flatbuffers "github.com/google/flatbuffers/go"
	"github.com/zeebo/blake3"

	"BluePods/internal/aggregation"
	"BluePods/internal/api"
	"BluePods/internal/consensus"
	"BluePods/internal/genesis"
	"BluePods/internal/logger"
	"BluePods/internal/network"
	"BluePods/internal/podvm"
	"BluePods/internal/state"
	"BluePods/internal/storage"
	"BluePods/internal/sync"
	"BluePods/internal/types"
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

// initAggregation initializes the aggregation subsystem.
func (n *Node) initAggregation(validators *consensus.ValidatorSet) {
	blsKey, err := aggregation.DeriveFromED25519(n.cfg.PrivateKey)
	if err != nil {
		logger.Warn("failed to derive BLS key, attestation handler disabled", "error", err)
		return
	}

	n.blsKey = blsKey
	n.attHandler = aggregation.NewHandler(n.state, n.blsKey)
	n.rendezvous = aggregation.NewRendezvous(validators)
	n.aggregator = aggregation.NewAggregator(n.network, validators, n.state)

	// Initialize epoch holders and set epoch transition callback
	if n.dag != nil {
		n.dag.InitEpochHolders()

		n.dag.OnEpochTransition(func(epoch uint64) {
			// Rebuild Rendezvous with the new epoch holders
			epochHolders := n.dag.EpochHolders()
			n.rendezvous = aggregation.NewRendezvous(epochHolders)

			logger.Info("rendezvous rebuilt for epoch",
				"epoch", epoch,
				"holders", epochHolders.Len(),
			)

			// Background scan for object redistribution
			go n.scanObjectsForEpoch()
		})
	}

	// Set up isHolder closure for execution and storage sharding
	myPubkey := n.myPubkey()
	isHolder := n.buildIsHolder(myPubkey)

	n.dag.SetIsHolder(isHolder)
	n.state.SetIsHolder(isHolder)

	// Wire object creation callback to tracker
	n.state.SetOnObjectCreated(func(id [32]byte, version uint64, replication uint16) {
		n.dag.TrackObject(id, version, replication)
	})

	// Set up ATX proof verifier
	n.dag.SetATXProofVerifier(n.buildATXVerifier(validators))

	logger.Info("aggregation initialized")
}

// buildIsHolder creates a closure that checks if this node is a holder for an object.
func (n *Node) buildIsHolder(myPubkey consensus.Hash) func(objectID [32]byte, replication uint16) bool {
	return func(objectID [32]byte, replication uint16) bool {
		// Singletons: all validators hold them
		if replication == 0 {
			return true
		}

		holders := n.rendezvous.ComputeHolders(objectID, int(replication))
		for _, h := range holders {
			if h == myPubkey {
				return true
			}
		}

		return false
	}
}

// buildATXVerifier returns a closure that verifies all BLS quorum proofs in an ATX.
func (n *Node) buildATXVerifier(validators *consensus.ValidatorSet) func(*types.AttestedTransaction) error {
	return func(atx *types.AttestedTransaction) error {
		var proof types.QuorumProof

		for i := 0; i < atx.ProofsLength(); i++ {
			if !atx.Proofs(&proof, i) {
				return fmt.Errorf("cannot read proof %d", i)
			}

			if err := n.verifySingleProof(atx, &proof, validators); err != nil {
				return fmt.Errorf("proof %d:\n%w", i, err)
			}
		}

		return nil
	}
}

// verifySingleProof verifies one QuorumProof against the ATX objects and validator BLS keys.
func (n *Node) verifySingleProof(atx *types.AttestedTransaction, proof *types.QuorumProof, validators *consensus.ValidatorSet) error {
	// Find the matching object in the ATX
	objIdx := findATXObjectIndex(atx, proof.ObjectIdBytes())
	if objIdx < 0 {
		return fmt.Errorf("object not found in ATX")
	}

	var obj types.Object
	if !atx.Objects(&obj, objIdx) {
		return fmt.Errorf("cannot read object at index %d", objIdx)
	}

	// Recompute expected hash from object data
	hash := aggregation.ComputeObjectHash(obj.ContentBytes(), obj.Version())

	// Compute holders using rendezvous
	var objectID [32]byte
	copy(objectID[:], proof.ObjectIdBytes())
	holders := n.rendezvous.ComputeHolders(objectID, int(obj.Replication()))

	// Extract signer BLS keys from bitmap
	blsKeys, signerCount := extractSignerBLSKeys(proof.SignerBitmapBytes(), holders, validators)
	if signerCount == 0 {
		return fmt.Errorf("no signers in bitmap")
	}

	// Verify quorum
	quorum := aggregation.QuorumSize(len(holders))
	if signerCount < quorum {
		return fmt.Errorf("insufficient signers: got %d, need %d", signerCount, quorum)
	}

	// Verify aggregated BLS signature
	if !aggregation.VerifyAggregated(proof.BlsSignatureBytes(), hash[:], blsKeys) {
		return fmt.Errorf("aggregated BLS signature invalid")
	}

	return nil
}

// findATXObjectIndex returns the index of the object with the given ID in the ATX, or -1.
func findATXObjectIndex(atx *types.AttestedTransaction, objectID []byte) int {
	var obj types.Object

	for i := 0; i < atx.ObjectsLength(); i++ {
		if !atx.Objects(&obj, i) {
			continue
		}

		if bytes.Equal(obj.IdBytes(), objectID) {
			return i
		}
	}

	return -1
}

// extractSignerBLSKeys maps a signer bitmap to BLS public keys via rendezvous holders.
// Returns the BLS keys and the number of signers.
func extractSignerBLSKeys(bitmap []byte, holders []consensus.Hash, validators *consensus.ValidatorSet) ([][]byte, int) {
	indices := aggregation.ParseSignerBitmap(bitmap)
	var keys [][]byte

	for _, idx := range indices {
		if idx >= len(holders) {
			continue
		}

		info := validators.Get(holders[idx])
		if info == nil || info.BLSPubkey == [48]byte{} {
			continue
		}

		keys = append(keys, info.BLSPubkey[:])
	}

	return keys, len(keys)
}

// myPubkey returns this node's public key as a consensus.Hash.
func (n *Node) myPubkey() consensus.Hash {
	var pk consensus.Hash
	copy(pk[:], n.cfg.PrivateKey.Public().(ed25519.PublicKey))
	return pk
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

	opts := []consensus.Option{
		consensus.WithGenesisTxs(txs),
		consensus.WithBootstrap(),
		consensus.WithMinValidators(n.cfg.MinValidators),
	}

	if n.cfg.EpochLength > 0 {
		opts = append(opts, consensus.WithEpochLength(n.cfg.EpochLength))
	}

	if n.cfg.MaxChurnPerEpoch > 0 {
		opts = append(opts, consensus.WithMaxChurnPerEpoch(n.cfg.MaxChurnPerEpoch))
	}

	return opts
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
	if err := n.network.Start(); err != nil {
		return fmt.Errorf("start network:\n%w", err)
	}

	n.setupMessageHandlers()
	n.setupRequestHandlers()

	// Start HTTP API
	n.api = api.New(n.cfg.HTTPAddress, n.dag, nil, n.dag, n.state, n.faucetConfig(), n.aggregator, n.newHolderRouter())
	if err := n.api.Start(); err != nil {
		return fmt.Errorf("start api:\n%w", err)
	}

	// Start snapshot manager for bootstrap nodes
	n.snapManager = sync.NewSnapshotManager(n.storage, n.dag)
	n.snapManager.Start()

	go n.processCommitted()

	return n.waitForShutdown()
}

// runValidator runs the node as a new validator: sync then participate.
func (n *Node) runValidator() error {
	// Create buffer to collect vertices during sync
	n.syncBuffer = sync.NewVertexBuffer()

	// Set up handler to buffer vertices
	n.network.OnMessage(func(peer *network.Peer, data []byte) {
		logger.Debug("buffering vertex", "from", peer.Address(), "len", len(data))
		n.syncBuffer.Add(data)
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
	n.api = api.New(n.cfg.HTTPAddress, n.dag, nil, n.dag, n.state, n.faucetConfig(), n.aggregator, n.newHolderRouter())
	if err := n.api.Start(); err != nil {
		return fmt.Errorf("start api:\n%w", err)
	}

	// Start snapshot manager
	n.snapManager = sync.NewSnapshotManager(n.storage, n.dag)
	n.snapManager.Start()

	logger.Info("validator mode active", "round", n.dag.Round())

	return n.waitForShutdown()
}

// registerAsValidator sends a register_validator raw transaction to the bootstrap node.
func (n *Node) registerAsValidator() error {
	var blsPubkeyBytes []byte
	if n.blsKey != nil {
		blsPubkeyBytes = n.blsKey.PublicKeyBytes()
	}

	tx := genesis.BuildRegisterValidatorRawTx(
		n.cfg.PrivateKey,
		n.systemPod,
		n.cfg.HTTPAddress,
		n.cfg.QUICAddress,
		blsPubkeyBytes,
	)

	registrationHTTP := n.getRegistrationHTTPAddr()
	logger.Info("registering as validator", "target", registrationHTTP)

	resp, err := http.Post(
		"http://"+registrationHTTP+"/tx",
		"application/octet-stream",
		bytes.NewReader(tx),
	)
	if err != nil {
		return fmt.Errorf("send registration tx:\n%w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusAccepted {
		return fmt.Errorf("registration failed: status %d", resp.StatusCode)
	}

	// Optimistic self-add: add ourselves to the local validator set immediately
	// so we can start producing vertices without waiting for the registration
	// to commit through the full DAG chain.
	pubKey := n.cfg.PrivateKey.Public().(ed25519.PublicKey)
	var pubHash consensus.Hash
	copy(pubHash[:], pubKey)

	var blsPub [48]byte
	if n.blsKey != nil {
		copy(blsPub[:], n.blsKey.PublicKeyBytes())
	}

	n.dag.AddValidator(pubHash, n.cfg.HTTPAddress, n.cfg.QUICAddress, blsPub)

	logger.Info("registration submitted, self-added to validator set")
	return nil
}

// getRegistrationHTTPAddr derives the HTTP address for validator registration.
// Uses RegistrationAddr if set, otherwise falls back to BootstrapAddr.
// Convention: HTTP port = QUIC port - 920 (e.g., QUIC 9000 → HTTP 8080).
func (n *Node) getRegistrationHTTPAddr() string {
	quicAddr := n.cfg.RegistrationAddr
	if quicAddr == "" {
		quicAddr = n.cfg.BootstrapAddr
	}

	host := quicAddr
	portStr := ""

	if idx := strings.LastIndex(host, ":"); idx != -1 {
		portStr = host[idx+1:]
		host = host[:idx]
	}

	if portStr != "" {
		if port, err := strconv.Atoi(portStr); err == nil {
			httpPort := port - 920
			if httpPort > 0 {
				return fmt.Sprintf("%s:%d", host, httpPort)
			}
		}
	}

	return host + ":8080"
}

// runListener runs the node in listener mode (observe only).
func (n *Node) runListener() error {
	// Create buffer to collect vertices during sync
	n.syncBuffer = sync.NewVertexBuffer()

	// Set up handler to buffer vertices
	n.network.OnMessage(func(peer *network.Peer, data []byte) {
		n.syncBuffer.Add(data)
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
		"vertices", n.syncBuffer.Len(),
		"minRound", n.syncBuffer.MinRound(),
		"maxRound", n.syncBuffer.MaxRound(),
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
	n.syncBuffer.Clear()
	n.syncBuffer = nil

	logger.Info("sync complete", "round", n.dag.Round())

	return nil
}

// snapshotResult holds the result of applying a snapshot.
type snapshotResult struct {
	lastCommittedRound uint64
	validators         []*consensus.ValidatorInfo
	vertices           []consensus.VertexEntry
	trackerEntries     []consensus.ObjectTrackerEntry
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
	}

	logger.Info("snapshot applied",
		"round", result.lastCommittedRound,
		"objects", snapshot.ObjectsLength(),
		"validators", len(result.validators),
		"vertices", len(result.vertices),
		"trackerEntries", len(result.trackerEntries),
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

	if n.cfg.EpochLength > 0 {
		opts = append(opts, consensus.WithEpochLength(n.cfg.EpochLength))
	}

	if n.cfg.MaxChurnPerEpoch > 0 {
		opts = append(opts, consensus.WithMaxChurnPerEpoch(n.cfg.MaxChurnPerEpoch))
	}

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
	vertices := n.syncBuffer.GetAll()

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
		"bufferMinRound", n.syncBuffer.MinRound(),
		"bufferMaxRound", n.syncBuffer.MaxRound(),
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

// faucetConfig returns the faucet configuration for the API server.
func (n *Node) faucetConfig() *api.FaucetConfig {
	return &api.FaucetConfig{
		PrivKey:   n.cfg.PrivateKey,
		SystemPod: n.systemPod,
	}
}

// newHolderRouter creates a HolderRouter for routing GetObject requests.
// Returns nil if aggregation is not initialized.
func (n *Node) newHolderRouter() api.HolderRouter {
	if n.rendezvous == nil {
		return nil
	}

	return &holderRouter{
		rendezvous: n.rendezvous,
		dag:        n.dag,
		ownPubkey:  n.myPubkey(),
	}
}

// holderRouter routes GetObject requests to remote holders.
type holderRouter struct {
	rendezvous *aggregation.Rendezvous // rendezvous computes holder mappings
	dag        *consensus.DAG          // dag provides validator info
	ownPubkey  consensus.Hash          // ownPubkey is this node's public key
}

// RouteGetObject tries to fetch an object from its holders.
func (hr *holderRouter) RouteGetObject(id [32]byte) ([]byte, error) {
	// Use replication=10 as minimum probe size
	holders := hr.rendezvous.ComputeHolders(id, 10)

	for _, h := range holders {
		// Skip self
		if h == hr.ownPubkey {
			continue
		}

		info := hr.dag.ValidatorSet().Get(h)
		if info == nil || info.HTTPAddr == "" {
			continue
		}

		data, err := fetchObjectFromHolder(info.HTTPAddr, id)
		if err == nil {
			return data, nil
		}
	}

	return nil, fmt.Errorf("object not found on any holder")
}

// holderClient is an HTTP client with timeout for holder-to-holder requests.
// Without a timeout, routing cascades can hang the HTTP server indefinitely.
var holderClient = &http.Client{Timeout: 5 * time.Second}

// fetchObjectFromHolder fetches an object from a remote holder via HTTP.
func fetchObjectFromHolder(httpAddr string, id [32]byte) ([]byte, error) {
	url := "http://" + httpAddr + "/object/" + hex.EncodeToString(id[:])

	resp, err := holderClient.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}

	var objResp struct {
		ID          string `json:"id"`
		Version     uint64 `json:"version"`
		Owner       string `json:"owner"`
		Replication uint16 `json:"replication"`
		Content     string `json:"content"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&objResp); err != nil {
		return nil, err
	}

	return rebuildObjectFromJSON(objResp.ID, objResp.Owner, objResp.Content, objResp.Version, objResp.Replication)
}

// rebuildObjectFromJSON reconstructs a FlatBuffer Object from JSON API response fields.
func rebuildObjectFromJSON(idHex, ownerHex, contentHex string, version uint64, replication uint16) ([]byte, error) {
	idBytes, err := hex.DecodeString(idHex)
	if err != nil || len(idBytes) != 32 {
		return nil, fmt.Errorf("invalid id")
	}

	ownerBytes, err := hex.DecodeString(ownerHex)
	if err != nil || len(ownerBytes) != 32 {
		return nil, fmt.Errorf("invalid owner")
	}

	contentBytes, err := hex.DecodeString(contentHex)
	if err != nil {
		return nil, fmt.Errorf("invalid content")
	}

	builder := flatbuffers.NewBuilder(512)

	idVec := builder.CreateByteVector(idBytes)
	ownerVec := builder.CreateByteVector(ownerBytes)
	contentVec := builder.CreateByteVector(contentBytes)

	types.ObjectStart(builder)
	types.ObjectAddId(builder, idVec)
	types.ObjectAddVersion(builder, version)
	types.ObjectAddOwner(builder, ownerVec)
	types.ObjectAddReplication(builder, replication)
	types.ObjectAddContent(builder, contentVec)
	offset := types.ObjectEnd(builder)

	builder.Finish(offset)

	return builder.FinishedBytes(), nil
}

// scanObjectsForEpoch performs a background scan after epoch transitions.
// Identifies objects that need to be fetched or can be dropped.
func (n *Node) scanObjectsForEpoch() {
	myPubkey := n.myPubkey()
	isHolder := n.buildIsHolder(myPubkey)

	hasLocal := func(id [32]byte) bool {
		return n.state.GetObject(id) != nil
	}

	result := n.dag.ScanObjects(isHolder, hasLocal)

	// Fetch objects we should hold but don't have
	for _, id := range result.NeedFetch {
		go func(objectID [32]byte) {
			if hr := n.newHolderRouter(); hr != nil {
				data, err := hr.RouteGetObject(objectID)
				if err != nil {
					logger.Debug("epoch scan: fetch failed", "id_prefix", objectID[:4], "error", err)
					return
				}

				n.state.SetObject(data)
			}
		}(id)
	}

	// Objects to drop are handled lazily (not deleted immediately)
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
