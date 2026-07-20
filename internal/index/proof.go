package index

import (
	"encoding/binary"
	"errors"

	"github.com/zeebo/blake3"
)

// errTruncatedProof indicates a serialized proof was malformed or cut short.
var errTruncatedProof = errors.New("index: truncated or malformed proof")

// Proof is a Merkle path authenticating a single key against a root. Siblings lists the
// sibling hashes along the key's path, ordered deepest first (Siblings[0] is closest to the
// leaf). Leaf is empty for an inclusion proof and for an absence proof against an empty
// subtree; for an absence proof against a position already occupied by a different key it
// holds that key's keyHash || valueHash (64 bytes), letting the verifier recompute the
// occupying leaf and confirm it is genuinely a different key.
type Proof struct {
	Siblings [][32]byte // Siblings are the path's sibling hashes, deepest first
	Leaf     []byte     // Leaf carries the occupying leaf for the other-leaf absence case, else empty
}

// Prove returns a proof for key against the tree's current root. A present key yields an
// inclusion proof; a missing key yields an absence proof, either against the empty subtree
// at its position or against the different leaf that already occupies it.
func (t *SMT) Prove(key []byte) Proof {
	siblings, leaf := prove(0, t.sortedEntries(), blake3.Sum256(key))
	return Proof{Siblings: siblings, Leaf: leaf}
}

// prove walks the key's path through the subtree at depth over entries (sorted by keyHash),
// collecting sibling hashes deepest first. It returns the occupying leaf's keyHash||valueHash
// when the path ends on a different compressed leaf, and nil otherwise (inclusion, or absence
// against an empty subtree).
func prove(depth int, entries []entry, keyHash [32]byte) ([][32]byte, []byte) {
	if len(entries) == 0 {
		return nil, nil
	}
	if len(entries) == 1 {
		if entries[0].keyHash == keyHash {
			return nil, nil
		}
		return nil, otherLeaf(entries[0])
	}
	mid := splitOnBit(entries, depth)
	if bit(keyHash, depth) == 0 {
		sib, leaf := prove(depth+1, entries[:mid], keyHash)
		return append(sib, hashSubtree(depth+1, entries[mid:])), leaf
	}
	sib, leaf := prove(depth+1, entries[mid:], keyHash)
	return append(sib, hashSubtree(depth+1, entries[:mid])), leaf
}

// otherLeaf encodes an occupying entry as keyHash || valueHash for an absence proof.
func otherLeaf(e entry) []byte {
	out := make([]byte, 0, 64)
	out = append(out, e.keyHash[:]...)
	return append(out, e.valueHash[:]...)
}

// Verify reports whether proof authenticates key against root. With value non-nil it checks
// an inclusion proof for key => value. With value nil it checks an absence proof: the key's
// position must resolve either to the empty subtree (empty Leaf) or to a different key's leaf
// (Leaf set to that key's keyHash||valueHash). Path binding comes from folding the target up
// with the key's own path bits, so a short or forged path cannot reproduce the root.
func Verify(root [32]byte, key, value []byte, p Proof) bool {
	if len(p.Siblings) > treeDepth {
		return false
	}
	keyHash := blake3.Sum256(key)
	target, ok := proofTarget(keyHash, value, p)
	if !ok {
		return false
	}
	return foldPath(target, keyHash, p.Siblings) == root
}

// proofTarget computes the hash that must sit at the key's position for the proof to hold,
// and reports whether the proof shape is consistent with the requested inclusion or absence.
func proofTarget(keyHash [32]byte, value []byte, p Proof) ([32]byte, bool) {
	if value != nil {
		if len(p.Leaf) != 0 {
			return [32]byte{}, false // inclusion proofs never carry an occupying leaf
		}
		return leafHash(keyHash, blake3.Sum256(value)), true
	}
	return absenceTarget(keyHash, p)
}

// absenceTarget computes the position hash for an absence proof: the empty-subtree default
// when no occupying leaf is given, or the occupying leaf's hash otherwise. An occupying leaf
// whose key equals the queried key is rejected, since that would prove presence.
func absenceTarget(keyHash [32]byte, p Proof) ([32]byte, bool) {
	if len(p.Leaf) == 0 {
		return defaultHashes[len(p.Siblings)], true
	}
	if len(p.Leaf) != 64 {
		return [32]byte{}, false
	}
	var otherKey, otherValue [32]byte
	copy(otherKey[:], p.Leaf[:32])
	copy(otherValue[:], p.Leaf[32:])
	if otherKey == keyHash {
		return [32]byte{}, false
	}
	return leafHash(otherKey, otherValue), true
}

// foldPath folds target upward through the sibling hashes using the key's path bits, and
// returns the root the proof commits to. Siblings are deepest first, so the branch depth
// decreases as we climb toward the root.
func foldPath(target, keyHash [32]byte, siblings [][32]byte) [32]byte {
	cur := target
	for i, sib := range siblings {
		depth := len(siblings) - 1 - i
		if bit(keyHash, depth) == 0 {
			cur = internalHash(cur, sib)
		} else {
			cur = internalHash(sib, cur)
		}
	}
	return cur
}

// Serialize encodes the proof as a schema-free, length-prefixed byte string clients can
// decode without FlatBuffers: a uint32 sibling count, each 32-byte sibling, then a uint32
// leaf length and the leaf bytes. All integers are big-endian.
func (p Proof) Serialize() []byte {
	buf := make([]byte, 0, 4+len(p.Siblings)*32+4+len(p.Leaf))
	buf = binary.BigEndian.AppendUint32(buf, uint32(len(p.Siblings)))
	for _, s := range p.Siblings {
		buf = append(buf, s[:]...)
	}
	buf = binary.BigEndian.AppendUint32(buf, uint32(len(p.Leaf)))
	return append(buf, p.Leaf...)
}

// DeserializeProof decodes a proof produced by Serialize, returning an error on any
// truncation, oversize count, or length mismatch.
func DeserializeProof(data []byte) (Proof, error) {
	siblings, rest, err := readSiblings(data)
	if err != nil {
		return Proof{}, err
	}
	leaf, err := readLeaf(rest)
	if err != nil {
		return Proof{}, err
	}
	return Proof{Siblings: siblings, Leaf: leaf}, nil
}

// readSiblings decodes the uint32 count and that many 32-byte siblings, returning the
// remaining bytes. The count is bounded by treeDepth to reject malformed or oversized input.
func readSiblings(data []byte) ([][32]byte, []byte, error) {
	if len(data) < 4 {
		return nil, nil, errTruncatedProof
	}
	count := binary.BigEndian.Uint32(data)
	data = data[4:]
	if count > treeDepth {
		return nil, nil, errTruncatedProof
	}
	siblings := make([][32]byte, count)
	for i := range siblings {
		if len(data) < 32 {
			return nil, nil, errTruncatedProof
		}
		copy(siblings[i][:], data[:32])
		data = data[32:]
	}
	return siblings, data, nil
}

// readLeaf decodes the uint32 leaf length followed by exactly that many bytes, requiring the
// length to match the remaining input precisely.
func readLeaf(data []byte) ([]byte, error) {
	if len(data) < 4 {
		return nil, errTruncatedProof
	}
	leafLen := binary.BigEndian.Uint32(data)
	data = data[4:]
	if uint32(len(data)) != leafLen {
		return nil, errTruncatedProof
	}
	if leafLen == 0 {
		return nil, nil
	}
	return append([]byte(nil), data...), nil
}
