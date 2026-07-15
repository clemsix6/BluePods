// Package scenarios is the functional and adversarial scenario corpus: plain
// Go tests, guarded by testing.Short(), that drive real BluePods clusters
// through test/harness and assert on typed events rather than log greps or
// fixed sleeps. See test/TESTING.md for how to run and extend it.
package scenarios

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"testing"
	"time"

	"BluePods/pkg/client"
	"BluePods/test/harness"
)

// eventPollInterval is the polling period for count-based waits (events that
// carry no attribute distinguishing one occurrence from the next). It is a
// bounded poll, not a sleep: every caller wraps it in a context deadline.
const eventPollInterval = 20 * time.Millisecond

// requireNoErr fails the test immediately if err is non-nil.
func requireNoErr(t *testing.T, err error) {
	t.Helper()

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// fundedWallet creates a fresh wallet, faucets it amount through cli, waits
// for the faucet transaction to commit successfully on n, and refreshes the
// resulting coin so the wallet's local version and balance are current. It
// returns the wallet and its funded coin ID.
func fundedWallet(ctx context.Context, t *testing.T, cli *client.Client, n *harness.Node, amount uint64) (*client.Wallet, [32]byte) {
	t.Helper()

	w := client.NewWallet()

	coinID, hash, err := cli.Faucet(w.Pubkey(), amount)
	requireNoErr(t, err)

	requireCommittedSuccess(ctx, t, n, hash)
	requireNoErr(t, w.RefreshCoin(cli, coinID))

	return w, coinID
}

// waitCommitted blocks until n's journal records tx.committed for hash,
// regardless of outcome, and returns the event so the caller inspects
// success/reason itself.
func waitCommitted(ctx context.Context, t *testing.T, n *harness.Node, hash [32]byte) harness.Event {
	t.Helper()

	ev, err := n.WaitEvent(ctx, "tx.committed", harness.Attr("tx", hex.EncodeToString(hash[:])))
	requireNoErr(t, err)

	return ev
}

// requireCommittedSuccess waits for tx.committed(hash) on n and fails the
// test if it did not succeed.
func requireCommittedSuccess(ctx context.Context, t *testing.T, n *harness.Node, hash [32]byte) harness.Event {
	t.Helper()

	ev, err := n.WaitEvent(ctx, "tx.committed",
		harness.Attr("tx", hex.EncodeToString(hash[:])), harness.Attr("success", true))
	requireNoErr(t, err)

	return ev
}

// requireCommittedReason waits for tx.committed(hash) on n with the given
// failure reason.
func requireCommittedReason(ctx context.Context, t *testing.T, n *harness.Node, hash [32]byte, reason string) harness.Event {
	t.Helper()

	ev, err := n.WaitEvent(ctx, "tx.committed",
		harness.Attr("tx", hex.EncodeToString(hash[:])), harness.Attr("success", false), harness.Attr("reason", reason))
	requireNoErr(t, err)

	return ev
}

// waitCommittedAll blocks until every alive node in c records tx.committed
// for hash matching preds, bounded by ctx.
func waitCommittedAll(ctx context.Context, t *testing.T, c *harness.Cluster, hash [32]byte, preds ...harness.Pred) {
	t.Helper()

	all := append([]harness.Pred{harness.Attr("tx", hex.EncodeToString(hash[:]))}, preds...)
	if err := c.WaitAll(ctx, "tx.committed", all...); err != nil {
		c.Dump(t)
		t.Fatalf("wait tx.committed(%x) on every node: %v", hash[:4], err)
	}
}

// waitEventCount blocks until n's journal holds at least min events named
// name matching preds, bounded by ctx. It is the scenario-level equivalent of
// the harness's own internal count wait, for events (like
// ingress.tx.rejected) that carry no attribute distinguishing one occurrence
// from the next: a plain WaitEvent could otherwise be satisfied by a stale,
// already-recorded occurrence instead of the one the caller just triggered.
func waitEventCount(ctx context.Context, t *testing.T, n *harness.Node, name string, min int, preds ...harness.Pred) {
	t.Helper()

	ticker := time.NewTicker(eventPollInterval)
	defer ticker.Stop()

	for {
		if len(n.Journal().Events(name, preds...)) >= min {
			return
		}

		select {
		case <-ticker.C:
			continue
		case <-ctx.Done():
			t.Fatalf("timeout waiting for %d %q events (have %d)", min, name, len(n.Journal().Events(name, preds...)))
			return
		}
	}
}

// coinBalance extracts the little-endian u64 balance from a coin object's
// content bytes.
func coinBalance(obj *client.ObjectInfo) uint64 {
	if len(obj.Content) < 8 {
		return 0
	}

	return binary.LittleEndian.Uint64(obj.Content[:8])
}

// waitOwner polls the routed GetObject until id's owner equals want, bounded
// by ctx. It is the event-free wait for state that has no per-node event to
// key on from the observer's side (ownership converges on holders after an
// attested commit).
func waitOwner(ctx context.Context, t *testing.T, cli *client.Client, id, want [32]byte) {
	t.Helper()

	ticker := time.NewTicker(eventPollInterval)
	defer ticker.Stop()

	for {
		obj, err := cli.GetObject(id)
		if err == nil && obj.Owner == want {
			return
		}

		select {
		case <-ticker.C:
			continue
		case <-ctx.Done():
			owner := "unreadable"
			if err == nil {
				owner = hex.EncodeToString(obj.Owner[:8])
			}
			t.Fatalf("timeout waiting for object %x owner %x (last owner: %s)", id[:4], want[:4], owner)
			return
		}
	}
}

// countHolders reports how many alive nodes hold id locally (GetObjectLocal
// returns nil on a non-holder without routing).
func countHolders(t *testing.T, c *harness.Cluster, id [32]byte) int {
	t.Helper()

	holders := 0
	for _, n := range c.Alive() {
		obj, err := client.NewQUICTransport(n.QUICAddr).GetObjectLocal(id)
		if err == nil && obj != nil {
			holders++
		}
	}

	return holders
}

// waitHolders polls until at least min alive nodes hold id locally, bounded
// by ctx.
func waitHolders(ctx context.Context, t *testing.T, c *harness.Cluster, id [32]byte, min int) {
	t.Helper()

	ticker := time.NewTicker(eventPollInterval)
	defer ticker.Stop()

	for {
		if countHolders(t, c, id) >= min {
			return
		}

		select {
		case <-ticker.C:
			continue
		case <-ctx.Done():
			t.Fatalf("timeout waiting for %d holders of object %x (have %d)", min, id[:4], countHolders(t, c, id))
			return
		}
	}
}

// requireSupplyIdentity fetches every alive node's fingerprint and asserts
// the protocol supply identity coinsTotal + totalBonded + deposits +
// feesInFlight == totalSupply on each, independently of cross-node
// convergence (each term is locally evaluable).
func requireSupplyIdentity(t *testing.T, c *harness.Cluster) {
	t.Helper()

	for _, n := range c.Alive() {
		fp, err := client.NewQUICTransport(n.QUICAddr).Fingerprint()
		requireNoErr(t, err)

		sum := fp.CoinsTotal + fp.TotalBonded + fp.Deposits + fp.FeesInFlight
		if sum != fp.TotalSupply {
			t.Fatalf("node %d: supply identity broken: coins(%d)+bonded(%d)+deposits(%d)+fees(%d)=%d != supply(%d)",
				n.Index, fp.CoinsTotal, fp.TotalBonded, fp.Deposits, fp.FeesInFlight, sum, fp.TotalSupply)
		}
	}
}
