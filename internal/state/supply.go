package state

import (
	"encoding/binary"
	"math"
)

// prefixSupply is the storage key for the persisted total-supply counter. The
// "m:" prefix marks it as consensus metadata, so it is excluded from object
// iteration and never leaks into the object snapshot.
var prefixSupply = []byte("m:supply")

// prefixCoinsTotal is the storage key for the persisted coins_total counter,
// next to prefixSupply under the same "m:" consensus-metadata prefix.
var prefixCoinsTotal = []byte("m:coins_total")

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

// loadCoinsTotal reads the persisted coins_total counter from storage into
// memory. A missing or malformed value leaves the counter at zero.
func (s *State) loadCoinsTotal() {
	if s.db == nil {
		return
	}

	data, err := s.db.Get(prefixCoinsTotal)
	if err != nil || len(data) < 8 {
		return
	}

	s.coinsTotal = binary.BigEndian.Uint64(data[:8])
}

// CoinsTotal returns the current protocol-maintained sum of coin balances.
func (s *State) CoinsTotal() uint64 {
	s.coinsTotalMu.Lock()
	defer s.coinsTotalMu.Unlock()

	return s.coinsTotal
}

// SetCoinsTotal overwrites coins_total, persisting the new value. Used at
// genesis seeding and snapshot restore.
func (s *State) SetCoinsTotal(total uint64) {
	s.coinsTotalMu.Lock()
	defer s.coinsTotalMu.Unlock()

	s.coinsTotal = total
	s.persistCoinsTotalLocked()
}

// AddCoins increases coins_total, saturating at MaxUint64.
func (s *State) AddCoins(amount uint64) {
	s.coinsTotalMu.Lock()
	defer s.coinsTotalMu.Unlock()

	if s.coinsTotal > math.MaxUint64-amount {
		s.coinsTotal = math.MaxUint64
	} else {
		s.coinsTotal += amount
	}

	s.persistCoinsTotalLocked()
}

// SubCoins decreases coins_total, flooring at zero.
func (s *State) SubCoins(amount uint64) {
	s.coinsTotalMu.Lock()
	defer s.coinsTotalMu.Unlock()

	if amount > s.coinsTotal {
		s.coinsTotal = 0
	} else {
		s.coinsTotal -= amount
	}

	s.persistCoinsTotalLocked()
}

// persistCoinsTotalLocked writes the in-memory coins_total to storage.
// The caller must hold coinsTotalMu.
func (s *State) persistCoinsTotalLocked() {
	if s.db == nil {
		return
	}

	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], s.coinsTotal)
	_ = s.db.Set(prefixCoinsTotal, buf[:])
}
