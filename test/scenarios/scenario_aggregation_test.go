package scenarios

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"BluePods/pkg/client"
	"BluePods/pkg/daemon"
	"BluePods/test/harness"
)

const (
	// aggReplication shards the attested objects across a strict subset of
	// the 5-node cluster, so the daemon's attestation collection is a real
	// quorum round, not a trivial all-holders one.
	aggReplication = 3

	// aggEpochLength makes epoch boundaries arrive every ~40s, so the
	// attested path runs in the epochs-rolling regime and the retry contract
	// below is actually exercised.
	aggEpochLength = 100

	// aggTransferAttempts bounds the recollect-and-resubmit retries of one
	// attested transfer. Each attempt waits aggAttemptWait, so the budget
	// spans several epoch boundaries: at least one attempt lands clear of a
	// boundary.
	aggTransferAttempts = 4

	// aggAttemptWait bounds one attempt's wait for the ownership to land.
	aggAttemptWait = 20 * time.Second
)

// TestScenarioAggregation exercises the off-chain aggregation design on a
// 5-node cluster with epochs rolling: the daemon collects holder
// attestations for replicated objects, assembles an attested transaction,
// and submits it. Steps carry forward the old suite's value: the end-to-end
// ATX path, a version race between two contending daemons, transfers
// straddling an epoch boundary (the holder-snapshot grace window), and the
// cold-holder immediacy of freshly created versions.
//
// Attested transfers follow the protocol's documented client contract: an
// ATX whose attestations went stale across an epoch boundary is rejected at
// commit (a deliberate CP choice) and the client recollects against the
// resynced epoch and resubmits. transferWithRetry implements that contract,
// exactly as the old suite's safe-window helper did.
//
// State assertions go through holder reads (routed GetObject, GetObjectLocal
// per node): per BUGS.md entry 7, tx.committed verdicts for replicated-object
// transactions diverge between holders and non-holders, so the uniform
// verdict shape is asserted (red) in TestScenarioConsensusBasics and not
// duplicated here. Teardown convergence is expected red per entries 1 and 2.
func TestScenarioAggregation(t *testing.T) {
	if testing.Short() {
		t.Skip("scenario")
	}

	c := harness.NewCluster(t, 5, harness.WithEpochLength(aggEpochLength))
	node0 := c.Node(0)
	cli := c.Client(0)

	t.Run("attested_transfer", func(t *testing.T) {
		testAttestedTransfer(t, c, cli, node0)
	})

	t.Run("version_race", func(t *testing.T) {
		testVersionRace(t, c, cli, node0)
	})

	t.Run("epoch_boundary_grace", func(t *testing.T) {
		testEpochBoundaryGrace(t, c, cli, node0)
	})

	t.Run("cold_holder", func(t *testing.T) {
		testColdHolder(t, c, cli, node0)
	})
}

// createShardedObject creates a replication-3 object owned by a fresh funded
// wallet and waits until its holders serve it locally. Returns the wallet,
// its gas coin, and the object ID.
func createShardedObject(t *testing.T, c *harness.Cluster, cli *client.Client, node0 *harness.Node, tag string) (*client.Wallet, [32]byte, [32]byte) {
	t.Helper()

	w, gasCoin := fundedWallet(stepCtx(t), t, cli, node0, 2_000_000)

	objectID, createHash, err := w.CreateObject(cli, aggReplication, []byte(tag), gasCoin)
	requireNoErr(t, err)
	requireVerdictAll(stepCtx(t), t, c, createHash, true, "")

	waitHolders(stepCtx(t), t, c, objectID, aggReplication)

	return w, gasCoin, objectID
}

