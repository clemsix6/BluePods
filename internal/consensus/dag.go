package consensus

import (
	"crypto/ed25519"
	"encoding/hex"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"BluePods/internal/genesis"
	"BluePods/internal/logger"
	"BluePods/internal/storage"
	"BluePods/internal/types"
)

const (
	// livenessTimeout is the fallback timeout to produce a vertex when idle.
	livenessTimeout = 500 * time.Millisecond

	// defaultGossipFanout is the number of peers to gossip vertices to.
	defaultGossipFanout = 40

	// defaultCommissionBPS is the fixed delegation commission when unset (10%).
	defaultCommissionBPS = 1000

	// defaultVotingCapMille is the per-validator voting cap when unset, in
	// per-mille of total stake (100 = 10%). The equal-share floor keeps a small
	// set's 2/3 quorum reachable even at this cap.
	defaultVotingCapMille = 100

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
	tracker     *objectTracker
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

	// Missing-ancestor recovery: when a decided anchor's causal batch is blocked on
	// an absent ancestor for two consecutive commit ticks, the missing ancestry is
	// requested from mesh peers through vertexFetcher. All three fields are guarded by
	// commitMu (the whole commit path holds it).
	vertexFetcher VertexFetcher // vertexFetcher requests missing ancestry from peers (nil = recovery disabled)
	stallAnchor   Hash          // stallAnchor is the anchor the commit cursor is currently blocked on
	stallTicks    int           // stallTicks counts consecutive commit ticks stalled on stallAnchor
	waitRound     uint64        // waitRound is the round the commit cursor is currently WAITing on
	waitTicks     int           // waitTicks counts consecutive commit ticks the cursor has WAITed on waitRound

	// Pending vertices buffer for out-of-order arrival
	pendingMu       sync.Mutex
	pendingVertices map[Hash][]byte // hash -> vertex data waiting for parents

	// Mode flags
	listenerMode   bool   // listenerMode disables vertex production
	isBootstrap    bool   // isBootstrap allows producing even with few validators
	minValidators  int    // minValidators is the threshold before non-bootstrap nodes produce
	minStake       uint64 // minStake is the minimum self-stake a bond must leave (0 = no minimum)
	commissionBPS  uint64 // commissionBPS is the fixed, governed delegation commission in basis points (1000 = 10%)
	votingCapMille uint64 // votingCapMille is the per-validator voting cap in per-mille of total stake (100 = 10%)
	gossipFanout   int    // gossipFanout is the number of peers to send each vertex to
	graceRounds    int    // graceRounds is the transition grace period (0 = use default 20)
	bufferRounds   int    // bufferRounds is the transition buffer period (0 = use default 10)

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

	// Regime latch (regime.go): the strict-regime boundary, a pure function of
	// committed history. Round R runs the relaxed bootstrap certificate iff the
	// latch is unset or R < strictStartRound. Guarded by commitMu.
	strictLatched    bool          // strictLatched reports whether the strict latch has fired
	strictStartRound uint64        // strictStartRound is the first strict-regime round (valid iff strictLatched)
	committedMembers map[Hash]bool // committedMembers are validators admitted by COMMITTED registrations or genesis, never by an optimistic self-add
	regimeDirty      bool          // regimeDirty flags that the latch or genesis snapshot changed and must be persisted with the cursor

	// Anchor-designation eligibility (eligible.go): the live produced set and the
	// eligible sets frozen with the holder snapshots. Guarded by commitMu.
	producedMembers     map[Hash]bool // producedMembers are members with at least one vertex in committed history
	producedDirty       bool          // producedDirty flags that the produced set changed and must be persisted with the cursor
	eligibleHolders     map[Hash]bool // eligibleHolders is the designation-eligible subset frozen with epochHolders
	prevEligibleHolders map[Hash]bool // prevEligibleHolders is the eligible subset frozen with prevEpochHolders
	nextEligibleHolders map[Hash]bool // nextEligibleHolders is the eligible subset frozen with nextEpochHolders

	// Epoch: frozen validator set for Rendezvous hashing.
	epochLength       uint64             // epochLength is the number of rounds per epoch (0 = disabled)
	currentEpoch      uint64             // currentEpoch is the current epoch number
	epochHolders      *ValidatorSet      // epochHolders is the frozen ValidatorSet for Rendezvous (current epoch)
	prevEpochHolders  *ValidatorSet      // prevEpochHolders is the previous epoch's snapshot, kept for the grace window
	nextEpochHolders  *ValidatorSet      // nextEpochHolders is a one-epoch-ahead snapshot so the anchor rule's forward scan can cross an epoch boundary without wedging
	pendingRemovals   map[Hash]bool      // pendingRemovals are validators to remove at next epoch
	epochAdditions    []Hash             // epochAdditions are validators added this epoch
	maxChurnPerEpoch  int                // maxChurnPerEpoch caps changes per epoch (0 = unlimited)
	onEpochTransition func(epoch uint64) // onEpochTransition is called when an epoch boundary is reached

	// Sharding: isHolder determines if this node stores/executes a given object.
	isHolder func(objectID [32]byte, replication uint16) bool

	// verifyATXProofs verifies BLS quorum proofs in a single AttestedTransaction.
	// It receives the commit round so it can select the correct holder snapshot.
	// Used as the inline fallback when no batch verifier is set (such as direct
	// unit tests); the round commit loop uses verifyATXProofsBatch instead.
	verifyATXProofs func(atx *types.AttestedTransaction, commitRound uint64) error

	// verifyATXProofsBatch verifies the BLS quorum proofs of a committed round's
	// ATXs in one parallel pass, returning one error per ATX in input order. When
	// set, the round commit loop uses it so the (pure, deterministic) signature
	// checks run across cores before the sequential apply.
	verifyATXProofsBatch func(atxs []*types.AttestedTransaction, commitRound uint64) []error

	// verifyTxAuth re-verifies an inner transaction's sender signature and hash in
	// the commit path, deterministically on every node. New defaults it to the real
	// verifyTxAuthenticity, so on a live node it is fail-closed by construction and
	// can never be nil: a forged transaction reaching commit via gossip cannot
	// commit unverified. Only unit tests that drive executeTx with deliberately
	// synthetic (unsigned) transactions override it, to isolate downstream logic.
	verifyTxAuth func(tx *types.Transaction) error

	// Fee system: protocol-level fee deduction and credits.
	coinStore      CoinStore            // coinStore provides access to coin objects for fee operations
	feeParams      *FeeParams           // feeParams holds fee constants (nil = fees disabled)
	computeHolders HolderFunc           // computeHolders computes holders for replication ratio
	delegations    DelegationEnumerator // delegations enumerates a validator's stake positions for the reward split

	// Epoch rewards: accumulated fees and round tracking per validator.
	epochFees           uint64          // epochFees accumulates total_epoch from all committed vertices this epoch
	epochRoundsProduced map[Hash]uint64 // epochRoundsProduced counts vertices produced per validator this epoch

	// Thermostat: per-epoch adaptive issuance. When thermostat is the zero value
	// (WithThermostat unset) every parameter is 0, so adjustRate holds the rate at
	// 0 and issuanceFor returns 0: the node mints nothing. The live node keeps the
	// thermostat off pending parameter calibration/governance, to preserve the
	// supply invariant.
	thermostat        thermostatParams // thermostat holds the issuance control loop parameters (zero = off)
	issuanceRateMicro uint64           // issuanceRateMicro is the current per-epoch issuance rate in millionths

	// Lifecycle
	stop chan struct{}
	wg   sync.WaitGroup
}

