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

// TestDelegatedMutators checks AddDelegated/SubDelegated including the floor-0
// behavior and the false return on an unknown pubkey.
func TestDelegatedMutators(t *testing.T) {
	vs := NewValidatorSet(nil)
	vs.Add(Hash{1}, "addr:9000", [48]byte{})

	if !vs.AddDelegated(Hash{1}, 100) {
		t.Fatal("AddDelegated on known validator should return true")
	}
	if got := vs.Get(Hash{1}).DelegatedTotal; got != 100 {
		t.Fatalf("want delegated 100, got %d", got)
	}

	if !vs.SubDelegated(Hash{1}, 30) {
		t.Fatal("SubDelegated on known validator should return true")
	}
	if got := vs.Get(Hash{1}).DelegatedTotal; got != 70 {
		t.Fatalf("want delegated 70, got %d", got)
	}

	// SubDelegated floors at 0, never underflows.
	vs.SubDelegated(Hash{1}, 1000)
	if got := vs.Get(Hash{1}).DelegatedTotal; got != 0 {
		t.Fatalf("want delegated floored at 0, got %d", got)
	}

	if vs.AddDelegated(Hash{9}, 1) || vs.SubDelegated(Hash{9}, 1) {
		t.Fatal("mutators on unknown pubkey should return false")
	}
}

// TestJailToggle checks Jail/Unjail toggle the flag and return false on unknown.
func TestJailToggle(t *testing.T) {
	vs := NewValidatorSet(nil)
	vs.Add(Hash{1}, "addr:9000", [48]byte{})

	if !vs.Jail(Hash{1}) {
		t.Fatal("Jail on known validator should return true")
	}
	if !vs.Get(Hash{1}).Jailed {
		t.Fatal("validator should be jailed")
	}

	if !vs.Unjail(Hash{1}) {
		t.Fatal("Unjail on known validator should return true")
	}
	if vs.Get(Hash{1}).Jailed {
		t.Fatal("validator should not be jailed")
	}

	if vs.Jail(Hash{9}) || vs.Unjail(Hash{9}) {
		t.Fatal("jail mutators on unknown pubkey should return false")
	}
}

// TestAddWithStake checks the stake-aware Add carries self-stake, delegated
// total, and the jail flag (used by the epoch holder snapshot rebuild).
func TestAddWithStake(t *testing.T) {
	vs := NewValidatorSet(nil)
	vs.AddWithStake(Hash{1}, "addr:9000", [48]byte{0xAB}, 500, 250, true)

	got := vs.Get(Hash{1})
	if got == nil {
		t.Fatal("validator not found")
	}
	if got.SelfStake != 500 || got.DelegatedTotal != 250 || !got.Jailed {
		t.Fatalf("want self=500 delegated=250 jailed=true, got self=%d delegated=%d jailed=%v",
			got.SelfStake, got.DelegatedTotal, got.Jailed)
	}
}
