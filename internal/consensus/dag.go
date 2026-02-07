package consensus

import (
	"crypto/ed25519"
	"encoding/hex"
	"sync"
	"sync/atomic"
	"time"

	"BluePods/internal/logger"
	"BluePods/internal/storage"
	"BluePods/internal/types"
)

const (
	// livenessTimeout is the fallback timeout to produce a vertex when idle.
	livenessTimeout = 500 * time.Millisecond

	// defaultGossipFanout is the number of peers to gossip vertices to.
	defaultGossipFanout = 40

	// transitionGraceRounds is how many rounds after minValidators is reached
	// before full quorum is required. During this period, vertices with at least
	// 1 valid parent are accepted to let the network converge.
	transitionGraceRounds = 20

	// transitionBufferRounds is extra rounds after grace where production is
	// throttled to the liveness loop (500ms). This prevents round-jumping
	// from vertex-triggered production and ensures sequential vertex creation.
	transitionBufferRounds = 10

)

// Executor executes committed transactions.
type Executor interface {
	Execute(tx []byte) error
}

// DAG manages the Mysticeti consensus.
type DAG struct {
	store       *store
	versions    *versionTracker
	validators  *ValidatorSet
	executor    Executor
	broadcaster Broadcaster
	systemPod   Hash
	epoch       uint64
	privKey     ed25519.PrivateKey
	pubKey      Hash

	// Round management
	round             atomic.Uint64
	lastProducedRound atomic.Uint64 // lastProducedRound tracks sequential production during transition
	roundMu           sync.Mutex
	pendingTxs        [][]byte

	// Output channel for committed transactions
	committed chan CommittedTx

	// Commit tracking
	commitMu      sync.Mutex
	lastCommitted uint64 // lastCommitted is the next round to commit

	// Pending vertices buffer for out-of-order arrival
	pendingMu       sync.Mutex
	pendingVertices map[Hash][]byte // hash -> vertex data waiting for parents

	// Mode flags
	listenerMode  bool // listenerMode disables vertex production
	isBootstrap   bool // isBootstrap allows producing even with few validators
	minValidators int  // minValidators is the threshold before non-bootstrap nodes produce

	// Sync mode: after sync, only reference trusted producers (from snapshot) until
	// we've produced our first vertex. This prevents referencing vertices from other
	// validators that the source node hasn't accepted yet.
	syncModeMu       sync.Mutex
	syncModeActive   bool          // syncModeActive is true after importing snapshot
	trustedProducers map[Hash]bool // trustedProducers are validators from the snapshot

	// Transition: when minValidators is reached, quorum check is relaxed for
	// transitionGraceRounds to let the network converge.
	transitionRound    atomic.Int64 // round when minValidators was reached (-1 = not yet)
	fullQuorumAchieved atomic.Bool  // fullQuorumAchieved is set when BFT quorum is first observed

	// Sharding: isHolder determines if this node stores/executes a given object.
	isHolder func(objectID [32]byte, replication uint16) bool

	// verifyATXProofs verifies BLS quorum proofs in an AttestedTransaction.
	verifyATXProofs func(*types.AttestedTransaction) error

	// Lifecycle
	stop chan struct{}
	wg   sync.WaitGroup
}

// Option configures the DAG during creation.
type Option func(*DAG)

// WithGenesisTxs sets transactions to include in the first vertex.
// These are injected into pendingTxs before the consensus loops start.
func WithGenesisTxs(txs [][]byte) Option {
	return func(d *DAG) {
		d.pendingTxs = append(d.pendingTxs, txs...)
	}
}

// WithLastCommittedRound sets the initial lastCommittedRound.
// Used when initializing from a snapshot. Sets lastCommitted to round+1
// because lastCommitted means "next round to commit".
func WithLastCommittedRound(round uint64) Option {
	return func(d *DAG) {
		d.lastCommitted = round + 1
	}
}

// WithListenerMode disables vertex production.
// The DAG will only process incoming vertices, not produce new ones.
func WithListenerMode() Option {
	return func(d *DAG) {
		d.listenerMode = true
	}
}

