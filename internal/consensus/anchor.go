package consensus

import (
	"encoding/binary"

	"github.com/zeebo/blake3"
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