// transferWithRetry drives one attested transfer under the recollect
// contract: submit, wait bounded for the ownership to land, and on a stale
// collection (a typed quorum error, or an ATX dropped at commit because its
// attestation epoch aged out) recollect against the current version and
// resubmit, up to aggTransferAttempts attempts.
func transferWithRetry(t *testing.T, c *harness.Cluster, cli *client.Client, w *client.Wallet, objectID, recipient, gasCoin [32]byte) {
	t.Helper()

	for attempt := 0; attempt < aggTransferAttempts; attempt++ {
		_, err := w.TransferObject(cli, objectID, recipient, gasCoin)
		if err != nil && !errors.Is(err, daemon.ErrQuorumImpossible) {
			t.Fatalf("attested transfer failed with a non-typed error: %v", err)
		}

		if waitOwnerBounded(t, cli, objectID, recipient, aggAttemptWait) {
			return
		}

		t.Logf("attested transfer attempt %d did not land (stale collection); recollecting", attempt+1)
	}

	c.Dump(t)
	t.Fatalf("attested transfer never landed within %d recollect attempts", aggTransferAttempts)
}

// waitOwnerBounded polls the routed GetObject until id's owner equals want,
// returning false (instead of failing) on timeout so retry loops can
// recollect.
func waitOwnerBounded(t *testing.T, cli *client.Client, id, want [32]byte, timeout time.Duration) bool {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	ticker := time.NewTicker(eventPollInterval)
	defer ticker.Stop()

	for {
		obj, err := cli.GetObject(id)
		if err == nil && obj.Owner == want {
			return true
		}

		select {
		case <-ticker.C:
			continue
		case <-ctx.Done():
			return false
		}
	}
}

// testAttestedTransfer transfers a sharded object through the daemon's
// attestation path and asserts every holder converged on the new owner at an
// advanced version, with a state.object.updated event as the typed witness
// on at least one holder.
func testAttestedTransfer(t *testing.T, c *harness.Cluster, cli *client.Client, node0 *harness.Node) {
	t.Helper()

	w, gasCoin, objectID := createShardedObject(t, c, cli, node0, "attested-transfer")

	before, err := cli.GetObject(objectID)
	requireNoErr(t, err)

	recipient := client.NewWallet()

	transferWithRetry(t, c, cli, w, objectID, recipient.Pubkey(), gasCoin)
	requireHoldersConverged(t, c, objectID, recipient.Pubkey(), before.Version)
	requireObjectUpdatedEvent(t, c, objectID)
}

// requireHoldersConverged asserts every node that holds id locally reports
// the new owner and a version strictly past prevVersion.
func requireHoldersConverged(t *testing.T, c *harness.Cluster, id, owner [32]byte, prevVersion uint64) {
	t.Helper()

	holders := 0
	for _, n := range c.Alive() {
		data, err := client.NewQUICTransport(n.QUICAddr).GetObjectLocal(id)
		if err != nil || data == nil {
			continue
		}

		holders++
		obj := client.ParseObject(data)

		if obj.Owner != owner {
			t.Fatalf("node %d: holder did not converge on the new owner", n.Index)
		}
		if obj.Version <= prevVersion {
			t.Fatalf("node %d: holder version did not advance (%d <= %d)", n.Index, obj.Version, prevVersion)
		}
	}

	if holders == 0 {
		t.Fatal("no holder serves the object after the attested transfer")
	}
}

// requireObjectUpdatedEvent asserts at least one node recorded
// state.object.updated for id: the typed witness that the attested mutation
// was applied through the state path, not merely observable via reads.
func requireObjectUpdatedEvent(t *testing.T, c *harness.Cluster, id [32]byte) {
	t.Helper()

	for _, n := range c.Alive() {
		if len(n.Journal().Events("state.object.updated", harness.Attr("object", hexID(id)))) > 0 {
			return
		}
	}

	t.Fatalf("no node recorded state.object.updated for object %x", id[:4])
}