// WithBootstrap marks this node as the bootstrap validator.
// Bootstrap nodes always produce vertices regardless of minValidators.
func WithBootstrap() Option {
	return func(d *DAG) {
		d.isBootstrap = true
	}
}

// WithMinValidators sets the minimum validators before producing.
// Non-bootstrap nodes wait until this threshold is reached.
func WithMinValidators(n int) Option {
	return func(d *DAG) {
		d.minValidators = n
	}
}

// WithImportData imports vertices and object versions from a snapshot BEFORE
// the background goroutines start. This eliminates the race where commitLoop
// and livenessLoop operate on incomplete state during import.
// Also triggers transition if the validator set already meets minValidators,
// since the synced node missed the original transition event.
func WithImportData(vertices []VertexEntry, versions []ObjectVersionEntry) Option {
	return func(d *DAG) {
		d.importVerticesLocked(vertices)
		d.importVersionsLocked(versions)

		// If this node joins a network that already reached minValidators,
		// fire the transition so quorum relaxation kicks in.
		if d.minValidators > 0 && d.validators.Len() >= d.minValidators {
			d.enterTransition(d.lastCommitted)
		}
	}
}

// New creates a DAG with the given parameters.
// Options are applied before starting the background goroutines.
func New(db *storage.Storage, validators *ValidatorSet, broadcaster Broadcaster, systemPod Hash, epoch uint64, privKey ed25519.PrivateKey, executor Executor, opts ...Option) *DAG {
	var pubKey Hash
	copy(pubKey[:], privKey.Public().(ed25519.PublicKey))

	d := &DAG{
		store:           newStore(db),
		versions:        newVersionTracker(),
		validators:      validators,
		executor:        executor,
		broadcaster:     broadcaster,
		systemPod:       systemPod,
		epoch:           epoch,
		privKey:         privKey,
		pubKey:          pubKey,
		committed:       make(chan CommittedTx, channelBuffer),
		pendingVertices: make(map[Hash][]byte),
		stop:            make(chan struct{}),
	}
	d.transitionRound.Store(-1) // not yet in transition

	for _, opt := range opts {
		opt(d)
	}

	d.wg.Add(2)
	go d.commitLoop()
	go d.livenessLoop()

	return d
}

// AddVertex validates and adds a vertex received from the network.
// Returns true if the vertex was new and added, false if duplicate or invalid.
// Vertices with missing parents are buffered and retried when parents arrive.
func (d *DAG) AddVertex(data []byte) bool {
	vertex := types.GetRootAsVertex(data, 0)

	// Use the hash stored inside the vertex, not a hash of the entire buffer.
	// The hash field is computed over the unsigned vertex data.
	var hash Hash
	if hashBytes := vertex.HashBytes(); len(hashBytes) == 32 {
		copy(hash[:], hashBytes)
	} else {
		logger.Warn("vertex missing hash field")
		return false
	}

	// Check if already stored or pending
	if d.store.has(hash) {
		return false
	}

	err := d.validateVertex(vertex, data)
	if err != nil {
		// Buffer vertex if parents are missing - they may arrive later
		if d.isMissingParentError(err) {
			d.bufferPendingVertex(hash, data)
			return false
		}

		logger.Debug("vertex rejected", "round", vertex.Round(), "error", err)
		return false
	}

	producer := extractProducer(vertex)
	round := vertex.Round()

	if !d.store.add(data, hash, round, producer) {
		return false // already exists
	}

	d.onVertexAdded(round)

	// Try to process any pending vertices that might now have their parents
	d.processPendingVertices()

	return true
}

// SubmitTx adds a transaction to be included in the next vertex.
func (d *DAG) SubmitTx(tx []byte) {
	d.roundMu.Lock()
	d.pendingTxs = append(d.pendingTxs, tx)
	d.roundMu.Unlock()

	d.tryProduceVertex()
}

// Committed returns a channel of finalized transactions.
func (d *DAG) Committed() <-chan CommittedTx {
	return d.committed
}

// Round returns the current round number.
func (d *DAG) Round() uint64 {
	return d.round.Load()
}

