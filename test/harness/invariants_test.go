package harness

import (
	"context"
	"encoding/hex"
	"errors"
	"testing"
	"time"

	"BluePods/internal/network"
	"BluePods/pkg/client"
)

// anchorLine builds a fabricated consensus.anchor.committed event line, for
// exercising checkRollback without a real node process. It reuses
// journal_test.go's eventLine helper (same package).
func anchorLine(t *testing.T, round uint64, anchor string) []byte {
	t.Helper()

	return eventLine(t, "consensus.anchor.committed", map[string]any{
		"round":    round,
		"anchor":   anchor,
		"producer": "aa",
		"vertices": 1,
	})
}

// fabricatedNode creates a *Node with no live process, for tests that only
// need its Journal and Index (checkRollback is a pure function of these).
func fabricatedNode(t *testing.T, index int) *Node {
	t.Helper()

	n, err := newNode(index, t.TempDir(), "", "")
	if err != nil {
		t.Fatalf("fabricated node %d: %v", index, err)
	}

	return n
}

// TestCheckRollbackDetectsNonIncreasingRound asserts a node whose own anchor
// rounds fail to strictly increase within a segment is flagged.
func TestCheckRollbackDetectsNonIncreasingRound(t *testing.T) {
	n := fabricatedNode(t, 0)

	if err := n.Journal().Append(anchorLine(t, 5, "aaaa")); err != nil {
		t.Fatalf("append: %v", err)
	}
	if err := n.Journal().Append(anchorLine(t, 5, "aaaa")); err != nil {
		t.Fatalf("append: %v", err)
	}

	if err := checkRollback([]*Node{n}); err == nil {
		t.Fatal("expected a rollback violation for a non-increasing round")
	}
}

// TestCheckRollbackDetectsContradiction asserts two nodes recording
// different anchors for the same round are flagged, even though each node's
// own sequence is internally strictly increasing.
func TestCheckRollbackDetectsContradiction(t *testing.T) {
	n0 := fabricatedNode(t, 0)
	n1 := fabricatedNode(t, 1)

	if err := n0.Journal().Append(anchorLine(t, 5, "aaaa")); err != nil {
		t.Fatalf("append: %v", err)
	}
	if err := n1.Journal().Append(anchorLine(t, 5, "bbbb")); err != nil {
		t.Fatalf("append: %v", err)
	}

	if err := checkRollback([]*Node{n0, n1}); err == nil {
		t.Fatal("expected a rollback violation for a contradicted anchor")
	}
}

// TestCheckRollbackAllowsIdenticalRedecision asserts a round re-decided
// identically (the legitimate post-crash re-decision case) is NOT flagged.
func TestCheckRollbackAllowsIdenticalRedecision(t *testing.T) {
	n0 := fabricatedNode(t, 0)
	n1 := fabricatedNode(t, 1)

	if err := n0.Journal().Append(anchorLine(t, 5, "aaaa")); err != nil {
		t.Fatalf("append: %v", err)
	}
	if err := n1.Journal().Append(anchorLine(t, 5, "aaaa")); err != nil {
		t.Fatalf("append: %v", err)
	}

	if err := checkRollback([]*Node{n0, n1}); err != nil {
		t.Fatalf("identical re-decision should not violate rollback: %v", err)
	}
}

