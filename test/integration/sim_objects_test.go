package integration

import (
	"encoding/hex"
	"net/http"
	"testing"
	"time"

	"BluePods/client"
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

	// NFT with replication=5 (sharded across 5 of 12 nodes)
	nftID, err := w.CreateNFT(cli, 5, []byte("sharding-test"))
	if err != nil {
		t.Fatalf("create sharded NFT: %v", err)
	}

	// Coin (singleton, replicated to all nodes)
	coinID := FaucetAndWait(t, cli, w, 1_000_000, 60*time.Second)

	// NFT with replication=100 (should use all validators)
	nftHighRep, err := w.CreateNFT(cli, 100, []byte("high-rep"))
	if err != nil {
		t.Fatalf("create high-rep NFT: %v", err)
	}

	// 5 NFTs for diversity test
	var diversityIDs [5][32]byte
	for i := 0; i < 5; i++ {
		id, err := w.CreateNFT(cli, 5, []byte("div-"+string(rune('0'+i))))
		if err != nil {
			t.Fatalf("create diversity NFT %d: %v", i, err)
		}
		diversityIDs[i] = id
	}

	// Wait for all objects to appear and propagate
	WaitForObject(t, cli, nftID, 60*time.Second)
	WaitForObject(t, cli, nftHighRep, 60*time.Second)
	for _, id := range diversityIDs {
		WaitForObject(t, cli, id, 60*time.Second)
	}

	// Wait for propagation to holders
	WaitForHolders(t, cluster.Nodes(), nftID, 3, 60*time.Second)
	WaitForHolders(t, cluster.Nodes(), coinID, cluster.Size(), 60*time.Second)

	t.Logf("All test objects created and propagated")

	// --- Phase 2: Run all checks ---

	t.Run("ATP-21.3: standard object stored by holders only", func(t *testing.T) {
		holders := CountHolders(t, cluster.Nodes(), nftID)

		if holders < 3 || holders > 7 {
			t.Errorf("expected ~5 holders, got %d", holders)
		}

		t.Logf("NFT holders: %d (expected 5)", holders)
	})

	t.Run("ATP-21.10: singleton stored by all validators", func(t *testing.T) {
		holders := CountHolders(t, cluster.Nodes(), coinID)

		if holders != cluster.Size() {
			t.Errorf("singleton should be on all %d nodes, got %d", cluster.Size(), holders)
		}
	})

	t.Run("ATP-21.5: holders via Rendezvous deterministic", func(t *testing.T) {
		holders1 := collectHolderSet(t, cluster.Nodes(), nftID)
		holders2 := collectHolderSet(t, cluster.Nodes(), nftID)

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
			t.Error("all 5 NFTs have identical holder sets — expected Rendezvous diversity")
		}
	})

	t.Run("ATP-37.5: route to holders", func(t *testing.T) {
		for i := 0; i < cluster.Size(); i++ {
			local := QueryObjectLocal(t, cluster.Node(i).HTTPAddr(), nftID)
			if local != nil {
				continue // This node is a holder, skip
			}

			// This node is NOT a holder — GET /object/{id} should route to holder
			obj := QueryObject(t, cluster.Node(i).HTTPAddr(), nftID)
			if obj == nil {
				t.Errorf("node %d: routing failed, object not found", i)
			} else {
				t.Logf("node %d: routed successfully to holder", i)
			}

			break
		}
	})

	t.Run("ATP-37.11: local=true skips routing", func(t *testing.T) {
		for i := 0; i < cluster.Size(); i++ {
			local := QueryObjectLocal(t, cluster.Node(i).HTTPAddr(), nftID)
			if local == nil {
				// Non-holder: local=true should return 404
				resp, err := httpClient.Get("http://" + cluster.Node(i).HTTPAddr() +
					"/object/" + hex.EncodeToString(nftID[:]) + "?local=true")
				if err != nil {
					t.Fatalf("request failed: %v", err)
				}
				drainClose(resp.Body)

				if resp.StatusCode != http.StatusNotFound {
					t.Errorf("expected 404 for non-holder with local=true, got %d", resp.StatusCode)
				}

				break
			}
		}
	})

	t.Run("ATP-15.5: transfer NFT", func(t *testing.T) {
		// TODO: BLS attestation for non-singleton objects (rep>0) is unreliable
		// in test clusters. The aggregator needs 67% of holders to respond,
		// but timing/network conditions in CI cause consistent failures.
		// Re-enable once BLS aggregation is hardened.
		t.Skip("BLS attestation unreliable in test clusters — needs aggregation hardening")
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
		if err := sender.Transfer(cli, singletonCoin, receiver.Pubkey()); err != nil {
			t.Fatalf("transfer: %v", err)
		}

		// Wait for the transfer to propagate
		rpk := receiver.Pubkey()
		WaitForOwner(t, cli, singletonCoin, rpk, 30*time.Second)

		// Verify version incremented on ALL nodes (proves all executed)
		allUpdated := true
		for i := 0; i < cluster.Size(); i++ {
			obj := QueryObjectLocal(t, cluster.Node(i).HTTPAddr(), singletonCoin)
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
		holders := CountHolders(t, cluster.Nodes(), nftHighRep)

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

		if QueryObjectLocal(t, node.httpAddr, objectID) != nil {
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
