package consensus

import (
	"bytes"
	"encoding/binary"
	"sort"
	"sync"

	"BluePods/internal/storage"
	"BluePods/internal/types"
)

// Key prefixes for storage.
var (
	prefixVertex = []byte("v:") // v:<hash> -> vertex bytes
	prefixRound  = []byte("r:") // r:<round>:<hash> -> producer
	prefixMeta   = []byte("m:") // m:latestRound -> uint64
)

// store holds DAG vertices with persistent storage.
type store struct {
	db *storage.Storage

	// In-memory indexes rebuilt from storage on startup.
	mu          sync.RWMutex
	byRound     map[uint64][]Hash // vertex hashes per round
	latestRound uint64            // highest round seen

	// committedVertices is the set of vertex hashes already folded into a commit
	// batch, loaded from storage at boot so a restart never re-applies them.
	committedVertices map[Hash]bool
	// commitFloor is the snapshot round at or below which vertices are treated as
	// already committed; it guards a freshly synced node against re-applying its
	// imported history.
	commitFloor uint64
	// commitFloorSet reports whether a commit floor has been established via a
	// snapshot import; without it, the zero floor would wrongly exclude round 0.
	commitFloorSet bool
}

// newStore creates a vertex store backed by the given storage.
func newStore(db *storage.Storage) *store {
	s := &store{
		db:                db,
		byRound:           make(map[uint64][]Hash),
		committedVertices: make(map[Hash]bool),
	}

	s.loadLatestRound()
	s.loadByRound()
	s.loadCommittedFlags()
	s.loadCommitFloor()

	return s
}

// loadByRound rebuilds the in-memory byRound index from the persisted round-index
// entries (r:<round>:<hash>) at boot. Without it a restart leaves byRound empty for
// every pre-crash round: a re-gossiped vertex is rejected by add as already stored
// and never re-indexed, so getByRoundProducer reports the round's producers absent
// and the commit loop spuriously blames or skips them, forking the committed order
// from a peer that never crashed. Every persisted round is rebuilt, including rounds
// above the commit cursor, so undecided rounds resolve identically after a restart.
// One prefix scan over the round index at boot, the same order as loadCommittedFlags.
func (s *store) loadByRound() {
	_ = s.db.IteratePrefix(prefixRound, func(key, _ []byte) error {
		if len(key) != len(prefixRound)+8+32 {
			return nil
		}

		round := binary.BigEndian.Uint64(key[len(prefixRound) : len(prefixRound)+8])
		var hash Hash
		copy(hash[:], key[len(prefixRound)+8:])

		s.byRound[round] = append(s.byRound[round], hash)
		return nil
	})
}

// add stores a vertex. Returns false if already exists.
func (s *store) add(data []byte, hash Hash, round uint64, producer Hash) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Check if exists
	if s.hasLocked(hash) {
		return false
	}

	// Store vertex data
	vertexKey := makeVertexKey(hash)
	_ = s.db.Set(vertexKey, data)

	// Store round index entry
	roundKey := makeRoundKey(round, hash)
	_ = s.db.Set(roundKey, producer[:])

	// Update in-memory index
	s.byRound[round] = append(s.byRound[round], hash)

	// Update latest round
	if round > s.latestRound {
		s.latestRound = round
		s.saveLatestRound(round)
	}

	return true
}

// get retrieves a vertex by hash. Returns nil if not found.
func (s *store) get(hash Hash) *types.Vertex {
	data := s.getRaw(hash)
	if data == nil {
		return nil
	}
	return types.GetRootAsVertex(data, 0)
}

// getRaw retrieves raw vertex bytes by hash.
func (s *store) getRaw(hash Hash) []byte {
	key := makeVertexKey(hash)
	data, err := s.db.Get(key)
	if err != nil {
		return nil
	}
	return data
}

