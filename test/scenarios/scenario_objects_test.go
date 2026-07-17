package scenarios

import (
	"bytes"
	"testing"

	"BluePods/pkg/client"
	"BluePods/test/harness"
)

const (
	// objectsScenarioSize is the validator count for TestScenarioObjects:
	// large enough that a replication-5 object shards across a strict
	// subset and rendezvous diversity is observable.
	objectsScenarioSize = 12

	// objectsReplication is the sharded objects' replication factor.
	objectsReplication = 5
)

// TestScenarioObjects drives a 12-node cluster through object sharding:
// rendezvous holder placement (exact count, deterministic, diverse across
// objects), singleton placement on every node, replication above the
// validator count clamping to all nodes, routing from a non-holder, and the
// local-only read flag.
//
// Teardown is still red on the per-node supply identity: the default stake
// setup's non-founder registrations keep the supply term inflated.
func TestScenarioObjects(t *testing.T) {
	if testing.Short() {
		t.Skip("scenario")
	}

	c := harness.NewCluster(t, objectsScenarioSize)
	node0 := c.Node(0)
	cli := c.Client(0)

	w, gasCoin := fundedWallet(stepCtx(t), t, cli, node0, 5_000_000)

	sharded, createHash, err := w.CreateObject(cli, objectsReplication, []byte("sharded"), gasCoin)
	requireNoErr(t, err)
	requireVerdictAll(stepCtx(t), t, c, createHash, true, "")
	waitHolders(stepCtx(t), t, c, sharded, objectsReplication)

	t.Run("sharded_holder_count", func(t *testing.T) {
		if got := countHolders(t, c, sharded); got != objectsReplication {
			t.Fatalf("replication-%d object held by %d nodes, want exactly %d", objectsReplication, got, objectsReplication)
		}
	})

	t.Run("rendezvous_deterministic", func(t *testing.T) {
		first := holderSet(t, c, sharded)
		second := holderSet(t, c, sharded)

		if !sameIntSet(first, second) {
			t.Fatalf("holder set changed between sweeps: %v then %v", first, second)
		}
	})

	t.Run("rendezvous_diversity", func(t *testing.T) {
		testRendezvousDiversity(t, c, cli, node0, w, gasCoin)
	})

	t.Run("singleton_on_all_nodes", func(t *testing.T) {
		if got := countHolders(t, c, gasCoin); got != objectsScenarioSize {
			t.Fatalf("singleton coin held by %d nodes, want all %d", got, objectsScenarioSize)
		}
	})

	t.Run("replication_above_validator_count", func(t *testing.T) {
		high, hash, err := w.CreateObject(cli, 100, []byte("high-rep"), gasCoin)
		requireNoErr(t, err)
		requireVerdictAll(stepCtx(t), t, c, hash, true, "")
		waitHolders(stepCtx(t), t, c, high, objectsScenarioSize)
	})

	t.Run("routing_and_local_only", func(t *testing.T) {
		testRoutingFromNonHolder(t, c, sharded)
	})

	t.Run("singleton_mutation_on_all_nodes", func(t *testing.T) {
		testSingletonMutationEverywhere(t, c, cli, node0)
	})

	t.Run("set_object_updates_sharded_content", func(t *testing.T) {
		testSetObjectUpdatesSharded(t, c, cli, w, gasCoin, sharded)
	})

	t.Run("holders_report_matches_actual_count", func(t *testing.T) {
		testHoldersReportMatchesActual(t, c, cli, sharded)
	})
}

// holderSet returns the set of alive node indices holding id locally.
func holderSet(t *testing.T, c *harness.Cluster, id [32]byte) map[int]bool {
	t.Helper()

	holders := make(map[int]bool)
	for _, n := range c.Alive() {
		data, err := client.NewQUICTransport(n.QUICAddr).GetObjectLocal(id)
		if err == nil && data != nil {
			holders[n.Index] = true
		}
	}

	return holders
}

// firstHolderNode returns the first alive node (by iteration order) holding
// id locally, failing the test if none does.
func firstHolderNode(t *testing.T, c *harness.Cluster, id [32]byte) *harness.Node {
	t.Helper()

	holders := holderSet(t, c, id)
	for _, n := range c.Alive() {
		if holders[n.Index] {
			return n
		}
	}

	t.Fatalf("no holder found for object %x", id[:4])
	return nil
}

// sameIntSet reports whether two index sets are identical.
func sameIntSet(a, b map[int]bool) bool {
	if len(a) != len(b) {
		return false
	}

	for k := range a {
		if !b[k] {
			return false
		}
	}

	return true
}

// testRendezvousDiversity creates five sharded objects and asserts they do
// not all land on one identical holder set: rendezvous placement must spread
// by object ID.
func testRendezvousDiversity(t *testing.T, c *harness.Cluster, cli *client.Client, node0 *harness.Node, w *client.Wallet, gasCoin [32]byte) {
	t.Helper()

	sets := make([]map[int]bool, 0, 5)

	for i := 0; i < 5; i++ {
		id, hash, err := w.CreateObject(cli, objectsReplication, []byte{byte('a' + i)}, gasCoin)
		requireNoErr(t, err)
		requireVerdictAll(stepCtx(t), t, c, hash, true, "")
		waitHolders(stepCtx(t), t, c, id, objectsReplication)

		sets = append(sets, holderSet(t, c, id))
	}

	for i := 1; i < len(sets); i++ {
		if !sameIntSet(sets[0], sets[i]) {
			return // at least two objects differ: diversity confirmed
		}
	}

	t.Fatalf("all 5 sharded objects landed on the identical holder set %v", sets[0])
}

