package scenarios

import (
	"encoding/hex"
	"testing"

	"BluePods/internal/genesis"
	"BluePods/pkg/client"
	"BluePods/test/harness"
)

const (
	// hierarchyScenarioSize is the validator count for TestScenarioHierarchy.
	hierarchyScenarioSize = 5

	// hierarchyReplication is the replication factor for objects created in
	// this scenario: a strict subset of the 5-node cluster, so a routed
	// GetObject from a non-holder node genuinely exercises routing rather than
	// trivially reading local state.
	hierarchyReplication = 3

	// objectParentKind mirrors internal/consensus's and pkg/client's
	// objectParentKind: the reparent target is another tracked object's ID
	// (a cascade parent, not a KeyRoot).
	objectParentKind byte = 1
)

// TestScenarioHierarchy drives a 5-node cluster through the cascade
// ownership model: nested object creation, a root transfer that leaves its
// child's own parent edge untouched, a rejected cycle, a leaf deletion that
// refunds its storage deposit, a rejected deletion of a still-parented
// object, and a sponsored declared-op transfer.
//
// pkg/client's CreateObject can only attach a new object under the sender's
// own KeyRoot (create_object's pod args carry only an owner key, no
// parent_kind) — there is no way to create an object directly under an
// ObjectParent. Every hierarchy in this scenario is therefore built with
// what exists: two KeyRoot creations followed by a declared Reparent that
// nests the second under the first, per the task brief's explicit
// instruction not to add new client API for this task.
func TestScenarioHierarchy(t *testing.T) {
	if testing.Short() {
		t.Skip("scenario")
	}

	c := harness.NewCluster(t, hierarchyScenarioSize)
	node0 := c.Node(0)
	cli := c.Client(0)

	w, gasCoin := fundedWallet(stepCtx(t), t, cli, node0, 10_000_000)

	t.Run("nested_creation", func(t *testing.T) {
		buildHierarchy(t, c, cli, w, gasCoin, "nested-root", "nested-leaf")
	})

	t.Run("transfer_moves_only_root", func(t *testing.T) {
		testTransferMovesOnlyRoot(t, c, cli, w, gasCoin)
	})

	t.Run("cycle_rejected", func(t *testing.T) {
		testCycleRejected(t, c, cli, w, gasCoin)
	})

	t.Run("delete_leaf_refunds", func(t *testing.T) {
		testDeleteLeafRefunds(t, c, cli, w, gasCoin)
	})

	t.Run("delete_parent_rejected", func(t *testing.T) {
		testDeleteParentRejected(t, c, cli, w, gasCoin)
	})

	t.Run("sponsored_transfer", func(t *testing.T) {
		testSponsoredHierarchyTransfer(t, c, node0, cli)
	})
}

// buildHierarchy creates two objects under w's own KeyRoot (root then leaf),
// asserting state.object.created lands on every node with owner=w's key for
// each, then declares a Reparent nesting leaf under root (kind=ObjectParent),
// asserting state.object.reparented lands on every node with the expected
// kind and parent. Returns the root and leaf IDs.
func buildHierarchy(t *testing.T, c *harness.Cluster, cli *client.Client, w *client.Wallet, gasCoin [32]byte, rootMeta, leafMeta string) (root, leaf [32]byte) {
	t.Helper()

	root = createObjectEverywhere(t, c, cli, w, gasCoin, rootMeta)
	leaf = createObjectEverywhere(t, c, cli, w, gasCoin, leafMeta)

	hash, err := w.Reparent(cli, leaf, root, gasCoin)
	requireNoErr(t, err)
	requireVerdictAll(stepCtx(t), t, c, hash, true, "")
	requireReparentedAll(t, c, leaf, objectParentKind, root)

	return root, leaf
}

