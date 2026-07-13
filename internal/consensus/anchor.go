package consensus

import (
	"bytes"
	"encoding/binary"
	"sort"

	"github.com/zeebo/blake3"

	"BluePods/internal/types"
)

// anchorDomain domain-separates the anchor-designation hash so its output can
// never collide with any other BLAKE3 use in the protocol.
var anchorDomain = []byte("bluepods/anchor/v1")

// anchorProducerFor returns the designated anchor producer for a round, chosen
// deterministically from the epoch holder snapshot so every node derives the same
// pivot without communication. The snapshot is selected by commitEpochForRound so
// production and commit weigh the same membership across an epoch boundary. It
// returns ok=false when no holder snapshot resolves or the set is empty.
func (d *DAG) anchorProducerFor(round uint64) (Hash, bool) {
	holders, ok := d.HoldersForEpoch(d.commitEpochForRound(round))
	if !ok || holders.Len() == 0 {
		return Hash{}, false
	}

	keys := holders.SortedKeys()

	var buf [8]byte
	binary.LittleEndian.PutUint64(buf[:], round)

	h := blake3.Sum256(append(append([]byte{}, anchorDomain...), buf[:]...))
	idx := binary.LittleEndian.Uint64(h[:8]) % uint64(len(keys))

	return keys[idx], true
}

// causalEntry pairs a vertex hash with its round for deterministic ordering.
type causalEntry struct {
	hash  Hash   // hash is the vertex hash
	round uint64 // round is the vertex's round
}

// causalBatch returns every not-yet-committed vertex in the anchor's causal
// history (the anchor included), ordered deterministically by round ascending
// then hash bytes ascending, so every node that holds the same DAG produces the
// identical batch. Committed vertices and vertices at or below the commit floor
// bound the walk and are excluded, so already-applied history is never revisited.
// ok is false when any referenced vertex is absent locally, signaling the caller
// to wait for the missing ancestor before committing.
func (s *store) causalBatch(anchor Hash) ([]Hash, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	collected, ok := s.collectCausal(anchor)
	if !ok {
		return nil, false
	}

	sort.Slice(collected, func(i, j int) bool {
		if collected[i].round != collected[j].round {
			return collected[i].round < collected[j].round
		}
		return bytes.Compare(collected[i].hash[:], collected[j].hash[:]) < 0
	})

	result := make([]Hash, len(collected))
	for i := range collected {
		result[i] = collected[i].hash
	}

	return result, true
}

// collectCausal walks the anchor's ancestry breadth-first over parent links,
// collecting not-yet-committed vertices above the commit floor. It returns ok
// false as soon as a referenced vertex is missing locally. The caller must hold
// s.mu.
func (s *store) collectCausal(anchor Hash) ([]causalEntry, bool) {
	seen := make(map[Hash]bool)
	queue := []Hash{anchor}
	var collected []causalEntry

	for len(queue) > 0 {
		hash := queue[0]
		queue = queue[1:]

		if seen[hash] || s.committedVertices[hash] {
			continue
		}
		seen[hash] = true

		vertex := s.get(hash)
		if vertex == nil {
			return nil, false
		}

		if round := vertex.Round(); !s.belowCommitFloor(round) {
			collected = append(collected, causalEntry{hash: hash, round: round})
			queue = appendParentHashes(queue, vertex)
		}
	}

	return collected, true
}

// belowCommitFloor reports whether round is at or below the snapshot commit floor,
// meaning the vertex is already committed and must bound the causal walk.
func (s *store) belowCommitFloor(round uint64) bool {
	return s.commitFloorSet && round <= s.commitFloor
}

// appendParentHashes appends the hashes of a vertex's parent links to queue.
func appendParentHashes(queue []Hash, vertex *types.Vertex) []Hash {
	var link types.VertexLink

	for i := 0; i < vertex.ParentsLength(); i++ {
		if vertex.Parents(&link, i) {
			queue = append(queue, extractLinkHash(&link))
		}
	}

	return queue
}