// testRoutingFromNonHolder finds a non-holder of id and asserts the two read
// modes disagree exactly as designed: local-only answers not-found without
// routing, while the routed read fetches the object from a holder.
func testRoutingFromNonHolder(t *testing.T, c *harness.Cluster, id [32]byte) {
	t.Helper()

	for _, n := range c.Alive() {
		transport := client.NewQUICTransport(n.QUICAddr)

		local, err := transport.GetObjectLocal(id)
		requireNoErr(t, err)
		if local != nil {
			continue // holder: not the node under test
		}

		routed, err := transport.GetObject(id)
		requireNoErr(t, err)
		if routed == nil {
			t.Fatalf("node %d (non-holder): routed read did not find the object", n.Index)
		}

		return
	}

	t.Fatalf("no non-holder found for object %x (replication should shard across a strict subset)", id[:4])
}

// testSingletonMutationEverywhere transfers a singleton coin and asserts the
// commit verdict is success on every node and the version advanced in every
// node's local copy: singleton mutations execute on all validators.
func testSingletonMutationEverywhere(t *testing.T, c *harness.Cluster, cli *client.Client, node0 *harness.Node) {
	t.Helper()

	w, coinID := fundedWallet(stepCtx(t), t, cli, node0, 500_000)

	obj, err := cli.GetObject(coinID)
	requireNoErr(t, err)
	v0 := obj.Version

	recipient := client.NewWallet()

	hash, err := w.Transfer(cli, coinID, recipient.Pubkey())
	requireNoErr(t, err)
	requireVerdictAll(stepCtx(t), t, c, hash, true, "")

	for _, n := range c.Alive() {
		data, err := client.NewQUICTransport(n.QUICAddr).GetObjectLocal(coinID)
		requireNoErr(t, err)
		if data == nil {
			t.Fatalf("node %d: singleton missing locally", n.Index)
		}

		local := client.ParseObject(data)
		if local.Version <= v0 {
			t.Fatalf("node %d: singleton version did not advance (%d <= %d)", n.Index, local.Version, v0)
		}
		if local.Owner != recipient.Pubkey() {
			t.Fatalf("node %d: singleton owner not updated", n.Index)
		}
	}
}

// testSetObjectUpdatesSharded overwrites the sharded (replication-5) object's
// content via Wallet.SetObject (which routes through the daemon's ATX
// aggregation, since the object is replicated) and confirms: success on a
// HOLDER of the object, a state.object.updated event for the object
// (requireObjectUpdatedEvent, from scenario_aggregation_test.go), the new
// content readable back as a Borsh-serialized wrapper (a suffix match,
// mirroring TestScenarioBootstrap's create_object assertion), and the
// version advanced on every node that holds the object locally.
//
// Per BUGS.md entry 7, a replicated-object mutation's commit verdict
// diverges between holders (success) and non-holders (ownership failure:
// validateMutableRefOwnership resolves the mutable ref through local state,
// which a non-holder does not have). This confronts the attested path
// through a holder's own verdict, exactly as TestScenarioAggregation does,
// rather than requiring uniformity across every node the way
// TestScenarioConsensusBasics deliberately does (and stays red on).
func testSetObjectUpdatesSharded(t *testing.T, c *harness.Cluster, cli *client.Client, w *client.Wallet, gasCoin, objectID [32]byte) {
	t.Helper()

	before, err := cli.GetObject(objectID)
	requireNoErr(t, err)

	holderNode := firstHolderNode(t, c, objectID)

	newContent := []byte("sharded content replaced by set_object")

	hash, err := w.SetObject(cli, objectID, newContent, gasCoin)
	requireNoErr(t, err)
	requireCommittedSuccess(stepCtx(t), t, holderNode, hash)
	requireObjectUpdatedEvent(t, c, objectID)

	after, err := cli.GetObject(objectID)
	requireNoErr(t, err)
	if !bytes.HasSuffix(after.Content, newContent) {
		t.Fatalf("set_object content: got %q, want a suffix of %q", after.Content, newContent)
	}

	for _, n := range c.Alive() {
		data, err := client.NewQUICTransport(n.QUICAddr).GetObjectLocal(objectID)
		requireNoErr(t, err)
		if data == nil {
			continue // non-holder: nothing to check locally
		}

		if local := client.ParseObject(data); local.Version <= before.Version {
			t.Fatalf("node %d: sharded object version did not advance (%d <= %d)", n.Index, local.Version, before.Version)
		}
	}
}

// testHoldersReportMatchesActual confronts Client.Holders (pkg/client/holders.go)
// against the harness's own countHolders: on this cluster every node stays
// alive throughout, so the reported Actual holder set must equal the
// rendezvous-computed Expected set exactly, and its size must match a fresh
// countHolders sweep — two independent ways of counting holders (probing
// every known validator's QUICAddr vs. probing every alive harness node)
// that must agree when both sets are "everyone".
func testHoldersReportMatchesActual(t *testing.T, c *harness.Cluster, cli *client.Client, id [32]byte) {
	t.Helper()

	report, err := cli.Holders(id)
	requireNoErr(t, err)

	if len(report.Actual) != len(report.Expected) {
		t.Fatalf("holders report: %d actual vs %d expected (all nodes alive, they must match)", len(report.Actual), len(report.Expected))
	}

	for pk := range report.Expected {
		if !report.Actual[pk] {
			t.Fatalf("expected holder %x missing from the actual holder set", pk[:8])
		}
	}

	if got := countHolders(t, c, id); got != len(report.Actual) {
		t.Fatalf("countHolders (%d) disagrees with Client.Holders' actual count (%d)", got, len(report.Actual))
	}
}
