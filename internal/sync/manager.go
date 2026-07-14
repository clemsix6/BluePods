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

	// ExportConsistentCut captures the commit cursor, the opaque regime state, the
	// committed-flagged vertices of the last historyRounds rounds, the tracker
	// entries, the validator set, the supply and issuance counters, and a storage
	// snapshot for object/signature/domain collection — all under ONE commit hold.
	// Pairing every field with the same committed frontier is load-bearing: read
	// outside the hold, the validator set can miss a registration whose round the
	// cut's cursor already passed, and a joiner booted from that snapshot forks its
	// validator set forever (it never re-decides a committed round). The same
	// applies to a joiner dropping an uncommitted anchor sibling (C-1) or landing
	// an epoch ahead on a torn cursor/epoch read (I2/I3). The caller Closes
	// DBSnapshot.
	ExportConsistentCut(historyRounds uint64) consensus.ConsistentCut
}

// DomainExporter exports domain entries for snapshot inclusion, reading them from
// the cut's consistent storage view so they pair with the exported objects.
type DomainExporter interface {
	ExportDomainsFrom(snap *storage.Snapshot) []state.DomainEntry
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
	// Skip if no new rounds since last snapshot. Round() here only gates
	// regeneration; every exported field comes from the cut below.
	m.mu.RLock()
	lastRound := m.round
	m.mu.RUnlock()

	if m.provider.Round() == lastRound && m.current != nil {
		return
	}

	// Capture EVERYTHING as ONE consistent cut (I2/I3): cursor, regime state,
	// committed-flagged vertices, tracker entries, validator set, supply, issuance,
	// and a storage snapshot for objects/signatures/domains. The commit loop holds
	// the same commitMu while it advances the cursor, marks vertices committed,
	// grows the validator set, and writes state, so this hold pins them all to one
	// committed frontier. Nothing exported may be read outside the cut: a validator
	// set read before it misses registrations the cut's cursor already passed (the
	// cut blocks behind a commit burst) and forks every joiner served that
	// snapshot. Export the LAST-DECIDED round (cursor-1): the importer's
	// WithLastCommittedRound(round) sets lastCommitted = round+1, so exporting the
	// raw cursor would skip a round on join (I4).
	cut := m.provider.ExportConsistentCut(vertexHistoryRounds)
	defer cut.DBSnapshot.Close()
	commitRound := lastDecidedRound(cut.Cursor)
	currentRound := cut.Round

	// Domains are decoded from the cut's storage view, so a domain registered
	// after the cut never dangles over an object missing from the cut's state.
	var domainEntries []state.DomainEntry
	if m.domains != nil {
		domainEntries = m.domains.ExportDomainsFrom(cut.DBSnapshot)
	}

	// Collect objects/signatures from the cut's storage snapshot, so they pair with
	// the same committed frontier as the cursor and the committed vertex flags.
	data, err := CreateSnapshot(cut.DBSnapshot, commitRound, cut.Validators, cut.Vertices, cut.TrackerEntries, domainEntries, cut.Supply, cut.IssuanceRate, cut.Regime)
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
		"validators", len(cut.Validators),
		"vertices", len(cut.Vertices),
		"trackerEntries", len(cut.TrackerEntries),
		"size", len(data),
		"compressed", len(compressed),
	)
}
