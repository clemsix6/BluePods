package index

import (
	"bytes"
	"fmt"
	"math"
	"math/rand"
	"testing"

	"github.com/zeebo/blake3"
)

// buildRandomTree returns a tree of n entries with keys key-0..key-(n-1) and deterministic
// random values, alongside the keys and values in insertion order.
func buildRandomTree(seed int64, n int) (*SMT, [][]byte, [][]byte) {
	rng := rand.New(rand.NewSource(seed))
	tr := New()
	var keys, vals [][]byte
	for i := 0; i < n; i++ {
		k := []byte(fmt.Sprintf("key-%d", i))
		v := make([]byte, 20)
		rng.Read(v)
		tr.Insert(k, v)
		keys = append(keys, k)
		vals = append(vals, v)
	}
	return tr, keys, vals
}

// proofsEqual reports whether two proofs carry the same siblings and leaf.
func proofsEqual(a, b Proof) bool {
	if len(a.Siblings) != len(b.Siblings) {
		return false
	}
	for i := range a.Siblings {
		if a.Siblings[i] != b.Siblings[i] {
			return false
		}
	}
	return bytes.Equal(a.Leaf, b.Leaf)
}

// TestInclusionVerifies checks that every present key produces a verifying inclusion proof.
func TestInclusionVerifies(t *testing.T) {
	tr, keys, vals := buildRandomTree(1, 500)
	root := tr.Root()
	for i, k := range keys {
		if !Verify(root, k, vals[i], tr.Prove(k)) {
			t.Fatalf("inclusion proof failed for key %d", i)
		}
	}
}

// TestInclusionRejectsTamperedValueAndRoot checks that a tampered value or a wrong root
// both fail an otherwise valid inclusion proof.
func TestInclusionRejectsTamperedValueAndRoot(t *testing.T) {
	tr, keys, vals := buildRandomTree(2, 200)
	root := tr.Root()
	k, v := keys[10], vals[10]
	p := tr.Prove(k)

	if !Verify(root, k, v, p) {
		t.Fatal("sanity: valid inclusion proof should verify")
	}
	bad := append([]byte(nil), v...)
	bad[0] ^= 0xFF
	if Verify(root, k, bad, p) {
		t.Fatal("tampered value verified")
	}
	wrongRoot := root
	wrongRoot[0] ^= 0xFF
	if Verify(wrongRoot, k, v, p) {
		t.Fatal("wrong root verified")
	}
}

// TestTruncatedSiblingsFails checks that dropping a sibling from either end of the path
// breaks verification.
func TestTruncatedSiblingsFails(t *testing.T) {
	tr, keys, vals := buildRandomTree(3, 500)
	root := tr.Root()

	var k, v []byte
	var p Proof
	for i := range keys {
		if pi := tr.Prove(keys[i]); len(pi.Siblings) >= 2 {
			k, v, p = keys[i], vals[i], pi
			break
		}
	}
	if k == nil {
		t.Fatal("no multi-sibling proof found")
	}
	if Verify(root, k, v, Proof{Siblings: p.Siblings[1:], Leaf: p.Leaf}) {
		t.Fatal("proof with the deepest sibling dropped verified")
	}
	if Verify(root, k, v, Proof{Siblings: p.Siblings[:len(p.Siblings)-1], Leaf: p.Leaf}) {
		t.Fatal("proof with the shallowest sibling dropped verified")
	}
}

// TestAbsenceEmptySubtree checks that a missing key whose position falls in an empty
// subtree produces a verifying absence proof with no occupying leaf.
func TestAbsenceEmptySubtree(t *testing.T) {
	tr, _, _ := buildRandomTree(4, 300)
	root := tr.Root()

	var k []byte
	var p Proof
	for i := 0; i < 100000; i++ {
		cand := []byte(fmt.Sprintf("absent-%d", i))
		if _, ok := tr.Get(cand); ok {
			continue
		}
		if pc := tr.Prove(cand); len(pc.Leaf) == 0 {
			k, p = cand, pc
			break
		}
	}
	if k == nil {
		t.Fatal("no empty-subtree absence case found")
	}
	if !Verify(root, k, nil, p) {
		t.Fatal("empty-subtree absence proof failed to verify")
	}
	if Verify(root, k, []byte("anything"), p) {
		t.Fatal("absence proof verified as an inclusion")
	}
}

