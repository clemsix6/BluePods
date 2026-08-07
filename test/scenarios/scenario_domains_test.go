package scenarios

import (
	"context"
	"encoding/hex"
	"testing"
	"time"

	"BluePods/pkg/client"
	"BluePods/test/harness"
)

const (
	// domainsScenarioSize is the validator count for TestScenarioDomains.
	domainsScenarioSize = 5

	// domainsEpochLength makes boundaries land quickly enough that the
	// scenario can reach a domain's expiry sweep (its registered term plus
	// the protocol's grace window plus one boundary) within its budget.
	domainsEpochLength = 20

	// domainsShortTerm is the rental term, in epochs, for the name this
	// scenario lets expire: the minimum lease, so its sweep is reachable
	// soonest.
	domainsShortTerm uint32 = 1

	// domainsLongTerm is the rental term for names this scenario keeps alive
	// through the run (renew, transfer): comfortably beyond the epochs the
	// expiry subtest crosses.
	domainsLongTerm uint32 = 100

	// domainsExpiryTimeout bounds the wait for the expiring name's sweep: the
	// protocol's default grace window (8 epochs) past its registered term
	// (1 epoch), plus one boundary, at domainsEpochLength rounds each.
	domainsExpiryTimeout = 6 * time.Minute
)

// TestScenarioDomains drives a 5-node cluster through the domain-name
// lifecycle: registration and resolution from a different node, first-come-
// first-served rejection of a duplicate registration, renewal moving a
// lease's expiry forward, transfer handing renewal rights to a new owner
// (proven functionally: the old owner loses renewal rights, the new owner
// gains them), and the epoch boundary's expiry sweep removing a lease past
// its grace window — asserted on every node, with the name no longer
// resolving anywhere afterward.
//
// The name this scenario lets expire is registered FIRST, before any other
// subtest, so its ~10-epoch clock (1 registered term + 8 grace epochs + 1
// boundary) runs in the background while the rest of the scenario's real
// wall-clock time elapses, and the final subtest waits out only what
// remains.
func TestScenarioDomains(t *testing.T) {
	if testing.Short() {
		t.Skip("scenario")
	}

	c := harness.NewCluster(t, domainsScenarioSize, harness.WithEpochLength(domainsEpochLength))
	node0 := c.Node(0)
	cli := c.Client(0)

	w, gasCoin := fundedWallet(stepCtx(t), t, cli, node0, 50_000_000)

	target := createObjectEverywhere(t, c, cli, w, gasCoin, "domain-target")

	const expireName = "expire-me"
	registerDomain(t, c, cli, w, gasCoin, expireName, target, domainsShortTerm)

	t.Run("register_resolve_duplicate", func(t *testing.T) {
		testRegisterResolveDuplicate(t, c, cli, w, gasCoin, target)
	})

	t.Run("resolve_proved_from_a_different_node", func(t *testing.T) {
		testResolveProvedFromADifferentNode(t, c, cli, w, gasCoin, target)
	})

	t.Run("renew_moves_expiry", func(t *testing.T) {
		testRenewMovesExpiry(t, c, cli, w, gasCoin, target)
	})

	t.Run("transfer_hands_the_name", func(t *testing.T) {
		testTransferHandsTheName(t, c, cli, w, gasCoin, target)
	})

	t.Run("expiry_sweep", func(t *testing.T) {
		testExpirySweep(t, c, expireName)
	})

	requireRentReachedEpochPool(t, node0)
	requireSupplyIdentity(t, c)
}

// registerDomain registers name (pointing at target, for termEpochs), waits
// for the registration to commit successfully on every node, then waits for
// state.domain.registered(name, target, tx) on every node. Returns the
// registering transaction's hash.
func registerDomain(t *testing.T, c *harness.Cluster, cli *client.Client, w *client.Wallet, gasCoin [32]byte, name string, target [32]byte, termEpochs uint32) [32]byte {
	t.Helper()

	hash, err := w.DomainRegister(cli, name, target, termEpochs, gasCoin)
	requireNoErr(t, err)
	requireVerdictAll(stepCtx(t), t, c, hash, true, "")
	requireDomainRegisteredAll(t, c, name, target, hash)

	return hash
}

// requireDomainRegisteredAll waits for state.domain.registered(name, object,
// tx) on every node.
func requireDomainRegisteredAll(t *testing.T, c *harness.Cluster, name string, objectID, hash [32]byte) {
	t.Helper()

	preds := []harness.Pred{
		harness.Attr("name", name),
		harness.Attr("object", hex.EncodeToString(objectID[:])),
		harness.Attr("tx", hex.EncodeToString(hash[:])),
	}
	if err := c.WaitAll(stepCtx(t), "state.domain.registered", preds...); err != nil {
		c.Dump(t)
		t.Fatalf("state.domain.registered for %q not observed on every node: %v", name, err)
	}
}

