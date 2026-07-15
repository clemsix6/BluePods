package harness

import (
	"context"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"
	"time"

	"BluePods/internal/network"
)

const (
	// convergenceTimeout bounds how long CheckInvariants waits for every
	// alive node to agree on one (round, fingerprint) pair.
	convergenceTimeout = 30 * time.Second

	// convergencePoll is the delay between fingerprint sweeps: rounds keep
	// advancing between polls, and the loop absorbs that skew.
	convergencePoll = 200 * time.Millisecond
)

// CheckInvariants runs the cardinal checks over all alive nodes: schema
// drift, zero rollback, convergence, and supply conservation. It is
// registered by NewCluster in t.Cleanup (running BEFORE node teardown)
// unless WithoutInvariants was given.
//
// Rollback runs BEFORE convergence deliberately: it needs no fingerprints, and
// convergence failing must never keep it (or the supply check after it) from
// running. checkConvergence itself reports a disagreement with t.Errorf, not
// t.Fatalf — Fatal's runtime.Goexit would unwind straight through this
// function, skipping checkSupplyT below it — and always returns the last
// completed fingerprint sweep, even after a timeout, so checkSupplyT still
// runs against whatever sweep is available.
func (c *Cluster) CheckInvariants(t *testing.T) {
	t.Helper()

	c.checkSchemaDrift(t)
	c.checkRollbackT(t)

	fps := c.checkConvergence(t)
	c.checkSupplyT(t, fps)
}

// checkSchemaDrift fails the test if any node ever produced an unparsable
// JSON log line: the schema-drift detector.
func (c *Cluster) checkSchemaDrift(t *testing.T) {
	t.Helper()

	for _, n := range c.Nodes() {
		if n == nil {
			continue
		}

		if err := n.ParseError(); err != nil {
			c.Dump(t)
			t.Fatalf("node %d: schema drift: %v", n.Index, err)
		}
	}
}

// checkConvergence polls Client(i).Fingerprint() on every alive node every
// convergencePoll until one sweep observes identical checksums AND identical
// committed rounds across all of them, or reports (via t.Errorf, not
// t.Fatalf) a disagreement after convergenceTimeout. Requiring the same
// round, not just the same checksum, matters: a node wedged at an idle tail
// can otherwise present a checksum that trivially matches a converged peer,
// which is exactly the shape of a broken heal. It always returns the last
// completed sweep (nil if every sweep errored), agreeing or not, so the
// supply check after it still runs against whatever fingerprints were
// actually collected — using t.Fatalf here would Goexit out of CheckInvariants
// and skip that check entirely.
func (c *Cluster) checkConvergence(t *testing.T) map[int]network.FingerprintResponse {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), convergenceTimeout)
	defer cancel()

	fps, err := pollConvergence(ctx, convergenceTimeout, convergencePoll, c.sweepFingerprints)
	if err != nil {
		c.Dump(t)
		t.Errorf("%v", err)
	}

	return fps
}

// pollConvergence repeatedly calls sweep every poll until it reports
// agreement or ctx ends, returning the last completed sweep (nil if every
// sweep errored) either way. timeout is only for the error message (ctx has
// already expired by the time it is formatted, so its own deadline cannot be
// read back). Factored out of checkConvergence as a pure function of its
// inputs so it can be driven by a fake sweep and a short ctx/timeout in
// tests, without waiting on the real convergenceTimeout.
func pollConvergence(
	ctx context.Context,
	timeout, poll time.Duration,
	sweep func() (map[int]network.FingerprintResponse, bool, error),
) (map[int]network.FingerprintResponse, error) {
	ticker := time.NewTicker(poll)
	defer ticker.Stop()

	var lastFPs map[int]network.FingerprintResponse
	var lastErr error

	for {
		fps, agree, err := sweep()
		if err != nil {
			lastErr = err
		} else {
			lastFPs = fps
			if agree {
				return fps, nil
			}
		}

		select {
		case <-ticker.C:
			continue
		case <-ctx.Done():
			return lastFPs, fmt.Errorf("convergence: nodes did not reach an identical (round, fingerprint) within %v (last error=%v, sweep=%s)",
				timeout, lastErr, describeFingerprints(lastFPs))
		}
	}
}

// sweepFingerprints fetches one fingerprint from every alive node. It
// returns agree=true when every node reports the same round and checksum.
// An empty alive set trivially agrees (nothing to converge).
func (c *Cluster) sweepFingerprints() (map[int]network.FingerprintResponse, bool, error) {
	alive := c.Alive()

	fps := make(map[int]network.FingerprintResponse, len(alive))

	for _, n := range alive {
		cli, err := c.newClientFor(n)
		if err != nil {
			return nil, false, fmt.Errorf("node %d: connect:\n%w", n.Index, err)
		}

		fp, err := cli.Fingerprint()
		if err != nil {
			return nil, false, fmt.Errorf("node %d: fingerprint:\n%w", n.Index, err)
		}
		if fp.Err != "" {
			return nil, false, fmt.Errorf("node %d: fingerprint refused: %s", n.Index, fp.Err)
		}

		fps[n.Index] = *fp
	}

	return fps, fingerprintsAgree(fps), nil
}