// LastCommittedRound returns the last round whose transactions have been committed.
func (d *DAG) LastCommittedRound() uint64 {
	d.commitMu.Lock()
	defer d.commitMu.Unlock()
	return d.lastCommitted
}

// FullQuorumAchieved returns true if BFT quorum has been observed.
func (d *DAG) FullQuorumAchieved() bool {
	return d.fullQuorumAchieved.Load()
}

// ValidatorCount returns the number of active validators.
func (d *DAG) ValidatorCount() int {
	return d.validators.Len()
}

// Validators returns all validator pubkeys as byte slices.
// Each pubkey is 32 bytes.
func (d *DAG) Validators() [][]byte {
	hashes := d.validators.Validators()
	result := make([][]byte, len(hashes))

	for i, h := range hashes {
		pubkey := make([]byte, 32)
		copy(pubkey, h[:])
		result[i] = pubkey
	}

	return result
}

// ValidatorsInfo returns all validators with their network addresses.
func (d *DAG) ValidatorsInfo() []*ValidatorInfo {
	return d.validators.All()
}

// OnValidatorAdded sets a callback that is called when a new validator is added.
// This is triggered by register_validator transactions during commit.
func (d *DAG) OnValidatorAdded(fn func(*ValidatorInfo)) {
	d.validators.OnAdd(fn)
}

// AddValidator adds a validator to the local validator set.
// Used for self-registration to allow immediate vertex production.
// Also triggers transition if this addition reaches minValidators threshold.
func (d *DAG) AddValidator(pubkey Hash, httpAddr, quicAddr string, blsPubkey [48]byte) {
	d.validators.Add(pubkey, httpAddr, quicAddr, blsPubkey)

	// Fire transition when the threshold is reached (same logic as handleRegisterValidator).
	// For synced nodes, the self-add may be what pushes the count to minValidators.
	if d.minValidators > 0 && d.validators.Len() >= d.minValidators {
		d.enterTransition(d.round.Load())
	}
}

// SetIsHolder sets the holder check function for execution/storage sharding.
// Must be called after DAG creation, before transactions are committed.
func (d *DAG) SetIsHolder(fn func(objectID [32]byte, replication uint16) bool) {
	d.isHolder = fn
}

// SetATXProofVerifier sets the function used to verify BLS quorum proofs in ATXs.
// Must be called after DAG creation, before transactions are committed.
func (d *DAG) SetATXProofVerifier(fn func(*types.AttestedTransaction) error) {
	d.verifyATXProofs = fn
}

// ValidatorSet returns the underlying validator set.
// Used by holderRouter to look up validator network addresses.
func (d *DAG) ValidatorSet() *ValidatorSet {
	return d.validators
}

// Close stops the DAG and waits for goroutines to finish.
func (d *DAG) Close() {
	close(d.stop)
	d.wg.Wait()
	close(d.committed)
}

// ExportVertices returns vertices from the specified round range for snapshot.
func (d *DAG) ExportVertices(fromRound, toRound uint64) []VertexEntry {
	return d.store.ExportVertices(fromRound, toRound)
}

// ImportVertices loads vertices from snapshot and rebuilds index.
// Also advances the local round to the max imported round.
// Enables sync mode to only reference these producers until first vertex is produced.
func (d *DAG) ImportVertices(entries []VertexEntry) uint64 {
	maxRound := d.importVerticesLocked(entries)
	return maxRound
}

// importVerticesLocked imports vertices and enables sync mode.
// Safe to call before goroutines start (used by WithImportData).
func (d *DAG) importVerticesLocked(entries []VertexEntry) uint64 {
	if len(entries) == 0 {
		return 0
	}

	// Collect unique producers from imported vertices
	producers := make(map[Hash]bool)
	for _, entry := range entries {
		v := types.GetRootAsVertex(entry.Data, 0)
		producer := extractProducer(v)
		producers[producer] = true
	}

	maxRound := d.store.ImportVertices(entries)

	// Advance local round to match imported state
	d.updateRound(maxRound)

	// Enable sync mode with trusted producers from snapshot
	d.syncModeMu.Lock()
	d.syncModeActive = true
	d.trustedProducers = producers
	d.syncModeMu.Unlock()

	logger.Info("sync mode enabled", "trustedProducers", len(producers))

	return maxRound
}

