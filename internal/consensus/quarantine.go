package consensus

import (
	"encoding/hex"
	"errors"

	"BluePods/internal/events"
	"BluePods/internal/logger"
	"BluePods/internal/types"
)

// prefixQuarantine marks a vertex this node PROVED wrong about its anchored
// index root: vq/<hash> -> {1}.
//
// The mark is what makes the verdict outlive the history that proved it. The
// root history behind anchorLie is a bounded window, and a restarted node
// rebuilds its index from committed state without restoring any of it — so a
// verdict re-derived on read silently decays to "unverifiable", and a vertex
// this node once disproved would become referenceable and relayable again after
// a reboot. Persisted, it does not.
//
// Like the committed flags under vc/ and the fault evidence under fault/, the
// prefixed 35-byte key is structurally invisible to every scan that feeds the
// snapshot, the convergence fingerprint or the object store: all of them select
// bare 32-byte keys. That is deliberate. The mark is NODE-LOCAL and must not be
// consensus state — whether a node could disprove an anchor depends on how far
// its own commit had advanced when the vertex arrived, so two honest nodes
// legitimately hold different quarantine sets and must still agree on state.
var prefixQuarantine = []byte("vq/")

// isQuarantineVerdict reports whether a validateVertex error is the quarantine
// verdict rather than a rejection. It is the one error that does NOT stop the
// vertex from being stored: everything else validateVertex returns is either a
// terminal rejection or a buffer case.
func isQuarantineVerdict(err error) bool {
	return errors.Is(err, errIndexRoot)
}

// quarantineVertex records the verdict and reports it. The vertex is already in
// the store by the time this runs: quarantine withholds a proven liar from
// relay and from reference, never from storage, because a node that cannot
// store a vertex cannot complete any causal batch containing it — and a batch
// it cannot complete is a commit cursor that never moves again.
func (d *DAG) quarantineVertex(v *types.Vertex, hash, producer Hash, round uint64) {
	d.store.markQuarantined(hash)
	d.reportQuarantine(v, hash, producer, round)
}

// reportQuarantine logs and emits the quarantine event, without writing the vq/
// mark. Split out of quarantineVertex for AddVertex's ingress path, which must
// write the mark BEFORE the vertex (see the ordering comment there) and so
// calls store.markQuarantined directly, ahead of the store write, then this
// once the vertex has landed.
func (d *DAG) reportQuarantine(v *types.Vertex, hash, producer Hash, round uint64) {
	logger.Warn("quarantining wrong-root vertex",
		"vertex", hex.EncodeToString(hash[:8]), "producer", hex.EncodeToString(producer[:8]),
		"round", round, "frontier", v.FrontierRound())

	events.VertexQuarantined(hash, producer, round, v.FrontierRound())
}

// isQuarantined reports whether this node has proved the vertex wrong about its
// anchor.
func (s *store) isQuarantined(hash Hash) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()

	return s.quarantinedVertices[hash]
}

// markQuarantined records the verdict in memory and on disk, so it holds across
// restarts.
func (s *store) markQuarantined(hash Hash) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.quarantinedVertices[hash] = true

	if err := s.db.Set(makeQuarantineKey(hash), []byte{1}); err != nil {
		logger.Error("persist quarantine mark", "error", err)
	}
}

// loadQuarantineFlags rebuilds the in-memory quarantine set from storage at
// boot, the same one prefix scan shape as loadCommittedFlags.
func (s *store) loadQuarantineFlags() {
	_ = s.db.IteratePrefix(prefixQuarantine, func(key, _ []byte) error {
		if len(key) == len(prefixQuarantine)+32 {
			var hash Hash
			copy(hash[:], key[len(prefixQuarantine):])
			s.quarantinedVertices[hash] = true
		}
		return nil
	})
}

// makeQuarantineKey creates the storage key for a vertex's quarantine mark.
func makeQuarantineKey(hash Hash) []byte {
	key := make([]byte, len(prefixQuarantine)+32)
	copy(key, prefixQuarantine)
	copy(key[len(prefixQuarantine):], hash[:])

	return key
}
