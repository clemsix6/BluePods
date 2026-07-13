package consensus

import (
	"encoding/binary"

	"BluePods/internal/storage"
)

// prefixCommitted marks a vertex already folded into a commit batch:
// vc/<hash> -> {1}. Persisted so a restart never re-applies a committed batch.
var prefixCommitted = []byte("vc/")

// commitFloorKey is the metadata key holding the snapshot commit floor, the round
// at or below which every vertex is treated as already committed after a sync.
var commitFloorKey = append(append([]byte{}, prefixMeta...), []byte("commitFloor")...)

// commitCursorKey is the metadata key holding the commit cursor: the next round the
// commit loop will decide. Persisted so a restart resumes strictly past already
// decided rounds and never re-derives a decision the churned holder set would answer
// differently. Distinct from the commit floor: a skipped round advances the cursor
// but not the floor, so its still-uncommitted vertices ride a later commit batch.
var commitCursorKey = append(append([]byte{}, prefixMeta...), []byte("commitCursor")...)

// isVertexCommitted reports whether the vertex has already been included in a
// commit batch.
func (s *store) isVertexCommitted(hash Hash) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()

	return s.committedVertices[hash]
}

// markVertexCommitted records the vertex as committed, in memory and on disk, so
// it is excluded from every future causal batch, including across restarts.
func (s *store) markVertexCommitted(hash Hash) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.markVertexCommittedLocked(hash)
}

// markVertexCommittedLocked records the vertex as committed. The caller must hold
// s.mu.
func (s *store) markVertexCommittedLocked(hash Hash) {
	s.committedVertices[hash] = true
	_ = s.db.Set(makeCommittedKey(hash), []byte{1})
}

// loadCommittedFlags rebuilds the in-memory committed set from storage at boot.
func (s *store) loadCommittedFlags() {
	_ = s.db.IteratePrefix(prefixCommitted, func(key, _ []byte) error {
		if len(key) == len(prefixCommitted)+32 {
			var hash Hash
			copy(hash[:], key[len(prefixCommitted):])
			s.committedVertices[hash] = true
		}
		return nil
	})
}

// setCommitFloorLocked raises the commit floor to round, never lowering it, and
// persists it. The caller must hold s.mu.
func (s *store) setCommitFloorLocked(round uint64) {
	if s.commitFloorSet && round <= s.commitFloor {
		return
	}

	s.commitFloor = round
	s.commitFloorSet = true
	s.saveCommitFloor(round)
}

// loadCommitFloor restores the persisted snapshot commit floor at boot.
func (s *store) loadCommitFloor() {
	data, err := s.db.Get(commitFloorKey)
	if err != nil || len(data) < 8 {
		return
	}

	s.commitFloor = binary.BigEndian.Uint64(data)
	s.commitFloorSet = true
}

// saveCommitFloor persists the snapshot commit floor.
func (s *store) saveCommitFloor(round uint64) {
	data := make([]byte, 8)
	binary.BigEndian.PutUint64(data, round)
	_ = s.db.Set(commitFloorKey, data)
}

// loadCommitCursor restores the persisted commit cursor at boot. The bool is false
// when no cursor has been persisted (a fresh node), leaving the caller's default.
func (s *store) loadCommitCursor() (uint64, bool) {
	data, err := s.db.Get(commitCursorKey)
	if err != nil || len(data) < 8 {
		return 0, false
	}

	return binary.BigEndian.Uint64(data), true
}

// saveCommitCursor persists the commit cursor (the next round to decide).
func (s *store) saveCommitCursor(round uint64) {
	data := make([]byte, 8)
	binary.BigEndian.PutUint64(data, round)
	_ = s.db.Set(commitCursorKey, data)
}

// saveCommitCursorBatch atomically persists the commit cursor together with extra
// metadata pairs (the epoch-boundary state) in ONE Pebble batch. Writing the cursor
// and the epoch state together closes the crash window in which a cursor advanced
// past an epoch boundary would be restored beside a stale holder set — the wedge
// where the cursor says epoch k while the holders still say k-1. SetBatch is atomic:
// either the whole boundary lands or none of it does.
func (s *store) saveCommitCursorBatch(round uint64, extra []storage.KeyValue) {
	data := make([]byte, 8)
	binary.BigEndian.PutUint64(data, round)

	pairs := append([]storage.KeyValue{{Key: commitCursorKey, Value: data}}, extra...)
	_ = s.db.SetBatch(pairs)
}

// makeCommittedKey creates the storage key for a vertex's committed flag.
func makeCommittedKey(hash Hash) []byte {
	key := make([]byte, len(prefixCommitted)+32)
	copy(key, prefixCommitted)
	copy(key[len(prefixCommitted):], hash[:])
	return key
}