// testRegisterResolveDuplicate registers "alpha", resolves it from a node
// DIFFERENT than the one that submitted the registration, and confirms a
// second registration of the same name fails: first-come-first-served on the
// name. The rival is a different, funded sender pointing at an object it
// itself created and owns — never the first registrant's target — so
// controls() has nothing to object to and the live-name collision is the
// ONLY rule left standing between the rival and a successful registration.
func testRegisterResolveDuplicate(t *testing.T, c *harness.Cluster, cli *client.Client, w *client.Wallet, gasCoin, target [32]byte) {
	t.Helper()

	const name = "alpha"
	registerDomain(t, c, cli, w, gasCoin, name, target, domainsLongTerm)

	const otherIdx = 1
	other := client.NewQUICTransport(c.Node(otherIdx).QUICAddr)

	resolved, found, err := other.DomainResolve(name)
	requireNoErr(t, err)
	if !found || resolved != target {
		t.Fatalf("node %d: resolve(%s) = (%x, %v), want (%x, true)", otherIdx, name, resolved[:8], found, target[:8])
	}

	rival, rivalGas := fundedWallet(stepCtx(t), t, cli, c.Node(0), 1_000_000)
	rivalTarget := createObjectEverywhere(t, c, cli, rival, rivalGas, "rival-target")

	hash, err := rival.DomainRegister(cli, name, rivalTarget, domainsLongTerm, rivalGas)
	requireNoErr(t, err)
	requireVerdictAll(stepCtx(t), t, c, hash, false, "declared_ops")
}

// testResolveProvedFromADifferentNode registers "demo.config", then resolves
// it through the FULL light-client verification chain from a node OTHER than
// the one that submitted the registration: a checkpoint minted from that
// node's own served state (mintCheckpoint), its GetIndexAnchor bundle
// verified against the checkpointed committee (VerifyAnchor, inside
// LightClient.Anchor), and the domain's inclusion proof checked against the
// resulting attested root (LightClient.ResolveDomain) — the whole spec §5
// chain a wallet or a foreign observer walks, never the serving node's bare
// word the way testRegisterResolveDuplicate's unproven DomainResolve is.
//
// "demo.config" is a dotted name: spec §8's namespace rule requires the
// sender to already own its immediate parent (the suffix "config"), so that
// root is registered first, by the same wallet, before the dotted name that
// depends on it.
func testResolveProvedFromADifferentNode(t *testing.T, c *harness.Cluster, cli *client.Client, w *client.Wallet, gasCoin, target [32]byte) {
	t.Helper()

	const suffix = "config"
	registerDomain(t, c, cli, w, gasCoin, suffix, target, domainsLongTerm)

	const name = "demo.config"
	registerDomain(t, c, cli, w, gasCoin, name, target, domainsLongTerm)

	const otherIdx = 2
	transport := client.NewQUICTransport(c.Node(otherIdx).QUICAddr)
	checkpoint := mintCheckpoint(t, transport)
	lc := client.NewLightClient(c.Client(otherIdx), checkpoint)

	leaf, found := resolveDomainProved(stepCtx(t), t, lc, name)
	if !found {
		t.Fatalf("node %d: proved resolve of %q found nothing", otherIdx, name)
	}
	if leaf.ObjectID != target {
		t.Fatalf("node %d: proved resolve of %q = %x, want %x", otherIdx, name, leaf.ObjectID[:8], target[:8])
	}
	registrant := w.Pubkey()
	if leaf.Owner != registrant {
		t.Fatalf("node %d: proved resolve of %q carries owner %x, want registrant %x", otherIdx, name, leaf.Owner[:8], registrant[:8])
	}
}

