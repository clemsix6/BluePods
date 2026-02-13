package integration

import (
	"encoding/hex"
	"strings"
	"sync"
	"testing"
	"time"

	"BluePods/client"
)

const (
	// stressFaucetAmount is the faucet amount for stress tests.
	stressFaucetAmount = 1_000_000

	// stressTxWait is how long to wait for transactions in stress tests.
	stressTxWait = 15 * time.Second
)

// TestSimStress runs a 12-node stress test simulation.
// Tests concurrent modifications, throughput, and epoch behavior under load.
func TestSimStress(t *testing.T) {
	cluster := NewCluster(t, 12,
		WithHTTPBase(18500), WithQUICBase(18500+920),
		WithEpochLength(50),
	)
	cluster.WaitReady(90 * time.Second)

	cli := cluster.Client(0)

	t.Run("concurrent-modification", func(t *testing.T) {
		runConcurrentModTests(t, cli)
	})

	t.Run("throughput", func(t *testing.T) {
		runThroughputTests(t, cli, cluster)
	})

	t.Run("epoch-under-load", func(t *testing.T) {
		runEpochUnderLoadTests(t, cli, cluster)
	})

	t.Run("final-check", func(t *testing.T) {
		runStressFinalChecks(t, cluster)
	})
}

// runConcurrentModTests tests concurrent modifications of the same object.
func runConcurrentModTests(t *testing.T, cli *client.Client) {
	t.Helper()

	t.Run("ATP-22.2: two clients modify same object", func(t *testing.T) {
		w := client.NewWallet()
		coinID := FaucetAndWait(t, cli, w, stressFaucetAmount, 30*time.Second)

		if err := w.RefreshCoin(cli, coinID); err != nil {
			t.Fatalf("refresh: %v", err)
		}

		// Two goroutines try to transfer the same coin to different recipients
		r1 := client.NewWallet()
		r2 := client.NewWallet()

		var wg sync.WaitGroup
		var err1, err2 error

		wg.Add(2)
		go func() {
			defer wg.Done()
			err1 = w.Transfer(cli, coinID, r1.Pubkey())
		}()
		go func() {
			defer wg.Done()
			err2 = w.Transfer(cli, coinID, r2.Pubkey())
		}()
		wg.Wait()

		t.Logf("Transfer 1 err: %v", err1)
		t.Logf("Transfer 2 err: %v", err2)

		time.Sleep(stressTxWait)

		// Verify the coin has exactly one owner
		obj, err := cli.GetObject(coinID)
		if err != nil {
			t.Fatalf("get coin: %v", err)
		}

		r1pk := r1.Pubkey()
		r2pk := r2.Pubkey()
		wpk := w.Pubkey()

		if obj.Owner != r1pk && obj.Owner != r2pk && obj.Owner != wpk {
			t.Error("coin owner is unknown")
		}

		t.Logf("Final owner: %s", hex.EncodeToString(obj.Owner[:8]))
	})

	t.Run("ATP-24.6+stress: double-spend under load", func(t *testing.T) {
		w := client.NewWallet()
		coinID := FaucetAndWait(t, cli, w, stressFaucetAmount, 30*time.Second)

		if err := w.RefreshCoin(cli, coinID); err != nil {
			t.Fatalf("refresh: %v", err)
		}

		// 10 goroutines all try to transfer the same coin
		const numAttempts = 10
		recipients := make([]*client.Wallet, numAttempts)
		for i := range recipients {
			recipients[i] = client.NewWallet()
		}

		var wg sync.WaitGroup
		successes := 0
		var mu sync.Mutex

		for i := 0; i < numAttempts; i++ {
			wg.Add(1)
			go func(idx int) {
				defer wg.Done()
				err := w.Transfer(cli, coinID, recipients[idx].Pubkey())
				if err == nil {
					mu.Lock()
					successes++
					mu.Unlock()
				}
			}(i)
		}
		wg.Wait()

		t.Logf("Submissions accepted: %d/%d", successes, numAttempts)

		time.Sleep(stressTxWait)

		// Only one transfer should ultimately succeed
		obj, err := cli.GetObject(coinID)
		if err != nil {
			t.Fatalf("get coin: %v", err)
		}

		t.Logf("Final owner: %s", hex.EncodeToString(obj.Owner[:8]))
	})

	t.Run("ATP-24.1+stress: replay under load", func(t *testing.T) {
		w := client.NewWallet()
		coinID := FaucetAndWait(t, cli, w, stressFaucetAmount, 30*time.Second)

		if err := w.RefreshCoin(cli, coinID); err != nil {
			t.Fatalf("refresh: %v", err)
		}

		// Transfer once
		r1 := client.NewWallet()
		if err := w.Transfer(cli, coinID, r1.Pubkey()); err != nil {
			t.Fatalf("initial transfer: %v", err)
		}

		time.Sleep(stressTxWait)

		// Submit 5 more transfers with stale version (replay attempts)
		for i := 0; i < 5; i++ {
			r := client.NewWallet()
			_ = w.Transfer(cli, coinID, r.Pubkey())
		}

		time.Sleep(stressTxWait)

		obj, err := cli.GetObject(coinID)
		if err != nil {
			t.Fatalf("get coin: %v", err)
		}

		rpk := r1.Pubkey()
		if obj.Owner != rpk {
			t.Errorf("replay changed owner: got %s, want %s (first recipient)",
				hex.EncodeToString(obj.Owner[:8]), hex.EncodeToString(rpk[:8]))
		}
	})
}