// fingerprintsAgree reports whether every fingerprint in fps shares the same
// round and checksum.
func fingerprintsAgree(fps map[int]network.FingerprintResponse) bool {
	first := true

	var round uint64
	var checksum [32]byte

	for _, fp := range fps {
		if first {
			round, checksum = fp.Round, fp.Checksum
			first = false
			continue
		}
		if fp.Round != round || fp.Checksum != checksum {
			return false
		}
	}

	return true
}

// describeFingerprints renders a sweep for failure diagnostics.
func describeFingerprints(fps map[int]network.FingerprintResponse) string {
	if len(fps) == 0 {
		return "<none>"
	}

	var b strings.Builder
	for i, fp := range fps {
		fmt.Fprintf(&b, "[node %d round=%d checksum=%s] ", i, fp.Round, hex.EncodeToString(fp.Checksum[:4]))
	}

	return b.String()
}

// checkRollbackT fails the test if checkRollback finds a contradiction.
func (c *Cluster) checkRollbackT(t *testing.T) {
	t.Helper()

	if err := checkRollback(c.Nodes()); err != nil {
		c.Dump(t)
		t.Fatalf("zero rollback: %v", err)
	}
}

// checkRollback verifies zero rollback across nodes: within each node's
// process-run segment, consensus.anchor.committed rounds strictly increase
// (a crash may legitimately re-decide the last round after restart, but must
// decide it identically); across all nodes and segments, any round committed
// twice must carry the same anchor hash. It is a pure function of the
// nodes' journals so it can be unit tested against fabricated ones.
func checkRollback(nodes []*Node) error {
	anchorsByRound := make(map[uint64]string)

	for _, n := range nodes {
		if n == nil {
			continue
		}

		if err := checkNodeRollback(n, anchorsByRound); err != nil {
			return err
		}
	}

	return nil
}

// checkNodeRollback walks one node's anchor.committed events, checking its
// own strictly-increasing-per-segment rule and recording (round -> anchor)
// into the shared cross-node map, failing if a round already has a
// different anchor recorded.
func checkNodeRollback(n *Node, anchorsByRound map[uint64]string) error {
	lastRoundBySeg := make(map[int]uint64)
	haveLastBySeg := make(map[int]bool)

	for _, e := range n.Journal().Events("consensus.anchor.committed") {
		round, ok := attrUint64(e.Attrs["round"])
		if !ok {
			continue
		}
		anchor, _ := e.Attrs["anchor"].(string)

		if haveLastBySeg[e.Seg] && round <= lastRoundBySeg[e.Seg] {
			return fmt.Errorf("node %d segment %d: anchor round %d did not strictly increase past %d",
				n.Index, e.Seg, round, lastRoundBySeg[e.Seg])
		}
		lastRoundBySeg[e.Seg] = round
		haveLastBySeg[e.Seg] = true

		if existing, ok := anchorsByRound[round]; ok && existing != anchor {
			return fmt.Errorf("round %d committed with two different anchors: %s and %s (contradiction observed on node %d)",
				round, existing, anchor, n.Index)
		}
		anchorsByRound[round] = anchor
	}

	return nil
}

// attrUint64 converts an event attribute (a JSON-decoded float64) to uint64.
func attrUint64(v any) (uint64, bool) {
	f, ok := toFloat64(v)
	if !ok {
		return 0, false
	}

	return uint64(f), true
}

// checkSupplyT fails the test if checkSupply finds a violation in fps.
func (c *Cluster) checkSupplyT(t *testing.T, fps map[int]network.FingerprintResponse) {
	t.Helper()

	if err := checkSupply(fps); err != nil {
		c.Dump(t)
		t.Fatalf("supply: %v", err)
	}
}

// checkSupply verifies coinsTotal + totalBonded + deposits + feesInFlight ==
// totalSupply on every fingerprint in fps. It is a pure function so it can
// be unit tested against a fabricated sweep.
func checkSupply(fps map[int]network.FingerprintResponse) error {
	for i, fp := range fps {
		sum := fp.CoinsTotal + fp.TotalBonded + fp.Deposits + fp.FeesInFlight
		if sum != fp.TotalSupply {
			return fmt.Errorf(
				"node %d: coinsTotal(%d)+totalBonded(%d)+deposits(%d)+feesInFlight(%d)=%d != totalSupply(%d)",
				i, fp.CoinsTotal, fp.TotalBonded, fp.Deposits, fp.FeesInFlight, sum, fp.TotalSupply)
		}
	}

	return nil
}
