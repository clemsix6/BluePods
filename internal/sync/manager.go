package sync

import (
	"sync"
	"time"

	"BluePods/internal/consensus"
	"BluePods/internal/logger"
	"BluePods/internal/state"
	"BluePods/internal/storage"
)

const (
	// defaultSnapshotInterval is the default interval between snapshots.
	defaultSnapshotInterval = 10 * time.Second
)

// SnapshotProvider provides data needed for snapshot creation.
type SnapshotProvider interface {
	// Round returns the current (latest) round number.
	Round() uint64

	// ValidatorsInfo returns all validators with their network addresses.
	ValidatorsInfo() []*consensus.ValidatorInfo

	// ExportVertices returns vertices from the specified round range.
	ExportVertices(fromRound, toRound uint64) []consensus.VertexEntry

	// ExportTrackerEntries returns all tracked objects with versions and replication.
	ExportTrackerEntries() []consensus.ObjectTrackerEntry

	// TotalSupply returns the protocol-maintained total token supply.
	TotalSupply() uint64

	// IssuanceRateMicro returns the thermostat's current per-epoch issuance rate
	// in millionths, persisted because the loop steps from the previous value.
	IssuanceRateMicro() uint64

	// ExportRegimeState returns the commit cursor together with the opaque consensus
	// regime state (current epoch, holder snapshots, strict latch) read as ONE consistent
	// cut, so a snapshot pairs its cursor and epoch state from the same committed frontier
	// and a joiner past the first epoch boundary resolves epochs instead of wedging.
	ExportRegimeState() (cursor uint64, blob []byte)
}

// DomainExporter exports domain entries for snapshot inclusion.
type DomainExporter interface {
	ExportDomains() []state.DomainEntry
}

// SnapshotManager creates periodic snapshots of the committed state.
type SnapshotManager struct {
	db       *storage.Storage // db is the underlying Pebble storage
	provider SnapshotProvider // provider provides consensus data
	domains  DomainExporter   // domains exports domain entries (nil = no domains)
	interval time.Duration    // interval between snapshot creation

	mu      sync.RWMutex
	current []byte // compressed snapshot data
	round   uint64 // lastCommittedRound of current snapshot

	stop chan struct{}
	wg   sync.WaitGroup
}

// NewSnapshotManager creates a snapshot manager that creates snapshots periodically.
func NewSnapshotManager(db *storage.Storage, provider SnapshotProvider) *SnapshotManager {
	return &SnapshotManager{
		db:       db,
		provider: provider,
		interval: defaultSnapshotInterval,
		stop:     make(chan struct{}),
	}
}

// SetDomainExporter sets the domain exporter for including domains in snapshots.
func (m *SnapshotManager) SetDomainExporter(de DomainExporter) {
	m.domains = de
}

// Start begins the periodic snapshot creation loop.
func (m *SnapshotManager) Start() {
	m.wg.Add(1)
	go m.loop()
}

// Stop stops the snapshot manager and waits for it to finish.
func (m *SnapshotManager) Stop() {
	close(m.stop)
	m.wg.Wait()
}

// Latest returns the most recent compressed snapshot and its round.
// Returns nil if no snapshot has been created yet.
func (m *SnapshotManager) Latest() (data []byte, round uint64) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	return m.current, m.round
}

// loop runs the periodic snapshot creation.
func (m *SnapshotManager) loop() {
	defer m.wg.Done()

	// Wait for genesis transactions to commit before first snapshot.
	// This ensures initial validators have their network addresses set.
	time.Sleep(2 * time.Second)
	m.createSnapshot()

	ticker := time.NewTicker(m.interval)
	defer ticker.Stop()

	for {
		select {
		case <-m.stop:
			return
		case <-ticker.C:
			m.createSnapshot()
		}
	}
}

// lastDecidedRound converts the consensus next-to-decide cursor into the last
// DECIDED round the importer expects: cursor-1, or 0 when nothing has been decided.
// This keeps the exported cursor semantic aligned with WithLastCommittedRound (I4).
func lastDecidedRound(cursor uint64) uint64 {
	if cursor == 0 {
		return 0
	}

	return cursor - 1
}

// vertexHistoryRounds is how many rounds of vertices to include in snapshot.
// This should be large enough to cover the buffer period plus some safety margin.
// Too small causes chain divergence when new nodes miss intermediate rounds.
const vertexHistoryRounds = 100

// createSnapshot creates a new snapshot and stores it.
func (m *SnapshotManager) createSnapshot() {
	// Read the commit cursor and the regime state as ONE atomic cut (I3): the commit loop
	// advances the cursor and transitions the epoch under the same commitMu, so reading the
	// cursor separately from the epoch state could pair a pre-boundary cursor with
	// post-boundary epoch state and push the joiner an epoch ahead forever. Export the
	// LAST-DECIDED round (cursor-1), not the next-to-decide cursor: the importer's
	// WithLastCommittedRound(round) sets lastCommitted = round+1, so exporting the raw
	// cursor would set the joiner one round ahead and silently skip a round on join (I4).
	regimeCursor, regimeState := m.provider.ExportRegimeState()
	commitRound := lastDecidedRound(regimeCursor)
	currentRound := m.provider.Round()

	// Skip if no new rounds since last snapshot
	m.mu.RLock()
	lastRound := m.round
	m.mu.RUnlock()

	if currentRound == lastRound && m.current != nil {
		return
	}

	// Get current validators with addresses
	validators := m.provider.ValidatorsInfo()

	// Get vertices up to current round (not just committed).
	// This ensures new nodes receive all vertices including non-committed ones,
	// preventing gaps that would block them from joining at the correct round.
	fromRound := uint64(0)
	if currentRound > vertexHistoryRounds {
		fromRound = currentRound - vertexHistoryRounds
	}
	vertices := m.provider.ExportVertices(fromRound, currentRound)

	// Get tracker entries
	trackerEntries := m.provider.ExportTrackerEntries()

	// Get domain entries
	var domainEntries []state.DomainEntry
	if m.domains != nil {
		domainEntries = m.domains.ExportDomains()
	}

	// Create snapshot with commitRound for state consistency
	data, err := CreateSnapshot(m.db, commitRound, validators, vertices, trackerEntries, domainEntries, m.provider.TotalSupply(), m.provider.IssuanceRateMicro(), regimeState)
	if err != nil {
		logger.Error("create snapshot", "error", err)
		return
	}

	// Compress snapshot
	compressed, err := CompressSnapshot(data)
	if err != nil {
		logger.Error("compress snapshot", "error", err)
		return
	}

	// Store snapshot - track currentRound to know when to regenerate
	m.mu.Lock()
	m.current = compressed
	m.round = currentRound
	m.mu.Unlock()

	logger.Debug("snapshot created",
		"commitRound", commitRound,
		"currentRound", currentRound,
		"validators", len(validators),
		"vertices", len(vertices),
		"trackerEntries", len(trackerEntries),
		"size", len(data),
		"compressed", len(compressed),
	)
}
