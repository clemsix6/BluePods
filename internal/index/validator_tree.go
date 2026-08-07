package index

import (
	"encoding/binary"

	"github.com/zeebo/blake3"
)

const (
	// ValidatorActive marks a leaf whose validator currently carries quorum
	// weight.
	ValidatorActive byte = 0

	// ValidatorJailed marks a leaf whose validator is jailed: present in the
	// snapshot for continuity, but contributing zero quorum weight.
	ValidatorJailed byte = 1
)

// ValidatorLeaf is the validator tree's leaf record: one validator's
// quorum-relevant snapshot at the moment the tree was last rebuilt.
type ValidatorLeaf struct {
	Pubkey      [32]byte // Pubkey is the validator's Ed25519 public key
	CappedStake uint64   // CappedStake is the validator's voting weight after the per-validator cap
	BLSKey      [48]byte // BLSKey is the validator's BLS public key for attestation signing
	Status      byte     // Status is ValidatorActive or ValidatorJailed
}

// ValidatorTree is a flat SMT keyed by blake3(pubkey), rebuilt wholesale from
// a snapshot rather than incrementally edited. During the genesis epoch,
// which freezes no snapshot, a caller rebuilds it from the live registration
// set as it changes; from the first epoch boundary on, a caller rebuilds it
// only from the frozen epoch snapshot (spec §4). The tree itself carries no
// opinion about which mode is in effect — Rebuild is the same operation
// either way, and is what makes a rebuild from a snapshot deterministic
// regardless of the snapshot's iteration order.
type ValidatorTree struct {
	smt *SMT

	// values retains the encoded leaf of every entry the last Rebuild took, in
	// no meaningful order. The SMT itself cannot be enumerated, and a quorum
	// verifier needs the WHOLE set rather than any subset of it: the capped
	// total is the denominator of the 2/3 test, and no inclusion proof
	// authenticates a total. Retaining what Rebuild already encoded is what
	// lets Values serve the set a client rebuilds Root() from.
	values [][]byte
}

// NewValidatorTree returns an empty validator tree.
func NewValidatorTree() *ValidatorTree {
	return &ValidatorTree{smt: New()}
}

// Rebuild replaces the tree's contents wholesale with entries: any leaf from
// a prior Rebuild is gone unless entries repeats it. The result depends only
// on the entries set, never on its order — the same guarantee the underlying
// SMT already provides for Insert order.
func (t *ValidatorTree) Rebuild(entries []ValidatorLeaf) {
	fresh := New()
	values := make([][]byte, 0, len(entries))

	for _, e := range entries {
		value := encodeValidatorLeaf(e)
		fresh.Insert(e.Pubkey[:], value)
		values = append(values, value)
	}

	t.smt = fresh
	t.values = values
}

// Values returns the encoded leaves the tree currently holds, exactly as it
// hashed them, so a verifier folds the identical bytes rather than re-encoding
// a decoded copy. Rebuilding a tree from them (ValidatorRootOf over
// DecodeValidatorLeaf) reproduces Root(), which is what authenticates the set
// as complete rather than merely as members.
func (t *ValidatorTree) Values() [][]byte {
	out := make([][]byte, len(t.values))
	for i, v := range t.values {
		out[i] = append([]byte(nil), v...)
	}

	return out
}

// ValidatorRootOf returns the root a validator tree holding exactly entries
// hashes to, without keeping the tree. It is the epoch's validator-set
// commitment as a single value: a pure function of the frozen holder snapshot,
// so every node that froze the same epoch computes the same 32 bytes, and a
// joiner can check an out-of-band checkpoint against the set it imported
// before weighing any quorum with it.
func ValidatorRootOf(entries []ValidatorLeaf) [32]byte {
	tree := NewValidatorTree()
	tree.Rebuild(entries)

	return tree.Root()
}

// Get returns the leaf stored for pubkey and whether one exists.
func (t *ValidatorTree) Get(pubkey [32]byte) (ValidatorLeaf, bool) {
	value, ok := t.smt.Get(pubkey[:])
	if !ok {
		return ValidatorLeaf{}, false
	}

	return DecodeValidatorLeaf(value)
}

// Root returns the tree's current Merkle root.
func (t *ValidatorTree) Root() [32]byte {
	return t.smt.Root()
}

// Prove returns an inclusion or absence proof for pubkey against Root().
func (t *ValidatorTree) Prove(pubkey [32]byte) Proof {
	return t.smt.Prove(pubkey[:])
}

// encodeValidatorLeaf serializes a validator leaf as Pubkey (32 bytes) ||
// CappedStake (8 bytes big-endian) || BLSKey (48 bytes) || Status (1 byte).
func encodeValidatorLeaf(l ValidatorLeaf) []byte {
	buf := make([]byte, 0, 32+8+48+1)
	buf = append(buf, l.Pubkey[:]...)
	buf = binary.BigEndian.AppendUint64(buf, l.CappedStake)
	buf = append(buf, l.BLSKey[:]...)
	buf = append(buf, l.Status)
	return buf
}

// DecodeValidatorLeaf reverses encodeValidatorLeaf, reporting ok=false on any
// length mismatch. Exported because a served validator set carries the raw
// leaf values, and reading the pubkey and capped stake out of the bytes the
// root covers is exactly what makes a client's quorum weighing authenticated.
func DecodeValidatorLeaf(data []byte) (ValidatorLeaf, bool) {
	if len(data) != 32+8+48+1 {
		return ValidatorLeaf{}, false
	}

	var l ValidatorLeaf
	copy(l.Pubkey[:], data[:32])
	data = data[32:]

	l.CappedStake = binary.BigEndian.Uint64(data[:8])
	data = data[8:]

	copy(l.BLSKey[:], data[:48])
	data = data[48:]

	l.Status = data[0]

	return l, true
}

// CombinedRoot folds the four sub-roots into the one value every vertex
// anchors: blake3(domain || parent || children || validator). The order is
// fixed so any external verifier recomputing it from the four proven
// sub-roots agrees byte for byte.
func CombinedRoot(domain, parent, children, validator [32]byte) [32]byte {
	var buf [128]byte
	copy(buf[0:32], domain[:])
	copy(buf[32:64], parent[:])
	copy(buf[64:96], children[:])
	copy(buf[96:128], validator[:])

	return blake3.Sum256(buf[:])
}
