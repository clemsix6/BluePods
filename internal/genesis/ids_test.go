package genesis

import "testing"

// TestGenesisCoinID_Deterministic confirms the derivation is stable for a given
// owner across calls.
func TestGenesisCoinID_Deterministic(t *testing.T) {
	var owner [32]byte
	owner[0] = 0xAB

	first := GenesisCoinID(owner)
	second := GenesisCoinID(owner)

	if first != second {
		t.Fatalf("GenesisCoinID not deterministic: %x != %x", first, second)
	}
}

// TestGenesisCoinID_DistinctPerOwner confirms different owners get different IDs.
func TestGenesisCoinID_DistinctPerOwner(t *testing.T) {
	var a, b [32]byte
	a[0] = 0x01
	b[0] = 0x02

	if GenesisCoinID(a) == GenesisCoinID(b) {
		t.Fatal("distinct owners produced the same genesis coin ID")
	}
}
