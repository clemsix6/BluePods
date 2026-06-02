package main

import (
	"testing"
	"time"

	"BluePods/internal/consensus"
)

func TestStatsCountsAndTPS(t *testing.T) {
	base := time.Unix(1000, 0)
	now := base
	s := newStats()
	s.now = func() time.Time { return now }

	for i := 0; i < 30; i++ {
		ok := i%3 != 0 // every third fails
		s.record(consensus.CommittedTx{Success: ok})
	}

	if s.TotalTx() != 30 || s.FailedTx() != 10 {
		t.Fatalf("totals wrong: total=%d failed=%d", s.TotalTx(), s.FailedTx())
	}
	// 30 commits inside a 10s window -> 3.0 tps.
	if tps := s.TPS(); tps < 2.99 || tps > 3.01 {
		t.Fatalf("tps = %f, want ~3.0", tps)
	}

	now = base.Add(11 * time.Second) // slide past the window
	if tps := s.TPS(); tps != 0 {
		t.Fatalf("tps after window = %f, want 0", tps)
	}
	if len(s.Recent()) != recentTxCap {
		t.Fatalf("recent len = %d, want %d", len(s.Recent()), recentTxCap)
	}
}