// createObjectEverywhere creates a hierarchyReplication object owned by w's
// own key, waits for its commit to succeed on every node, and asserts
// state.object.created (owner=w's key — creation only ever attaches under a
// KeyRoot) lands on every node. Returns the new object's ID.
func createObjectEverywhere(t *testing.T, c *harness.Cluster, cli *client.Client, w *client.Wallet, gasCoin [32]byte, metadata string) [32]byte {
	t.Helper()

	id, hash, err := w.CreateObject(cli, hierarchyReplication, []byte(metadata), gasCoin)
	requireNoErr(t, err)
	requireVerdictAll(stepCtx(t), t, c, hash, true, "")

	owner := w.Pubkey()
	preds := []harness.Pred{
		harness.Attr("object", hex.EncodeToString(id[:])),
		harness.Attr("tx", hex.EncodeToString(hash[:])),
		harness.Attr("owner", hex.EncodeToString(owner[:])),
	}
	if err := c.WaitAll(stepCtx(t), "state.object.created", preds...); err != nil {
		c.Dump(t)
		t.Fatalf("state.object.created for %x not observed (owner=w's key) on every node: %v", id[:4], err)
	}

	return id
}

// requireReparentedAll waits until every alive node's journal carries a
// state.object.reparented event for objectID with the expected kind and
// parent, failing the test (with a cluster dump) if any node never records
// one.
func requireReparentedAll(t *testing.T, c *harness.Cluster, objectID [32]byte, kind byte, parent [32]byte) {
	t.Helper()

	preds := []harness.Pred{
		harness.Attr("object", hex.EncodeToString(objectID[:])),
		harness.Attr("kind", kind),
		harness.Attr("parent", hex.EncodeToString(parent[:])),
	}
	if err := c.WaitAll(stepCtx(t), "state.object.reparented", preds...); err != nil {
		c.Dump(t)
		t.Fatalf("state.object.reparented for %x (kind=%d, parent=%x) not observed on every node: %v",
			objectID[:4], kind, parent[:4], err)
	}
}

// testTransferMovesOnlyRoot builds a fresh root/leaf pair, transfers the root
// to a fresh recipient's key (a declared reparent to KeyRoot — an object
// transfer), and confirms: the reparent lands on every node with the
// recipient as the new KeyRoot parent, ONLY the root's version moved (the
// leaf's version is unchanged and the transfer's own tx hash never appears on
// a reparent event for the leaf — only the object named in the transaction's
// mutable refs is touched), and a client talking to a DIFFERENT node than the
// one that submitted the transfer reads the new owner back.
func testTransferMovesOnlyRoot(t *testing.T, c *harness.Cluster, cli *client.Client, w *client.Wallet, gasCoin [32]byte) {
	t.Helper()

	root, leaf := buildHierarchy(t, c, cli, w, gasCoin, "transfer-root", "transfer-leaf")

	rootBefore, err := cli.GetObject(root)
	requireNoErr(t, err)
	leafBefore, err := cli.GetObject(leaf)
	requireNoErr(t, err)

	recipient := client.NewWallet()
	recipientPub := recipient.Pubkey()

	hash, err := w.TransferObject(cli, root, recipientPub, gasCoin)
	requireNoErr(t, err)
	requireVerdictAll(stepCtx(t), t, c, hash, true, "")
	requireReparentedAll(t, c, root, keyRootKind, recipientPub)

	transferTxPreds := []harness.Pred{
		harness.Attr("object", hex.EncodeToString(leaf[:])),
		harness.Attr("tx", hex.EncodeToString(hash[:])),
	}
	for _, n := range c.Alive() {
		if got := n.Journal().Events("state.object.reparented", transferTxPreds...); len(got) != 0 {
			t.Fatalf("node %d: leaf %x carries a reparent event from the root's own transfer tx: %v", n.Index, leaf[:4], got)
		}
	}

	// A client talking to a node other than the one that submitted the
	// transfer (node0), to confirm the new owner is visible network-wide,
	// not just to the submitter.
	const otherIdx = 1
	other := c.Client(otherIdx)

	rootAfter, err := other.GetObject(root)
	requireNoErr(t, err)
	if rootAfter.Owner != recipientPub {
		t.Fatalf("node %d: root owner after transfer: got %x, want recipient %x", otherIdx, rootAfter.Owner[:8], recipientPub[:8])
	}
	if rootAfter.Version <= rootBefore.Version {
		t.Fatalf("root version did not advance on transfer: got %d, want > %d", rootAfter.Version, rootBefore.Version)
	}

	leafAfter, err := other.GetObject(leaf)
	requireNoErr(t, err)
	if leafAfter.Version != leafBefore.Version {
		t.Fatalf("leaf version moved after only its root was transferred: got %d, want unchanged %d", leafAfter.Version, leafBefore.Version)
	}
}

