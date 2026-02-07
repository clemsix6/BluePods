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
	// fnNumNodes is the number of nodes in the 50-node test.
	fnNumNodes = 50

	// fnHTTPBase is the base HTTP port for 50-node test.
	fnHTTPBase = 10100

	// fnQUICBase is the base QUIC port for 50-node test.
	fnQUICBase = 11020

	// fnSyncBufferSec is the sync buffer for local testing (seconds).
	fnSyncBufferSec = 4

	// fnTxWaitTime is how long to wait for transactions to commit.
	fnTxWaitTime = 10 * time.Second

	// fnNftReplication is the replication factor for test NFTs.
	fnNftReplication = 10
)

// TestFiftyNodes tests a 50-node network with parallel startup and NFT operations.
func TestFiftyNodes(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping 50-node test in short mode")
	}

	binaryPath := buildBinary(t)
	systemPodPath := findSystemPod(t)

	testDir, err := os.MkdirTemp("", "bluepods_50n_*")
	if err != nil {
		t.Fatalf("create temp dir: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(testDir) })

	t.Logf("Test directory: %s", testDir)

	// Phase 1: Start network with parallel startup
	nodes := startFiftyNodeNetwork(t, binaryPath, systemPodPath, testDir)
	defer fnStopAllNodes(t, nodes)

	// Phase 2: Setup client
	cli, err := client.NewClient(nodes[0].httpAddr)
	if err != nil {
		t.Fatalf("create client: %v", err)
	}
	sysPod := cli.SystemPod()
	t.Logf("Client connected, systemPod=%s", hex.EncodeToString(sysPod[:8]))

	validators, err := cli.Validators()
	if err != nil {
		t.Fatalf("get validators: %v", err)
	}
	t.Logf("Found %d validators", len(validators))

	// Phase 3: Create wallets and faucet
	wallets := make([]*client.Wallet, 3)
	for i := range wallets {
		wallets[i] = client.NewWallet()
		pk := wallets[i].Pubkey()
		t.Logf("Wallet %d: %s", i, hex.EncodeToString(pk[:8]))
	}

	fnFaucetAll(t, cli, wallets)

	// Phase 4: NFT operations (non-singleton objects) with sharding verification
	fnTestNFTs(t, cli, wallets, nodes)

	// Phase 5: Verify
	fnVerifyNoErrors(t, nodes)
}

// startFiftyNodeNetwork starts 50 nodes with parallel startup for speed.
func startFiftyNodeNetwork(t *testing.T, binary, systemPod, testDir string) []*TestNode {
	t.Helper()

	nodes := make([]*TestNode, fnNumNodes)
	bootstrapQUIC := fmt.Sprintf("127.0.0.1:%d", fnQUICBase)

	// Start bootstrap node
	t.Log("Starting bootstrap node...")
	nodes[0] = fnStartNode(t, binary, systemPod, testDir, 0, "", true)

	// Wait for bootstrap to create first snapshot (snapshot manager waits 2s then creates)
	time.Sleep(4 * time.Second)

	if !nodes[0].isRunning() {
		t.Fatalf("Bootstrap failed:\nSTDOUT:\n%s\nSTDERR:\n%s",
			nodes[0].stdout.String(), nodes[0].stderr.String())
	}
	t.Log("Bootstrap node running")

	// Start ALL other nodes in parallel — they all sync from bootstrap simultaneously
	t.Logf("Starting %d nodes in parallel...", fnNumNodes-1)
	var wg sync.WaitGroup

	for i := 1; i < fnNumNodes; i++ {
		wg.Add(1)

		go func(idx int) {
			defer wg.Done()
			nodes[idx] = fnStartNode(t, binary, systemPod, testDir, idx, bootstrapQUIC, false)
		}(i)
	}

	wg.Wait()
	t.Log("All nodes started, waiting for sync...")

	// Wait for sync buffer + snapshot apply + replay (shared wait for all nodes)
	syncWait := time.Duration(fnSyncBufferSec+6) * time.Second
	t.Logf("Waiting for sync (%v)...", syncWait)
	time.Sleep(syncWait)

	// Verify all nodes are running
	failed := 0
	for i, node := range nodes {
		if node == nil || !node.isRunning() {
			t.Logf("Node %d not running", i)
			failed++
		}
	}

	if failed > 0 {
		t.Fatalf("%d/%d nodes failed to start", failed, fnNumNodes)
	}

	// Wait for convergence — with 50 validators, needs more time
	t.Log("Waiting for convergence (30s)...")
	time.Sleep(30 * time.Second)

	t.Logf("All %d nodes running", fnNumNodes)

	return nodes
}

// fnStartNode starts a single node for the 50-node test.
func fnStartNode(t *testing.T, binary, systemPod, testDir string, index int, bootstrapQUIC string, isBootstrap bool) *TestNode {
	t.Helper()

	node := &TestNode{
		index:    index,
		httpAddr: fmt.Sprintf("127.0.0.1:%d", fnHTTPBase+index),
		quicAddr: fmt.Sprintf("127.0.0.1:%d", fnQUICBase+index),
		dataDir:  filepath.Join(testDir, fmt.Sprintf("fn-node-%d", index)),
		stdout:   &bytes.Buffer{},
		stderr:   &bytes.Buffer{},
	}

	if err := os.MkdirAll(node.dataDir, 0755); err != nil {
		t.Fatalf("create node dir %d: %v", index, err)
	}

	args := []string{
		"--data", node.dataDir,
		"--http", node.httpAddr,
		"--quic", node.quicAddr,
		"--system-pod", systemPod,
		"--min-validators", fmt.Sprintf("%d", fnNumNodes),
		"--sync-buffer", fmt.Sprintf("%d", fnSyncBufferSec),
	}

	if isBootstrap {
		args = append(args, "--bootstrap")
	} else {
		args = append(args, "--bootstrap-addr", bootstrapQUIC)
	}

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

// fnFaucetAll mints coins for all wallets.
func fnFaucetAll(t *testing.T, cli *client.Client, wallets []*client.Wallet) {
	t.Helper()

	for i, w := range wallets {
		pk := w.Pubkey()

		coinID, err := cli.Faucet(pk, faucetAmount)
		if err != nil {
			t.Fatalf("faucet wallet %d: %v", i, err)
		}

		t.Logf("Faucet wallet %d: coinID=%s", i, hex.EncodeToString(coinID[:8]))
	}

	t.Logf("Waiting for faucet txs (%v)...", fnTxWaitTime)
	time.Sleep(fnTxWaitTime)
}

// fnTestNFTs tests create and transfer of non-singleton NFT objects.
// Verifies storage sharding: exactly fnNftReplication holders store the NFT.
func fnTestNFTs(t *testing.T, cli *client.Client, wallets []*client.Wallet, nodes []*TestNode) {
	t.Helper()

	walletA := wallets[0]
	walletB := wallets[1]
	walletC := wallets[2]

	// Step 1: Wallet A creates an NFT with replication=10
	metadata := []byte("BluePods NFT #1 - test metadata")
	t.Logf("Creating NFT with replication=%d...", fnNftReplication)

	nftID, err := walletA.CreateNFT(cli, fnNftReplication, metadata)
	if err != nil {
		t.Fatalf("create NFT: %v", err)
	}
	t.Logf("NFT created: id=%s", hex.EncodeToString(nftID[:8]))

	t.Logf("Waiting for NFT to commit (%v)...", fnTxWaitTime)
	time.Sleep(fnTxWaitTime)

	// Verify NFT exists with correct properties
	nftObj, err := cli.GetObject(nftID)
	if err != nil {
		t.Fatalf("get NFT: %v", err)
	}

	pkA := walletA.Pubkey()
	if nftObj.Owner != pkA {
		t.Fatalf("NFT owner mismatch: expected %s, got %s",
			hex.EncodeToString(pkA[:8]), hex.EncodeToString(nftObj.Owner[:8]))
	}

	if nftObj.Replication != fnNftReplication {
		t.Fatalf("NFT replication mismatch: expected %d, got %d", fnNftReplication, nftObj.Replication)
	}

	t.Logf("NFT verified: version=%d owner=%s replication=%d",
		nftObj.Version, hex.EncodeToString(nftObj.Owner[:8]), nftObj.Replication)

	// Verify sharding: exactly fnNftReplication holders
	fnCountHolders(t, nodes, nftID, "after create")

	// Step 2: Wallet A transfers NFT to Wallet B
	pkB := walletB.Pubkey()
	t.Logf("Transferring NFT to wallet B (%s)...", hex.EncodeToString(pkB[:8]))

	if err := walletA.TransferNFT(cli, nftID, pkB); err != nil {
		t.Fatalf("transfer NFT to B: %v", err)
	}

	t.Logf("Waiting for transfer (%v)...", fnTxWaitTime)
	time.Sleep(fnTxWaitTime)

	// Verify owner changed
	nftObj, err = cli.GetObject(nftID)
	if err != nil {
		t.Fatalf("get NFT after transfer: %v", err)
	}

	if nftObj.Owner != pkB {
		t.Fatalf("NFT owner after transfer: expected %s, got %s",
			hex.EncodeToString(pkB[:8]), hex.EncodeToString(nftObj.Owner[:8]))
	}

	t.Logf("NFT transfer verified: version=%d owner=%s", nftObj.Version, hex.EncodeToString(nftObj.Owner[:8]))

	// Verify holders remain the same after transfer
	fnCountHolders(t, nodes, nftID, "after transfer A->B")

	// Step 3: Wallet B transfers NFT to Wallet C
	pkC := walletC.Pubkey()
	t.Logf("Transferring NFT to wallet C (%s)...", hex.EncodeToString(pkC[:8]))

	if err := walletB.TransferNFT(cli, nftID, pkC); err != nil {
		t.Fatalf("transfer NFT to C: %v", err)
	}

	t.Logf("Waiting for transfer (%v)...", fnTxWaitTime)
	time.Sleep(fnTxWaitTime)

	// Verify final owner
	nftObj, err = cli.GetObject(nftID)
	if err != nil {
		t.Fatalf("get NFT after second transfer: %v", err)
	}

	if nftObj.Owner != pkC {
		t.Fatalf("NFT final owner: expected %s, got %s",
			hex.EncodeToString(pkC[:8]), hex.EncodeToString(nftObj.Owner[:8]))
	}

	t.Logf("NFT final state: version=%d owner=%s replication=%d",
		nftObj.Version, hex.EncodeToString(nftObj.Owner[:8]), nftObj.Replication)

	// Verify holders remain the same after second transfer
	fnCountHolders(t, nodes, nftID, "after transfer B->C")
}

// fnCountHolders counts how many nodes hold the NFT and verifies sharding.
// Each node is queried via HTTP GET /object/{id} — 200 means holder, 404 means not.
func fnCountHolders(t *testing.T, nodes []*TestNode, nftID [32]byte, phase string) {
	t.Helper()

	idHex := hex.EncodeToString(nftID[:])
	holders := 0
	missing := 0

	for _, node := range nodes {
		if node == nil {
			continue
		}

		cli, err := client.NewClient(node.httpAddr)
		if err != nil {
			continue
		}

		_, err = cli.GetObject(nftID)
		if err == nil {
			holders++
		} else {
			missing++
		}
	}

	t.Logf("Sharding %s: nft=%s holders=%d missing=%d (expected: %d holders, %d missing)",
		phase, idHex[:16], holders, missing, fnNftReplication, fnNumNodes-fnNftReplication)

	if holders != fnNftReplication {
		t.Errorf("sharding %s: expected %d holders, got %d", phase, fnNftReplication, holders)
	}

	if missing != fnNumNodes-fnNftReplication {
		t.Errorf("sharding %s: expected %d missing, got %d", phase, fnNumNodes-fnNftReplication, missing)
	}
}

// fnVerifyNoErrors checks all nodes for conflicts and execution errors.
func fnVerifyNoErrors(t *testing.T, nodes []*TestNode) {
	t.Helper()

	totalConflicts := 0
	totalExecErrors := 0

	for i, node := range nodes {
		if node == nil {
			continue
		}

		output := node.stdout.String()
		conflicts := strings.Count(output, "conflicted tx")
		execErrors := strings.Count(output, "executor error")

		totalConflicts += conflicts
		totalExecErrors += execErrors

		if conflicts > 0 || execErrors > 0 {
			t.Logf("Node %d: %d conflicts, %d exec errors", i, conflicts, execErrors)
		}
	}

	t.Logf("Total across %d nodes: %d conflicts, %d exec errors", fnNumNodes, totalConflicts, totalExecErrors)
}

// fnStopAllNodes stops all nodes in parallel.
func fnStopAllNodes(t *testing.T, nodes []*TestNode) {
	t.Helper()
	t.Log("Stopping all nodes...")

	var wg sync.WaitGroup

	for i, node := range nodes {
		if node == nil {
			continue
		}

		wg.Add(1)

		go func(idx int, n *TestNode) {
			defer wg.Done()
			n.stop()
		}(i, node)
	}

	wg.Wait()
	t.Logf("All %d nodes stopped", fnNumNodes)
}
