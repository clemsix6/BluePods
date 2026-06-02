package main

import "testing"

// TestPickReserveVersionSerialSplits verifies that back-to-back faucet splits,
// while the committed reserve version has not yet advanced, each receive a
// distinct increasing version so none lose the reserve version race.
func TestPickReserveVersionSerialSplits(t *testing.T) {
	n := &Node{}

	// The reserve coin is committed at version 5; the next three splits arrive
	// before any of them commits, so state still reports 5 each time.
	const committed uint64 = 5

	v0 := n.pickReserveVersion(committed)
	v1 := n.pickReserveVersion(committed)
	v2 := n.pickReserveVersion(committed)

	if v0 != 5 || v1 != 6 || v2 != 7 {
		t.Fatalf("expected versions 5,6,7 for serial splits, got %d,%d,%d", v0, v1, v2)
	}
}

// TestPickReserveVersionSelfHealsFromState verifies the in-memory counter catches
// up when the committed version has advanced past it (e.g. after a restart or
// when splits have actually committed), rather than reusing a stale lower version.
func TestPickReserveVersionSelfHealsFromState(t *testing.T) {
	n := &Node{}

	// First split off a freshly committed version 2.
	if got := n.pickReserveVersion(2); got != 2 {
		t.Fatalf("first split: expected version 2, got %d", got)
	}

	// State now reports the split committed and the version advanced to 10. The
	// next split must follow committed state, not the stale in-memory 3.
	if got := n.pickReserveVersion(10); got != 10 {
		t.Fatalf("after commit advanced state: expected version 10, got %d", got)
	}

	if got := n.pickReserveVersion(10); got != 11 {
		t.Fatalf("subsequent in-flight split: expected version 11, got %d", got)
	}
}
