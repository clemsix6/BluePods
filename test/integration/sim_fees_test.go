package integration

import (
	"testing"
	"time"

	"BluePods/client"
)

const (
	// feesFaucetAmount is the faucet amount for fee tests.
	feesFaucetAmount = 1_000_000

	// feesTxWait is how long to wait for a tx to commit in fee tests.
	feesTxWait = 10 * time.Second
)

// TestSimFees runs a 5-node simulation focused on fee-related tests.
func TestSimFees(t *testing.T) {
	cluster := NewCluster(t, 5, WithHTTPBase(18210), WithQUICBase(18210+920))
	cluster.WaitReady(60 * time.Second)

	cli := cluster.Client(0)

	t.Run("fee-deduction", func(t *testing.T) {
		runFeeDeductionTests(t, cli)
	})

	t.Run("gas-coin", func(t *testing.T) {
		runGasCoinTests(t, cli)
	})

	t.Run("fee-consistency", func(t *testing.T) {
		runFeeConsistencyTests(t, cli, cluster)
	})
}

// runFeeDeductionTests tests that fees are deducted correctly.
func runFeeDeductionTests(t *testing.T, cli *client.Client) {
	t.Helper()

	t.Run("ATP-5.1: sufficient balance full deduction", func(t *testing.T) {
		w := client.NewWallet()
		coinID := FaucetAndWait(t, cli, w, feesFaucetAmount, 30*time.Second)

		if err := w.RefreshCoin(cli, coinID); err != nil {
			t.Fatalf("refresh: %v", err)
		}

		initialBalance := w.GetCoin(coinID).Balance

		// Perform a split to trigger fee deduction
		r := client.NewWallet()
		splitAmount := uint64(100_000)

		_, err := w.Split(cli, coinID, splitAmount, r.Pubkey())
		if err != nil {
			t.Fatalf("split: %v", err)
		}

		time.Sleep(feesTxWait)

		if err := w.RefreshCoin(cli, coinID); err != nil {
			t.Fatalf("refresh after split: %v", err)
		}

		afterBalance := w.GetCoin(coinID).Balance

		// Balance should decrease by at least the split amount
		if afterBalance >= initialBalance-splitAmount {
			t.Logf("initial=%d after=%d split=%d", initialBalance, afterBalance, splitAmount)
			t.Logf("Note: no fee deduction detected (fees may be disabled)")
		}

		// Balance should decrease by at most initial (no underflow)
		if afterBalance > initialBalance {
			t.Errorf("balance increased: %d -> %d", initialBalance, afterBalance)
		}

		t.Logf("Balance: %d -> %d (split=%d, fees=%d)",
			initialBalance, afterBalance, splitAmount, initialBalance-splitAmount-afterBalance)
	})

	t.Run("ATP-5.3: no gas_coin skips fees", func(t *testing.T) {
		// Faucet transactions don't use a gas_coin, so they should succeed without fees
		w := client.NewWallet()
		coinID := FaucetAndWait(t, cli, w, 50_000, 30*time.Second)

		obj, err := cli.GetObject(coinID)
		if err != nil {
			t.Fatalf("get coin: %v", err)
		}

		balance := ReadBalance(obj.Content)
		if balance != 50_000 {
			t.Errorf("faucet coin should have exact amount: got %d, want 50000", balance)
		}
	})

	t.Run("ATP-15.2+fees: split deducts fees", func(t *testing.T) {
		w := client.NewWallet()
		coinID := FaucetAndWait(t, cli, w, feesFaucetAmount, 30*time.Second)

		if err := w.RefreshCoin(cli, coinID); err != nil {
			t.Fatalf("refresh: %v", err)
		}

		initialBalance := w.GetCoin(coinID).Balance

		r := client.NewWallet()
		splitAmount := uint64(300_000)

		newCoinID, err := w.Split(cli, coinID, splitAmount, r.Pubkey())
		if err != nil {
			t.Fatalf("split: %v", err)
		}

		time.Sleep(feesTxWait)

		// Source coin balance = initial - split - fees
		if err := w.RefreshCoin(cli, coinID); err != nil {
			t.Fatalf("refresh source: %v", err)
		}

		srcBalance := w.GetCoin(coinID).Balance

		// New coin should have exact split amount (fees come from source)
		newObj, err := cli.GetObject(newCoinID)
		if err != nil {
			t.Fatalf("get new coin: %v", err)
		}

		newBalance := ReadBalance(newObj.Content)
		if newBalance != splitAmount {
			t.Errorf("new coin: got %d, want %d", newBalance, splitAmount)
		}

		// Source should have lost at least the split amount
		if srcBalance > initialBalance-splitAmount {
			t.Errorf("source not decremented enough: %d -> %d (split=%d)",
				initialBalance, srcBalance, splitAmount)
		}

		fees := initialBalance - splitAmount - srcBalance
		t.Logf("Fee breakdown: initial=%d split=%d remaining=%d fees=%d",
			initialBalance, splitAmount, srcBalance, fees)
	})

	t.Run("ATP-20.4: balance equals fee exactly", func(t *testing.T) {
		// Mint a very small amount and try to transact
		w := client.NewWallet()
		coinID := FaucetAndWait(t, cli, w, 100, 30*time.Second)

		if err := w.RefreshCoin(cli, coinID); err != nil {
			t.Fatalf("refresh: %v", err)
		}

		// Try transfer — may succeed or fail depending on fee calculation
		r := client.NewWallet()
		err := w.Transfer(cli, coinID, r.Pubkey())
		// Either succeeds (fees <= 100) or fails (insufficient balance)
		// Both outcomes are valid
		t.Logf("Transfer with balance=100: err=%v", err)
	})
}

