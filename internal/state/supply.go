package state

import (
	"encoding/binary"
	"math"
)

// prefixSupply is the storage key for the persisted total-supply counter. The
// "m:" prefix marks it as consensus metadata, so it is excluded from object
// iteration and never leaks into the object snapshot.
var prefixSupply = []byte("m:supply")

// loadSupply reads the persisted total supply from storage into memory.
// A missing or malformed value leaves the counter at zero.
func (s *State) loadSupply() {
	if s.db == nil {
		return
	}

	data, err := s.db.Get(prefixSupply)
	if err != nil || len(data) < 8 {
		return
	}

	s.totalSupply = binary.BigEndian.Uint64(data[:8])
}

// TotalSupply returns the current protocol-maintained total token supply.
func (s *State) TotalSupply() uint64 {
	s.supplyMu.Lock()
	defer s.supplyMu.Unlock()

	return s.totalSupply
}

// SetTotalSupply overwrites the total supply, persisting the new value.
// Used at genesis seeding and snapshot restore.
func (s *State) SetTotalSupply(supply uint64) {
	s.supplyMu.Lock()
	defer s.supplyMu.Unlock()

	s.totalSupply = supply
	s.persistSupplyLocked()
}

// AddSupply increases the total supply, saturating at MaxUint64.
func (s *State) AddSupply(amount uint64) {
	s.supplyMu.Lock()
	defer s.supplyMu.Unlock()

	if s.totalSupply > math.MaxUint64-amount {
		s.totalSupply = math.MaxUint64
	} else {
		s.totalSupply += amount
	}

	s.persistSupplyLocked()
}

// SubSupply decreases the total supply, flooring at zero.
func (s *State) SubSupply(amount uint64) {
	s.supplyMu.Lock()
	defer s.supplyMu.Unlock()

	if amount > s.totalSupply {
		s.totalSupply = 0
	} else {
		s.totalSupply -= amount
	}

	s.persistSupplyLocked()
}

// persistSupplyLocked writes the in-memory total supply to storage.
// The caller must hold supplyMu.
func (s *State) persistSupplyLocked() {
	if s.db == nil {
		return
	}

	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], s.totalSupply)
	_ = s.db.Set(prefixSupply, buf[:])
}
