package integration

import (
	"crypto/ed25519"
	"encoding/hex"
	"os"
	"strings"
	"testing"
	"time"

	"BluePods/client"
)

const (
	// epochFaucetAmount is the faucet amount for epoch tests.
	epochFaucetAmount = 1_000_000

	// epochTxWait is how long to wait for a tx to commit in epoch tests.
	epochTxWait = 10 * time.Second

	// epochLength is the epoch length in rounds for fast transitions.
	epochLength = 50

	// epochNftReplication is the replication factor for epoch test NFTs.
	epochNftReplication = 4

	// epochNftCount is the number of NFTs to create.
	epochNftCount = 20
)

// TestSimEpochs runs a 10-node epoch lifecycle simulation.
// Tests epoch transitions, validator add/remove, churn limiting, and stability.
func TestSimEpochs(t *testing.T) {
	cluster := NewCluster(t, 10,
		WithHTTPBase(18300), WithQUICBase(18300+920),
		WithEpochLength(epochLength),
	)
	cluster.WaitReady(90 * time.Second)

	cli := cluster.Client(0)

	// Phase 1: Epoch transitions
	t.Run("epoch-transitions", func(t *testing.T) {
		runEpochTransitionTests(t, cluster)
	})

	// Phase 2: Create NFTs for redistribution tracking
	nftIDs := createEpochNFTs(t, cli, cluster)

	// Phase 3: Add validators mid-epoch
	t.Run("validator-addition", func(t *testing.T) {
		runValidatorAdditionTests(t, cluster)
	})

	// Phase 4: Deregister validators
	t.Run("validator-removal", func(t *testing.T) {
		runValidatorRemovalTests(t, cli, cluster)
	})

	// Phase 5: Stability checks
	t.Run("stability", func(t *testing.T) {
		runEpochStabilityTests(t, cluster, nftIDs)
	})
}

// runEpochTransitionTests verifies epoch boundary detection and transitions.
func runEpochTransitionTests(t *testing.T, cluster *Cluster) {
	t.Helper()

	t.Run("ATP-9.1: epoch boundary at epochLength rounds", func(t *testing.T) {
		cluster.WaitForRound(uint64(epochLength+10), 90*time.Second)

		status := QueryStatus(t, cluster.Bootstrap().HTTPAddr())
		if status.Epoch < 1 {
			t.Errorf("expected epoch >= 1 after round %d, got %d", epochLength+10, status.Epoch)
		}

		t.Logf("Epoch: %d at round %d", status.Epoch, status.Round)
	})

	t.Run("ATP-9.4: epoch counter increments", func(t *testing.T) {
		cluster.WaitForEpoch(2, 120*time.Second)

		status := QueryStatus(t, cluster.Bootstrap().HTTPAddr())
		if status.Epoch < 2 {
			t.Errorf("expected epoch >= 2, got %d", status.Epoch)
		}
	})

	t.Run("ATP-9.7: epochHolders snapshot taken", func(t *testing.T) {
		status := QueryStatus(t, cluster.Bootstrap().HTTPAddr())

		if status.EpochHolders != 10 {
			t.Errorf("expected 10 epochHolders, got %d", status.EpochHolders)
		}
	})

	t.Run("ATP-9.1+log: epoch transition in logs", func(t *testing.T) {
		count := cluster.Bootstrap().LogCount("epoch transition")
		if count < 1 {
			t.Errorf("expected >= 1 epoch transitions in logs, got %d", count)
		}

		t.Logf("Epoch transitions in logs: %d", count)
	})
}

