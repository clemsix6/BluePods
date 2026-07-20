package index

import (
	"bytes"
	"sort"

	"github.com/zeebo/blake3"
)

const (
	// treeDepth is the number of bits in a key position. blake3 produces 256 bits, so
	// the tree is 256 levels deep and every distinct key diverges before this depth.
	treeDepth = 256

	// leafPrefix domain-separates leaf hashes: blake3(0x00 || keyHash || valueHash).
	leafPrefix = 0x00

	// internalPrefix domain-separates internal-node hashes: blake3(0x01 || left || right).
	internalPrefix = 0x01
)

// defaultHashes[d] is the root hash of a completely empty subtree rooted at depth d.
// The base defaultHashes[treeDepth] is the all-zero empty placeholder (the JMT-standard
// null value, chosen here because the brief fixes the leaf and internal hashes but not
// the empty base); each shallower level is the internal hash of two empty children, so
// defaultHashes[0] is the root of an empty tree.
var defaultHashes = computeDefaultHashes()

// computeDefaultHashes precomputes the empty-subtree hash for every depth 0..treeDepth.
func computeDefaultHashes() [treeDepth + 1][32]byte {
	var d [treeDepth + 1][32]byte
	// d[treeDepth] stays the zero value: the canonical empty-leaf placeholder.
	for depth := treeDepth - 1; depth >= 0; depth-- {
		d[depth] = internalHash(d[depth+1], d[depth+1])
	}
	return d
}

// entry is a single key/value pair materialized in the tree, keyed by its position.
type entry struct {
	keyHash   [32]byte // keyHash is blake3(key), the 256-bit position of the entry
	valueHash [32]byte // valueHash is blake3(value), folded into the leaf hash
	value     []byte   // value is the raw stored bytes, returned verbatim by Get
}

// SMT is an in-memory sparse Merkle tree over blake3 key positions with JMT-style path
// compression: a subtree holding exactly one entry collapses to that entry's leaf hash.
// The root is computed functionally from the full key set on demand, which is bit-identical
// to an incremental Jellyfish Merkle Tree yet immune to leaf-spill and sibling-reconstruction
// bugs. It is derived state, rebuilt by the caller, and is not safe for concurrent use.
type SMT struct {
	entries map[[32]byte]entry // entries maps a key position to its stored entry

	rootCache [32]byte // rootCache holds the last computed root while dirty is false
	dirty     bool     // dirty marks the root cache stale after a mutation
}

// New returns an empty SMT whose root is defaultHashes[0].
func New() *SMT {
	return &SMT{entries: make(map[[32]byte]entry), dirty: true}
}

// Insert adds or overwrites the entry for key with value.
func (t *SMT) Insert(key, value []byte) {
	kh := blake3.Sum256(key)
	t.entries[kh] = entry{
		keyHash:   kh,
		valueHash: blake3.Sum256(value),
		value:     append([]byte(nil), value...),
	}
	t.dirty = true
}

// Delete removes the entry for key; deleting an absent key is a no-op.
func (t *SMT) Delete(key []byte) {
	delete(t.entries, blake3.Sum256(key))
	t.dirty = true
}

// Get returns a copy of the value stored for key and whether it is present.
func (t *SMT) Get(key []byte) ([]byte, bool) {
	e, ok := t.entries[blake3.Sum256(key)]
	if !ok {
		return nil, false
	}
	return append([]byte(nil), e.value...), true
}

// Root returns the tree's Merkle root, recomputing it only when entries have changed.
func (t *SMT) Root() [32]byte {
	if t.dirty {
		t.rootCache = hashSubtree(0, t.sortedEntries())
		t.dirty = false
	}
	return t.rootCache
}

// sortedEntries returns the entries sorted by keyHash. That order makes each bit-level
// partition contiguous, which is what lets the recursive hashing and proving split the
// slice in two rather than iterate a map.
func (t *SMT) sortedEntries() []entry {
	out := make([]entry, 0, len(t.entries))
	for _, e := range t.entries {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool {
		return bytes.Compare(out[i].keyHash[:], out[j].keyHash[:]) < 0
	})
	return out
}

// hashSubtree computes the compressed hash of the subtree at depth over entries (sorted by
// keyHash). An empty subtree folds to its default hash, a single entry collapses to its leaf
// hash, and otherwise the entries split on the depth-th bit into two child subtrees.
func hashSubtree(depth int, entries []entry) [32]byte {
	if len(entries) == 0 {
		return defaultHashes[depth]
	}
	if len(entries) == 1 {
		return leafHash(entries[0].keyHash, entries[0].valueHash)
	}
	mid := splitOnBit(entries, depth)
	left := hashSubtree(depth+1, entries[:mid])
	right := hashSubtree(depth+1, entries[mid:])
	return internalHash(left, right)
}

// splitOnBit returns the index partitioning entries (sorted by keyHash) into those with a
// 0 bit and those with a 1 bit at position depth. Entries before the index have bit 0.
func splitOnBit(entries []entry, depth int) int {
	return sort.Search(len(entries), func(i int) bool {
		return bit(entries[i].keyHash, depth) == 1
	})
}

// leafHash computes blake3(0x00 || keyHash || valueHash), the compressed leaf hash.
func leafHash(keyHash, valueHash [32]byte) [32]byte {
	var buf [1 + 32 + 32]byte
	buf[0] = leafPrefix
	copy(buf[1:33], keyHash[:])
	copy(buf[33:], valueHash[:])
	return blake3.Sum256(buf[:])
}

// internalHash computes blake3(0x01 || left || right), the hash of an internal node.
func internalHash(left, right [32]byte) [32]byte {
	var buf [1 + 32 + 32]byte
	buf[0] = internalPrefix
	copy(buf[1:33], left[:])
	copy(buf[33:], right[:])
	return blake3.Sum256(buf[:])
}

// bit returns the bit at position i of h, most-significant-bit first (bit 0 is the top bit
// of the first byte). This orders the tree consistently with a byte-wise sort of keyHash.
func bit(h [32]byte, i int) int {
	return int((h[i/8] >> (7 - uint(i%8))) & 1)
}
