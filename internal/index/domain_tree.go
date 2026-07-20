package index

import "encoding/binary"

// DomainLeaf is the domain tree's leaf record: a name bound to an object, with
// the registrant/owner and the lease's expiry epoch. The name rides in the
// leaf value (not just the key position) so a proof is self-describing: a
// verifier who only knows the claimed name can check it against the leaf it
// decodes, rather than trusting the server's claim.
type DomainLeaf struct {
	Name        string   // Name is the domain name this leaf answers for
	ObjectID    [32]byte // ObjectID is the object the name currently resolves to
	Owner       [32]byte // Owner is the registrant, or whoever a transfer handed the name to
	ExpiryEpoch uint64   // ExpiryEpoch is the epoch after which the lease is no longer current
}

// DomainTree is a flat SMT keyed by blake3(name), answering domain resolution
// with one inclusion or absence proof per name.
type DomainTree struct {
	smt *SMT // smt is the underlying tree; the type never reaches outside this file
}

// NewDomainTree returns an empty domain tree.
func NewDomainTree() *DomainTree {
	return &DomainTree{smt: New()}
}

// Set inserts or overwrites the leaf for leaf.Name.
func (t *DomainTree) Set(leaf DomainLeaf) {
	t.smt.Insert([]byte(leaf.Name), encodeDomainLeaf(leaf))
}

// Remove deletes the leaf for name; removing an absent name is a no-op.
func (t *DomainTree) Remove(name string) {
	t.smt.Delete([]byte(name))
}

// Get returns the leaf stored for name and whether one exists.
func (t *DomainTree) Get(name string) (DomainLeaf, bool) {
	value, ok := t.smt.Get([]byte(name))
	if !ok {
		return DomainLeaf{}, false
	}

	leaf, ok := decodeDomainLeaf(value)
	return leaf, ok
}

// Root returns the tree's current Merkle root.
func (t *DomainTree) Root() [32]byte {
	return t.smt.Root()
}

// Prove returns an inclusion or absence proof for name against Root().
func (t *DomainTree) Prove(name string) Proof {
	return t.smt.Prove([]byte(name))
}

// encodeDomainLeaf serializes a domain leaf as: uint32(len(Name)) big-endian,
// Name bytes, ObjectID (32 bytes), Owner (32 bytes), ExpiryEpoch (8 bytes
// big-endian).
func encodeDomainLeaf(l DomainLeaf) []byte {
	buf := make([]byte, 0, 4+len(l.Name)+32+32+8)
	buf = binary.BigEndian.AppendUint32(buf, uint32(len(l.Name)))
	buf = append(buf, l.Name...)
	buf = append(buf, l.ObjectID[:]...)
	buf = append(buf, l.Owner[:]...)
	buf = binary.BigEndian.AppendUint64(buf, l.ExpiryEpoch)
	return buf
}

// decodeDomainLeaf reverses encodeDomainLeaf, reporting ok=false on any
// truncation or length mismatch.
func decodeDomainLeaf(data []byte) (DomainLeaf, bool) {
	if len(data) < 4 {
		return DomainLeaf{}, false
	}

	nameLen := binary.BigEndian.Uint32(data)
	data = data[4:]

	if uint64(len(data)) != uint64(nameLen)+32+32+8 {
		return DomainLeaf{}, false
	}

	var l DomainLeaf
	l.Name = string(data[:nameLen])
	data = data[nameLen:]

	copy(l.ObjectID[:], data[:32])
	data = data[32:]

	copy(l.Owner[:], data[:32])
	data = data[32:]

	l.ExpiryEpoch = binary.BigEndian.Uint64(data[:8])

	return l, true
}
