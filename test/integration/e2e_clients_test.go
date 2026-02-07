package integration

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"BluePods/client"
)

const (
	// e2eNumNodes is the number of nodes in the e2e test network.
	e2eNumNodes = 5

	// e2eHTTPBase is the base HTTP port for e2e test nodes.
	e2eHTTPBase = 8090

	// e2eQUICBase is the base QUIC port for e2e test nodes.
	e2eQUICBase = 9010

	// faucetAmount is the amount of tokens to mint for each wallet.
	faucetAmount = 1_000_000

	// txWaitTime is how long to wait for a transaction to commit.
	txWaitTime = 8 * time.Second
)

// TestE2EClients tests the full client flow: faucet, split, transfer.
func TestE2EClients(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping e2e test in short mode")
	}

	binaryPath := buildBinary(t)
	systemPodPath := findSystemPod(t)

	testDir, err := os.MkdirTemp("", "bluepods_e2e_*")
	if err != nil {
		t.Fatalf("create temp dir: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(testDir) })

	t.Logf("Test directory: %s", testDir)

	// Phase 1: Start network
	nodes := startE2ENetwork(t, binaryPath, systemPodPath, testDir)
	defer stopAllNodes(t, nodes)

	// Phase 2: Setup clients
	wallets, cli := setupClients(t, nodes[0].httpAddr)

	// Phase 3: Faucet
	coinIDs := faucetAll(t, cli, wallets)

	// Phase 4: Transactions
	runTransactions(t, cli, wallets, coinIDs)

	// Phase 5: Verification
	verifyNoConflicts(t, nodes)
}

// startE2ENetwork starts the e2e test network with custom ports.
func startE2ENetwork(t *testing.T, binary, systemPod, testDir string) []*TestNode {
	t.Helper()

	nodes := make([]*TestNode, e2eNumNodes)
	bootstrapQUIC := fmt.Sprintf("127.0.0.1:%d", e2eQUICBase)

	// Start bootstrap node
	t.Log("Starting bootstrap node...")
	nodes[0] = startE2ENode(t, binary, systemPod, testDir, 0, "", "", true)

	time.Sleep(2 * time.Second)
	if !nodes[0].isRunning() {
		t.Fatalf("Bootstrap failed:\nSTDOUT:\n%s\nSTDERR:\n%s",
			nodes[0].stdout.String(), nodes[0].stderr.String())
	}

	// Start validator nodes 1..N-1
	for i := 1; i < e2eNumNodes; i++ {
		t.Logf("Starting node %d...", i)
		nodes[i] = startE2ENode(t, binary, systemPod, testDir, i, bootstrapQUIC, "", false)

		t.Logf("Waiting for node %d to sync...", i)
		time.Sleep(syncWaitTime)

		if !nodes[i].isRunning() {
			t.Fatalf("Node %d failed:\n%s", i, nodes[i].stderr.String())
		}
	}

	// Wait for convergence
	t.Log("Waiting for convergence (15s)...")
	time.Sleep(15 * time.Second)

	// Verify all nodes are running and producing
	for i, node := range nodes {
		if !node.isRunning() {
			t.Fatalf("Node %d not running after convergence", i)
		}
	}

	return nodes
}

// startE2ENode starts a node with e2e-specific port offsets.
func startE2ENode(t *testing.T, binary, systemPod, testDir string, index int, syncAddr, registrationAddr string, isBootstrap bool) *TestNode {
	t.Helper()

	node := &TestNode{
		index:    index,
		httpAddr: fmt.Sprintf("127.0.0.1:%d", e2eHTTPBase+index),
		quicAddr: fmt.Sprintf("127.0.0.1:%d", e2eQUICBase+index),
		dataDir:  filepath.Join(testDir, fmt.Sprintf("e2e-node-%d", index)),
		stdout:   &bytes.Buffer{},
		stderr:   &bytes.Buffer{},
	}

	if err := os.MkdirAll(node.dataDir, 0755); err != nil {
		t.Fatalf("create node dir: %v", err)
	}

	args := buildE2ENodeArgs(node, systemPod, syncAddr, registrationAddr, isBootstrap)

	ctx, cancel := context.WithCancel(context.Background())
	node.cancel = cancel

	node.cmd = exec.CommandContext(ctx, binary, args...)
	node.cmd.Stdout = node.stdout
	node.cmd.Stderr = node.stderr

	if err := node.cmd.Start(); err != nil {
		t.Fatalf("start node %d: %v", index, err)
	}

	return node
}

// buildE2ENodeArgs builds command-line arguments for an e2e node.
func buildE2ENodeArgs(node *TestNode, systemPod, syncAddr, registrationAddr string, isBootstrap bool) []string {
	args := []string{
		"--data", node.dataDir,
		"--http", node.httpAddr,
		"--quic", node.quicAddr,
		"--system-pod", systemPod,
		"--min-validators", fmt.Sprintf("%d", e2eNumNodes),
	}

	if isBootstrap {
		args = append(args, "--bootstrap")
	} else {
		args = append(args, "--bootstrap-addr", syncAddr)
		if registrationAddr != "" && registrationAddr != syncAddr {
			args = append(args, "--registration-addr", registrationAddr)
		}
	}

	return args
}

// setupClients creates wallets and a client connected to the bootstrap node.
func setupClients(t *testing.T, bootstrapAddr string) ([]*client.Wallet, *client.Client) {
	t.Helper()

	cli, err := client.NewClient(bootstrapAddr)
	if err != nil {
		t.Fatalf("create client: %v", err)
	}

	sysPod := cli.SystemPod()
	t.Logf("Client connected, systemPod=%s", hex.EncodeToString(sysPod[:8]))

	// Verify validators
	validators, err := cli.Validators()
	if err != nil {
		t.Fatalf("get validators: %v", err)
	}

	t.Logf("Found %d validators", len(validators))
	if len(validators) != e2eNumNodes {
		t.Fatalf("expected %d validators, got %d", e2eNumNodes, len(validators))
	}

	// Create 3 wallets
	wallets := make([]*client.Wallet, 3)
	for i := range wallets {
		wallets[i] = client.NewWallet()
		pk := wallets[i].Pubkey()
		t.Logf("Wallet %d: %s", i, hex.EncodeToString(pk[:8]))
	}

	return wallets, cli
}

// faucetAll mints coins for all wallets and verifies them.
func faucetAll(t *testing.T, cli *client.Client, wallets []*client.Wallet) [][32]byte {
	t.Helper()

	coinIDs := make([][32]byte, len(wallets))

	for i, w := range wallets {
		pk := w.Pubkey()

		coinID, err := cli.Faucet(pk, faucetAmount)
		if err != nil {
			t.Fatalf("faucet wallet %d: %v", i, err)
		}

		coinIDs[i] = coinID
		t.Logf("Faucet wallet %d: coinID=%s", i, hex.EncodeToString(coinID[:8]))
	}

	// Wait for mint transactions to commit
	t.Logf("Waiting for faucet txs to commit (%v)...", txWaitTime)
	time.Sleep(txWaitTime)

	// Verify each coin
	for i, coinID := range coinIDs {
		verifyFaucetCoin(t, cli, wallets[i], coinID)
	}

	return coinIDs
}

// verifyFaucetCoin checks that a faucet coin was created correctly.
func verifyFaucetCoin(t *testing.T, cli *client.Client, w *client.Wallet, coinID [32]byte) {
	t.Helper()

	obj, err := cli.GetObject(coinID)
	if err != nil {
		t.Fatalf("get faucet coin %s: %v", hex.EncodeToString(coinID[:8]), err)
	}

	pk := w.Pubkey()
	if obj.Owner != pk {
		t.Fatalf("coin owner mismatch: expected %s, got %s",
			hex.EncodeToString(pk[:8]), hex.EncodeToString(obj.Owner[:8]))
	}

	t.Logf("Coin %s verified: version=%d owner=%s",
		hex.EncodeToString(coinID[:8]), obj.Version, hex.EncodeToString(obj.Owner[:8]))
}

// runTransactions executes the split and transfer test sequence.
func runTransactions(t *testing.T, cli *client.Client, wallets []*client.Wallet, coinIDs [][32]byte) {
	t.Helper()

	walletA := wallets[0]
	walletB := wallets[1]
	walletC := wallets[2]

	// Step 1: Wallet A splits 30% to Wallet B
	splitAmount := uint64(faucetAmount * 30 / 100)
	newCoinID := testSplit(t, cli, walletA, coinIDs[0], splitAmount, walletB.Pubkey())

	// Step 2: Wallet B transfers the received coin to Wallet C
	testTransfer(t, cli, walletB, newCoinID, walletC.Pubkey())

	// Step 3: Wallet C splits and sends part to Wallet A
	splitAmount2 := uint64(splitAmount * 50 / 100)
	_ = testSplit(t, cli, walletC, newCoinID, splitAmount2, walletA.Pubkey())
}

// testSplit performs a split and verifies the result.
func testSplit(t *testing.T, cli *client.Client, w *client.Wallet, coinID [32]byte, amount uint64, recipient [32]byte) [32]byte {
	t.Helper()

	if err := w.RefreshCoin(cli, coinID); err != nil {
		t.Fatalf("refresh coin before split: %v", err)
	}

	coin := w.GetCoin(coinID)
	t.Logf("Split: coin=%s version=%d balance=%d amount=%d",
		hex.EncodeToString(coinID[:8]), coin.Version, coin.Balance, amount)

	if coin.Balance < amount {
		t.Fatalf("insufficient balance: %d < %d", coin.Balance, amount)
	}

	newCoinID, err := w.Split(cli, coinID, amount, recipient)
	if err != nil {
		t.Fatalf("split failed: %v", err)
	}

	t.Logf("Split submitted, newCoinID=%s", hex.EncodeToString(newCoinID[:8]))
	t.Logf("Waiting for split to commit (%v)...", txWaitTime)
	time.Sleep(txWaitTime)

	// Verify source coin
	verifySplitSource(t, cli, coinID, coin.Balance-amount)

	// Verify new coin
	verifySplitTarget(t, cli, newCoinID, amount, recipient)

	return newCoinID
}

// verifySplitSource checks the source coin after split.
func verifySplitSource(t *testing.T, cli *client.Client, coinID [32]byte, expectedBalance uint64) {
	t.Helper()

	obj, err := cli.GetObject(coinID)
	if err != nil {
		t.Fatalf("get source coin after split: %v", err)
	}

	balance := readBalance(obj.Content)
	t.Logf("Source coin: version=%d balance=%d (expected %d)", obj.Version, balance, expectedBalance)

	if balance != expectedBalance {
		t.Fatalf("source balance mismatch: got %d, expected %d", balance, expectedBalance)
	}
}

// verifySplitTarget checks the new coin created by split.
func verifySplitTarget(t *testing.T, cli *client.Client, coinID [32]byte, expectedBalance uint64, expectedOwner [32]byte) {
	t.Helper()

	obj, err := cli.GetObject(coinID)
	if err != nil {
		t.Fatalf("get new coin after split: %v", err)
	}

	if obj.Owner != expectedOwner {
		t.Fatalf("new coin owner mismatch: expected %s, got %s",
			hex.EncodeToString(expectedOwner[:8]), hex.EncodeToString(obj.Owner[:8]))
	}

	balance := readBalance(obj.Content)
	t.Logf("New coin: version=%d balance=%d owner=%s",
		obj.Version, balance, hex.EncodeToString(obj.Owner[:8]))

	if balance != expectedBalance {
		t.Fatalf("new coin balance mismatch: got %d, expected %d", balance, expectedBalance)
	}
}

// testTransfer performs a transfer and verifies the result.
func testTransfer(t *testing.T, cli *client.Client, w *client.Wallet, coinID [32]byte, recipient [32]byte) {
	t.Helper()

	if err := w.RefreshCoin(cli, coinID); err != nil {
		t.Fatalf("refresh coin before transfer: %v", err)
	}

	coin := w.GetCoin(coinID)
	t.Logf("Transfer: coin=%s version=%d balance=%d -> %s",
		hex.EncodeToString(coinID[:8]), coin.Version, coin.Balance,
		hex.EncodeToString(recipient[:8]))

	if err := w.Transfer(cli, coinID, recipient); err != nil {
		t.Fatalf("transfer failed: %v", err)
	}

	t.Logf("Waiting for transfer to commit (%v)...", txWaitTime)
	time.Sleep(txWaitTime)

	// Verify coin owner changed
	obj, err := cli.GetObject(coinID)
	if err != nil {
		t.Fatalf("get coin after transfer: %v", err)
	}

	if obj.Owner != recipient {
		t.Fatalf("transfer owner mismatch: expected %s, got %s",
			hex.EncodeToString(recipient[:8]), hex.EncodeToString(obj.Owner[:8]))
	}

	t.Logf("Transfer verified: owner=%s version=%d",
		hex.EncodeToString(obj.Owner[:8]), obj.Version)
}

// readBalance extracts the u64 balance from coin content bytes (little-endian).
func readBalance(content []byte) uint64 {
	if len(content) < 8 {
		return 0
	}

	return uint64(content[0]) |
		uint64(content[1])<<8 |
		uint64(content[2])<<16 |
		uint64(content[3])<<24 |
		uint64(content[4])<<32 |
		uint64(content[5])<<40 |
		uint64(content[6])<<48 |
		uint64(content[7])<<56
}

// verifyNoConflicts checks that no test transactions produced conflicts.
func verifyNoConflicts(t *testing.T, nodes []*TestNode) {
	t.Helper()

	for i, node := range nodes {
		output := node.stdout.String()
		conflictCount := strings.Count(output, "conflicted tx")
		execErrors := strings.Count(output, "executor error")

		t.Logf("Node %d: %d conflicts, %d exec errors", i, conflictCount, execErrors)

		if conflictCount > 0 {
			for _, line := range strings.Split(output, "\n") {
				if strings.Contains(line, "conflicted tx") {
					t.Logf("  CONFLICT: %s", line)
				}
			}
		}
	}
}

// stopAllNodes stops all test nodes.
func stopAllNodes(t *testing.T, nodes []*TestNode) {
	t.Helper()
	t.Log("Stopping all nodes...")

	var wg sync.WaitGroup

	for i, node := range nodes {
		wg.Add(1)

		go func(idx int, n *TestNode) {
			defer wg.Done()
			n.stop()
			t.Logf("Node %d stopped", idx)
		}(i, node)
	}

	wg.Wait()
	t.Log("All nodes stopped")
}
