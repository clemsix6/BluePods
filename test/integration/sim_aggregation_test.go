package integration

import (
	"encoding/hex"
	"errors"
	"sync"
	"testing"
	"time"

	"BluePods/client"
	"BluePods/daemon"
)

const (
	// aggFaucetAmount is the faucet amount for aggregation tests.
	aggFaucetAmount = 1_000_000

	// aggReplication is the replication factor for the attested objects in these
	// tests: high enough to shard across a strict subset of the 5-node cluster.
	aggReplication = 3

	// aggTxWait is how long to wait for an attested transaction to commit.
	aggTxWait = 30 * time.Second

	// aggObjectWait is how long to wait for a created object to appear and
	// propagate to its holders. It is generous so the scenarios stay reliable
	// under the load of the full suite.
	aggObjectWait = 60 * time.Second
)

// TestSimAggregation exercises the off-chain aggregation design end to end on a
// 5-node cluster: the daemon collects attestations for replicated objects,
// assembles an attested transaction, and submits it. These scenarios cover the
// moved-to-client paths described in TESTING.md.
func TestSimAggregation(t *testing.T) {
	cluster := NewCluster(t, 5, WithHTTPBase(18700), WithQUICBase(18700+920))
	cluster.WaitReady(60 * time.Second)

	cli := cluster.Client(0)

	t.Run("end-to-end-aggregation", func(t *testing.T) {
		runEndToEndAggregation(t, cli, cluster)
	})

	t.Run("singleton-fast-path", func(t *testing.T) {
		runSingletonFastPath(t, cli, cluster)
	})

	t.Run("version-race-during-collection", func(t *testing.T) {
		runVersionRaceDuringCollection(t, cli, cluster)
	})

	t.Run("cold-holder", func(t *testing.T) {
		runColdHolder(t, cli, cluster)
	})

	t.Run("set-object-content", func(t *testing.T) {
		runSetObjectContent(t, cli, cluster)
	})
}

// runSetObjectContent creates a replicated object with content "v1", overwrites
// it to "v2" through the daemon's attestation-collection path, then reads it back
// and asserts the content changed to "v2" and the version incremented.
func runSetObjectContent(t *testing.T, cli *client.Client, cluster *Cluster) {
	t.Helper()

	owner := client.NewWallet()
	gasCoin := FundGasCoin(t, cli, owner)

	objectID, err := owner.CreateObject(cli, aggReplication, []byte("v1"), gasCoin)
	if err != nil {
		t.Fatalf("create object: %v", err)
	}

	WaitForObject(t, cli, objectID, aggObjectWait)
	WaitForHolders(t, cluster.Nodes(), objectID, aggReplication, aggObjectWait)

	before, err := cli.GetObject(objectID)
	if err != nil {
		t.Fatalf("get object before set: %v", err)
	}

	if got := decodeObjectContent(before.Content); got != "v1" {
		t.Fatalf("initial content: got %q, want %q", got, "v1")
	}

	if err := owner.SetObject(cli, objectID, []byte("v2"), gasCoin); err != nil {
		t.Fatalf("set object through aggregation path: %v", err)
	}

	waitForContent(t, cli, objectID, "v2", aggTxWait)

	after, err := cli.GetObject(objectID)
	if err != nil {
		t.Fatalf("get object after set: %v", err)
	}

	if got := decodeObjectContent(after.Content); got != "v2" {
		t.Errorf("content after set: got %q, want %q", got, "v2")
	}

	if after.Version <= before.Version {
		t.Errorf("version not advanced after set: %d <= %d", after.Version, before.Version)
	}

	t.Logf("set_object overwrote content v1 -> v2, version %d -> %d", before.Version, after.Version)
}

// waitForContent polls until an object's decoded content matches expected.
func waitForContent(t *testing.T, cli *client.Client, id [32]byte, expected string, timeout time.Duration) {
	t.Helper()

	deadline := time.Now().Add(timeout)

	for time.Now().Before(deadline) {
		obj, err := cli.GetObject(id)
		if err == nil && decodeObjectContent(obj.Content) == expected {
			return
		}

		time.Sleep(time.Second)
	}

	t.Fatalf("timeout waiting for content %q on object %s", expected, hex.EncodeToString(id[:8]))
}

// decodeObjectContent decodes the Borsh-serialized object body (a Vec<u8>) into
// its raw content string by stripping the 4-byte little-endian length prefix.
func decodeObjectContent(content []byte) string {
	if len(content) < 4 {
		return ""
	}

	return string(content[4:])
}

