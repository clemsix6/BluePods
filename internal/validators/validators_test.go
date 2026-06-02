package validators

import "testing"

func TestValidatorSetAddAndLen(t *testing.T) {
	vs := NewValidatorSet([]Hash{{1}, {2}, {3}})
	if vs.Len() != 3 {
		t.Fatalf("want 3, got %d", vs.Len())
	}
	vs.Add(Hash{4}, "addr:9000", [48]byte{0xAB})
	if vs.Len() != 4 {
		t.Fatalf("want 4 after Add, got %d", vs.Len())
	}
}

// TestStakeFieldsZeroValue checks that a freshly added validator has zero stake
// and is not jailed, and that the Get/All copies carry the stake/jail fields.
func TestStakeFieldsZeroValue(t *testing.T) {
	vs := NewValidatorSet(nil)
	vs.Add(Hash{1}, "addr:9000", [48]byte{0xAB})

	got := vs.Get(Hash{1})
	if got == nil {
		t.Fatal("validator not found")
	}
	if got.SelfStake != 0 || got.DelegatedTotal != 0 || got.Jailed {
		t.Fatalf("want zero stake and not jailed, got self=%d delegated=%d jailed=%v",
			got.SelfStake, got.DelegatedTotal, got.Jailed)
	}

	all := vs.All()
	if len(all) != 1 {
		t.Fatalf("want 1 validator in All, got %d", len(all))
	}
	if all[0].SelfStake != 0 || all[0].DelegatedTotal != 0 || all[0].Jailed {
		t.Fatalf("All copy should carry zero stake and not jailed")
	}
}
