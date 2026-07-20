package index

import (
	"bytes"
	"sort"
)

// This file holds the task-2.1 functional core: the root and proof are computed
// from scratch over the full entry set every call, in O(n). The live tree no
// longer uses it (smt.go/smt_inc.go rehash only the touched path), but it is
// retained as the differential oracle the incremental tree is checked against
// after every mutation, and as a plain reference for the hash rules.

// functionalRoot recomputes the Merkle root from scratch over the entry set,
// independently of the materialized tree.
func functionalRoot(entries []entry) [32]byte {
	return hashSubtree(0, sortEntries(entries))
}

// functionalProve recomputes a proof for keyHash from scratch over the entry set.
func functionalProve(entries []entry, keyHash [32]byte) Proof {
	siblings, leaf := prove(0, sortEntries(entries), keyHash)
	return Proof{Siblings: siblings, Leaf: leaf}
}

// entriesSnapshot returns a copy of the live entry set, the source of truth the
// oracle recomputes from and the incremental tree mutates.
func (t *SMT) entriesSnapshot() []entry {
	out := make([]entry, 0, len(t.entries))
	for _, e := range t.entries {
		out = append(out, e)
	}
	return out
}

// sortEntries returns entries sorted by keyHash. That order makes each bit-level
// partition contiguous, which is what lets the recursive hashing and proving
// split the slice in two rather than iterate a map.
func sortEntries(entries []entry) []entry {
	out := append([]entry(nil), entries...)
	sort.Slice(out, func(i, j int) bool {
		return bytes.Compare(out[i].keyHash[:], out[j].keyHash[:]) < 0
	})
	return out
}

// hashSubtree computes the compressed hash of the subtree at depth over entries
// (sorted by keyHash). An empty subtree folds to its default hash, a single
// entry collapses to its leaf hash, and otherwise the entries split on the
// depth-th bit into two child subtrees.
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

// prove walks keyHash's path through the subtree at depth over entries (sorted by
// keyHash), collecting sibling hashes deepest first. It returns the occupying
// leaf's keyHash||valueHash when the path ends on a different compressed leaf,
// and nil otherwise (inclusion, or absence against an empty subtree).
func prove(depth int, entries []entry, keyHash [32]byte) ([][32]byte, []byte) {
	if len(entries) == 0 {
		return nil, nil
	}
	if len(entries) == 1 {
		if entries[0].keyHash == keyHash {
			return nil, nil
		}
		return nil, otherLeaf(entries[0].keyHash, entries[0].valueHash)
	}
	mid := splitOnBit(entries, depth)
	if bit(keyHash, depth) == 0 {
		sib, leaf := prove(depth+1, entries[:mid], keyHash)
		return append(sib, hashSubtree(depth+1, entries[mid:])), leaf
	}
	sib, leaf := prove(depth+1, entries[mid:], keyHash)
	return append(sib, hashSubtree(depth+1, entries[:mid])), leaf
}

// splitOnBit returns the index partitioning entries (sorted by keyHash) into
// those with a 0 bit and those with a 1 bit at position depth. Entries before
// the index have bit 0.
func splitOnBit(entries []entry, depth int) int {
	return sort.Search(len(entries), func(i int) bool {
		return bit(entries[i].keyHash, depth) == 1
	})
}