// ExportVersions returns all object versions for snapshot.
func (d *DAG) ExportVersions() []ObjectVersionEntry {
	return d.versions.Export()
}

// ImportVersions loads object versions from snapshot.
func (d *DAG) ImportVersions(entries []ObjectVersionEntry) {
	d.importVersionsLocked(entries)
}

// importVersionsLocked imports object versions.
// Safe to call before goroutines start (used by WithImportData).
func (d *DAG) importVersionsLocked(entries []ObjectVersionEntry) {
	if len(entries) == 0 {
		return
	}

	d.versions.Import(entries)
	logger.Info("imported object versions", "count", len(entries))
}

// onVertexAdded is called after a vertex is stored.
// Production is throttled to the liveness loop (500ms) during the entire
// transition period: grace + buffer + convergence (until full quorum is observed).
// This prevents round-jumping and ensures vertices propagate via gossip.
func (d *DAG) onVertexAdded(round uint64) {
	d.updateRound(round)

	if d.shouldTriggerProduction() {
		d.tryProduceVertex()
	}
}

// shouldTriggerProduction returns true if vertex-triggered production is allowed.
// In multi-validator mode, always returns false to force production via the liveness
// loop (500ms). Without this, each received vertex triggers production which gossips
// and triggers more production on other nodes, creating a cascading runaway on
// low-latency networks.
func (d *DAG) shouldTriggerProduction() bool {
	if d.minValidators > 0 {
		return false
	}
	return true
}

// updateRound advances the local round if needed.
func (d *DAG) updateRound(round uint64) {
	for {
		current := d.round.Load()
		if round <= current {
			return
		}
		if d.round.CompareAndSwap(current, round) {
			return
		}
	}
}

// tryProduceVertex attempts to produce a vertex if conditions are met.
func (d *DAG) tryProduceVertex() {
	if d.listenerMode {
		return
	}

	// Non-bootstrap nodes wait until minValidators threshold is reached.
	// This prevents chain divergence during network initialization.
	if !d.isBootstrap && d.minValidators > 0 && d.validators.Len() < d.minValidators {
		logger.Debug("cannot produce: waiting for min validators",
			"current", d.validators.Len(),
			"required", d.minValidators)
		return
	}

	// Security: only registered validators can produce vertices
	if !d.validators.Contains(d.pubKey) {
		logger.Debug("cannot produce: not in validator set",
			"pubkey", hex.EncodeToString(d.pubKey[:8]),
			"validators", d.validators.Len())
		return
	}

	d.roundMu.Lock()
	defer d.roundMu.Unlock()

	round := d.nextProductionRound()

	if !d.canProduceVertex(round) {
		prevRound := round - 1
		logger.Debug("cannot produce: no quorum",
			"round", round,
			"needed", d.validators.QuorumSize(),
			"validatorsAtPrev", d.countValidatorVertices(prevRound),
			"totalAtPrev", d.store.countByRound(prevRound),
			"myValidators", d.validators.Len())
		if round%20 == 0 {
			d.debugRoundVertices(prevRound)
		}
		return
	}

	parents := d.collectParents(round)
	txs := d.takePendingTxs()

	data := d.buildVertex(round, parents, txs)

	// Validate our own vertex before accepting it
	if !d.addOwnVertex(data, round) {
		d.pendingTxs = append(txs, d.pendingTxs...)
		return
	}

	builtVertex := types.GetRootAsVertex(data, 0)
	var logHash Hash
	if hashBytes := builtVertex.HashBytes(); len(hashBytes) == 32 {
		copy(logHash[:], hashBytes)
	}
	logger.Info("produced vertex", "round", round, "txs", len(txs), "hash", hex.EncodeToString(logHash[:8]), "parents", len(parents))

	// Disable sync mode after first successful vertex production.
	d.disableSyncMode()

	d.sendVertex(data)
	d.lastProducedRound.Store(round)
	d.updateRound(round + 1)
}

