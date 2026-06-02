package integration

import (
	"testing"
	"time"

	"BluePods/pkg/client"
)

// TestSimObjects runs a 12-node simulation for object sharding and routing.
// Creates all test objects upfront, then runs checks to minimize runtime
// and avoid CPU starvation from sustained vertex production.
func TestSimObjects(t *testing.T) {
	cluster := NewCluster(t, 12,
		WithHTTPBase(18400), WithQUICBase(18400+920),
	)
	cluster.WaitReady(90 * time.Second)

	cli := cluster.Client(0)

	// --- Phase 1: Create all test objects upfront ---

	w := client.NewWallet()

	// Coin (singleton, replicated to all nodes). Fauceted first so it can pay gas
	// for every object creation below (mandatory gas-coin model).
	coinID := FaucetAndWait(t, cli, w, 1_000_000, 60*time.Second)

	// Object with replication=5 (sharded across 5 of 12 nodes)
	objectID, _, err := w.CreateObject(cli, 5, []byte("sharding-test"), coinID)
	if err != nil {
		t.Fatalf("create sharded object: %v", err)
	}

	// Object with replication=100 (should use all validators)
	highRepObjectID, _, err := w.CreateObject(cli, 100, []byte("high-rep"), coinID)
	if err != nil {
		t.Fatalf("create high-rep object: %v", err)
	}

	// 5 objects for diversity test
	var diversityIDs [5][32]byte
	for i := 0; i < 5; i++ {
		id, _, err := w.CreateObject(cli, 5, []byte("div-"+string(rune('0'+i))), coinID)
		if err != nil {
			t.Fatalf("create diversity object %d: %v", i, err)
		}
		diversityIDs[i] = id
	}

	// Wait for all objects to appear and propagate
	WaitForObject(t, cli, objectID, 60*time.Second)
	WaitForObject(t, cli, highRepObjectID, 60*time.Second)
	for _, id := range diversityIDs {
		WaitForObject(t, cli, id, 60*time.Second)
	}

	// Wait for propagation to holders
	WaitForHolders(t, cluster.Nodes(), objectID, 3, 60*time.Second)
	WaitForHolders(t, cluster.Nodes(), coinID, cluster.Size(), 60*time.Second)

	t.Logf("All test objects created and propagated")

	// --- Phase 2: Run all checks ---

	t.Run("ATP-21.3: standard object stored by holders only", func(t *testing.T) {
		holders := CountHolders(t, cluster.Nodes(), objectID)

		if holders < 3 || holders > 7 {
			t.Errorf("expected ~5 holders, got %d", holders)
		}

		t.Logf("object holders: %d (expected 5)", holders)
	})

	t.Run("ATP-21.10: singleton stored by all validators", func(t *testing.T) {
		holders := CountHolders(t, cluster.Nodes(), coinID)

		if holders != cluster.Size() {
			t.Errorf("singleton should be on all %d nodes, got %d", cluster.Size(), holders)
		}
	})

	t.Run("ATP-21.5: holders via Rendezvous deterministic", func(t *testing.T) {
		holders1 := collectHolderSet(t, cluster.Nodes(), objectID)
		holders2 := collectHolderSet(t, cluster.Nodes(), objectID)

		if len(holders1) != len(holders2) {
			t.Errorf("holder count changed: %d -> %d", len(holders1), len(holders2))
		}

		for idx := range holders1 {
			if !holders2[idx] {
				t.Errorf("node %d was holder in first query but not second", idx)
			}
		}
	})

	t.Run("ATP-12.24: different objects get different holders", func(t *testing.T) {
		holderSets := make([]map[int]bool, 5)
		for i, id := range diversityIDs {
			holderSets[i] = collectHolderSet(t, cluster.Nodes(), id)
		}

		allSame := true
		for i := 1; i < 5; i++ {
			if !sameHolderSet(holderSets[0], holderSets[i]) {
				allSame = false
				break
			}
		}

		if allSame {
			t.Error("all 5 objects have identical holder sets — expected Rendezvous diversity")
		}
	})

	t.Run("ATP-37.5: route to holders", func(t *testing.T) {
		for i := 0; i < cluster.Size(); i++ {
			local := QueryObjectLocal(t, cluster.Node(i).Addr(), objectID)
			if local != nil {
				continue // This node is a holder, skip
			}

			// This node is NOT a holder — GET /object/{id} should route to holder
			obj := QueryObject(t, cluster.Node(i).Addr(), objectID)
			if obj == nil {
				t.Errorf("node %d: routing failed, object not found", i)
			} else {
				t.Logf("node %d: routed successfully to holder", i)
			}

			break
		}
	})

	t.Run("ATP-37.11: local-only skips routing", func(t *testing.T) {
		for i := 0; i < cluster.Size(); i++ {
			// A non-holder must answer local-only with not-found (no routing),
			// while the routing fetch for the same object succeeds.
			if QueryObjectLocal(t, cluster.Node(i).Addr(), objectID) != nil {
				continue
			}

			if QueryObjectLocal(t, cluster.Node(i).Addr(), objectID) != nil {
				t.Errorf("node %d: local-only returned an object on a non-holder", i)
			}

			if QueryObject(t, cluster.Node(i).Addr(), objectID) == nil {
				t.Errorf("node %d: routing fetch should still find the object", i)
			}

			break
		}
	})

	t.Run("ATP-15.5: transfer object", func(t *testing.T) {
		// Replicated object transfer through the daemon's off-chain attestation
		// collection: a quorum of the 5 holders signs and the attested
		// transaction commits, moving the object to a new owner.
		w := client.NewWallet()
		gasCoin := FundGasCoin(t, cli, w)

		transferObjectID, _, err := w.CreateObject(cli, 5, []byte("transfer-object"), gasCoin)
		if err != nil {
			t.Fatalf("create object: %v", err)
		}

		WaitForObject(t, cli, transferObjectID, 30*time.Second)
		WaitForHolders(t, cluster.Nodes(), transferObjectID, 3, 30*time.Second)

		recipient := client.NewWallet()
		if _, err := w.TransferObject(cli, transferObjectID, recipient.Pubkey(), gasCoin); err != nil {
			t.Fatalf("transfer object: %v", err)
		}

		rpk := recipient.Pubkey()
		WaitForOwner(t, cli, transferObjectID, rpk, 30*time.Second)
	})

	t.Run("ATP-21.11: singleton mutable executes on all validators", func(t *testing.T) {
		// Transfer a coin (singleton, rep=0) and verify version updates on ALL nodes
		sender := client.NewWallet()
		singletonCoin := FaucetAndWait(t, cli, sender, 500_000, 60*time.Second)

		if err := sender.RefreshCoin(cli, singletonCoin); err != nil {
			t.Fatalf("refresh: %v", err)
		}

		v0 := sender.GetCoin(singletonCoin).Version

		// Transfer the singleton coin
		receiver := client.NewWallet()
		if _, err := sender.Transfer(cli, singletonCoin, receiver.Pubkey()); err != nil {
			t.Fatalf("transfer: %v", err)
		}

		// Wait for the transfer to propagate
		rpk := receiver.Pubkey()
		WaitForOwner(t, cli, singletonCoin, rpk, 30*time.Second)

		// Verify version incremented on ALL nodes (proves all executed)
		allUpdated := true
		for i := 0; i < cluster.Size(); i++ {
			obj := QueryObjectLocal(t, cluster.Node(i).Addr(), singletonCoin)
			if obj == nil {
				t.Errorf("node %d: singleton not found locally", i)
				allUpdated = false
				continue
			}

			if obj.Version <= v0 {
				t.Errorf("node %d: version not incremented (%d <= %d)", i, obj.Version, v0)
				allUpdated = false
			}
		}

		if allUpdated {
			t.Logf("All %d nodes executed singleton mutation (v%d → v%d+)",
				cluster.Size(), v0, v0+1)
		}
	})

	t.Run("ATP-32.1: replication > validators uses all", func(t *testing.T) {
		holders := CountHolders(t, cluster.Nodes(), highRepObjectID)

		if holders != cluster.Size() {
			t.Errorf("rep=100 should use all %d validators, got %d holders",
				cluster.Size(), holders)
		}
	})
}

// collectHolderSet returns a map of node indices that hold an object locally.
func collectHolderSet(t *testing.T, nodes []*Node, objectID [32]byte) map[int]bool {
	t.Helper()

	holders := make(map[int]bool)

	for i, node := range nodes {
		if node == nil {
			continue
		}

		if QueryObjectLocal(t, node.addr, objectID) != nil {
			holders[i] = true
		}
	}

	return holders
}

// sameHolderSet checks if two holder sets are identical.
func sameHolderSet(a, b map[int]bool) bool {
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
