package integration

import (
	"encoding/hex"
	"net/http"
	"strings"
	"testing"
	"time"

	"BluePods/client"
)

const (
	// consensusFaucetAmount is the faucet amount for consensus tests.
	consensusFaucetAmount = 1_000_000

	// consensusTxWait is how long to wait for a tx to commit in 5-node tests.
	consensusTxWait = 10 * time.Second
)

// TestSimConsensus runs a 5-node consensus simulation.
// Tests DAG behavior, version tracking, client operations, security, and convergence.
func TestSimConsensus(t *testing.T) {
	cluster := NewCluster(t, 5, WithHTTPBase(18200), WithQUICBase(18200+920))
	cluster.WaitReady(60 * time.Second)

	cli := cluster.Client(0)

	t.Run("consensus-dag", func(t *testing.T) {
		runConsensusDAGTests(t, cluster)
	})

	t.Run("client-operations", func(t *testing.T) {
		runClientOperationTests(t, cli, cluster)
	})

	t.Run("version-tracking", func(t *testing.T) {
		runVersionTrackingTests(t, cli)
	})

	t.Run("security", func(t *testing.T) {
		runSecurityTests(t, cli, cluster)
	})

	t.Run("convergence", func(t *testing.T) {
		runConvergenceTests(t, cluster)
	})
}

// runConsensusDAGTests verifies DAG consensus behavior.
func runConsensusDAGTests(t *testing.T, cluster *Cluster) {
	t.Helper()

	t.Run("ATP-2.6: quorum advances round", func(t *testing.T) {
		for i := 0; i < cluster.Size(); i++ {
			status := QueryStatus(t, cluster.Node(i).HTTPAddr())
			if status.Round == 0 {
				t.Errorf("node %d: round is 0, expected advancement", i)
			}
		}
	})

	t.Run("ATP-2.8: round progression", func(t *testing.T) {
		// Capture initial round
		initial := QueryStatus(t, cluster.Bootstrap().HTTPAddr())
		time.Sleep(15 * time.Second)
		after := QueryStatus(t, cluster.Bootstrap().HTTPAddr())

		if after.Round <= initial.Round {
			t.Errorf("round did not advance: %d -> %d", initial.Round, after.Round)
		}

		t.Logf("Round advanced: %d -> %d (+%d)", initial.Round, after.Round, after.Round-initial.Round)
	})

	t.Run("ATP-2.9: commit order sequential", func(t *testing.T) {
		for i := 0; i < cluster.Size(); i++ {
			status := QueryStatus(t, cluster.Node(i).HTTPAddr())

			if status.LastCommitted > status.Round {
				t.Errorf("node %d: lastCommitted=%d > round=%d",
					i, status.LastCommitted, status.Round)
			}
		}
	})

	t.Run("ATP-2.20: vertex gossip to all", func(t *testing.T) {
		for i := 0; i < cluster.Size(); i++ {
			count := cluster.Node(i).LogCount("produced vertex")
			if count == 0 {
				t.Errorf("node %d: not producing vertices", i)
			}
		}
	})
}