// Option configures the DAG during creation.
type Option func(*DAG)

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

// WithMinStake sets the minimum self-stake a bond must leave a validator with.
// A bond that would leave self-stake below this value is rejected. 0 disables it.
func WithMinStake(stake uint64) Option {
	return func(d *DAG) {
		d.minStake = stake
	}
}

// WithCommissionBPS sets the fixed delegation commission in basis points. This
// is a single governed parameter for the whole network (not per-validator), so
// it avoids a commission field, rate-limited change mechanics, and rug-pull risk.
// Default is 1000 (10%).
func WithCommissionBPS(bps uint64) Option {
	return func(d *DAG) {
		d.commissionBPS = bps
	}
}

// WithVotingCapMille sets the per-validator voting cap in per-mille of total
// stake (100 = 10%). Capping voting power keeps the global order decentralized
// even when delegation concentrates stake; reward weight is uncapped. The cap is
// loose while the set is small (the equal-share floor keeps a 2/3 quorum
// reachable) and tightened toward ~10% as the set grows. Default is 100.
func WithVotingCapMille(capMille uint64) Option {
	return func(d *DAG) {
		d.votingCapMille = capMille
	}
}

// WithThermostat enables the adaptive issuance thermostat with the default
// parameters and seeds the per-epoch rate to the genesis rate. It is opt-in: a
// DAG built without it leaves thermostat zero-valued, so the rate holds at 0 and
// the node mints no issuance. Enable it only together with reward crediting, so
// minted supply is always backed by a credit and the supply invariant holds.
func WithThermostat(p thermostatParams) Option {
	return func(d *DAG) {
		d.thermostat = p
		d.issuanceRateMicro = p.GenesisRateMicro
	}
}