// testCycleRejected builds a fresh root/leaf pair (leaf already nested under
// root) and attempts to reparent root under its own leaf — closing a
// two-object cycle. Staged validation's cycle walk must reject it uniformly:
// tx.committed carries success=false, reason=declared_ops on every node, and
// neither object's parent edge moves.
func testCycleRejected(t *testing.T, c *harness.Cluster, cli *client.Client, w *client.Wallet, gasCoin [32]byte) {
	t.Helper()

	root, leaf := buildHierarchy(t, c, cli, w, gasCoin, "cycle-root", "cycle-leaf")

	hash, err := w.Reparent(cli, root, leaf, gasCoin)
	requireNoErr(t, err)
	requireVerdictAll(stepCtx(t), t, c, hash, false, "declared_ops")

	after, err := cli.GetObject(root)
	requireNoErr(t, err)
	if after.Owner == leaf {
		t.Fatalf("cycle-rejected reparent still moved root under leaf")
	}
}

// testDeleteLeafRefunds builds a fresh root/leaf pair and deletes the leaf
// (no children of its own): the delete must succeed everywhere, emitting
// state.object.deleted with a positive refund and fees.deposit.refunded
// naming the gas coin, on every node.
func testDeleteLeafRefunds(t *testing.T, c *harness.Cluster, cli *client.Client, w *client.Wallet, gasCoin [32]byte) {
	t.Helper()

	_, leaf := buildHierarchy(t, c, cli, w, gasCoin, "delete-leaf-root", "delete-leaf-leaf")

	hash, err := w.DeleteObject(cli, leaf, gasCoin)
	requireNoErr(t, err)
	requireVerdictAll(stepCtx(t), t, c, hash, true, "")

	requireObjectDeletedWithRefund(t, c, leaf, hash)
	requireDepositRefunded(t, c, leaf, gasCoin)
}

// requireObjectDeletedWithRefund waits for state.object.deleted(leaf, hash)
// on every node, then confirms each node's own copy carries a positive
// refund attribute.
func requireObjectDeletedWithRefund(t *testing.T, c *harness.Cluster, leaf, hash [32]byte) {
	t.Helper()

	preds := []harness.Pred{
		harness.Attr("object", hex.EncodeToString(leaf[:])),
		harness.Attr("tx", hex.EncodeToString(hash[:])),
	}
	if err := c.WaitAll(stepCtx(t), "state.object.deleted", preds...); err != nil {
		c.Dump(t)
		t.Fatalf("state.object.deleted for %x not observed on every node: %v", leaf[:4], err)
	}

	for _, n := range c.Alive() {
		evs := n.Journal().Events("state.object.deleted", harness.Attr("object", hex.EncodeToString(leaf[:])))
		if len(evs) == 0 {
			t.Fatalf("node %d: no state.object.deleted for leaf %x", n.Index, leaf[:4])
		}
		if refund, ok := evs[0].Attrs["refund"].(float64); !ok || refund <= 0 {
			t.Fatalf("node %d: state.object.deleted carries no positive refund: %v", n.Index, evs[0].Attrs)
		}
	}
}

// requireDepositRefunded waits for fees.deposit.refunded(leaf, gasCoin) on
// every node, then confirms each node's own copy carries a positive amount.
func requireDepositRefunded(t *testing.T, c *harness.Cluster, leaf, gasCoin [32]byte) {
	t.Helper()

	preds := []harness.Pred{
		harness.Attr("object", hex.EncodeToString(leaf[:])),
		harness.Attr("coin", hex.EncodeToString(gasCoin[:])),
	}
	if err := c.WaitAll(stepCtx(t), "fees.deposit.refunded", preds...); err != nil {
		c.Dump(t)
		t.Fatalf("fees.deposit.refunded for %x (coin=%x) not observed on every node: %v", leaf[:4], gasCoin[:4], err)
	}

	for _, n := range c.Alive() {
		evs := n.Journal().Events("fees.deposit.refunded", harness.Attr("object", hex.EncodeToString(leaf[:])))
		if len(evs) == 0 {
			t.Fatalf("node %d: no fees.deposit.refunded for leaf %x", n.Index, leaf[:4])
		}
		if amount, ok := evs[0].Attrs["amount"].(float64); !ok || amount <= 0 {
			t.Fatalf("node %d: fees.deposit.refunded carries no positive amount: %v", n.Index, evs[0].Attrs)
		}
	}
}