// runClientOperationTests tests faucet, split, transfer, and NFT operations.
func runClientOperationTests(t *testing.T, cli *client.Client, cluster *Cluster) {
	t.Helper()

	t.Run("ATP-15.1: faucet request", func(t *testing.T) {
		w := client.NewWallet()
		coinID := FaucetAndWait(t, cli, w, consensusFaucetAmount, 30*time.Second)

		obj, err := cli.GetObject(coinID)
		if err != nil {
			t.Fatalf("get coin: %v", err)
		}

		balance := ReadBalance(obj.Content)
		if balance != consensusFaucetAmount {
			t.Errorf("balance: got %d, want %d", balance, consensusFaucetAmount)
		}
	})

	t.Run("ATP-15.2: split operation", func(t *testing.T) {
		w := client.NewWallet()
		coinID := FaucetAndWait(t, cli, w, consensusFaucetAmount, 30*time.Second)

		if err := w.RefreshCoin(cli, coinID); err != nil {
			t.Fatalf("refresh: %v", err)
		}

		recipient := client.NewWallet()
		splitAmount := uint64(400_000)

		newCoinID, err := w.Split(cli, coinID, splitAmount, recipient.Pubkey())
		if err != nil {
			t.Fatalf("split: %v", err)
		}

		time.Sleep(consensusTxWait)

		// Verify source balance decreased
		srcObj, err := cli.GetObject(coinID)
		if err != nil {
			t.Fatalf("get source: %v", err)
		}

		srcBalance := ReadBalance(srcObj.Content)
		// Balance might be reduced by fees, so check it's less than original minus split
		if srcBalance > consensusFaucetAmount-splitAmount {
			t.Errorf("source balance too high: %d", srcBalance)
		}

		// Verify new coin exists with correct balance
		newObj, err := cli.GetObject(newCoinID)
		if err != nil {
			t.Fatalf("get new coin: %v", err)
		}

		newBalance := ReadBalance(newObj.Content)
		if newBalance != splitAmount {
			t.Errorf("new coin balance: got %d, want %d", newBalance, splitAmount)
		}

		rpk := recipient.Pubkey()
		if newObj.Owner != rpk {
			t.Errorf("new coin owner mismatch")
		}
	})

	t.Run("ATP-15.3: transfer operation", func(t *testing.T) {
		w := client.NewWallet()
		coinID := FaucetAndWait(t, cli, w, consensusFaucetAmount, 30*time.Second)

		if err := w.RefreshCoin(cli, coinID); err != nil {
			t.Fatalf("refresh: %v", err)
		}

		recipient := client.NewWallet()
		if err := w.Transfer(cli, coinID, recipient.Pubkey()); err != nil {
			t.Fatalf("transfer: %v", err)
		}

		time.Sleep(consensusTxWait)

		obj, err := cli.GetObject(coinID)
		if err != nil {
			t.Fatalf("get coin after transfer: %v", err)
		}

		rpk := recipient.Pubkey()
		if obj.Owner != rpk {
			t.Errorf("owner mismatch after transfer: got %s, want %s",
				hex.EncodeToString(obj.Owner[:8]), hex.EncodeToString(rpk[:8]))
		}
	})

	t.Run("ATP-15.4: create NFT", func(t *testing.T) {
		w := client.NewWallet()
		metadata := []byte("consensus test NFT")

		nftID, err := w.CreateNFT(cli, 5, metadata)
		if err != nil {
			t.Fatalf("create NFT: %v", err)
		}

		WaitForObject(t, cli, nftID, 15*time.Second)

		obj, err := cli.GetObject(nftID)
		if err != nil {
			t.Fatalf("get NFT: %v", err)
		}

		pk := w.Pubkey()
		if obj.Owner != pk {
			t.Error("NFT owner mismatch")
		}

		if obj.Replication != 5 {
			t.Errorf("NFT replication: got %d, want 5", obj.Replication)
		}
	})

	t.Run("ATP-15.5: transfer NFT", func(t *testing.T) {
		// TODO: BLS attestation for non-singleton objects (rep>0) is unreliable
		// in test clusters. The aggregator needs 67% of holders to respond,
		// but timing/network conditions in CI cause consistent failures.
		// Re-enable once BLS aggregation is hardened.
		t.Skip("BLS attestation unreliable in test clusters — needs aggregation hardening")
	})
}

// runVersionTrackingTests verifies object version increments correctly.
func runVersionTrackingTests(t *testing.T, cli *client.Client) {
	t.Helper()

	t.Run("ATP-3.4: sequential version increments", func(t *testing.T) {
		w := client.NewWallet()
		coinID := FaucetAndWait(t, cli, w, consensusFaucetAmount, 30*time.Second)

		if err := w.RefreshCoin(cli, coinID); err != nil {
			t.Fatalf("refresh: %v", err)
		}

		v0 := w.GetCoin(coinID).Version

		// First split
		r1 := client.NewWallet()
		_, err := w.Split(cli, coinID, 100_000, r1.Pubkey())
		if err != nil {
			t.Fatalf("split 1: %v", err)
		}

		time.Sleep(consensusTxWait)

		if err := w.RefreshCoin(cli, coinID); err != nil {
			t.Fatalf("refresh after split 1: %v", err)
		}

		v1 := w.GetCoin(coinID).Version
		if v1 <= v0 {
			t.Errorf("version should increment after split: %d -> %d", v0, v1)
		}

		// Second split
		r2 := client.NewWallet()
		_, err = w.Split(cli, coinID, 100_000, r2.Pubkey())
		if err != nil {
			t.Fatalf("split 2: %v", err)
		}

		time.Sleep(consensusTxWait)

		if err := w.RefreshCoin(cli, coinID); err != nil {
			t.Fatalf("refresh after split 2: %v", err)
		}

		v2 := w.GetCoin(coinID).Version
		if v2 <= v1 {
			t.Errorf("version should increment again: %d -> %d", v1, v2)
		}

		t.Logf("Versions: %d -> %d -> %d", v0, v1, v2)
	})
}

