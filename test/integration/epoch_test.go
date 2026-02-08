package integration

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
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
	// epNumInitialNodes is the number of initial validators.
	epNumInitialNodes = 10

	// epNumLateJoiners is the number of validators that join mid-epoch.
	epNumLateJoiners = 2

	// epNumDeregistered is the number of validators that deregister.
	epNumDeregistered = 2

	// epHTTPBase is the base HTTP port for epoch test nodes.
	epHTTPBase = 12100

	// epQUICBase is the base QUIC port for epoch test nodes.
	// Convention: QUIC port = HTTP port + 920 (see getRegistrationHTTPAddr).
	epQUICBase = epHTTPBase + 920

	// epEpochLength is the epoch length in rounds for fast epoch transitions.
	epEpochLength = 50

	// epSyncBufferSec is the sync buffer duration.
	epSyncBufferSec = 4

	// epTxWait is how long to wait for transactions to commit.
	epTxWait = 10 * time.Second

	// epNftReplication is the replication factor for test NFTs.
	epNftReplication = 4

	// epNftCount is the number of NFTs to create.
	epNftCount = 20
)

// TestEpochValidatorLifecycle tests the full epoch system with real nodes:
// 1. Start 10 validators with --epoch-length 50
// 2. Wait for network convergence and epoch transition
// 3. Create 20 NFTs with replication=4, verify sharding
// 4. Add 2 new validators → wait for epoch transition → verify holders update
// 5. Deregister 2 validators → wait for epoch → verify objects redistributed
// 6. Verify network stability and no errors throughout
func TestEpochValidatorLifecycle(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping epoch integration test in short mode")
	}

	binaryPath := buildBinary(t)
	systemPodPath := findSystemPod(t)

	testDir, err := os.MkdirTemp("", "bluepods_epoch_*")
	if err != nil {
		t.Fatalf("create temp dir: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(testDir) })

	t.Logf("Test directory: %s", testDir)
	t.Logf("Epoch length: %d rounds", epEpochLength)

	// =========================================================================
	// Phase 1: Start initial 10-node network with epoch support
	// =========================================================================
	t.Log("=== Phase 1: Start 10-node network with epochs ===")
	nodes := epStartNetwork(t, binaryPath, systemPodPath, testDir, epNumInitialNodes)
	defer epStopAllNodes(t, nodes)

	// Wait for all validators to register
	epWaitForValidators(t, nodes[0].httpAddr, epNumInitialNodes, 60*time.Second)

	// Wait for network to advance past first epoch boundary (round 50)
	t.Logf("Waiting for network to reach round %d...", epEpochLength+10)
	epWaitForRound(t, nodes[0].httpAddr, epEpochLength+10, 90*time.Second)

	// Verify epoch via API
	httpCli := &http.Client{Timeout: 5 * time.Second}
	status := epQueryStatus(httpCli, nodes[0].httpAddr)

	if status == nil {
		t.Fatalf("failed to get status after first epoch")
	}

	if status.Epoch < 1 {
		t.Fatalf("expected epoch >= 1 via API, got %d", status.Epoch)
	}
	t.Logf("Epoch via API: %d, epochHolders: %d", status.Epoch, status.EpochHolders)

	// Secondary validation: log grep
	bootstrapOutput := nodes[0].stdout.String()
	epochCount := strings.Count(bootstrapOutput, "epoch transition")
	t.Logf("Bootstrap node: %d epoch transitions (log)", epochCount)

	if epochCount < 1 {
		t.Fatalf("bootstrap expected >= 1 epoch transitions in logs, got %d", epochCount)
	}

	// =========================================================================
	// Phase 2: Create NFTs with replication and verify sharding
	// =========================================================================
	t.Log("=== Phase 2: Create NFTs and verify sharding ===")

	cli, err := client.NewClient(nodes[0].httpAddr)
	if err != nil {
		t.Fatalf("create client: %v", err)
	}

	wallets := make([]*client.Wallet, 3)
	for i := range wallets {
		wallets[i] = client.NewWallet()
	}

	// Faucet wallets
	for i, w := range wallets {
		pk := w.Pubkey()
		_, err := cli.Faucet(pk, faucetAmount)
		if err != nil {
			t.Fatalf("faucet wallet %d: %v", i, err)
		}
	}

	t.Logf("Waiting for faucet (%v)...", epTxWait)
	time.Sleep(epTxWait)

	// Create NFTs
	nftIDs := make([][32]byte, epNftCount)
	for i := 0; i < epNftCount; i++ {
		walletIdx := i % len(wallets)
		metadata := []byte(fmt.Sprintf("epoch-test-nft-%d", i))

		nftID, err := wallets[walletIdx].CreateNFT(cli, epNftReplication, metadata)
		if err != nil {
			t.Fatalf("create NFT %d: %v", i, err)
		}

		nftIDs[i] = nftID
	}

	t.Logf("Created %d NFTs, waiting for commit (%v)...", epNftCount, epTxWait)
	time.Sleep(epTxWait)

	// Verify NFTs are committed by querying bootstrap locally.
	// With parallel startup, only bootstrap commits (other nodes stuck in sync gap).
	// Bootstrap stores NFTs it's a holder for via Rendezvous hashing.
	bootstrapLocal := 0
	for _, nftID := range nftIDs {
		url := fmt.Sprintf("http://%s/object/%s?local=true", nodes[0].httpAddr, hex.EncodeToString(nftID[:]))
		resp, err := httpCli.Get(url)
		if err != nil {
			continue
		}
		resp.Body.Close()

		if resp.StatusCode == http.StatusOK {
			bootstrapLocal++
		}
	}

	t.Logf("Bootstrap holds %d/%d NFTs locally (expected ~%d with replication=%d, validators=%d)",
		bootstrapLocal, epNftCount, epNftCount*epNftReplication/epNumInitialNodes, epNftReplication, epNumInitialNodes)

	if bootstrapLocal == 0 {
		// No NFTs at all — check if they were committed
		bootstrapLog := nodes[0].stdout.String()
		commitCount := strings.Count(bootstrapLog, "committed tx")
		createNftCount := strings.Count(bootstrapLog, "create_nft")
		t.Logf("Bootstrap logs: %d committed tx, %d create_nft references", commitCount, createNftCount)
		t.Fatalf("bootstrap holds 0 NFTs locally — NFTs may not have been committed")
	}

	// =========================================================================
	// Phase 3: Add 2 new validators mid-epoch
	// =========================================================================
	t.Logf("=== Phase 3: Add %d new validators ===", epNumLateJoiners)

	bootstrapQUIC := fmt.Sprintf("127.0.0.1:%d", epQUICBase)
	totalNodes := epNumInitialNodes

	for i := 0; i < epNumLateJoiners; i++ {
		idx := totalNodes + i
		t.Logf("Starting late joiner node %d...", idx)

		node := epStartNode(t, binaryPath, systemPodPath, testDir,
			idx, bootstrapQUIC, false, epNumInitialNodes)
		nodes = append(nodes, node)
	}

	totalNodes += epNumLateJoiners

	// Wait for new validators to register
	epWaitForValidators(t, nodes[0].httpAddr, totalNodes, 60*time.Second)

	// Wait for an epoch to pass so epochHolders includes new validators
	currentRound := epGetRound(t, nodes[0].httpAddr)
	nextEpochRound := ((currentRound / epEpochLength) + 1) * epEpochLength
	t.Logf("Current round: %d, waiting for next epoch at round %d...", currentRound, nextEpochRound+5)
	epWaitForRound(t, nodes[0].httpAddr, nextEpochRound+5, 90*time.Second)

	// Verify epoch transition happened with new validator count
	bootstrapOutput = nodes[0].stdout.String()
	epochTransitions := strings.Count(bootstrapOutput, "epoch transition")
	t.Logf("After join: %d epoch transitions, %d validators", epochTransitions, totalNodes)

	// Verify epochHolders includes new validators via API
	status = epQueryStatus(httpCli, nodes[0].httpAddr)
	if status != nil && status.EpochHolders != totalNodes {
		t.Errorf("expected %d epoch holders after join, got %d", totalNodes, status.EpochHolders)
	}

	// =========================================================================
	// Phase 4: Deregister 2 validators
	// =========================================================================
	t.Logf("=== Phase 4: Deregister %d validators ===", epNumDeregistered)

	// Load private keys of nodes to deregister (nodes 8 and 9)
	for i := 0; i < epNumDeregistered; i++ {
		nodeIdx := epNumInitialNodes - 2 + i // nodes 8 and 9
		keyPath := filepath.Join(nodes[nodeIdx].dataDir, "key")

		keyData, err := os.ReadFile(keyPath)
		if err != nil {
			t.Fatalf("read key file for node %d: %v", nodeIdx, err)
		}

		privKey := ed25519.PrivateKey(keyData)
		validatorWallet := client.NewWalletFromKey(privKey)

		pk := validatorWallet.Pubkey()
		t.Logf("Deregistering validator node %d (pubkey=%s)...",
			nodeIdx, hex.EncodeToString(pk[:8]))

		if err := validatorWallet.DeregisterValidator(cli); err != nil {
			t.Fatalf("deregister validator %d: %v", nodeIdx, err)
		}
	}

	t.Logf("Waiting for deregister txs to commit (%v)...", epTxWait)
	time.Sleep(epTxWait)

	// Wait for epoch transition to apply removals
	currentRound = epGetRound(t, nodes[0].httpAddr)
	nextEpochRound = ((currentRound / epEpochLength) + 1) * epEpochLength
	t.Logf("Current round: %d, waiting for epoch at round %d to apply removals...", currentRound, nextEpochRound+5)
	epWaitForRound(t, nodes[0].httpAddr, nextEpochRound+5, 90*time.Second)

	// Verify validator count decreased
	expectedAfterDeregister := totalNodes - epNumDeregistered
	status = epQueryStatus(httpCli, nodes[0].httpAddr)

	if status == nil {
		t.Fatalf("failed to get status after deregister")
	}

	t.Logf("Validators after deregister: %d (expected %d)", status.Validators, expectedAfterDeregister)

	if status.Validators != expectedAfterDeregister {
		t.Errorf("expected %d validators after deregister, got %d", expectedAfterDeregister, status.Validators)
	}

	// Verify epochHolders also decreased
	if status.EpochHolders != expectedAfterDeregister {
		t.Errorf("expected %d epoch holders after deregister, got %d", expectedAfterDeregister, status.EpochHolders)
	}

	// Verify bootstrap still holds NFTs locally after deregistration
	bootstrapLocalAfter := 0
	for _, nftID := range nftIDs {
		url := fmt.Sprintf("http://%s/object/%s?local=true", nodes[0].httpAddr, hex.EncodeToString(nftID[:]))
		resp, err := httpCli.Get(url)
		if err != nil {
			continue
		}
		resp.Body.Close()

		if resp.StatusCode == http.StatusOK {
			bootstrapLocalAfter++
		}
	}

	t.Logf("After deregister: bootstrap holds %d/%d NFTs locally (was %d)", bootstrapLocalAfter, epNftCount, bootstrapLocal)

	// With fewer validators, bootstrap should hold >= as many NFTs as before
	// (fewer validators = higher probability of being a holder per object)
	if bootstrapLocalAfter < bootstrapLocal {
		t.Errorf("expected bootstrap to hold >= %d NFTs after deregister (2 fewer validators), got %d",
			bootstrapLocal, bootstrapLocalAfter)
	}

	// =========================================================================
	// Phase 5: Verify network stability
	// =========================================================================
	t.Log("=== Phase 5: Verify network stability ===")
	time.Sleep(5 * time.Second)

	activeNodes := 0
	for i, node := range nodes {
		if !node.isRunning() {
			// Deregistered nodes may still be running
			if i >= epNumInitialNodes-epNumDeregistered && i < epNumInitialNodes {
				continue
			}
			t.Errorf("Node %d should be running but isn't", i)
		} else {
			activeNodes++
		}
	}
	t.Logf("Active nodes: %d", activeNodes)

	// Verify epoch counter via API
	status = epQueryStatus(httpCli, nodes[0].httpAddr)
	if status != nil {
		t.Logf("Final epoch via API: %d, epochHolders: %d", status.Epoch, status.EpochHolders)
		if status.Epoch < 3 {
			t.Errorf("expected epoch >= 3 via API, got %d", status.Epoch)
		}
	}

	// Secondary: verify via log grep
	bootstrapOutput = nodes[0].stdout.String()
	finalEpochCount := strings.Count(bootstrapOutput, "epoch transition")
	t.Logf("Bootstrap final epoch transitions (log): %d", finalEpochCount)

	if finalEpochCount < 3 {
		t.Errorf("expected >= 3 epoch transitions on bootstrap, got %d", finalEpochCount)
	}

	// Check for errors
	epVerifyNoErrors(t, nodes)

	t.Log("=== Epoch lifecycle test PASSED ===")
}

// epStartNetwork starts the initial validator network with epoch support.
func epStartNetwork(t *testing.T, binary, systemPod, testDir string, numNodes int) []*TestNode {
	t.Helper()

	nodes := make([]*TestNode, numNodes)
	bootstrapQUIC := fmt.Sprintf("127.0.0.1:%d", epQUICBase)

	// Start bootstrap
	t.Log("Starting bootstrap node...")
	nodes[0] = epStartNode(t, binary, systemPod, testDir, 0, "", true, numNodes)

	time.Sleep(4 * time.Second)

	if !nodes[0].isRunning() {
		t.Fatalf("Bootstrap failed:\nSTDOUT:\n%s\nSTDERR:\n%s",
			nodes[0].stdout.String(), nodes[0].stderr.String())
	}

	// Start remaining nodes in parallel
	t.Logf("Starting %d nodes in parallel...", numNodes-1)
	var wg sync.WaitGroup

	for i := 1; i < numNodes; i++ {
		wg.Add(1)

		go func(idx int) {
			defer wg.Done()
			nodes[idx] = epStartNode(t, binary, systemPod, testDir,
				idx, bootstrapQUIC, false, numNodes)
		}(i)
	}

	wg.Wait()
	t.Log("All nodes started, waiting for sync...")

	// Wait for sync + convergence (30s like 50-node test)
	syncWait := time.Duration(epSyncBufferSec+6) * time.Second
	t.Logf("Waiting for sync (%v)...", syncWait)
	time.Sleep(syncWait)

	// Verify all nodes running
	failed := 0
	for i, node := range nodes {
		if node == nil || !node.isRunning() {
			t.Logf("Node %d not running", i)
			failed++
		}
	}

	if failed > 0 {
		t.Fatalf("%d/%d nodes failed to start", failed, numNodes)
	}

	// Wait for convergence
	t.Log("Waiting for convergence (30s)...")
	time.Sleep(30 * time.Second)

	t.Logf("All %d nodes running", numNodes)
	return nodes
}

// epStartNode starts a single node with epoch configuration.
func epStartNode(t *testing.T, binary, systemPod, testDir string, index int, bootstrapQUIC string, isBootstrap bool, minValidators int) *TestNode {
	t.Helper()

	node := &TestNode{
		index:    index,
		httpAddr: fmt.Sprintf("127.0.0.1:%d", epHTTPBase+index),
		quicAddr: fmt.Sprintf("127.0.0.1:%d", epQUICBase+index),
		dataDir:  filepath.Join(testDir, fmt.Sprintf("ep-node-%d", index)),
		stdout:   &bytes.Buffer{},
		stderr:   &bytes.Buffer{},
	}

	if err := os.MkdirAll(node.dataDir, 0755); err != nil {
		t.Fatalf("create node dir %d: %v", index, err)
	}

	keyPath := filepath.Join(node.dataDir, "key")

	args := []string{
		"--data", node.dataDir,
		"--http", node.httpAddr,
		"--quic", node.quicAddr,
		"--key", keyPath,
		"--system-pod", systemPod,
		"--min-validators", fmt.Sprintf("%d", minValidators),
		"--sync-buffer", fmt.Sprintf("%d", epSyncBufferSec),
		"--epoch-length", fmt.Sprintf("%d", epEpochLength),
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

// epWaitForValidators polls /status until the expected number of validators is reached.
func epWaitForValidators(t *testing.T, httpAddr string, expected int, timeout time.Duration) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	httpCli := &http.Client{Timeout: 5 * time.Second}

	for time.Now().Before(deadline) {
		status := epQueryStatus(httpCli, httpAddr)
		if status != nil && status.Validators >= expected {
			t.Logf("Validators reached: %d (expected %d)", status.Validators, expected)
			return
		}

		time.Sleep(2 * time.Second)
	}

	t.Fatalf("timeout waiting for %d validators on %s", expected, httpAddr)
}

// epWaitForRound polls /status until the network reaches the target round.
func epWaitForRound(t *testing.T, httpAddr string, targetRound uint64, timeout time.Duration) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	httpCli := &http.Client{Timeout: 5 * time.Second}

	for time.Now().Before(deadline) {
		status := epQueryStatus(httpCli, httpAddr)
		if status != nil && status.Round >= targetRound {
			t.Logf("Round reached: %d (target %d)", status.Round, targetRound)
			return
		}

		time.Sleep(2 * time.Second)
	}

	t.Fatalf("timeout waiting for round %d on %s", targetRound, httpAddr)
}

// epGetRound returns the current round from the /status endpoint.
func epGetRound(t *testing.T, httpAddr string) uint64 {
	t.Helper()

	httpCli := &http.Client{Timeout: 5 * time.Second}
	status := epQueryStatus(httpCli, httpAddr)

	if status == nil {
		t.Fatalf("failed to get status from %s", httpAddr)
	}

	return status.Round
}

// epQueryStatus queries the /status endpoint, returns nil on error.
func epQueryStatus(httpCli *http.Client, addr string) *statusResponse {
	resp, err := httpCli.Get("http://" + addr + "/status")
	if err != nil {
		return nil
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil
	}

	var status statusResponse
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		return nil
	}

	return &status
}

// epVerifyNoErrors checks for unexpected errors in node logs.
// Ignores expected duplicate errors from gossip (register/deregister_validator).
func epVerifyNoErrors(t *testing.T, nodes []*TestNode) {
	t.Helper()

	for i, node := range nodes {
		if node == nil {
			continue
		}

		for _, line := range strings.Split(node.stdout.String(), "\n") {
			if !strings.Contains(line, "executor error") {
				continue
			}

			// Expected: gossip duplicates for validator registration/deregistration
			if strings.Contains(line, "func=register_validator") {
				continue
			}
			if strings.Contains(line, "func=deregister_validator") {
				continue
			}

			t.Errorf("Node %d unexpected error: %s", i, line)
		}
	}
}

// epStopAllNodes stops all nodes in parallel.
func epStopAllNodes(t *testing.T, nodes []*TestNode) {
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
	t.Logf("All nodes stopped")
}