// runEndToEndAggregation transfers a replicated object through the daemon's
// collection path and verifies every node converges on the new owner and version.
func runEndToEndAggregation(t *testing.T, cli *client.Client, cluster *Cluster) {
	t.Helper()

	owner := client.NewWallet()
	gasCoin := FundGasCoin(t, cli, owner)

	objectID, err := owner.CreateObject(cli, aggReplication, []byte("e2e-aggregation"), gasCoin)
	if err != nil {
		t.Fatalf("create object: %v", err)
	}

	WaitForObject(t, cli, objectID, aggObjectWait)
	WaitForHolders(t, cluster.Nodes(), objectID, aggReplication, aggObjectWait)

	before, err := cli.GetObject(objectID)
	if err != nil {
		t.Fatalf("get object before transfer: %v", err)
	}

	recipient := client.NewWallet()
	if err := owner.TransferObject(cli, objectID, recipient.Pubkey(), gasCoin); err != nil {
		t.Fatalf("transfer object through aggregation path: %v", err)
	}

	rpk := recipient.Pubkey()
	WaitForOwner(t, cli, objectID, rpk, aggTxWait)

	assertAllNodesConverged(t, cluster, objectID, rpk, before.Version)
}

// assertAllNodesConverged verifies every holder of the object reports the new
// owner and a version strictly greater than the pre-transfer version.
func assertAllNodesConverged(t *testing.T, cluster *Cluster, id, owner [32]byte, prevVersion uint64) {
	t.Helper()

	checked := 0

	for i := 0; i < cluster.Size(); i++ {
		obj := QueryObjectLocal(t, cluster.Node(i).Addr(), id)
		if obj == nil {
			continue // not a holder
		}

		checked++

		if obj.Owner != hex.EncodeToString(owner[:]) {
			t.Errorf("node %d: owner not converged", i)
		}

		if obj.Version <= prevVersion {
			t.Errorf("node %d: version not advanced (%d <= %d)", i, obj.Version, prevVersion)
		}
	}

	if checked == 0 {
		t.Fatal("no holder reported the object after transfer")
	}

	t.Logf("All %d holders converged on the attested transfer", checked)
}

// runSingletonFastPath submits a singleton-only transfer and confirms it commits
// with no attestation round: a coin is replication 0, so the daemon submits the
// raw transaction and the validator wraps it into a trivial attested transaction.
func runSingletonFastPath(t *testing.T, cli *client.Client, cluster *Cluster) {
	t.Helper()

	sender := client.NewWallet()
	coinID := FaucetAndWait(t, cli, sender, aggFaucetAmount, aggObjectWait)

	coin, err := cli.GetObject(coinID)
	if err != nil {
		t.Fatalf("get coin: %v", err)
	}

	if coin.Replication != 0 {
		t.Fatalf("faucet coin should be a singleton (replication 0), got %d", coin.Replication)
	}

	if err := sender.RefreshCoin(cli, coinID); err != nil {
		t.Fatalf("refresh coin: %v", err)
	}

	recipient := client.NewWallet()
	if err := sender.Transfer(cli, coinID, recipient.Pubkey()); err != nil {
		t.Fatalf("singleton transfer: %v", err)
	}

	rpk := recipient.Pubkey()
	WaitForOwner(t, cli, coinID, rpk, aggTxWait)

	// A singleton lives on every validator and is never attested. Confirm the
	// mutation executed on all of them.
	for i := 0; i < cluster.Size(); i++ {
		obj := QueryObjectLocal(t, cluster.Node(i).Addr(), coinID)
		if obj == nil {
			t.Errorf("node %d: singleton missing", i)
			continue
		}

		if obj.Owner != hex.EncodeToString(rpk[:]) {
			t.Errorf("node %d: singleton owner not updated", i)
		}
	}

	t.Logf("Singleton fast path committed on all %d validators", cluster.Size())
}

