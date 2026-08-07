package index

import "testing"

// snapshotEntries returns a small fixed validator snapshot used by several
// tests below.
func snapshotEntries() []ValidatorLeaf {
	return []ValidatorLeaf{
		{Pubkey: [32]byte{0x01}, CappedStake: 100, BLSKey: [48]byte{0xAA}, Status: ValidatorActive},
		{Pubkey: [32]byte{0x02}, CappedStake: 200, BLSKey: [48]byte{0xBB}, Status: ValidatorActive},
		{Pubkey: [32]byte{0x03}, CappedStake: 0, BLSKey: [48]byte{0xCC}, Status: ValidatorJailed},
	}
}

// TestValidatorTree_RebuildDeterministic verifies that rebuilding from the
// same snapshot in a different order yields the same root — a client
// re-deriving the tree from a validator list obtained in any order must
// agree with the anchored value.
func TestValidatorTree_RebuildDeterministic(t *testing.T) {
	entries := snapshotEntries()
	shuffled := []ValidatorLeaf{entries[2], entries[0], entries[1]}

	a := NewValidatorTree()
	a.Rebuild(entries)

	b := NewValidatorTree()
	b.Rebuild(shuffled)

	if a.Root() != b.Root() {
		t.Errorf("roots differ by rebuild order: %x != %x", a.Root(), b.Root())
	}
}

// TestValidatorTree_RebuildReplacesWholesale verifies a second Rebuild call
// drops any leaf the new entries slice does not repeat.
func TestValidatorTree_RebuildReplacesWholesale(t *testing.T) {
	tree := NewValidatorTree()
	tree.Rebuild(snapshotEntries())

	tree.Rebuild([]ValidatorLeaf{{Pubkey: [32]byte{0x09}, CappedStake: 5, Status: ValidatorActive}})

	if _, ok := tree.Get([32]byte{0x01}); ok {
		t.Error("leaf from the first Rebuild survived a wholesale second Rebuild")
	}
	if _, ok := tree.Get([32]byte{0x09}); !ok {
		t.Error("leaf from the second Rebuild is missing")
	}
}

// TestValidatorTree_GetRoundTrip verifies a rebuilt leaf decodes back exactly.
func TestValidatorTree_GetRoundTrip(t *testing.T) {
	tree := NewValidatorTree()
	tree.Rebuild(snapshotEntries())

	got, ok := tree.Get([32]byte{0x02})
	if !ok {
		t.Fatal("expected leaf to be found")
	}

	want := snapshotEntries()[1]
	if got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

// --- CombinedRoot ---

// TestCombinedRoot_ChangesWithEverySubRoot verifies the combined root moves
// when ANY one of the four sub-roots changes, and that permuting which root
// occupies which slot changes the result (the domain-separated ordering
// matters, not just the multiset of inputs).
func TestCombinedRoot_ChangesWithEverySubRoot(t *testing.T) {
	d := [32]byte{0x01}
	p := [32]byte{0x02}
	c := [32]byte{0x03}
	v := [32]byte{0x04}

	base := CombinedRoot(d, p, c, v)

	cases := []struct {
		name string
		root [32]byte
	}{
		{"domain", CombinedRoot([32]byte{0xFF}, p, c, v)},
		{"parent", CombinedRoot(d, [32]byte{0xFF}, c, v)},
		{"children", CombinedRoot(d, p, [32]byte{0xFF}, v)},
		{"validator", CombinedRoot(d, p, c, [32]byte{0xFF})},
	}

	for _, tc := range cases {
		if tc.root == base {
			t.Errorf("combined root unchanged after perturbing %s sub-root", tc.name)
		}
	}

	if swapped := CombinedRoot(p, d, c, v); swapped == base {
		t.Error("combined root unchanged after swapping domain and parent slots")
	}
}

// TestCombinedRoot_Deterministic verifies the same four sub-roots always fold
// to the same combined root.
func TestCombinedRoot_Deterministic(t *testing.T) {
	d := [32]byte{0x01}
	p := [32]byte{0x02}
	c := [32]byte{0x03}
	v := [32]byte{0x04}

	if CombinedRoot(d, p, c, v) != CombinedRoot(d, p, c, v) {
		t.Error("CombinedRoot is not deterministic for identical inputs")
	}
}

// TestValidatorTree_ValuesRebuildTheRoot verifies the served leaf set is a
// complete, self-authenticating description of the tree: decoding Values and
// rebuilding from them reproduces Root(), which is the check a light client
// runs before weighing a quorum with those stakes.
func TestValidatorTree_ValuesRebuildTheRoot(t *testing.T) {
	tree := NewValidatorTree()
	tree.Rebuild(snapshotEntries())

	values := tree.Values()
	if len(values) != len(snapshotEntries()) {
		t.Fatalf("values: got %d leaves, want %d", len(values), len(snapshotEntries()))
	}

	decoded := make([]ValidatorLeaf, 0, len(values))
	for i, v := range values {
		leaf, ok := DecodeValidatorLeaf(v)
		if !ok {
			t.Fatalf("leaf %d did not decode: %x", i, v)
		}
		decoded = append(decoded, leaf)
	}

	if root := ValidatorRootOf(decoded); root != tree.Root() {
		t.Errorf("rebuilt root %x != tree root %x", root, tree.Root())
	}
}

// TestValidatorTree_ValuesFollowRebuild verifies a rebuild replaces the served
// set wholesale, so a client can never be handed a stale epoch's members
// beside the current root.
func TestValidatorTree_ValuesFollowRebuild(t *testing.T) {
	tree := NewValidatorTree()
	tree.Rebuild(snapshotEntries())
	tree.Rebuild([]ValidatorLeaf{{Pubkey: [32]byte{0x09}, CappedStake: 5, Status: ValidatorActive}})

	values := tree.Values()
	if len(values) != 1 {
		t.Fatalf("values: got %d leaves, want 1", len(values))
	}

	leaf, ok := DecodeValidatorLeaf(values[0])
	if !ok || leaf.Pubkey != ([32]byte{0x09}) {
		t.Fatalf("stale leaf served: %+v", leaf)
	}
}