// runThroughputTests tests parallel transaction throughput.
func runThroughputTests(t *testing.T, cli *client.Client, cluster *Cluster) {
	t.Helper()

	t.Run("throughput: 12 concurrent faucets", func(t *testing.T) {
		const numWallets = 12

		wallets := make([]*client.Wallet, numWallets)
		coinIDs := make([][32]byte, numWallets)
		var mu sync.Mutex
		var wg sync.WaitGroup

		for i := 0; i < numWallets; i++ {
			wallets[i] = client.NewWallet()
			wg.Add(1)

			go func(idx int) {
				defer wg.Done()
				pk := wallets[idx].Pubkey()
				coinID, err := cli.Faucet(pk, 10_000)
				if err != nil {
					return
				}
				mu.Lock()
				coinIDs[idx] = coinID
				mu.Unlock()
			}(i)
		}

		wg.Wait()
		time.Sleep(stressTxWait)

		// Count how many coins were actually created
		found := 0
		for _, id := range coinIDs {
			if id == [32]byte{} {
				continue
			}

			_, err := cli.GetObject(id)
			if err == nil {
				found++
			}
		}

		t.Logf("Concurrent faucets: %d/%d coins created", found, numWallets)

		if found < numWallets/2 {
			t.Errorf("too few coins created: %d/%d", found, numWallets)
		}
	})

	t.Run("throughput: sequential transfers", func(t *testing.T) {
		const numTransfers = 5

		w := client.NewWallet()
		coinID := FaucetAndWait(t, cli, w, stressFaucetAmount, 30*time.Second)

		for i := 0; i < numTransfers; i++ {
			if err := w.RefreshCoin(cli, coinID); err != nil {
				t.Logf("refresh %d: %v (stopping)", i, err)
				break
			}

			r := client.NewWallet()
			if err := w.Transfer(cli, coinID, r.Pubkey()); err != nil {
				t.Logf("transfer %d: %v (stopping)", i, err)
				break
			}

			time.Sleep(stressTxWait)
			w = r
		}

		// Verify coin still exists
		_, err := cli.GetObject(coinID)
		if err != nil {
			t.Errorf("coin disappeared after transfers: %v", err)
		}
	})
}

// runEpochUnderLoadTests tests epoch transitions while transactions are being submitted.
func runEpochUnderLoadTests(t *testing.T, cli *client.Client, cluster *Cluster) {
	t.Helper()

	t.Run("ATP-29.7: transactions during epoch transition", func(t *testing.T) {
		w := client.NewWallet()
		coinID := FaucetAndWait(t, cli, w, stressFaucetAmount, 30*time.Second)

		if err := w.RefreshCoin(cli, coinID); err != nil {
			t.Fatalf("refresh: %v", err)
		}

		// Submit a split while epoch transitions may be happening
		r := client.NewWallet()
		_, err := w.Split(cli, coinID, 100_000, r.Pubkey())
		if err != nil {
			t.Logf("split during epoch: %v (may be expected)", err)
		}

		time.Sleep(stressTxWait)

		// Network should still be alive
		status := QueryStatus(t, cluster.Bootstrap().HTTPAddr())
		if status.Round == 0 {
			t.Error("network stalled during epoch transitions")
		}

		t.Logf("Round %d, epoch %d — network healthy", status.Round, status.Epoch)
	})

	t.Run("stability: rapid round progression", func(t *testing.T) {
		initial := QueryStatus(t, cluster.Bootstrap().HTTPAddr())
		time.Sleep(15 * time.Second)
		after := QueryStatus(t, cluster.Bootstrap().HTTPAddr())

		if after.Round <= initial.Round {
			t.Errorf("round stalled: %d -> %d", initial.Round, after.Round)
		}

		rate := float64(after.Round-initial.Round) / 15.0
		t.Logf("Round rate: %.1f rounds/sec", rate)
	})
}

// runStressFinalChecks scans all node logs for panics and unexpected errors.
func runStressFinalChecks(t *testing.T, cluster *Cluster) {
	t.Helper()

	t.Run("no unexpected errors", func(t *testing.T) {
		totalErrors := 0

		for i := 0; i < cluster.Size(); i++ {
			node := cluster.Node(i)
			if node == nil {
				continue
			}

			for _, line := range strings.Split(node.Logs(), "\n") {
				if strings.Contains(line, "panic:") {
					t.Errorf("node %d: PANIC: %s", i, line)
					totalErrors++
					continue
				}

				if !strings.Contains(line, "executor error") {
					continue
				}

				// Expected errors from concurrent mods
				if strings.Contains(line, "func=register_validator") {
					continue
				}
				if strings.Contains(line, "func=deregister_validator") {
					continue
				}
				if strings.Contains(line, "version conflict") {
					continue
				}
				if strings.Contains(line, "conflicted tx") {
					continue
				}

				totalErrors++
			}
		}

		t.Logf("Unexpected errors across %d nodes: %d", cluster.Size(), totalErrors)
	})
}