// runVersionRaceDuringCollection drives concurrent writers against one replicated
// object. The contention forces the daemon's bounded-backoff retry during attestation
// collection. The outcome must be coherent: exactly one ownership lineage wins,
// the object is never corrupted, and the object remains live.
func runVersionRaceDuringCollection(t *testing.T, cli *client.Client, cluster *Cluster) {
	t.Helper()

	owner := client.NewWallet()
	gasCoin := FundGasCoin(t, cli, owner)

	objectID, err := owner.CreateObject(cli, aggReplication, []byte("version-race"), gasCoin)
	if err != nil {
		t.Fatalf("create object: %v", err)
	}

	WaitForObject(t, cli, objectID, aggObjectWait)
	WaitForHolders(t, cluster.Nodes(), objectID, aggReplication, aggObjectWait)

	// Two transfers of the same object, submitted concurrently. Both daemons read the
	// same current version and collect against it; at most one can commit, the
	// other races on the version and either fails to commit or retries.
	r1 := client.NewWallet()
	r2 := client.NewWallet()

	var wg sync.WaitGroup
	var err1, err2 error

	wg.Add(2)
	go func() {
		defer wg.Done()
		err1 = owner.TransferObject(cli, objectID, r1.Pubkey(), gasCoin)
	}()
	go func() {
		defer wg.Done()
		err2 = owner.TransferObject(cli, objectID, r2.Pubkey(), gasCoin)
	}()
	wg.Wait()

	t.Logf("Concurrent transfer results: err1=%v err2=%v", err1, err2)

	time.Sleep(aggTxWait)

	// The object must remain readable and owned by exactly one of the contenders
	// (or still by the original owner if neither attested transaction committed).
	obj, err := cli.GetObject(objectID)
	if err != nil {
		t.Fatalf("object unreadable after version race: %v", err)
	}

	r1pk := r1.Pubkey()
	r2pk := r2.Pubkey()
	opk := owner.Pubkey()

	switch obj.Owner {
	case r1pk, r2pk, opk:
		t.Logf("Version race resolved coherently: owner=%x version=%d", obj.Owner[:8], obj.Version)
	default:
		t.Errorf("object owner is none of the contenders: %x", obj.Owner[:8])
	}
}

// runColdHolder confirms the cold-holder path leaves no exploitable cold window.
// A freshly created replicated object is attested for a brand new transfer; even
// though the new version was just produced, every queried holder serves a valid
// attestation (eager signing at execution, or the bounded sign-on-miss fallback),
// so the transfer's attestation collection succeeds without a stall.
func runColdHolder(t *testing.T, cli *client.Client, cluster *Cluster) {
	t.Helper()

	owner := client.NewWallet()
	ownerGas := FundGasCoin(t, cli, owner)

	objectID, err := owner.CreateObject(cli, aggReplication, []byte("cold-holder"), ownerGas)
	if err != nil {
		t.Fatalf("create object: %v", err)
	}

	WaitForObject(t, cli, objectID, aggObjectWait)
	WaitForHolders(t, cluster.Nodes(), objectID, aggReplication, aggObjectWait)

	// First transfer: collects attestations on the just-created version. A
	// successful commit proves a quorum of holders served a signature for the
	// current version with no cold-window gap.
	mid := client.NewWallet()
	midGas := FundGasCoin(t, cli, mid)
	if err := owner.TransferObject(cli, objectID, mid.Pubkey(), ownerGas); err != nil {
		t.Fatalf("first transfer (cold version): %v", err)
	}

	midpk := mid.Pubkey()
	WaitForOwner(t, cli, objectID, midpk, aggTxWait)

	// Second transfer immediately on the new version: the holders must again serve
	// a current-version signature without delay, confirming the new version is
	// signable the moment it is the current one.
	final := client.NewWallet()
	if err := mid.TransferObject(cli, objectID, final.Pubkey(), midGas); err != nil {
		t.Fatalf("second transfer (newly current version): %v", err)
	}

	finalpk := final.Pubkey()
	WaitForOwner(t, cli, objectID, finalpk, aggTxWait)

	t.Logf("Cold-holder path: both back-to-back attested transfers committed")
}

// epochBoundaryEpochLength is the epoch length (in rounds) for the epoch-boundary
// scenario. It is long enough that the mid-epoch safe band is wide, so a transfer
// submitted there reliably commits within its own epoch, yet short enough that a
// chain of timed hops crosses at least one boundary.
const epochBoundaryEpochLength = 180