// getByRound returns all vertex hashes for a given round.
func (s *store) getByRound(round uint64) []Hash {
	s.mu.RLock()
	defer s.mu.RUnlock()

	hashes := s.byRound[round]
	result := make([]Hash, len(hashes))
	copy(result, hashes)

	return result
}

// getByRoundProducer returns every locally-held vertex hash produced by producer
// at round, sorted ascending by hash bytes. The order is derived from the hashes
// alone, so two nodes that ingested an equivocator's vertices in different arrival
// orders return the identical slice. It deliberately returns ALL of an
// equivocator's vertices rather than picking a local winner: a locally-chosen
// candidate is view-dependent and would fork the committed log. Arbitration among
// them is by vote in the commit rule, never here. The bool is false when the
// producer holds no vertex at the round.
func (s *store) getByRoundProducer(round uint64, producer Hash) ([]Hash, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var matches []Hash
	for _, hash := range s.byRound[round] {
		if s.producerAt(round, hash) == producer {
			matches = append(matches, hash)
		}
	}

	sort.Slice(matches, func(i, j int) bool {
		return bytes.Compare(matches[i][:], matches[j][:]) < 0
	})

	return matches, len(matches) > 0
}

// producerAt reads the producer recorded for a vertex in the round index. It
// returns the zero hash when the round-index entry is missing or malformed.
func (s *store) producerAt(round uint64, hash Hash) Hash {
	var producer Hash

	data, err := s.db.Get(makeRoundKey(round, hash))
	if err != nil || len(data) != 32 {
		return producer
	}

	copy(producer[:], data)
	return producer
}

// producersInRange returns the set of producers with at least one stored vertex
// at any round in [from, to]. Producers are read from the round index, so the
// result reflects exactly the locally stored frontier.
func (s *store) producersInRange(from, to uint64) map[Hash]bool {
	s.mu.RLock()
	defer s.mu.RUnlock()

	seen := make(map[Hash]bool)
	for r := from; r <= to; r++ {
		for _, hash := range s.byRound[r] {
			seen[s.producerAt(r, hash)] = true
		}
	}

	return seen
}

// highestRound returns the greatest round for which any vertex is stored. It
// bounds the anchor rule's forward scan so an undecided round yields WAIT rather
// than scanning unboundedly.
func (s *store) highestRound() uint64 {
	s.mu.RLock()
	defer s.mu.RUnlock()

	return s.latestRound
}

// countByRound returns the number of vertices in a round.
func (s *store) countByRound(round uint64) int {
	s.mu.RLock()
	defer s.mu.RUnlock()

	return len(s.byRound[round])
}

// has checks if a vertex exists.
func (s *store) has(hash Hash) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()

	return s.hasLocked(hash)
}

// hasLocked checks if a vertex exists (caller must hold lock).
func (s *store) hasLocked(hash Hash) bool {
	key := makeVertexKey(hash)
	data, _ := s.db.Get(key)
	return data != nil
}

// loadLatestRound loads the latest round from storage.
func (s *store) loadLatestRound() {
	key := append(prefixMeta, []byte("latestRound")...)
	data, err := s.db.Get(key)
	if err != nil || len(data) < 8 {
		return
	}
	s.latestRound = binary.BigEndian.Uint64(data)
}

// saveLatestRound persists the latest round to storage.
func (s *store) saveLatestRound(round uint64) {
	key := append(prefixMeta, []byte("latestRound")...)
	data := make([]byte, 8)
	binary.BigEndian.PutUint64(data, round)
	_ = s.db.Set(key, data)
}

// loadMetaUint64 reads a big-endian uint64 metadata value. ok is false when the key
// is absent or malformed, so the caller keeps its default.
func (s *store) loadMetaUint64(key []byte) (uint64, bool) {
	data, err := s.db.Get(key)
	if err != nil || len(data) < 8 {
		return 0, false
	}

	return binary.BigEndian.Uint64(data), true
}

// loadMetaBytes reads a raw metadata value, or nil when it is absent.
func (s *store) loadMetaBytes(key []byte) []byte {
	data, err := s.db.Get(key)
	if err != nil {
		return nil
	}

	return data
}