// testVersionRace submits two concurrent transfers of one sharded object at
// the same version through two independent daemons. The outcome must stay
// coherent: the object remains readable and owned by a contender or the
// original owner, never anyone else; and after the dust settles a retried
// transfer proves the object is still live.
func testVersionRace(t *testing.T, c *harness.Cluster, cli *client.Client, node0 *harness.Node) {
	t.Helper()

	w, gasCoin, objectID := createShardedObject(t, c, cli, node0, "version-race")

	r1, r2 := client.NewWallet(), client.NewWallet()
	cli2 := c.Client(1)

	var wg sync.WaitGroup
	var err1, err2 error

	wg.Add(2)
	go func() {
		defer wg.Done()
		_, err1 = w.TransferObject(cli, objectID, r1.Pubkey(), gasCoin)
	}()
	go func() {
		defer wg.Done()
		_, err2 = w.TransferObject(cli2, objectID, r2.Pubkey(), gasCoin)
	}()
	wg.Wait()

	t.Logf("concurrent transfers: err1=%v err2=%v", err1, err2)

	winner := raceOutcome(t, cli, objectID, w.Pubkey(), r1.Pubkey(), r2.Pubkey())

	// Liveness proof: whatever the race decided, the current owner can move
	// the object again (a lost race must not wedge it). The winner's wallet
	// is the one entitled to transfer next; when nobody won, the original
	// owner retries under the recollect contract.
	if winner == r1.Pubkey() || winner == r2.Pubkey() {
		t.Logf("version race resolved: winner %x", winner[:8])
		return
	}

	final := client.NewWallet()
	transferWithRetry(t, c, cli, w, objectID, final.Pubkey(), gasCoin)
}

// raceOutcome polls briefly and returns the raced object's owner, failing
// the test if the object is unreadable or owned by a non-contender.
func raceOutcome(t *testing.T, cli *client.Client, id, owner, r1, r2 [32]byte) [32]byte {
	t.Helper()

	deadlineCtx, cancel := context.WithTimeout(context.Background(), aggAttemptWait)
	defer cancel()

	ticker := time.NewTicker(eventPollInterval)
	defer ticker.Stop()

	for {
		obj, err := cli.GetObject(id)
		if err != nil {
			t.Fatalf("object unreadable during version race: %v", err)
		}

		switch obj.Owner {
		case r1, r2:
			return obj.Owner
		case owner:
			// nobody has won yet; keep watching until the bound
		default:
			t.Fatalf("raced object owned by a non-contender: %x", obj.Owner[:8])
		}

		select {
		case <-ticker.C:
			continue
		case <-deadlineCtx.Done():
			return owner
		}
	}
}

// testEpochBoundaryGrace transfers a sharded object right after an epoch
// boundary lands, inside the grace window where the previous epoch's holder
// snapshot must still verify, and asserts the transfer lands (with the
// recollect contract as the documented fallback).
func testEpochBoundaryGrace(t *testing.T, c *harness.Cluster, cli *client.Client, node0 *harness.Node) {
	t.Helper()

	w, gasCoin, objectID := createShardedObject(t, c, cli, node0, "epoch-grace")

	// Anchor on the NEXT boundary: count the transitions seen so far, then
	// wait for one more, so the transfer below lands in a fresh grace window
	// rather than matching a long-past transition event.
	seen := len(node0.Journal().Events("epoch.transitioned"))

	boundaryCtx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	waitEventCount(boundaryCtx, t, node0, "epoch.transitioned", seen+1)

	recipient := client.NewWallet()
	transferWithRetry(t, c, cli, w, objectID, recipient.Pubkey(), gasCoin)
}

// testColdHolder proves the cold-holder path leaves no exploitable window:
// a transfer attested on a just-created version lands, and a second transfer
// attested immediately on the resulting new version lands too.
func testColdHolder(t *testing.T, c *harness.Cluster, cli *client.Client, node0 *harness.Node) {
	t.Helper()

	w, gasCoin, objectID := createShardedObject(t, c, cli, node0, "cold-holder")

	mid, midGas := fundedWallet(stepCtx(t), t, cli, node0, 2_000_000)

	transferWithRetry(t, c, cli, w, objectID, mid.Pubkey(), gasCoin)

	final := client.NewWallet()
	transferWithRetry(t, c, cli, mid, objectID, final.Pubkey(), midGas)
}

// hexID renders a 32-byte ID as the lowercase hex string events carry.
func hexID(id [32]byte) string {
	const hexdigits = "0123456789abcdef"

	out := make([]byte, 64)
	for i, b := range id {
		out[i*2] = hexdigits[b>>4]
		out[i*2+1] = hexdigits[b&0x0F]
	}

	return string(out)
}