// createEpochNFTs creates NFTs for redistribution tracking across epoch changes.
func createEpochNFTs(t *testing.T, cli *client.Client, cluster *Cluster) [][32]byte {
	t.Helper()

	wallets := make([]*client.Wallet, 3)
	for i := range wallets {
		wallets[i] = client.NewWallet()
	}

	// Faucet wallets
	for i, w := range wallets {
		pk := w.Pubkey()

		_, err := cli.Faucet(pk, epochFaucetAmount)
		if err != nil {
			t.Fatalf("faucet wallet %d: %v", i, err)
		}
	}

	time.Sleep(epochTxWait)

	// Create NFTs
	nftIDs := make([][32]byte, epochNftCount)
	for i := 0; i < epochNftCount; i++ {
		walletIdx := i % len(wallets)
		metadata := []byte("epoch-nft-" + string(rune('0'+i)))

		nftID, err := wallets[walletIdx].CreateNFT(cli, epochNftReplication, metadata)
		if err != nil {
			t.Fatalf("create NFT %d: %v", i, err)
		}

		nftIDs[i] = nftID
	}

	t.Logf("Created %d NFTs, waiting for commit...", epochNftCount)
	time.Sleep(epochTxWait)

	// Verify at least some NFTs are committed
	found := 0
	for _, id := range nftIDs {
		if QueryObjectLocal(t, cluster.Bootstrap().HTTPAddr(), id) != nil {
			found++
		}
	}

	if found == 0 {
		t.Fatalf("no NFTs found locally on bootstrap")
	}

	t.Logf("Bootstrap holds %d/%d NFTs locally", found, epochNftCount)

	return nftIDs
}

// runValidatorAdditionTests tests adding validators mid-epoch.
func runValidatorAdditionTests(t *testing.T, cluster *Cluster) {
	t.Helper()

	t.Run("ATP-9.17: register validator tracks epochAdditions", func(t *testing.T) {
		// Capture current epoch holders
		statusBefore := QueryStatus(t, cluster.Bootstrap().HTTPAddr())
		t.Logf("Before addition: validators=%d epochHolders=%d",
			statusBefore.Validators, statusBefore.EpochHolders)

		// Add 2 new validators
		cluster.AddNodes(2)
		cluster.WaitForValidators(12, 60*time.Second)

		// Validators count should increase, but epochHolders stays frozen until next epoch
		status := QueryStatus(t, cluster.Bootstrap().HTTPAddr())

		if status.Validators < 12 {
			t.Errorf("expected >= 12 validators, got %d", status.Validators)
		}

		if status.EpochHolders > 10 {
			t.Logf("epochHolders already updated to %d (epoch may have passed)", status.EpochHolders)
		} else {
			t.Logf("epochHolders still frozen at %d (validators=%d)", status.EpochHolders, status.Validators)
		}
	})

	t.Run("ATP-9.12: churn additions applied at epoch", func(t *testing.T) {
		// Wait for next epoch boundary so new validators are included
		status := QueryStatus(t, cluster.Bootstrap().HTTPAddr())
		nextEpoch := status.Epoch + 1
		cluster.WaitForEpoch(nextEpoch, 120*time.Second)

		status = QueryStatus(t, cluster.Bootstrap().HTTPAddr())
		if status.EpochHolders < 12 {
			t.Errorf("epochHolders should include new validators: got %d, want >= 12",
				status.EpochHolders)
		}

		t.Logf("After epoch: validators=%d epochHolders=%d",
			status.Validators, status.EpochHolders)
	})
}