// makeVertexKey creates a storage key for vertex data.
func makeVertexKey(hash Hash) []byte {
	key := make([]byte, len(prefixVertex)+32)
	copy(key, prefixVertex)
	copy(key[len(prefixVertex):], hash[:])
	return key
}

// makeRoundKey creates a storage key for round index.
func makeRoundKey(round uint64, hash Hash) []byte {
	key := make([]byte, len(prefixRound)+8+32)
	copy(key, prefixRound)
	binary.BigEndian.PutUint64(key[len(prefixRound):], round)
	copy(key[len(prefixRound)+8:], hash[:])
	return key
}

// VertexEntry represents a vertex with its round for snapshot serialization.
type VertexEntry struct {
	Round     uint64 // Round is the vertex's DAG round
	Data      []byte // Data is the serialized vertex (FlatBuffers Vertex)
	Committed bool   // Committed reports whether the vertex was already folded into a commit batch at the export cut
}

// ExportVertices returns all vertices from the given round range, each tagged with
// whether it was already folded into a commit batch. The committed flag is read
// from committedVertices under the same lock as byRound, so it pairs with the
// exported vertices at vertex grain (C-1). Used for snapshot creation.
func (s *store) ExportVertices(fromRound, toRound uint64) []VertexEntry {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var entries []VertexEntry

	for round := fromRound; round <= toRound; round++ {
		hashes := s.byRound[round]
		for _, hash := range hashes {
			data := s.getRaw(hash)
			if data != nil {
				entries = append(entries, VertexEntry{
					Round:     round,
					Data:      data,
					Committed: s.committedVertices[hash],
				})
			}
		}
	}

	return entries
}

// ImportVertices loads vertices from snapshot data and rebuilds the index. A vertex
// is marked committed EXACTLY when its entry carries the committed flag (the
// source's committed frontier at vertex grain), never by a round threshold: an
// uncommitted same-round sibling of a decided anchor must stay uncommitted so it
// rides a later batch, as it does on the source (C-1). The commit floor is set one
// below the lowest imported round, so it bounds the causal walk strictly below the
// imported window without ever standing in for the committed marker. Returns the
// highest round imported.
func (s *store) ImportVertices(entries []VertexEntry) uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()

	var maxRound, minRound uint64
	minSet := false

	for _, entry := range entries {
		if !minSet || entry.Round < minRound {
			minRound = entry.Round
			minSet = true
		}

		if s.importVertexLocked(entry) && entry.Round > maxRound {
			maxRound = entry.Round
		}
	}

	// Bound the walk strictly below the imported window (minRound). Pre-window rounds
	// were not imported and their vertices are already-applied history the committed
	// flags stop the walk at; the floor is only the backstop below them. Round 0 has
	// nothing beneath it, so no floor is set when the window reaches the origin.
	if minSet && minRound > 0 {
		s.setCommitFloorLocked(minRound - 1)
	}

	if maxRound > s.latestRound {
		s.latestRound = maxRound
		s.saveLatestRound(maxRound)
	}

	return maxRound
}

// importVertexLocked persists one snapshot vertex and its round-index entry,
// marking it committed only when the entry's committed flag is set. It returns
// false when the vertex already exists. The caller must hold s.mu.
func (s *store) importVertexLocked(entry VertexEntry) bool {
	vertex := types.GetRootAsVertex(entry.Data, 0)

	var hash, producer Hash
	copy(hash[:], vertex.HashBytes())
	copy(producer[:], vertex.ProducerBytes())

	if s.hasLocked(hash) {
		return false
	}

	_ = s.db.Set(makeVertexKey(hash), entry.Data)
	_ = s.db.Set(makeRoundKey(entry.Round, hash), producer[:])
	s.byRound[entry.Round] = append(s.byRound[entry.Round], hash)

	if entry.Committed {
		s.markVertexCommittedLocked(hash)
	}

	return true
}
