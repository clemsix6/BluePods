package state

import (
	"testing"
)

// TestTotalSupply_FreshState confirms a freshly created State starts with zero
// total supply.
func TestTotalSupply_FreshState(t *testing.T) {
	db := newTestStorage(t)
	s := New(db, nil)

	if got := s.TotalSupply(); got != 0 {
		t.Errorf("fresh TotalSupply: got %d, want 0", got)
	}
}

// TestAddSupply_Accumulates confirms AddSupply increases the counter.
func TestAddSupply_Accumulates(t *testing.T) {
	db := newTestStorage(t)
	s := New(db, nil)

	s.AddSupply(100)

	if got := s.TotalSupply(); got != 100 {
		t.Errorf("after AddSupply(100): got %d, want 100", got)
	}
}

// TestSubSupply_Decrements confirms SubSupply reduces the counter.
func TestSubSupply_Decrements(t *testing.T) {
	db := newTestStorage(t)
	s := New(db, nil)

	s.AddSupply(100)
	s.SubSupply(30)

	if got := s.TotalSupply(); got != 70 {
		t.Errorf("after SubSupply(30): got %d, want 70", got)
	}
}

// TestSubSupply_FloorsAtZero confirms SubSupply never underflows below zero.
func TestSubSupply_FloorsAtZero(t *testing.T) {
	db := newTestStorage(t)
	s := New(db, nil)

	s.AddSupply(70)
	s.SubSupply(1000)

	if got := s.TotalSupply(); got != 0 {
		t.Errorf("after SubSupply(1000) on 70: got %d, want 0", got)
	}
}

// TestSetTotalSupply_Overrides confirms SetTotalSupply replaces the counter.
func TestSetTotalSupply_Overrides(t *testing.T) {
	db := newTestStorage(t)
	s := New(db, nil)

	s.AddSupply(100)
	s.SetTotalSupply(500)

	if got := s.TotalSupply(); got != 500 {
		t.Errorf("after SetTotalSupply(500): got %d, want 500", got)
	}
}

// TestTotalSupply_PersistsAcrossReopen confirms the counter is loaded from
// storage when a new State is opened on the same database.
func TestTotalSupply_PersistsAcrossReopen(t *testing.T) {
	db := newTestStorage(t)

	s := New(db, nil)
	s.SetTotalSupply(12345)

	reopened := New(db, nil)

	if got := reopened.TotalSupply(); got != 12345 {
		t.Errorf("reopened TotalSupply: got %d, want 12345", got)
	}
}