// testRenewMovesExpiry registers "beta", captures the epoch just before
// renewing, then renews it and asserts state.domain.renewed lands on every
// node with an expiry at least epochAtRenew + the renewal term — a lower
// bound that holds regardless of how far the epoch moves between the read
// and the renewal's commit, so the assertion never flakes on timing.
func testRenewMovesExpiry(t *testing.T, c *harness.Cluster, cli *client.Client, w *client.Wallet, gasCoin, target [32]byte) {
	t.Helper()

	const name = "beta"
	registerDomain(t, c, cli, w, gasCoin, name, target, domainsLongTerm)

	status, err := cli.Status()
	requireNoErr(t, err)

	hash, err := w.DomainRenew(cli, name, domainsLongTerm, gasCoin)
	requireNoErr(t, err)
	requireVerdictAll(stepCtx(t), t, c, hash, true, "")

	preds := []harness.Pred{
		harness.Attr("name", name),
		harness.Attr("tx", hex.EncodeToString(hash[:])),
		harness.AttrGE("expiry", status.Epoch+uint64(domainsLongTerm)),
	}
	if err := c.WaitAll(stepCtx(t), "state.domain.renewed", preds...); err != nil {
		c.Dump(t)
		t.Fatalf("state.domain.renewed for %q not observed on every node: %v", name, err)
	}
}

// testTransferHandsTheName registers "gamma", transfers it to a fresh owner,
// asserts state.domain.transferred lands on every node, then proves the
// effect functionally: the OLD owner can no longer renew it (rejected,
// declared_ops), while the NEW owner can.
func testTransferHandsTheName(t *testing.T, c *harness.Cluster, cli *client.Client, w *client.Wallet, gasCoin, target [32]byte) {
	t.Helper()

	const name = "gamma"
	registerDomain(t, c, cli, w, gasCoin, name, target, domainsLongTerm)

	newOwner, newOwnerGas := fundedWallet(stepCtx(t), t, cli, c.Node(0), 1_000_000)
	newOwnerPub := newOwner.Pubkey()

	hash, err := w.DomainTransfer(cli, name, newOwnerPub, gasCoin)
	requireNoErr(t, err)
	requireVerdictAll(stepCtx(t), t, c, hash, true, "")

	preds := []harness.Pred{
		harness.Attr("name", name),
		harness.Attr("owner", hex.EncodeToString(newOwnerPub[:])),
		harness.Attr("tx", hex.EncodeToString(hash[:])),
	}
	if err := c.WaitAll(stepCtx(t), "state.domain.transferred", preds...); err != nil {
		c.Dump(t)
		t.Fatalf("state.domain.transferred for %q not observed on every node: %v", name, err)
	}

	oldHash, err := w.DomainRenew(cli, name, domainsLongTerm, gasCoin)
	requireNoErr(t, err)
	requireVerdictAll(stepCtx(t), t, c, oldHash, false, "declared_ops")

	newHash, err := newOwner.DomainRenew(cli, name, domainsLongTerm, newOwnerGas)
	requireNoErr(t, err)
	requireVerdictAll(stepCtx(t), t, c, newHash, true, "")
}

// testExpirySweep waits for the epoch boundary that sweeps name past its
// grace window — state.domain.deleted(name, reason="expired", tx=zero hash)
// on every node — then confirms the name no longer resolves anywhere.
func testExpirySweep(t *testing.T, c *harness.Cluster, name string) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), domainsExpiryTimeout)
	defer cancel()

	var zeroHash [32]byte
	preds := []harness.Pred{
		harness.Attr("name", name),
		harness.Attr("reason", "expired"),
		harness.Attr("tx", hex.EncodeToString(zeroHash[:])),
	}
	if err := c.WaitAll(ctx, "state.domain.deleted", preds...); err != nil {
		c.Dump(t)
		t.Fatalf("state.domain.deleted (expired) for %q not observed on every node: %v", name, err)
	}

	for _, n := range c.Alive() {
		_, found, err := client.NewQUICTransport(n.QUICAddr).DomainResolve(name)
		requireNoErr(t, err)
		if found {
			t.Fatalf("node %d: %q still resolves after its expiry sweep", n.Index, name)
		}
	}
}

// requireRentReachedEpochPool is a smoke check: it confirms at least one
// epoch.rewards.distributed event carried a positive pool during the run, so
// the epoch boundary's distribution pass fires at all in this scenario's
// mixed traffic (domain rent alongside object creation and reparenting).
// It does NOT isolate the domain rent's own contribution to that pool — with
// several fee-paying operations sharing the run, any one of them satisfies
// this bound — so it is not evidence specific to rent reaching the pool.
// Rent-specific coverage (rate x declared term feeding SplitFee's epoch
// share) is unit-level: internal/consensus/fees_ops_test.go and
// events_emit_test.go.
func requireRentReachedEpochPool(t *testing.T, node0 *harness.Node) {
	t.Helper()

	for _, ev := range node0.Journal().Events("epoch.rewards.distributed") {
		if pool, ok := ev.Attrs["pool"].(float64); ok && pool > 0 {
			return
		}
	}

	t.Fatal("no epoch.rewards.distributed observed with a positive pool")
}