// WithIssuanceRate seeds the per-epoch issuance rate (in millionths), overriding
// the genesis default. Used on the sync path to restore the persisted rate at DAG
// construction: the rate lives only on the DAG (not in DB-backed state), and the
// DAG does not exist when the snapshot is applied, so it must ride a construction
// Option rather than a post-sync setter (which would nil-panic).
func WithIssuanceRate(rateMicro uint64) Option {
	return func(d *DAG) {
		d.issuanceRateMicro = rateMicro
	}
}

// WithGossipFanout sets the number of peers to send each vertex to.
// Higher values increase reliability at the cost of more bandwidth.
// Default is 40 (defaultGossipFanout).
func WithGossipFanout(n int) Option {
	return func(d *DAG) {
		d.gossipFanout = n
	}
}

// WithTransitionGrace sets the number of grace rounds after minValidators is reached.
// During grace, quorum is relaxed to let the network converge.
// 0 means use the default (20 rounds).
func WithTransitionGrace(rounds int) Option {
	return func(d *DAG) {
		d.graceRounds = rounds
	}
}

// WithTransitionBuffer sets the extra buffer rounds after the grace period.
// 0 means use the default (10 rounds).
func WithTransitionBuffer(rounds int) Option {
	return func(d *DAG) {
		d.bufferRounds = rounds
	}
}

// WithEpochLength sets the number of rounds per epoch.
// Object holding uses a frozen ValidatorSet snapshotted at epoch boundaries.
// 0 means epochs are disabled.
func WithEpochLength(length uint64) Option {
	return func(d *DAG) {
		d.epochLength = length
	}
}

// WithMaxChurnPerEpoch sets the maximum validator changes per epoch.
// 0 means unlimited churn.
func WithMaxChurnPerEpoch(n int) Option {
	return func(d *DAG) {
		d.maxChurnPerEpoch = n
	}
}

