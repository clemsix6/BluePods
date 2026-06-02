package consensus

import (
	"testing"

	"BluePods/internal/types"
)

// TestDelegationID checks the position ID is deterministic and distinct per pair.
func TestDelegationID(t *testing.T) {
	delegator := [32]byte{0x01}
	validator := [32]byte{0x02}

	a := DelegationID(delegator, validator)
	b := DelegationID(delegator, validator)
	if a != b {
		t.Fatalf("DelegationID is not deterministic: %x != %x", a[:4], b[:4])
	}

	// Distinct per delegator.
	other := DelegationID([32]byte{0x09}, validator)
	if a == other {
		t.Fatal("DelegationID must differ for a different delegator")
	}

	// Distinct per validator.
	otherVal := DelegationID(delegator, [32]byte{0x09})
	if a == otherVal {
		t.Fatal("DelegationID must differ for a different validator")
	}

	// Swapping the pair must not collide.
	if a == DelegationID(validator, delegator) {
		t.Fatal("DelegationID must be order-sensitive between delegator and validator")
	}
}

// TestDelegationContentRoundTrip checks the content codec round-trips.
func TestDelegationContentRoundTrip(t *testing.T) {
	validator := [32]byte{0xAB, 0xCD}
	amount := uint64(123456789)

	content := encodeDelegationContent(validator, amount)

	gotValidator, gotAmount, ok := decodeDelegationContent(content)
	if !ok {
		t.Fatal("decodeDelegationContent failed on valid content")
	}
	if gotValidator != validator {
		t.Fatalf("validator round-trip = %x, want %x", gotValidator[:4], validator[:4])
	}
	if gotAmount != amount {
		t.Fatalf("amount round-trip = %d, want %d", gotAmount, amount)
	}
}

// TestDecodeDelegationContentRejectsShort checks malformed content is rejected.
func TestDecodeDelegationContentRejectsShort(t *testing.T) {
	if _, _, ok := decodeDelegationContent(make([]byte, 39)); ok {
		t.Fatal("content shorter than 40 bytes must be rejected")
	}
}

// TestBuildDelegationObject checks the position object carries the delegator as
// owner, the deterministic ID, and the decodable content.
func TestBuildDelegationObject(t *testing.T) {
	delegator := [32]byte{0x11}
	validator := [32]byte{0x22}
	amount := uint64(500)

	data := buildDelegationObject(delegator, validator, amount)

	obj := types.GetRootAsObject(data, 0)

	wantID := DelegationID(delegator, validator)
	if got := obj.IdBytes(); string(got) != string(wantID[:]) {
		t.Fatalf("object ID = %x, want %x", got[:4], wantID[:4])
	}
	if got := obj.OwnerBytes(); string(got) != string(delegator[:]) {
		t.Fatalf("owner = %x, want delegator %x", got[:4], delegator[:4])
	}

	gotValidator, gotAmount, ok := decodeDelegationContent(obj.ContentBytes())
	if !ok || gotValidator != validator || gotAmount != amount {
		t.Fatalf("content decode = (%x, %d, %v), want (%x, %d, true)", gotValidator[:4], gotAmount, ok, validator[:4], amount)
	}
}