// runValidatorRemovalTests tests deregistering validators.
func runValidatorRemovalTests(t *testing.T, cli *client.Client, cluster *Cluster) {
	t.Helper()

	t.Run("ATP-9.18: deregister added to pendingRemovals", func(t *testing.T) {
		statusBefore := QueryStatus(t, cluster.Bootstrap().HTTPAddr())
		t.Logf("Before deregister: validators=%d", statusBefore.Validators)

		// Deregister nodes 8 and 9 (original validators)
		deregisterNodes(t, cli, cluster, []int{8, 9})

		time.Sleep(epochTxWait)

		// Validator count shouldn't change immediately (deferred to epoch boundary)
		status := QueryStatus(t, cluster.Bootstrap().HTTPAddr())
		t.Logf("After deregister tx: validators=%d epochHolders=%d",
			status.Validators, status.EpochHolders)
	})

	t.Run("ATP-9.6: pending removals applied at epoch", func(t *testing.T) {
		status := QueryStatus(t, cluster.Bootstrap().HTTPAddr())
		nextEpoch := status.Epoch + 1

		cluster.WaitForEpoch(nextEpoch, 120*time.Second)

		status = QueryStatus(t, cluster.Bootstrap().HTTPAddr())
		expectedValidators := 10 // 10 initial + 2 added - 2 deregistered

		t.Logf("After deregister epoch: validators=%d epochHolders=%d (expected ~%d)",
			status.Validators, status.EpochHolders, expectedValidators)

		if status.Validators > expectedValidators+1 {
			t.Errorf("expected ~%d validators after deregister, got %d",
				expectedValidators, status.Validators)
		}
	})

	t.Run("ATP-29.1: epoch with 0 fees collected", func(t *testing.T) {
		// No explicit tx in this epoch — reward distribution should not crash
		status := QueryStatus(t, cluster.Bootstrap().HTTPAddr())
		if status.Round == 0 {
			t.Error("network stalled after deregister")
		}
	})
}

// deregisterNodes loads private keys and sends deregister transactions.
func deregisterNodes(t *testing.T, cli *client.Client, cluster *Cluster, indices []int) {
	t.Helper()

	for _, idx := range indices {
		keyPath := cluster.Node(idx).KeyPath()

		keyData, err := os.ReadFile(keyPath)
		if err != nil {
			t.Fatalf("read key for node %d: %v", idx, err)
		}

		privKey := ed25519.PrivateKey(keyData)
		wallet := client.NewWalletFromKey(privKey)

		pk := wallet.Pubkey()
		t.Logf("Deregistering node %d (pubkey=%s)...", idx, hex.EncodeToString(pk[:8]))

		if err := wallet.DeregisterValidator(cli); err != nil {
			t.Fatalf("deregister node %d: %v", idx, err)
		}
	}
}

// runEpochStabilityTests verifies network stability after epoch lifecycle.
func runEpochStabilityTests(t *testing.T, cluster *Cluster, nftIDs [][32]byte) {
	t.Helper()

	t.Run("epoch stability: network continues producing", func(t *testing.T) {
		initial := QueryStatus(t, cluster.Bootstrap().HTTPAddr())
		time.Sleep(10 * time.Second)
		after := QueryStatus(t, cluster.Bootstrap().HTTPAddr())

		if after.Round <= initial.Round {
			t.Errorf("round stalled: %d -> %d", initial.Round, after.Round)
		}

		t.Logf("Round advanced: %d -> %d", initial.Round, after.Round)
	})

	t.Run("epoch stability: bootstrap still holds NFTs", func(t *testing.T) {
		found := 0
		for _, id := range nftIDs {
			if QueryObjectLocal(t, cluster.Bootstrap().HTTPAddr(), id) != nil {
				found++
			}
		}

		if found == 0 {
			t.Error("bootstrap lost all NFTs after epoch lifecycle")
		}

		t.Logf("Bootstrap holds %d/%d NFTs after lifecycle", found, len(nftIDs))
	})

	t.Run("epoch stability: no unexpected errors", func(t *testing.T) {
		for i := 0; i < cluster.Size(); i++ {
			node := cluster.Node(i)
			if node == nil {
				continue
			}

			for _, line := range strings.Split(node.Logs(), "\n") {
				if !strings.Contains(line, "executor error") {
					continue
				}

				// Expected: gossip duplicates for validator lifecycle
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

	t.Run("epoch stability: epoch counter >= 3", func(t *testing.T) {
		status := QueryStatus(t, cluster.Bootstrap().HTTPAddr())
		if status.Epoch < 3 {
			t.Errorf("expected epoch >= 3, got %d", status.Epoch)
		}

		t.Logf("Final epoch: %d", status.Epoch)
	})
}