// nextProductionRound returns the round to produce at.
// Always produces sequentially from the last produced round to prevent
// gaps when d.round jumps ahead from received vertices. This ensures
// the DAG has a complete parent chain at every round.
func (d *DAG) nextProductionRound() uint64 {
	round := d.round.Load()

	lp := d.lastProducedRound.Load()
	if lp > 0 {
		nextProd := lp + 1
		if nextProd < round {
			round = nextProd
		}
	}

	return round
}

// disableSyncMode turns off sync mode after producing first vertex.
// trustedProducers are kept for transition parent filtering.
func (d *DAG) disableSyncMode() {
	d.syncModeMu.Lock()
	defer d.syncModeMu.Unlock()

	if d.syncModeActive {
		d.syncModeActive = false
		// Keep trustedProducers - used by collectParents during transition
		logger.Info("sync mode disabled")
	}
}

// isInTransition returns true if we're in the grace period after minValidators was reached.
// During this period, quorum checks are relaxed to let the network converge.
func (d *DAG) isInTransition() bool {
	tr := d.transitionRound.Load()
	if tr < 0 {
		return false // minValidators not yet reached
	}

	return d.round.Load() < uint64(tr)+transitionGraceRounds
}

// isInTransitionOrBuffer returns true during transition OR the buffer period after.
// The buffer gives extra rounds for late transition vertices to propagate through gossip
// before parent existence validation is re-enabled.
// Uses the node's current round (for production-side decisions).
func (d *DAG) isInTransitionOrBuffer() bool {
	tr := d.transitionRound.Load()
	if tr < 0 {
		return false
	}

	return d.round.Load() < uint64(tr)+transitionGraceRounds+transitionBufferRounds
}

// isRoundInTransitionOrBuffer checks if quorum should be relaxed for a given round.
// Returns true during the fixed transition+buffer window, and also during the
// convergence period after the window until full BFT quorum is first observed.
// This avoids a hard boundary problem where the last transition round's vertices
// haven't propagated when full quorum is required.
func (d *DAG) isRoundInTransitionOrBuffer(round uint64) bool {
	tr := d.transitionRound.Load()
	if tr < 0 {
		return false
	}

	// Fixed window: always relax during grace+buffer
	if round < uint64(tr)+transitionGraceRounds+transitionBufferRounds {
		return true
	}

	// Extended convergence: keep relaxing until full quorum is observed.
	// Once full quorum is achieved, strict validation kicks in permanently.
	if !d.fullQuorumAchieved.Load() {
		return true
	}

	return false
}

// enterTransition records that minValidators has been reached at the given round.
// The round parameter is the commit round (deterministic) or the current round
// (for optimistic self-add). Only fires once via CAS.
func (d *DAG) enterTransition(atRound uint64) {
	if d.transitionRound.CompareAndSwap(-1, int64(atRound)) {
		logger.Info("transition started",
			"commitRound", atRound,
			"graceRounds", transitionGraceRounds,
			"bufferRounds", transitionBufferRounds,
		)
	}
}

// addOwnVertex validates and stores a locally produced vertex.
// Unlike AddVertex, it skips parent validation (we just collected them)
// and doesn't trigger recursive production attempts.
func (d *DAG) addOwnVertex(data []byte, round uint64) bool {
	vertex := types.GetRootAsVertex(data, 0)

	// Validate producer and signature (security checks)
	if err := d.validateProducer(vertex); err != nil {
		logger.Warn("own vertex rejected: invalid producer", "error", err)
		return false
	}

	if err := d.validateSignature(vertex); err != nil {
		logger.Warn("own vertex rejected: invalid signature", "error", err)
		return false
	}

	// Use the hash stored inside the vertex
	var hash Hash
	if hashBytes := vertex.HashBytes(); len(hashBytes) == 32 {
		copy(hash[:], hashBytes)
	} else {
		logger.Warn("own vertex missing hash field")
		return false
	}

	if !d.store.add(data, hash, round, d.pubKey) {
		return false // already exists
	}

	return true
}