// testDeleteParentRejected builds a fresh root/leaf pair and attempts to
// delete the root while the leaf is still nested under it: staged
// validation's child-count check must reject it uniformly (declared_ops) on
// every node, and the root must remain readable afterward (never deleted).
func testDeleteParentRejected(t *testing.T, c *harness.Cluster, cli *client.Client, w *client.Wallet, gasCoin [32]byte) {
	t.Helper()

	root, _ := buildHierarchy(t, c, cli, w, gasCoin, "delete-parent-root", "delete-parent-leaf")

	hash, err := w.DeleteObject(cli, root, gasCoin)
	requireNoErr(t, err)
	requireVerdictAll(stepCtx(t), t, c, hash, false, "declared_ops")

	if _, err := cli.GetObject(root); err != nil {
		t.Fatalf("rejected delete of a still-parented root left it unreadable: %v", err)
	}
}

// testSponsoredHierarchyTransfer creates an object under a fresh sender's own
// key, then has a distinct sponsor co-sign and pay for a declared
// reparent-to-KeyRoot (an object transfer) via SignSponsoredOp/
// SubmitSponsored — the sponsored path scenario_sponsored_test.go exercises
// for a coin, applied here to a hierarchy object. It asserts fees.deducted
// names the SPONSOR's coin, never the sender's, and that the transfer applies
// (the object's owner moves to the recipient).
func testSponsoredHierarchyTransfer(t *testing.T, c *harness.Cluster, node0 *harness.Node, cli *client.Client) {
	t.Helper()

	const funding = uint64(1_000_000)

	sender, senderGas := fundedWallet(stepCtx(t), t, cli, node0, funding)
	sponsor, sponsorGas := fundedWallet(stepCtx(t), t, cli, node0, funding)
	recipient := client.NewWallet()
	recipientPub := recipient.Pubkey()

	objID := createObjectEverywhere(t, c, cli, sender, senderGas, "sponsored-root")

	obj, err := cli.GetObject(objID)
	requireNoErr(t, err)

	op := client.SponsoredOp{
		Operations:  []genesis.DeclaredOp{{Kind: reparentOpKind, ObjectID: objID[:], TargetKind: keyRootKind, Target: recipientPub[:]}},
		MutableRefs: []genesis.ObjectRefData{{ID: objID, Version: obj.Version}},
	}

	// Comfortably beyond any epoch this scenario could reach, mirroring
	// testSponsoredSuccess in scenario_sponsored_test.go.
	const farFutureEpoch = uint64(1_000_000)

	signed := sender.SignSponsoredOp(op, sponsor.Pubkey(), sponsorGas, farFutureEpoch)

	hash, err := sponsor.SubmitSponsored(cli, signed)
	requireNoErr(t, err)
	requireVerdictAll(stepCtx(t), t, c, hash, true, "")

	ev, err := node0.WaitEvent(stepCtx(t), "fees.deducted",
		harness.Attr("tx", hex.EncodeToString(hash[:])),
		harness.Attr("coin", hex.EncodeToString(sponsorGas[:])),
		harness.Attr("covered", true))
	requireNoErr(t, err)

	if fee, ok := ev.Attrs["amount"].(float64); !ok || fee <= 0 {
		t.Fatalf("fees.deducted carries no positive amount: %v", ev.Attrs)
	}

	after, err := cli.GetObject(objID)
	requireNoErr(t, err)
	if after.Owner != recipientPub {
		t.Fatalf("sponsored object transfer did not move ownership: got %x, want recipient %x", after.Owner[:8], recipientPub[:8])
	}
}