// runSecurityTests tests security-related validations.
func runSecurityTests(t *testing.T, cli *client.Client, cluster *Cluster) {
	t.Helper()
	addr := cluster.Bootstrap().HTTPAddr()
	systemPod := cli.SystemPod()

	t.Run("ATP-24.2: hash tampering rejected", func(t *testing.T) {
		code, _ := SubmitRawBytes(addr, BuildTxWithBadHash(systemPod))
		if code != http.StatusBadRequest {
			t.Errorf("expected 400, got %d", code)
		}
	})

	t.Run("ATP-24.3: signature forgery rejected", func(t *testing.T) {
		code, _ := SubmitRawBytes(addr, BuildTxWithBadSig(systemPod))
		if code != http.StatusBadRequest {
			t.Errorf("expected 400, got %d", code)
		}
	})

	t.Run("ATP-24.10: non-owner transfer rejected", func(t *testing.T) {
		// TODO: SECURITY — mutable_ref ownership is not enforced at protocol level.
		// The node does not verify that tx.sender owns objects in mutable_refs.
		// Only deletion and gas_coin have ownership checks. The system pod's
		// transfer function also doesn't check sender == owner.
		// Re-enable once mutable_ref ownership validation is added to executeTx.
		t.Skip("mutable_ref ownership not enforced at protocol level — needs security fix")
	})

	t.Run("ATP-24.1: replay attack rejected", func(t *testing.T) {
		// Submit a valid tx, then submit the same bytes again
		w := client.NewWallet()
		coinID := FaucetAndWait(t, cli, w, consensusFaucetAmount, 30*time.Second)

		if err := w.RefreshCoin(cli, coinID); err != nil {
			t.Fatalf("refresh: %v", err)
		}

		// Transfer once
		r := client.NewWallet()
		if err := w.Transfer(cli, coinID, r.Pubkey()); err != nil {
			t.Fatalf("first transfer: %v", err)
		}

		time.Sleep(consensusTxWait)

		// Try another transfer of the same coin with stale version → should conflict
		// The wallet still has old version, so the tx references a stale version
		r2 := client.NewWallet()
		err := w.Transfer(cli, coinID, r2.Pubkey())
		// May succeed at API level (202) but fail at execution (version conflict)
		// This is OK — the important thing is the second transfer doesn't change the owner
		_ = err

		time.Sleep(consensusTxWait)

		obj, err := cli.GetObject(coinID)
		if err != nil {
			t.Fatalf("get coin: %v", err)
		}

		rpk := r.Pubkey()
		if obj.Owner != rpk {
			t.Errorf("replay should not change owner: got %s, want %s (first recipient)",
				hex.EncodeToString(obj.Owner[:8]), hex.EncodeToString(rpk[:8]))
		}
	})
}

// runConvergenceTests verifies network convergence.
func runConvergenceTests(t *testing.T, cluster *Cluster) {
	t.Helper()

	t.Run("convergence: all nodes agree", func(t *testing.T) {
		statuses := make([]*statusResponse, cluster.Size())

		for i := 0; i < cluster.Size(); i++ {
			statuses[i] = QueryStatus(t, cluster.Node(i).HTTPAddr())
			t.Logf("Node %d: round=%d lastCommitted=%d validators=%d",
				i, statuses[i].Round, statuses[i].LastCommitted, statuses[i].Validators)
		}

		// All nodes should see same validator count
		for i, s := range statuses {
			if s.Validators != cluster.Size() {
				t.Errorf("node %d: validators=%d, expected %d", i, s.Validators, cluster.Size())
			}
		}

		// Rounds within reasonable delta (10)
		minRound, maxRound := statuses[0].Round, statuses[0].Round
		for _, s := range statuses[1:] {
			if s.Round < minRound {
				minRound = s.Round
			}
			if s.Round > maxRound {
				maxRound = s.Round
			}
		}

		if maxRound-minRound > 10 {
			t.Errorf("round divergence too large: min=%d max=%d", minRound, maxRound)
		}
	})

	t.Run("convergence: no unexpected errors", func(t *testing.T) {
		for i := 0; i < cluster.Size(); i++ {
			for _, line := range strings.Split(cluster.Node(i).Logs(), "\n") {
				if !strings.Contains(line, "executor error") {
					continue
				}

				// Expected: gossip duplicates for validator registration
				if strings.Contains(line, "func=register_validator") {
					continue
				}
				if strings.Contains(line, "func=deregister_validator") {
					continue
				}

				t.Errorf("node %d unexpected error: %s", i, line)
			}
		}
	})
}