// canProduceVertex checks if we have quorum to produce.
func (d *DAG) canProduceVertex(round uint64) bool {
	if round == 0 {
		return true
	}

	// During init, bootstrap produces alone until transition starts.
	// After transition fires (via commit path), full quorum is required.
	if d.isBootstrap && d.minValidators > 0 && d.transitionRound.Load() < 0 {
		return d.countValidatorVertices(round-1) >= 1
	}

	return d.hasQuorumFromRound(round - 1)
}

// hasQuorumFromRound checks if we have enough vertices from known validators.
// Uses the same transition logic as validateParentsQuorum to stay consistent.
func (d *DAG) hasQuorumFromRound(round uint64) bool {
	count := d.countValidatorVertices(round)
	required := d.validators.QuorumSize()

	// BFT quorum met: production can proceed
	if count >= required {
		// Mark convergence complete once BFT quorum is observed after the
		// fixed transition window. markFullQuorumAchieved gates on
		// !isInTransitionOrBuffer() internally, so this is safe to call
		// every time.
		d.markFullQuorumAchieved()
		return true
	}

	// Transition + convergence period: relax quorum to let the network converge.
	if d.isInTransitionOrBuffer() || d.isRoundInTransitionOrBuffer(round) {
		return count >= 1
	}

	logger.Debug("quorum check failed",
		"round", round,
		"validatorVertices", count,
		"requiredQuorum", required,
	)

	return false
}

// markFullQuorumAchieved records that BFT quorum has been observed after the
// fixed transition window. Once set, both production and commit paths require
// strict BFT quorum instead of quorum=1.
func (d *DAG) markFullQuorumAchieved() {
	if d.transitionRound.Load() >= 0 && !d.fullQuorumAchieved.Load() && !d.isInTransitionOrBuffer() {
		d.fullQuorumAchieved.Store(true)
		logger.Info("full quorum achieved, transition complete")
	}
}


// countValidatorVertices counts vertices at a round that are from known validators.
func (d *DAG) countValidatorVertices(round uint64) int {
	hashes := d.store.getByRound(round)
	count := 0

	for _, h := range hashes {
		v := d.store.get(h)
		if v == nil {
			continue
		}

		producer := extractProducer(v)
		if d.validators.Contains(producer) {
			count++
		}
	}

	return count
}

// debugRoundVertices logs detailed information about vertices at a specific round.
// Used for debugging quorum issues.
func (d *DAG) debugRoundVertices(round uint64) {
	hashes := d.store.getByRound(round)

	for i, h := range hashes {
		v := d.store.get(h)
		if v == nil {
			continue
		}

		producer := extractProducer(v)
		isKnown := d.validators.Contains(producer)

		logger.Debug("vertex at round",
			"round", round,
			"index", i,
			"producer", hex.EncodeToString(producer[:8]),
			"isKnownValidator", isKnown,
			"hash", hex.EncodeToString(h[:8]),
		)
	}
}

// collectParents gathers parent hashes from the previous round.
// During sync mode, only includes vertices from trusted producers (from snapshot)
// to avoid referencing vertices that other nodes may not have.
// During transition, references ALL parents to form cross-references immediately.
func (d *DAG) collectParents(round uint64) []Hash {
	if round == 0 {
		return nil
	}

	allParents := d.store.getByRound(round - 1)

	// Check if we need to filter parents (sync mode only)
	d.syncModeMu.Lock()
	syncActive := d.syncModeActive
	trusted := d.trustedProducers
	d.syncModeMu.Unlock()

	// Normal mode or transition: return all parents.
	// During transition, parent validation is skipped on receiving nodes,
	// so cross-references are safe and help the network converge faster.
	if !syncActive {
		return allParents
	}

	// Sync mode: only reference trusted producers from snapshot.
	// This is the first vertex after importing a snapshot.
	filtered := make([]Hash, 0, len(allParents))
	for _, h := range allParents {
		v := d.store.get(h)
		if v == nil {
			continue
		}

		producer := extractProducer(v)
		if trusted != nil && trusted[producer] {
			filtered = append(filtered, h)
		}
	}

	return filtered
}

// takePendingTxs takes and clears pending transactions.
func (d *DAG) takePendingTxs() [][]byte {
	txs := d.pendingTxs
	d.pendingTxs = nil
	return txs
}