// TestAbsenceOtherLeaf crafts a missing key whose position is occupied by a different
// compressed leaf and checks the resulting absence proof verifies.
func TestAbsenceOtherLeaf(t *testing.T) {
	occupant, query := twoKeysSharingFirstByte(t)
	tr := New()
	tr.Insert(occupant, []byte("occupant-value"))
	addDisjointKeys(t, tr, occupant, 40)
	root := tr.Root()

	if _, ok := tr.Get(query); ok {
		t.Fatal("query key unexpectedly present")
	}
	p := tr.Prove(query)
	if len(p.Leaf) != 64 {
		t.Fatalf("expected other-leaf proof (64-byte leaf), got %d", len(p.Leaf))
	}
	var occKey [32]byte
	copy(occKey[:], p.Leaf[:32])
	if occKey != blake3.Sum256(occupant) {
		t.Fatal("occupying leaf is not the occupant")
	}
	if occKey == blake3.Sum256(query) {
		t.Fatal("occupying leaf equals the query key")
	}
	if !Verify(root, query, nil, p) {
		t.Fatal("other-leaf absence proof failed to verify")
	}
	if !Verify(root, occupant, []byte("occupant-value"), tr.Prove(occupant)) {
		t.Fatal("occupant should still prove present")
	}
}

// TestProofSizeLogarithmic checks that proof sizes scale with log2 of the entry count.
func TestProofSizeLogarithmic(t *testing.T) {
	const n = 4096
	tr, keys, _ := buildRandomTree(9, n)
	tr.Root()

	total, max := 0, 0
	for _, k := range keys {
		s := len(tr.Prove(k).Siblings)
		total += s
		if s > max {
			max = s
		}
	}
	avg := float64(total) / float64(n)
	log2n := math.Log2(n)
	if avg < log2n-4 || avg > 2*log2n+8 {
		t.Fatalf("average proof size %.1f is not ~log2(%d)=%.1f", avg, n, log2n)
	}
	if max > 64 {
		t.Fatalf("max proof size %d is unexpectedly deep", max)
	}
}

// TestProofSerializationRoundTrip checks that inclusion and both absence proof shapes
// survive a serialize/deserialize round trip and still verify.
func TestProofSerializationRoundTrip(t *testing.T) {
	tr, keys, vals := buildRandomTree(5, 300)
	root := tr.Root()

	inclusion := tr.Prove(keys[3])
	empty := firstAbsence(t, tr, false)
	occupant, query := twoKeysSharingFirstByte(t)
	tr.Insert(occupant, []byte("v"))
	addDisjointKeys(t, tr, occupant, 20)
	root2 := tr.Root()
	other := tr.Prove(query)

	assertRoundTrip(t, inclusion)
	assertRoundTrip(t, empty.proof)
	assertRoundTrip(t, other)

	if !Verify(root, keys[3], vals[3], mustDeserialize(t, inclusion)) {
		t.Fatal("inclusion proof failed after round trip")
	}
	if !Verify(root2, query, nil, mustDeserialize(t, other)) {
		t.Fatal("other-leaf absence proof failed after round trip")
	}
}

// TestDeserializeRejectsTruncated checks that malformed serialized proofs are rejected.
func TestDeserializeRejectsTruncated(t *testing.T) {
	tr, keys, _ := buildRandomTree(6, 200)
	full := tr.Prove(keys[7]).Serialize()

	cases := [][]byte{
		nil,                    // empty input
		full[:2],               // partial count header
		full[:4],               // count header, no sibling data
		full[:6],               // mid-sibling truncation
		append(bytes.Clone(full), 0x00), // trailing byte, leaf length mismatch
	}
	for i, c := range cases {
		if _, err := DeserializeProof(c); err == nil {
			t.Fatalf("case %d: expected a deserialization error", i)
		}
	}
	if _, err := DeserializeProof(full); err != nil {
		t.Fatalf("full proof should deserialize: %v", err)
	}
}