// WithImportData imports vertices and tracker entries from a snapshot BEFORE
// the background goroutines start. This eliminates the race where commitLoop
// and livenessLoop operate on incomplete state during import.
// Also triggers transition if the validator set already meets minValidators,
// since the synced node missed the original transition event.
func WithImportData(vertices []VertexEntry, trackerEntries []ObjectTrackerEntry) Option {
	return func(d *DAG) {
		d.importVerticesLocked(vertices)
		d.importTrackerEntriesLocked(trackerEntries)

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
		store:               newStore(db),
		tracker:             newObjectTracker(db),
		validators:          validators,
		executor:            executor,
		broadcaster:         broadcaster,
		systemPod:           systemPod,
		epoch:               epoch,
		privKey:             privKey,
		pubKey:              pubKey,
		committed:           make(chan CommittedTx, channelBuffer),
		pendingVertices:     make(map[Hash][]byte),
		pendingRemovals:     make(map[Hash]bool),
		epochRoundsProduced: make(map[Hash]uint64),
		commissionBPS:       defaultCommissionBPS,
		votingCapMille:      defaultVotingCapMille,
		verifyTxAuth:        verifyTxAuthenticity,
		stop:                make(chan struct{}),
	}
	d.transitionRound.Store(-1) // not yet in transition

	// Resume the commit cursor from persisted state so a restart never re-derives an
	// already decided round. A snapshot option (WithLastCommittedRound) applies below
	// and takes precedence for a freshly synced node.
	if cursor, ok := d.store.loadCommitCursor(); ok {
		d.lastCommitted = cursor
	}

	// Restore currentEpoch and the holder snapshots BEFORE any commit decision (the
	// commitLoop below is the first caller of anchorStatus/HoldersForEpoch), so a
	// restart past the first epoch boundary resolves anchors and quorums against the
	// same holder set it decided under instead of wedging or falling back to the
	// genesis live set. A fresh (never-restarted) node has no persisted epoch state,
	// so this is a no-op; the sync options below install their own state as before.
	d.restoreEpochState()

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
		// Buffer vertex if parents are missing or producer is unknown.
		// Missing parents arrive later via gossip.
		// Unknown producers become known when their register_validator tx commits.
		if d.isMissingParentError(err) || d.isUnknownProducerError(err) {
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

// TotalSupply returns the protocol-maintained total supply from the coin store.
// Returns 0 when no coin store is wired (the fee system is disabled).
func (d *DAG) TotalSupply() uint64 {
	if d.coinStore == nil {
		return 0
	}
	return d.coinStore.TotalSupply()
}

// CoinsTotal returns the protocol-maintained sum of coin balances from the coin
// store. Returns 0 when no coin store is wired (the fee system is disabled).
func (d *DAG) CoinsTotal() uint64 {
	if d.coinStore == nil {
		return 0
	}
	return d.coinStore.CoinsTotal()
}

// FullQuorumAchieved returns true if BFT quorum has been observed.
func (d *DAG) FullQuorumAchieved() bool {
	return d.fullQuorumAchieved.Load()
}

// IssuanceRateMicro returns the thermostat's current per-epoch issuance rate in
// millionths. It is 0 when the thermostat is off. Persisted in snapshots because
// the control loop steps from the previous value and cannot be re-derived.
func (d *DAG) IssuanceRateMicro() uint64 {
	return d.issuanceRateMicro
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
func (d *DAG) AddValidator(pubkey Hash, quicAddr string, blsPubkey [48]byte) {
	d.validators.Add(pubkey, quicAddr, blsPubkey)

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

// VertexFetcher requests vertices that are missing locally from mesh peers. The
// commit loop calls it when a decided anchor's causal history is not fully present.
// Implementations MUST be asynchronous and non-blocking: FetchVertices is invoked
// under the commit lock and must never perform network I/O inline.
type VertexFetcher interface {
	// FetchVertices requests the given vertex hashes from peers. It must return
	// immediately; a fetched vertex re-enters through AddVertex and is picked up by
	// the commit loop on a later tick.
	FetchVertices(hashes []Hash)
}

// SetVertexFetcher installs the fetcher the commit loop uses to recover a decided
// anchor's missing ancestry. It is nil-safe: with no fetcher set the recovery
// trigger is a no-op and the node relies on gossip and the pending buffer alone.
// EVERY node construction path must call this — a fetcher left unset ships
// missing-ancestor recovery as dead code, and a joiner whose ancestry is never
// gossiped stalls. Called at construction, before heavy commit activity.
func (d *DAG) SetVertexFetcher(f VertexFetcher) {
	d.commitMu.Lock()
	defer d.commitMu.Unlock()

	d.vertexFetcher = f
}

// VertexFetcherWired reports whether a vertex fetcher has been installed. It lets
// each node construction path be tested to actually wire missing-ancestor recovery,
// guarding the exact regression that once shipped it as dead code.
func (d *DAG) VertexFetcherWired() bool {
	d.commitMu.Lock()
	defer d.commitMu.Unlock()

	return d.vertexFetcher != nil
}

// VertexBytes returns the raw serialized bytes of a locally-held vertex by hash, or
// nil when it is absent. It backs the mesh vertex-fetch handler, which serves the
// single requested vertex and enumerates nothing else.
func (d *DAG) VertexBytes(hash Hash) []byte {
	return d.store.getRaw(hash)
}

// TrackObject registers a created object in the tracker.
// Called by the state layer when a new object is created during execution.
func (d *DAG) TrackObject(id [32]byte, version uint64, replication uint16, fees uint64) {
	var h Hash
	copy(h[:], id[:])
	d.tracker.trackObject(h, version, replication, fees)
}

// SetATXProofVerifier sets the inline single-ATX BLS proof verifier. The
// verifier receives the commit round so it can pick the holder snapshot of the
// epoch the attestations belong to. The round commit loop prefers the batch
// verifier; this remains the fallback when no batch verifier is set. Must be
// called after DAG creation, before transactions are committed.
func (d *DAG) SetATXProofVerifier(fn func(atx *types.AttestedTransaction, commitRound uint64) error) {
	d.verifyATXProofs = fn
}

// SetATXProofBatchVerifier sets the function the round commit loop uses to verify
// a round's BLS quorum proofs in one parallel pass. It returns one error per ATX
// in input order (nil when that ATX's proofs are valid). Each verdict must equal
// what the single verifier would return for the same ATX, so the set of accepted
// ATXs and their apply order are unchanged. Must be called after DAG creation,
// before transactions are committed.
func (d *DAG) SetATXProofBatchVerifier(fn func(atxs []*types.AttestedTransaction, commitRound uint64) []error) {
	d.verifyATXProofsBatch = fn
}

// SetFeeSystem configures protocol-level fee deduction.
// When feeParams is non-nil, fees are deducted from gas_coin before execution.
func (d *DAG) SetFeeSystem(store CoinStore, params *FeeParams, holders HolderFunc) {
	d.coinStore = store
	d.feeParams = params
	d.computeHolders = holders

	// The same store (*state.State in production) also enumerates delegation
	// positions for the reward split. Stored via assertion so the call site
	// signature is unchanged; nil when the store does not implement it.
	if de, ok := store.(DelegationEnumerator); ok {
		d.delegations = de
	}
}

// SeedGenesis seeds the initial ledger state directly: the genesis coin object
// and supply counters (SeedGenesisLedger) plus the founding validator with its
// bonded self-stake and reward coin (SeedGenesisValidator). Genesis is state,
// not transactions. Must be called AFTER SetFeeSystem (so coinStore is wired)
// and before the node produces.
//
// A bootstrap node restarting over its own data directory must NOT call this:
// re-running the ledger seed would overwrite the reserve coin and supply
// counters back to their genesis values, discarding everything committed
// since. It calls SeedGenesisValidator alone instead — see that method's
// docstring.
func (d *DAG) SeedGenesis(is genesis.InitialState) {
	d.SeedGenesisLedger(is)
	d.SeedGenesisValidator(is)
}

// SeedGenesisLedger seeds the genesis coin object into the coin store and the
// supply/coins_total counters. It must run exactly once per chain: a caller
// detecting a restart (the genesis coin already exists) must skip it.
func (d *DAG) SeedGenesisLedger(is genesis.InitialState) {
	d.coinStore.SetObject(is.Coin)
	d.coinStore.SetTotalSupply(is.Supply)

	// coins_total starts at the seeded coin's balance, NOT is.Supply: the
	// founder's self-stake is locked OUT of the coin by BuildInitialState, so it
	// is bonded (counted as total_bonded), not sitting in a coin balance.
	d.coinStore.SetCoinsTotal(is.Supply - is.SelfStake)
}

// SeedGenesisValidator seeds the founding validator into the validator set:
// its network address, its bonded self-stake, and its reward coin (its own
// genesis coin — without a designation the founder's liquid epoch reward share
// has nowhere to land and silently vanishes at every boundary with a non-zero
// pool). It then admits the founder to the committed member set and freezes
// the genesis holder snapshot so the anchor path resolves epoch 0 without ever
// reading the live set.
//
// Every step here is idempotent (Add/SetSelfStake/SetRewardCoin overwrite,
// recordCommittedMember is a set-add), so unlike SeedGenesisLedger this MUST
// run on every bootstrap start, restart included: nothing else restores the
// live validator set in bootstrap mode, and skipping it on restart would leave
// the founder with zero live self-stake and a broken total_bonded.
func (d *DAG) SeedGenesisValidator(is genesis.InitialState) {
	var bls [48]byte
	copy(bls[:], is.BLS)

	d.validators.Add(is.Pubkey, is.QUIC, bls) // founder is already in the set; Add back-fills addresses
	d.validators.SetSelfStake(is.Pubkey, is.SelfStake)
	d.validators.SetRewardCoin(is.Pubkey, is.CoinID)

	// Under commitMu because the commit loop is already running.
	d.commitMu.Lock()
	d.recordCommittedMember(is.Pubkey, d.lastCommitted)
	d.commitMu.Unlock()
}

// ValidatorSet returns the underlying validator set.
// Used by holderRouter to look up validator network addresses.
func (d *DAG) ValidatorSet() *ValidatorSet {
	return d.validators
}

// EpochHolders returns the frozen validator set used for Rendezvous.
// Returns the current validators if epochs are not initialized yet.
func (d *DAG) EpochHolders() *ValidatorSet {
	if d.epochHolders != nil {
		return d.epochHolders
	}
	return d.validators
}

// Epoch returns the current epoch number.
func (d *DAG) Epoch() uint64 {
	return d.currentEpoch
}

// EpochLength returns the number of rounds per epoch (0 when epochs are disabled).
func (d *DAG) EpochLength() uint64 {
	return d.epochLength
}

// EpochHoldersCount returns the number of validators in the frozen epoch set.
// Falls back to the active validator count if epochs are not initialized.
func (d *DAG) EpochHoldersCount() int {
	if d.epochHolders != nil {
		return d.epochHolders.Len()
	}

	return d.validators.Len()
}

// OnEpochTransition sets a callback that fires on epoch boundaries.
// Used to trigger Rendezvous rebuilding and background scanning.
func (d *DAG) OnEpochTransition(fn func(epoch uint64)) {
	d.onEpochTransition = fn
}

// AddPendingRemoval marks a validator for removal at the next epoch boundary.
func (d *DAG) AddPendingRemoval(pubkey Hash) {
	d.pendingRemovals[pubkey] = true
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

// ExportTrackerEntries returns all tracked objects for snapshot.
func (d *DAG) ExportTrackerEntries() []ObjectTrackerEntry {
	return d.tracker.Export()
}

// ImportTrackerEntries loads tracker entries from snapshot.
func (d *DAG) ImportTrackerEntries(entries []ObjectTrackerEntry) {
	d.importTrackerEntriesLocked(entries)
}

// importTrackerEntriesLocked imports tracker entries.
// Safe to call before goroutines start (used by WithImportData).
func (d *DAG) importTrackerEntriesLocked(entries []ObjectTrackerEntry) {
	if len(entries) == 0 {
		return
	}

	d.tracker.Import(entries)
	logger.Info("imported tracker entries", "count", len(entries))
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

	// Security and liveness gate: a node produces once it is itself a registered
	// validator (its self-add or a committed registration put it in the set) — never
	// waiting for the full minValidators set. Under the deterministic anchor rule a
	// committed validator is the designated anchor producer for a share of rounds; if a
	// registered validator withheld production until it saw the whole set, its
	// designated relaxed rounds could never certify and the commit cursor would wedge
	// before the set ever reached minValidators (a bootstrap deadlock). The committed
	// log stays deterministic regardless of who produces, so early production cannot
	// fork it.
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

// effectiveGrace returns the configured grace rounds or the default.
func (d *DAG) effectiveGrace() uint64 {
	if d.graceRounds > 0 {
		return uint64(d.graceRounds)
	}
	return transitionGraceRounds
}

// effectiveBuffer returns the configured buffer rounds or the default.
func (d *DAG) effectiveBuffer() uint64 {
	if d.bufferRounds > 0 {
		return uint64(d.bufferRounds)
	}
	return transitionBufferRounds
}

// isInTransition returns true if we're in the grace period after minValidators was reached.
// During this period, quorum checks are relaxed to let the network converge.
func (d *DAG) isInTransition() bool {
	tr := d.transitionRound.Load()
	if tr < 0 {
		return false // minValidators not yet reached
	}

	return d.round.Load() < uint64(tr)+d.effectiveGrace()
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

	return d.round.Load() < uint64(tr)+d.effectiveGrace()+d.effectiveBuffer()
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
	if round < uint64(tr)+d.effectiveGrace()+d.effectiveBuffer() {
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
			"graceRounds", d.effectiveGrace(),
			"bufferRounds", d.effectiveBuffer(),
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

// hasQuorumFromRound checks whether the given round's known-validator producers
// carry a 2/3 capped-stake majority. The stake is read from the SAME holder
// snapshot the committer uses (HoldersForEpoch(commitEpochForRound(round))), so
// production and commit never weigh against different stake sets across an epoch
// boundary. During the transition/convergence window the check is relaxed to a
// single producer so the network can converge before stake weighting is
// authoritative. ValidatorSet.QuorumSize is no longer the commit/production
// authority; it remains only for relaxed-count logging.
func (d *DAG) hasQuorumFromRound(round uint64) bool {
	// Transition + convergence period: relax quorum to let the network converge.
	if d.isInTransitionOrBuffer() || d.isRoundInTransitionOrBuffer(round) {
		return d.countValidatorVertices(round) >= 1
	}

	producers := d.knownProducersAt(d.store.getByRound(round))
	if len(producers) == 0 {
		return false
	}

	// Production-local quorum check: unlike the anchor path this may weigh against
	// the live set when no snapshot resolves, because it only gates whether THIS node
	// produces a vertex — it never decides the committed log, so it cannot fork it.
	set, ok := d.HoldersForEpoch(d.commitEpochForRound(round))
	if !ok {
		set = d.validators
	}

	cappedSum, cappedTotal := d.cappedStakeOf(set, producers)
	if quorumReached(cappedSum, cappedTotal) {
		return true
	}

	logger.Debug("quorum check failed",
		"round", round,
		"validatorProducers", len(producers),
		"cappedSum", cappedSum,
		"cappedTotal", cappedTotal,
	)

	return false
}

// markFullQuorumAchieved records that BFT quorum has been observed in the commit
// path after the fixed transition window. This is called from checkCommits when
// a round is committed with full BFT quorum. Using commit-side (not production-side)
// observation prevents early-joining nodes from prematurely ending convergence.
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
		fanout := d.gossipFanout
		if fanout == 0 {
			fanout = defaultGossipFanout
		}
		_ = d.broadcaster.Gossip(data, fanout)
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

// isUnknownProducerError checks if the error is due to an unknown producer.
// These vertices should be buffered and retried when the producer registers.
func (d *DAG) isUnknownProducerError(err error) bool {
	if err == nil {
		return false
	}

	errStr := err.Error()
	return len(errStr) > 17 && errStr[:17] == "unknown producer:"
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

// pendingEntry holds a pending vertex with its hash for sorted processing.
type pendingEntry struct {
	hash  Hash   // hash is the vertex hash
	data  []byte // data is the serialized vertex
	round uint64 // round is the vertex round (for sorting)
}

// tryProcessPending attempts to process all pending vertices once.
// Vertices are sorted by round so parents are processed before children.
// Returns the number of vertices successfully processed.
func (d *DAG) tryProcessPending() int {
	sorted := d.snapshotPendingSorted()
	if len(sorted) == 0 {
		return 0
	}

	processed := 0

	for _, entry := range sorted {
		vertex := types.GetRootAsVertex(entry.data, 0)

		err := d.validateVertex(vertex, entry.data)
		if err != nil {
			// Keep in buffer if parents are missing or producer is unknown.
			// These conditions resolve when parents arrive or producer registers.
			if d.isMissingParentError(err) || d.isUnknownProducerError(err) {
				continue
			}

			// Other validation error, remove from buffer
			d.removePendingVertex(entry.hash)
			logger.Debug("pending vertex rejected", "round", vertex.Round(), "error", err)
			continue
		}

		// Validation passed, add to store
		producer := extractProducer(vertex)

		if d.store.add(entry.data, entry.hash, entry.round, producer) {
			d.onVertexAdded(entry.round)
			processed++
			logger.Debug("processed pending vertex", "round", entry.round)
		}

		d.removePendingVertex(entry.hash)
	}

	return processed
}

// snapshotPendingSorted returns a copy of pending vertices sorted by round.
// Lower rounds are processed first so parents are resolved before children.
func (d *DAG) snapshotPendingSorted() []pendingEntry {
	d.pendingMu.Lock()
	entries := make([]pendingEntry, 0, len(d.pendingVertices))

	for h, data := range d.pendingVertices {
		v := types.GetRootAsVertex(data, 0)
		entries = append(entries, pendingEntry{hash: h, data: data, round: v.Round()})
	}

	d.pendingMu.Unlock()

	sort.Slice(entries, func(i, j int) bool {
		return entries[i].round < entries[j].round
	})

	return entries
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