// TestPollConvergenceReturnsImmediatelyOnAgreement asserts a sweep that
// already agrees on its first call returns right away with no error, without
// waiting for a poll tick.
func TestPollConvergenceReturnsImmediatelyOnAgreement(t *testing.T) {
	want := map[int]network.FingerprintResponse{0: {Round: 3}, 1: {Round: 3}}
	calls := 0
	sweep := func() (map[int]network.FingerprintResponse, bool, error) {
		calls++
		return want, true, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	fps, err := pollConvergence(ctx, time.Second, time.Hour, sweep)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if calls != 1 {
		t.Fatalf("want exactly 1 sweep call, got %d", calls)
	}
	if len(fps) != len(want) {
		t.Fatalf("fps = %v, want %v", fps, want)
	}
}

// TestPollConvergenceReturnsLastSweepOnTimeout asserts that when sweep never
// agrees, pollConvergence reports an error AND still returns the last
// completed (disagreeing) sweep once ctx ends — the supply check downstream
// needs that sweep even though convergence itself failed (I2/harness
// soundness: this must never look like a Fatal that skips the rest of
// CheckInvariants).
func TestPollConvergenceReturnsLastSweepOnTimeout(t *testing.T) {
	disagreeing := map[int]network.FingerprintResponse{0: {Round: 1}, 1: {Round: 2}}
	sweep := func() (map[int]network.FingerprintResponse, bool, error) {
		return disagreeing, false, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	fps, err := pollConvergence(ctx, 50*time.Millisecond, 10*time.Millisecond, sweep)
	if err == nil {
		t.Fatal("expected a convergence timeout error")
	}
	if len(fps) != len(disagreeing) {
		t.Fatalf("want the last completed (disagreeing) sweep returned, got %v", fps)
	}
}

// TestPollConvergenceReturnsNilOnAllErrors asserts that when every sweep call
// errors (no fingerprint ever collected), pollConvergence reports an error and
// a nil sweep rather than a stale or fabricated one.
func TestPollConvergenceReturnsNilOnAllErrors(t *testing.T) {
	sweep := func() (map[int]network.FingerprintResponse, bool, error) {
		return nil, false, errors.New("connect refused")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()

	fps, err := pollConvergence(ctx, 30*time.Millisecond, 10*time.Millisecond, sweep)
	if err == nil {
		t.Fatal("expected a convergence timeout error")
	}
	if fps != nil {
		t.Fatalf("want a nil sweep when every sweep call errored, got %v", fps)
	}
}

// TestCheckSupplyFlagsOffByOne asserts the supply checker accepts a balanced
// fingerprint and rejects one off by a single unit.
func TestCheckSupplyFlagsOffByOne(t *testing.T) {
	balanced := map[int]network.FingerprintResponse{
		0: {CoinsTotal: 10, TotalBonded: 20, Deposits: 5, FeesInFlight: 1, TotalSupply: 36},
	}
	if err := checkSupply(balanced); err != nil {
		t.Fatalf("balanced fingerprint should pass: %v", err)
	}

	offByOne := map[int]network.FingerprintResponse{
		0: {CoinsTotal: 10, TotalBonded: 20, Deposits: 5, FeesInFlight: 1, TotalSupply: 37},
	}
	if err := checkSupply(offByOne); err == nil {
		t.Fatal("expected a supply violation for an off-by-one fingerprint")
	}
}

// TestClusterPassesInvariantsAfterTraffic drives real faucet traffic through
// a single-node cluster and lets teardown's automatic CheckInvariants run
// the full suite (schema drift, convergence, zero rollback, supply) against
// it end to end: a real Fingerprint QUIC round trip, a real convergence
// sweep, real consensus.anchor.committed events, and real supply terms from
// genesis-seeded state.
//
// This deliberately stays single-node. A 2+ validator cluster hits a
// reproducible, pre-existing project bug (not a harness defect — confirmed
// by direct byte-level inspection of the diverging state and reported
// separately) where cross-node fingerprints never converge: registering a
// second validator leaves the nodes' epochAdditions bookkeeping asymmetric
// until the first epoch transition (from cmd/node's "optimistic self-add"
// racing the commit-time replay of the same registration), and once an
// epoch transition does happen, the founder's reward coin — which doubles
// as the genesis reserve coin — ends up with a node-dependent balance,
// consistent with reward distribution being sensitive to validator
// insertion order. Single-node avoids both: there is only one node to
// (trivially) agree with itself, still meaningfully exercising every step
// of the checker against a live process. Multi-node convergence is
// exercised by cluster_test.go's TestClusterBasics with WithoutInvariants,
// pending a project-side fix.
func TestClusterPassesInvariantsAfterTraffic(t *testing.T) {
	c := NewCluster(t, 1)

	w := client.NewWallet()
	cli := c.Client(0)

	_, hash, err := cli.Faucet(w.Pubkey(), 1_000_000)
	if err != nil {
		t.Fatalf("faucet: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := c.WaitAll(ctx, "tx.committed", Attr("tx", hex.EncodeToString(hash[:]))); err != nil {
		t.Fatalf("wait faucet commit: %v", err)
	}

	// Teardown's t.Cleanup runs CheckInvariants automatically after this
	// function returns.
}
