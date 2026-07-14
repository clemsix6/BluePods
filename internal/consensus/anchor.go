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
// production and commit weigh the same membership across an epoch boundary. The
// rotation is restricted to the snapshot's ELIGIBLE members — those with at least
// one committed vertex at the snapshot's freeze — falling back to the full
// snapshot while none is eligible, so a member is never designated before it has
// demonstrably produced (the registration-to-production gap wedge). It returns
// ok=false when no holder snapshot resolves or the set is empty.
func (d *DAG) anchorProducerFor(round uint64) (Hash, bool) {
	epoch := d.commitEpochForRound(round)

	holders, ok := d.HoldersForEpoch(epoch)
	if !ok || holders.Len() == 0 {
		return Hash{}, false
	}

	keys := eligibleKeys(holders, d.eligibleForEpoch(epoch))

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

// missingAncestors walks a decided anchor's causal history and returns the hashes
// of every referenced vertex that is absent locally — the frontier the commit loop
// is blocked on. It mirrors collectCausal's walk (same committed and commit-floor
// bounds, same parent links) but, instead of aborting at the first gap, records
// each absent hash and keeps going, so one call surfaces the whole current
// frontier. An absent vertex's own ancestry is unknown and cannot be walked, so a
// deeper gap only surfaces on a later call once this frontier arrives. The order is
// arrival-derived and not relied upon: the fetcher deduplicates and the walk re-runs
// each stalled tick.
func (s *store) missingAncestors(anchor Hash) []Hash {
	s.mu.RLock()
	defer s.mu.RUnlock()

	seen := make(map[Hash]bool)
	queue := []Hash{anchor}
	var missing []Hash

	for len(queue) > 0 {
		hash := queue[0]
		queue = queue[1:]

		if seen[hash] || s.committedVertices[hash] {
			continue
		}
		seen[hash] = true

		vertex := s.get(hash)
		if vertex == nil {
			missing = append(missing, hash)
			continue
		}

		if round := vertex.Round(); !s.belowCommitFloor(round) {
			queue = appendParentHashes(queue, vertex)
		}
	}

	return missing
}

// belowCommitFloor reports whether round is at or below the snapshot commit floor,
// meaning the vertex is already committed and must bound the causal walk.
func (s *store) belowCommitFloor(round uint64) bool {
	return s.commitFloorSet && round <= s.commitFloor
}

// missingFrontierAbove returns the hashes of every vertex referenced by the stored,
// not-yet-committed vertices at rounds >= fromRound that is absent locally, bounded
// by the commit floor. Unlike missingAncestors, which walks a single DECIDED anchor,
// it seeds the walk from the WHOLE forward frontier the node holds, so a commit
// cursor wedged on an UNDECIDED round — where no decided anchor exists to seed the
// walk — can still surface the historical vertices it is missing. A synced joiner
// that jumped past the in-flight frontier on import lacks vertices in the rounds
// between its floor and its join round, which forward gossip never redelivers; this
// is the frontier it must fetch to make progress. The order is arrival-derived and
// not relied upon: the fetcher deduplicates and the walk re-runs each stalled tick.
func (s *store) missingFrontierAbove(fromRound uint64) []Hash {
	s.mu.RLock()
	defer s.mu.RUnlock()

	seen := make(map[Hash]bool)
	var queue []Hash

	for round, hashes := range s.byRound {
		if round >= fromRound {
			queue = append(queue, hashes...)
		}
	}

	var missing []Hash

	for len(queue) > 0 {
		hash := queue[0]
		queue = queue[1:]

		if seen[hash] || s.committedVertices[hash] {
			continue
		}
		seen[hash] = true

		vertex := s.get(hash)
		if vertex == nil {
			missing = append(missing, hash)
			continue
		}

		if round := vertex.Round(); !s.belowCommitFloor(round) {
			queue = appendParentHashes(queue, vertex)
		}
	}

	return missing
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
