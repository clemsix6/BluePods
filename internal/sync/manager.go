package sync

import (
	"sync"
	"time"

	"BluePods/internal/consensus"
	"BluePods/internal/logger"
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

	// LastCommittedRound returns the last committed round number.
	LastCommittedRound() uint64

	// ValidatorsInfo returns all validators with their network addresses.
	ValidatorsInfo() []*consensus.ValidatorInfo

	// ExportVertices returns vertices from the specified round range.
	ExportVertices(fromRound, toRound uint64) []consensus.VertexEntry

	// ExportTrackerEntries returns all tracked objects with versions and replication.
	ExportTrackerEntries() []consensus.ObjectTrackerEntry
}

// SnapshotManager creates periodic snapshots of the committed state.
type SnapshotManager struct {
	db       *storage.Storage
	provider SnapshotProvider
	interval time.Duration

	mu       sync.RWMutex
	current  []byte // compressed snapshot data
	round    uint64 // lastCommittedRound of current snapshot

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

// vertexHistoryRounds is how many rounds of vertices to include in snapshot.
// This should be large enough to cover the buffer period plus some safety margin.
// Too small causes chain divergence when new nodes miss intermediate rounds.
const vertexHistoryRounds = 100

// createSnapshot creates a new snapshot and stores it.
func (m *SnapshotManager) createSnapshot() {
	commitRound := m.provider.LastCommittedRound()
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

	// Create snapshot with commitRound for state consistency
	data, err := CreateSnapshot(m.db, commitRound, validators, vertices, trackerEntries)
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