// TestAbsenceRejectsSelfLeaf checks the one soundness check that the path fold alone does
// not provide: an absence proof forged from a present key's own honest inclusion path,
// relabeled as an other-leaf occupant equal to the queried key itself. The forged proof
// reuses the honest siblings and sets Leaf to the queried key's own keyHash || valueHash;
// its fold target is then identical to the honest inclusion proof's leaf hash, so without
// the otherKey == keyHash guard in absenceTarget (proof.go) it would fold to the real root
// and falsely prove a present key absent.
func TestAbsenceRejectsSelfLeaf(t *testing.T) {
	tr, keys, vals := buildRandomTree(11, 300)
	root := tr.Root()
	k, v := keys[42], vals[42]

	honest := tr.Prove(k)
	if len(honest.Leaf) != 0 {
		t.Fatal("sanity: a present key should yield an inclusion proof with an empty Leaf")
	}
	if !Verify(root, k, v, honest) {
		t.Fatal("sanity: honest inclusion proof should verify")
	}

	keyHash := blake3.Sum256(k)
	valueHash := blake3.Sum256(v)
	forged := Proof{
		Siblings: honest.Siblings,
		Leaf:     append(append([]byte{}, keyHash[:]...), valueHash[:]...),
	}
	if Verify(root, k, nil, forged) {
		t.Fatal("forged absence proof for a present key's own leaf verified")
	}
}

// TestOversizedSiblingsRejected checks the bounds guard at the top of Verify: a proof
// carrying more siblings than the tree is deep must be rejected outright, before any
// indexing into defaultHashes (which is sized treeDepth+1 and would be indexed out of
// range by an absence proof this long), and without panicking.
func TestOversizedSiblingsRejected(t *testing.T) {
	tr, keys, vals := buildRandomTree(12, 50)
	root := tr.Root()
	k, v := keys[0], vals[0]

	oversized := Proof{Siblings: make([][32]byte, treeDepth+1)}
	if Verify(root, k, v, oversized) {
		t.Fatal("oversized inclusion proof verified")
	}
	if Verify(root, k, nil, oversized) {
		t.Fatal("oversized absence proof verified")
	}
}

// absenceCase pairs a missing key with its absence proof.
type absenceCase struct {
	key   []byte
	proof Proof
}

// firstAbsence returns the first missing key whose proof matches the requested occupied
// shape: occupied=false selects the empty-subtree case, true selects the other-leaf case.
func firstAbsence(t *testing.T, tr *SMT, occupied bool) absenceCase {
	t.Helper()
	for i := 0; i < 100000; i++ {
		cand := []byte(fmt.Sprintf("miss-%d", i))
		if _, ok := tr.Get(cand); ok {
			continue
		}
		p := tr.Prove(cand)
		if (len(p.Leaf) != 0) == occupied {
			return absenceCase{cand, p}
		}
	}
	t.Fatalf("no absence case found (occupied=%v)", occupied)
	return absenceCase{}
}

// assertRoundTrip serializes then deserializes p and requires an identical proof.
func assertRoundTrip(t *testing.T, p Proof) {
	t.Helper()
	if !proofsEqual(p, mustDeserialize(t, p)) {
		t.Fatal("proof changed across a serialization round trip")
	}
}

// mustDeserialize serializes p, deserializes the bytes, and fails on any error.
func mustDeserialize(t *testing.T, p Proof) Proof {
	t.Helper()
	got, err := DeserializeProof(p.Serialize())
	if err != nil {
		t.Fatalf("deserialize: %v", err)
	}
	return got
}

// twoKeysSharingFirstByte returns two keys whose positions share their first byte, found
// by pigeonhole over 256 possible leading bytes.
func twoKeysSharingFirstByte(t *testing.T) (a, b []byte) {
	t.Helper()
	seen := map[byte][]byte{}
	for i := 0; i < 5_000_000; i++ {
		k := []byte(fmt.Sprintf("olf-%d", i))
		h := blake3.Sum256(k)
		if prev, ok := seen[h[0]]; ok {
			return prev, k
		}
		seen[h[0]] = k
	}
	t.Fatal("no shared-first-byte pair found")
	return nil, nil
}

// addDisjointKeys inserts n keys whose positions differ from occupant in the first byte, so
// occupant remains an isolated compressed leaf.
func addDisjointKeys(t *testing.T, tr *SMT, occupant []byte, n int) {
	t.Helper()
	occByte := blake3.Sum256(occupant)[0]
	added := 0
	for i := 0; added < n && i < 5_000_000; i++ {
		k := []byte(fmt.Sprintf("disjoint-%d", i))
		if blake3.Sum256(k)[0] == occByte {
			continue
		}
		tr.Insert(k, []byte(fmt.Sprintf("dv-%d", i)))
		added++
	}
	if added < n {
		t.Fatalf("added only %d disjoint keys", added)
	}
}
