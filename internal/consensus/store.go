package consensus

import (
	"encoding/binary"
	"sync"

	"BluePods/internal/storage"
	"BluePods/internal/types"
)

// Key prefixes for storage.
var (
	prefixVertex = []byte("v:")  // v:<hash> -> vertex bytes
	prefixRound  = []byte("r:")  // r:<round>:<hash> -> producer
	prefixMeta   = []byte("m:")  // m:latestRound -> uint64
)

// store holds DAG vertices with persistent storage.
type store struct {
	db *storage.Storage

	// In-memory indexes rebuilt from storage on startup.
	mu          sync.RWMutex
	byRound     map[uint64][]Hash // vertex hashes per round
	latestRound uint64            // highest round seen
}

// newStore creates a vertex store backed by the given storage.
func newStore(db *storage.Storage) *store {
	s := &store{
		db:      db,
		byRound: make(map[uint64][]Hash),
	}

	s.loadLatestRound()

	return s
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