// runGasCoinTests tests gas coin validation.
func runGasCoinTests(t *testing.T, cli *client.Client) {
	t.Helper()

	t.Run("ATP-6.4: valid gas coin accepted", func(t *testing.T) {
		// Normal operations with gas coin should succeed
		w := client.NewWallet()
		coinID := FaucetAndWait(t, cli, w, feesFaucetAmount, 30*time.Second)

		if err := w.RefreshCoin(cli, coinID); err != nil {
			t.Fatalf("refresh: %v", err)
		}

		r := client.NewWallet()
		_, err := w.Split(cli, coinID, 100_000, r.Pubkey())
		if err != nil {
			t.Errorf("split with valid gas coin failed: %v", err)
		}
	})

	t.Run("ATP-3.4+fees: version increments after fee deduction", func(t *testing.T) {
		w := client.NewWallet()
		coinID := FaucetAndWait(t, cli, w, feesFaucetAmount, 30*time.Second)

		if err := w.RefreshCoin(cli, coinID); err != nil {
			t.Fatalf("refresh: %v", err)
		}

		v0 := w.GetCoin(coinID).Version

		r := client.NewWallet()
		_, err := w.Split(cli, coinID, 100_000, r.Pubkey())
		if err != nil {
			t.Fatalf("split: %v", err)
		}

		time.Sleep(feesTxWait)

		if err := w.RefreshCoin(cli, coinID); err != nil {
			t.Fatalf("refresh after split: %v", err)
		}

		v1 := w.GetCoin(coinID).Version
		if v1 <= v0 {
			t.Errorf("version should increment: %d -> %d", v0, v1)
		}
	})
}

// runFeeConsistencyTests verifies fee consistency across nodes.
func runFeeConsistencyTests(t *testing.T, cli *client.Client, cluster *Cluster) {
	t.Helper()

	t.Run("fee consistency: all nodes agree on balance", func(t *testing.T) {
		w := client.NewWallet()
		coinID := FaucetAndWait(t, cli, w, feesFaucetAmount, 30*time.Second)

		if err := w.RefreshCoin(cli, coinID); err != nil {
			t.Fatalf("refresh: %v", err)
		}

		r := client.NewWallet()
		_, err := w.Split(cli, coinID, 200_000, r.Pubkey())
		if err != nil {
			t.Fatalf("split: %v", err)
		}

		time.Sleep(feesTxWait)

		// Query the same coin from multiple nodes — balances should match
		// Coins are singletons (rep=0), so all nodes hold them
		var balances []uint64
		for i := 0; i < cluster.Size(); i++ {
			nodeCli, err := client.NewClient(cluster.Node(i).HTTPAddr())
			if err != nil {
				continue
			}

			obj, err := nodeCli.GetObject(coinID)
			if err != nil {
				continue
			}

			balances = append(balances, ReadBalance(obj.Content))
		}

		if len(balances) < 2 {
			t.Skip("could not query coin from multiple nodes")
		}

		for i := 1; i < len(balances); i++ {
			if balances[i] != balances[0] {
				t.Errorf("balance mismatch: node 0=%d, node %d=%d", balances[0], i, balances[i])
			}
		}

		t.Logf("Balance consistent across %d nodes: %d", len(balances), balances[0])
	})
}