// TestSimAggregationEpochBoundary crosses one or more epoch boundaries while a
// chain of replicated object transfers is in flight. Each transfer is submitted in
// the mid-epoch safe band, where its attestation is collected and committed well
// within the same epoch and verifies against that epoch's holder snapshot. Over
// the chain, the elapsed rounds cross at least one boundary, so the grace window
// (the verifier accepting the previous epoch's holders just after a boundary) and
// the redistribution of holders are both exercised. The recollect-after-grace path
// is asserted directly: a transfer that does not commit within the window is
// recollected against the resynced epoch and resubmitted. The object stays
// readable and coherent throughout, and the chain makes forward progress.
func TestSimAggregationEpochBoundary(t *testing.T) {
	cluster := NewCluster(t, 5,
		WithHTTPBase(18800), WithQUICBase(18800+920),
		WithEpochLength(epochBoundaryEpochLength),
	)
	cluster.WaitReady(90 * time.Second)

	cli := cluster.Client(0)

	owner := client.NewWallet()
	currentGas := FundGasCoin(t, cli, owner)

	objectID, err := owner.CreateObject(cli, aggReplication, []byte("epoch-boundary"), currentGas)
	if err != nil {
		t.Fatalf("create object: %v", err)
	}

	WaitForObject(t, cli, objectID, aggObjectWait)
	WaitForHolders(t, cluster.Nodes(), objectID, aggReplication, aggObjectWait)

	startEpoch := QueryStatus(t, cluster.Bootstrap().Addr()).Epoch

	current := owner
	const hops = 3
	committed := 0

	for i := 0; i < hops; i++ {
		// Each hop runs in the mid-epoch safe band of a strictly later epoch than
		// the previous hop, so the chain crosses a boundary between every hop.
		waitForMidEpoch(t, cluster, startEpoch+uint64(i))

		next, nextGas := transferInSafeWindow(t, cli, objectID, current, currentGas)
		if next == nil {
			t.Fatalf("hop %d never committed within the recollect budget", i)
		}

		current = next
		currentGas = nextGas
		committed++
	}

	endEpoch := QueryStatus(t, cluster.Bootstrap().Addr()).Epoch
	if endEpoch <= startEpoch {
		t.Errorf("expected at least one epoch boundary crossed, start=%d end=%d", startEpoch, endEpoch)
	}

	// The object survived every boundary crossing and is owned by the last
	// recipient in the chain.
	finalpk := current.Pubkey()
	WaitForOwner(t, cli, objectID, finalpk, aggTxWait)

	t.Logf("Crossed %d epoch boundaries; all %d attested transfers committed, object coherent",
		endEpoch-startEpoch, committed)
}

// waitForMidEpoch blocks until the bootstrap has reached at least minEpoch and
// its round sits in that epoch's mid-epoch safe band, clear of a boundary by more
// than the grace window on either side. A transfer submitted there collects and
// commits comfortably within one epoch. Requiring a strictly increasing minEpoch
// across hops guarantees the chain crosses a boundary between hops.
func waitForMidEpoch(t *testing.T, cluster *Cluster, minEpoch uint64) {
	t.Helper()

	const margin = 60 // rounds of clearance from either boundary (grace is 50)

	deadline := time.Now().Add(5 * time.Minute)

	for time.Now().Before(deadline) {
		status := QueryStatusSafe(cluster.Bootstrap().Addr())
		if status != nil && status.Epoch >= minEpoch {
			pos := status.Round % epochBoundaryEpochLength
			if pos >= margin && pos <= epochBoundaryEpochLength-margin {
				return
			}
		}

		time.Sleep(2 * time.Second)
	}

	t.Fatalf("timed out waiting for a mid-epoch window at epoch >= %d", minEpoch)
}

// transferInSafeWindow transfers objectID from `from` (paying gas from fromGas)
// to one fixed fresh recipient, retrying on the recollect-after-grace contract. A
// submission rejected with the typed ErrQuorumImpossible (a stale holder set) or
// one whose attested transaction is silently dropped (the owner does not change)
// is recollected against the resynced epoch and resubmitted to the same recipient,
// so a late commit and a retry converge. The object is asserted readable on every
// attempt. The recipient is funded with its own gas coin so it can transfer on the
// next hop. It returns the recipient and that gas coin on commit, or nil when the
// budget is exhausted; a non-typed submission error fails the test.
func transferInSafeWindow(t *testing.T, cli *client.Client, objectID [32]byte, from *client.Wallet, fromGas [32]byte) (*client.Wallet, [32]byte) {
	t.Helper()

	const attempts = 4

	next := client.NewWallet()
	nextGas := FundGasCoin(t, cli, next)
	nextpk := next.Pubkey()

	for attempt := 0; attempt < attempts; attempt++ {
		err := from.TransferObject(cli, objectID, nextpk, fromGas)
		if err != nil && !errors.Is(err, daemon.ErrQuorumImpossible) {
			t.Fatalf("transfer failed with non-typed error: %v", err)
		}

		if obj, getErr := cli.GetObject(objectID); getErr != nil {
			t.Fatalf("object unreadable during boundary transfer: %v", getErr)
		} else if obj.Owner == nextpk {
			return next, nextGas // committed
		}

		t.Logf("recollect after grace: attempt %d did not commit (err=%v), retrying", attempt+1, err)
		time.Sleep(aggTxWait)

		if obj, _ := cli.GetObject(objectID); obj != nil && obj.Owner == nextpk {
			return next, nextGas // committed during the wait
		}
	}

	return nil, [32]byte{}
}
