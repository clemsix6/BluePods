package main

import (
	"sync"
	"time"

	"BluePods/internal/consensus"
)

const (
	// tpsWindow is the rolling window over which transactions-per-second is computed.
	tpsWindow = 10 * time.Second

	// recentTxCap is how many recent committed transactions the dashboard keeps.
	recentTxCap = 20
)

// Stats accumulates transaction-derived counters from the committed stream. It is
// the single source for the node's total, failed, and TPS figures, read by the
// dashboard and the QUIC status handler. It deliberately holds no round, epoch,
// or peer count: those are read live from their authoritative sources.
type Stats struct {
	mu       sync.Mutex              // mu guards all fields below
	totalTx  uint64                  // totalTx is every committed transaction seen
	failedTx uint64                  // failedTx is the subset that did not apply
	window   []time.Time             // window holds commit times within tpsWindow
	recent   []consensus.CommittedTx // recent is a ring of the last recentTxCap commits
	now      func() time.Time        // now is the clock, injectable for tests
}

// newStats creates an empty Stats backed by the wall clock.
func newStats() *Stats {
	return &Stats{now: time.Now}
}

// record folds one committed transaction into the counters and the TPS window.
func (s *Stats) record(tx consensus.CommittedTx) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.totalTx++
	if !tx.Success {
		s.failedTx++
	}

	s.window = append(s.window, s.now())
	s.pruneWindow()

	s.recent = append(s.recent, tx)
	if len(s.recent) > recentTxCap {
		s.recent = s.recent[len(s.recent)-recentTxCap:]
	}
}

// pruneWindow drops commit times older than tpsWindow. The caller holds mu.
func (s *Stats) pruneWindow() {
	cutoff := s.now().Add(-tpsWindow)
	i := 0
	for i < len(s.window) && s.window[i].Before(cutoff) {
		i++
	}
	s.window = s.window[i:]
}

// TotalTx returns the total committed transactions seen.
func (s *Stats) TotalTx() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.totalTx
}

// FailedTx returns the committed transactions that did not apply.
func (s *Stats) FailedTx() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.failedTx
}

// TPS returns the current transactions-per-second over the rolling window.
func (s *Stats) TPS() float64 {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.pruneWindow()

	return float64(len(s.window)) / tpsWindow.Seconds()
}

// Recent returns a copy of the recent committed transactions, oldest first.
func (s *Stats) Recent() []consensus.CommittedTx {
	s.mu.Lock()
	defer s.mu.Unlock()

	out := make([]consensus.CommittedTx, len(s.recent))
	copy(out, s.recent)

	return out
}