// sendVertex broadcasts a vertex to the network.
func (d *DAG) sendVertex(data []byte) {
	if d.broadcaster != nil {
		_ = d.broadcaster.Gossip(data, defaultGossipFanout)
	}
}

// isMissingParentError checks if the error is due to missing parents.
// These vertices should be buffered and retried when parents arrive.
func (d *DAG) isMissingParentError(err error) bool {
	if err == nil {
		return false
	}

	errStr := err.Error()
	return len(errStr) > 16 && errStr[:16] == "parent not found"
}

// bufferPendingVertex adds a vertex to the pending buffer.
// Vertices are deduplicated by hash.
func (d *DAG) bufferPendingVertex(hash Hash, data []byte) {
	d.pendingMu.Lock()
	defer d.pendingMu.Unlock()

	if _, exists := d.pendingVertices[hash]; exists {
		return
	}

	// Make a copy of data to avoid holding references
	dataCopy := make([]byte, len(data))
	copy(dataCopy, data)
	d.pendingVertices[hash] = dataCopy

	vertex := types.GetRootAsVertex(data, 0)
	producer := extractProducer(vertex)
	logger.Debug("buffered pending vertex",
		"round", vertex.Round(),
		"producer", hex.EncodeToString(producer[:8]),
		"pending", len(d.pendingVertices),
	)
}

// processPendingVertices retries vertices whose parents may now be available.
// Processes in a loop until no more progress is made.
func (d *DAG) processPendingVertices() {
	for {
		processed := d.tryProcessPending()
		if processed == 0 {
			return
		}
	}
}

// tryProcessPending attempts to process all pending vertices once.
// Returns the number of vertices successfully processed.
func (d *DAG) tryProcessPending() int {
	d.pendingMu.Lock()
	pending := make(map[Hash][]byte, len(d.pendingVertices))
	for h, data := range d.pendingVertices {
		pending[h] = data
	}
	d.pendingMu.Unlock()

	processed := 0

	for hash, data := range pending {
		vertex := types.GetRootAsVertex(data, 0)

		err := d.validateVertex(vertex, data)
		if err != nil {
			// Still has missing parents, keep in buffer
			if d.isMissingParentError(err) {
				continue
			}

			// Other validation error, remove from buffer
			d.removePendingVertex(hash)
			logger.Debug("pending vertex rejected", "round", vertex.Round(), "error", err)
			continue
		}

		// Validation passed, add to store
		producer := extractProducer(vertex)
		round := vertex.Round()

		if d.store.add(data, hash, round, producer) {
			d.onVertexAdded(round)
			processed++
			logger.Debug("processed pending vertex", "round", round)
		}

		d.removePendingVertex(hash)
	}

	return processed
}

// PurgePendingBeforeRound removes pending vertices whose round is before minRound.
// These vertices have parents that predate the snapshot and will never resolve.
// Returns the number of purged vertices.
func (d *DAG) PurgePendingBeforeRound(minRound uint64) int {
	d.pendingMu.Lock()
	defer d.pendingMu.Unlock()

	purged := 0
	for hash, data := range d.pendingVertices {
		v := types.GetRootAsVertex(data, 0)
		if v.Round() < minRound {
			delete(d.pendingVertices, hash)
			purged++
		}
	}

	if purged > 0 {
		logger.Info("purged orphan pending vertices",
			"purged", purged,
			"remaining", len(d.pendingVertices),
			"minRound", minRound,
		)
	}

	return purged
}

// removePendingVertex removes a vertex from the pending buffer.
func (d *DAG) removePendingVertex(hash Hash) {
	d.pendingMu.Lock()
	defer d.pendingMu.Unlock()

	delete(d.pendingVertices, hash)
}

// livenessLoop ensures progress even when idle by periodically trying to produce.
// Also periodically retries pending vertices in case parents arrived.
func (d *DAG) livenessLoop() {
	defer d.wg.Done()

	ticker := time.NewTicker(livenessTimeout)
	defer ticker.Stop()

	for {
		select {
		case <-d.stop:
			return
		case <-ticker.C:
			d.processPendingVertices()
			d.tryProduceVertex()
		}
	}
}
