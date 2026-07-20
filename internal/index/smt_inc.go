package index

// node is a materialized tree node: either a leaf holding one entry's position and
// value hash, or an internal node with two child slots split on the depth-th bit. A
// nil *node stands for an empty subtree, whose hash is defaultHashes at its depth.
//
// The invariant that keeps the structure bit-identical to the functional recompute:
// an internal node always has at least two entries below it, so it never has a leaf
// on one side and an empty (nil) child on the other — such a pair is a single entry
// and must be a bare leaf. insertNode's split and deleteNode's collapse both preserve
// this.
type node struct {
	keyHash   [32]byte // leaf only: the entry's blake3 position
	valueHash [32]byte // leaf only: the entry's value hash
	left      *node    // internal only: child on the 0-bit side, nil if empty
	right     *node    // internal only: child on the 1-bit side, nil if empty
	hash      [32]byte // leaf: its leaf hash; internal: memoized subtree hash when !dirty
	dirty     bool     // internal only: hash must be recomputed before use
	isLeaf    bool     // discriminates a leaf from an internal node
}

// newLeaf builds a leaf for (keyHash, valueHash) with its leaf hash precomputed, so
// the hash is charged to the mutation rather than to Root.
func newLeaf(keyHash, valueHash [32]byte) *node {
	return &node{
		isLeaf:    true,
		keyHash:   keyHash,
		valueHash: valueHash,
		hash:      leafHash(keyHash, valueHash),
	}
}

// insertNode inserts (keyHash, valueHash) into the subtree n at depth and returns the
// subtree's new root. An empty slot takes a collapsed leaf; a matching leaf is
// overwritten in place; a different leaf spills into a fresh split; an internal node is
// descended by the key's depth-th bit and marked dirty on the way back up.
func insertNode(n *node, depth int, keyHash, valueHash [32]byte) *node {
	if n == nil {
		return newLeaf(keyHash, valueHash)
	}
	if n.isLeaf {
		if n.keyHash == keyHash {
			n.valueHash = valueHash
			n.hash = leafHash(keyHash, valueHash)
			return n
		}
		return splitLeaves(n, newLeaf(keyHash, valueHash), depth)
	}
	if bit(keyHash, depth) == 0 {
		n.left = insertNode(n.left, depth+1, keyHash, valueHash)
	} else {
		n.right = insertNode(n.right, depth+1, keyHash, valueHash)
	}
	n.dirty = true
	return n
}

// splitLeaves places two distinct leaves that share their prefix up to depth into a
// chain of internal nodes, branching them apart at the first bit where their positions
// differ. Every internal node it creates is born dirty; the leaves already carry their
// hashes. Distinct blake3 positions guarantee divergence before depth treeDepth.
func splitLeaves(a, b *node, depth int) *node {
	ba, bb := bit(a.keyHash, depth), bit(b.keyHash, depth)
	if ba != bb {
		return branch(a, b, ba)
	}
	return branch(splitLeaves(a, b, depth+1), nil, ba)
}

// branch returns a dirty internal node placing first on the side given by its bit and
// second on the other side (second may be nil, an empty slot).
func branch(first, second *node, firstBit int) *node {
	n := &node{dirty: true}
	if firstBit == 0 {
		n.left, n.right = first, second
	} else {
		n.left, n.right = second, first
	}
	return n
}

// deleteNode removes keyHash from the subtree n at depth and returns the subtree's new
// root. It descends by the key's depth-th bit, marks the path dirty, and collapses each
// internal node that a removal has left holding a single entry back into that lone leaf.
func deleteNode(n *node, depth int, keyHash [32]byte) *node {
	if n == nil {
		return nil
	}
	if n.isLeaf {
		if n.keyHash == keyHash {
			return nil
		}
		return n
	}
	if bit(keyHash, depth) == 0 {
		n.left = deleteNode(n.left, depth+1, keyHash)
	} else {
		n.right = deleteNode(n.right, depth+1, keyHash)
	}
	n.dirty = true
	return collapse(n)
}

// collapse restores the internal-node invariant after a deletion: an internal node
// whose subtree now holds a single entry (one nil child, the other a leaf) is replaced
// by that leaf, floating it up. A node with two entries or more is returned unchanged.
func collapse(n *node) *node {
	if n.left == nil && n.right != nil && n.right.isLeaf {
		return n.right
	}
	if n.right == nil && n.left != nil && n.left.isLeaf {
		return n.left
	}
	return n
}

// nodeHash returns the hash of the subtree rooted at n, whose top sits at depth. A nil
// subtree folds to its default hash and a leaf returns its precomputed hash, both
// without recursion. A clean internal node returns its memoized hash; a dirty one
// recomputes it from its children, caches it, and clears the flag, so a Root or Prove
// call rehashes exactly the nodes a mutation dirtied.
func nodeHash(n *node, depth int) [32]byte {
	if n == nil {
		return defaultHashes[depth]
	}
	if n.isLeaf {
		return n.hash
	}
	if n.dirty {
		n.hash = internalHash(nodeHash(n.left, depth+1), nodeHash(n.right, depth+1))
		n.dirty = false
	}
	return n.hash
}
