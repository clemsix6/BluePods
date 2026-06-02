package state

import (
	"encoding/binary"
	"testing"

	flatbuffers "github.com/google/flatbuffers/go"

	"BluePods/internal/types"
)

// buildPositionObject builds a serialized delegation-position object: owner is
// the delegator, content is validator(32) || amount(8 LE).
func buildPositionObject(id, delegator, validator [32]byte, amount uint64) []byte {
	content := make([]byte, 40)
	copy(content[:32], validator[:])
	binary.LittleEndian.PutUint64(content[32:], amount)

	b := flatbuffers.NewBuilder(256)
	idVec := b.CreateByteVector(id[:])
	ownerVec := b.CreateByteVector(delegator[:])
	contentVec := b.CreateByteVector(content)
	types.ObjectStart(b)
	types.ObjectAddId(b, idVec)
	types.ObjectAddVersion(b, 0)
	types.ObjectAddOwner(b, ownerVec)
	types.ObjectAddReplication(b, 0)
	types.ObjectAddContent(b, contentVec)
	types.ObjectAddFees(b, 0)
	b.Finish(types.ObjectEnd(b))

	return b.FinishedBytes()
}

// buildPlainCoin builds a serialized coin object (8-byte balance content) so the
// enumerator must skip it (content length != 40).
func buildPlainCoin(id, owner [32]byte, balance uint64) []byte {
	content := make([]byte, 8)
	binary.LittleEndian.PutUint64(content, balance)

	b := flatbuffers.NewBuilder(256)
	idVec := b.CreateByteVector(id[:])
	ownerVec := b.CreateByteVector(owner[:])
	contentVec := b.CreateByteVector(content)
	types.ObjectStart(b)
	types.ObjectAddId(b, idVec)
	types.ObjectAddVersion(b, 1)
	types.ObjectAddOwner(b, ownerVec)
	types.ObjectAddReplication(b, 0)
	types.ObjectAddContent(b, contentVec)
	types.ObjectAddFees(b, 0)
	b.Finish(types.ObjectEnd(b))

	return b.FinishedBytes()
}

// TestDelegationsFor returns only the positions targeting the queried validator.
func TestDelegationsFor(t *testing.T) {
	db := newTestStorage(t)
	s := New(db, nil)

	v := [32]byte{0xAA}
	otherVal := [32]byte{0xBB}
	d1 := [32]byte{0x01}
	d2 := [32]byte{0x02}
	d3 := [32]byte{0x03}

	// Two positions targeting v, one targeting another validator, plus a coin.
	s.SetObject(buildPositionObject([32]byte{0x10}, d1, v, 100))
	s.SetObject(buildPositionObject([32]byte{0x11}, d2, v, 300))
	s.SetObject(buildPositionObject([32]byte{0x12}, d3, otherVal, 500))
	s.SetObject(buildPlainCoin([32]byte{0x13}, d1, 9999))

	entries := s.DelegationsFor(v)
	if len(entries) != 2 {
		t.Fatalf("DelegationsFor(v) returned %d entries, want 2", len(entries))
	}

	byOwner := map[[32]byte]uint64{}
	for _, e := range entries {
		byOwner[e.Delegator] = e.Amount
	}
	if byOwner[d1] != 100 || byOwner[d2] != 300 {
		t.Fatalf("amounts = %v, want {d1:100, d2:300}", byOwner)
	}
	if _, ok := byOwner[d3]; ok {
		t.Fatal("position targeting another validator must be excluded")
	}
}

// TestDelegationsFor_Empty returns nil when no positions target the validator.
func TestDelegationsFor_Empty(t *testing.T) {
	db := newTestStorage(t)
	s := New(db, nil)

	s.SetObject(buildPlainCoin([32]byte{0x20}, [32]byte{0x01}, 100))

	if entries := s.DelegationsFor([32]byte{0xAA}); len(entries) != 0 {
		t.Fatalf("DelegationsFor on no-positions store = %d, want 0", len(entries))
	}
}
