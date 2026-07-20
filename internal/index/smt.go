package index

import (
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
//
// NORMATIVE for any external verifier: defaultHashes[treeDepth] (treeDepth = 256) is
// exactly 32 zero bytes ([32]byte{}), not the hash of anything. A client-side verifier
// must start from that same all-zero base and fold upward with internalHash(d, d) at
// each shallower depth, or its roots will never agree with this package's.
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

// entry is a single key/value pair, keyed by its position. It is the unit of the
// entries map, which backs Get and feeds the differential oracle.
type entry struct {
	keyHash   [32]byte // keyHash is blake3(key), the 256-bit position of the entry
	valueHash [32]byte // valueHash is blake3(value), folded into the leaf hash
	value     []byte   // value is the raw stored bytes, returned verbatim by Get
}

// SMT is an in-memory sparse Merkle tree over blake3 key positions with JMT-style path
// compression: a subtree holding exactly one entry collapses to that entry's leaf hash.
// It materializes only the non-empty paths (see smt_inc.go) and memoizes each internal
// node's subtree hash, so a mutation dirties only its root-to-leaf path and Root rehashes
// just those nodes. The result is bit-identical to the from-scratch functional recompute
// in smt_oracle.go, which the differential test checks after every mutation. The tree is
// derived state, rebuilt by the caller, and is not safe for concurrent use.
type SMT struct {
	// entries maps a key position to its stored entry. It backs Get and the
	// differential oracle; the materialized tree carries only the hashes.
	entries map[[32]byte]entry

	// root is the materialized tree's node at depth 0, nil for an empty tree.
	root *node
}

// New returns an empty SMT whose root is defaultHashes[0].
func New() *SMT {
	return &SMT{entries: make(map[[32]byte]entry)}
}

// Insert adds or overwrites the entry for key with value.
func (t *SMT) Insert(key, value []byte) {
	kh := blake3.Sum256(key)
	vh := blake3.Sum256(value)
	t.entries[kh] = entry{
		keyHash:   kh,
		valueHash: vh,
		value:     append([]byte(nil), value...),
	}
	t.root = insertNode(t.root, 0, kh, vh)
}

// Delete removes the entry for key; deleting an absent key is a no-op, leaving the
// materialized tree and its cached hashes untouched.
func (t *SMT) Delete(key []byte) {
	kh := blake3.Sum256(key)
	if _, ok := t.entries[kh]; !ok {
		return
	}
	delete(t.entries, kh)
	t.root = deleteNode(t.root, 0, kh)
}

// Get returns a copy of the value stored for key and whether it is present.
func (t *SMT) Get(key []byte) ([]byte, bool) {
	e, ok := t.entries[blake3.Sum256(key)]
	if !ok {
		return nil, false
	}
	return append([]byte(nil), e.value...), true
}

// Root returns the tree's Merkle root, rehashing only the internal nodes dirtied
// since the last call. An empty tree hashes to defaultHashes[0].
func (t *SMT) Root() [32]byte {
	return nodeHash(t.root, 0)
}

// hashCount counts leaf and internal node hashes since the last resetHashCount.
// It is an unexported test hook: the benchmark resets it, performs one update,
// and reads it back to assert a single mutation rehashes only O(log n) nodes
// rather than the whole tree. The single non-atomic increment per node hash is
// negligible and the SMT is never used concurrently.
var hashCount uint64

// resetHashCount zeroes the node-hash counter; tests call it before a measured
// operation.
func resetHashCount() { hashCount = 0 }

// readHashCount returns the number of node hashes since the last reset.
func readHashCount() uint64 { return hashCount }

// leafHash computes blake3(0x00 || keyHash || valueHash), the compressed leaf hash.
func leafHash(keyHash, valueHash [32]byte) [32]byte {
	hashCount++
	var buf [1 + 32 + 32]byte
	buf[0] = leafPrefix
	copy(buf[1:33], keyHash[:])
	copy(buf[33:], valueHash[:])
	return blake3.Sum256(buf[:])
}

// internalHash computes blake3(0x01 || left || right), the hash of an internal node.
func internalHash(left, right [32]byte) [32]byte {
	hashCount++
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
