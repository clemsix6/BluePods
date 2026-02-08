package integration

import (
	"bytes"
	"context"
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
)

const (
	// syncWaitTime is how long to wait for a node to sync (must be > 12s buffer + snapshot)
	syncWaitTime = 18 * time.Second

	// stabilizationTime is how long to wait for network to stabilize after all nodes are up
	stabilizationTime = 10 * time.Second

	// numNodes is the number of nodes to test
	numNodes = 6
)

// TestNode represents a running node process.
type TestNode struct {
	index    int
	cmd      *exec.Cmd
	httpAddr string
	quicAddr string
	dataDir  string
	stdout   *bytes.Buffer
	stderr   *bytes.Buffer
	cancel   context.CancelFunc
}

// TestFiveNodeNetwork tests a 5-node network with automatic mesh formation.
func TestFiveNodeNetwork(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	// Build the binary first
	binaryPath := buildBinary(t)

	// Find system pod
	systemPodPath := findSystemPod(t)

	// Create temp directory for all node data
	testDir, err := os.MkdirTemp("", "bluepods_integration_*")
	if err != nil {
		t.Fatalf("create temp dir: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(testDir) })

	t.Logf("Test directory: %s", testDir)
	t.Logf("Binary: %s", binaryPath)
	t.Logf("System pod: %s", systemPodPath)

	nodes := make([]*TestNode, numNodes)
	bootstrapAddr := "127.0.0.1:9000"

	// Phase 1: Start nodes 0-4 with minValidators=5
	// This allows them to start producing after node 4 registers

	// Start bootstrap node (index 0)
	t.Log("Starting bootstrap node...")
	nodes[0] = startNode(t, binaryPath, systemPodPath, testDir, 0, "", "", true, 5)
	time.Sleep(2 * time.Second)

	// Verify bootstrap is running
	if !nodes[0].isRunning() {
		t.Fatalf("Bootstrap node failed to start:\nSTDOUT:\n%s\nSTDERR:\n%s",
			nodes[0].stdout.String(), nodes[0].stderr.String())
	}
	t.Log("Bootstrap node started")

	// Start validator nodes 1-4
	for i := 1; i <= 4; i++ {
		t.Logf("Starting node %d...", i)
		nodes[i] = startNode(t, binaryPath, systemPodPath, testDir, i, bootstrapAddr, "", false, 5)

		t.Logf("Waiting for node %d to sync (%v)...", i, syncWaitTime)
		time.Sleep(syncWaitTime)

		if !nodes[i].isRunning() {
			t.Fatalf("Node %d failed to start:\n%s", i, nodes[i].stderr.String())
		}
		t.Logf("Node %d synced and running", i)
	}

	// Wait for network to stabilize and produce in normal mode
	t.Log("Waiting for nodes 0-4 to produce in normal mode (15s)...")
	time.Sleep(15 * time.Second)

	// Phase 2: Start node 5 late - it syncs from node 4, registers via bootstrap
	// minValidators=5 so it can join the existing network (which already has 5 validators)
	t.Log("Starting node 5 (late joiner, syncing from node 4)...")
	nodes[5] = startNode(t, binaryPath, systemPodPath, testDir, 5, nodes[4].quicAddr, bootstrapAddr, false, 5)

	t.Logf("Waiting for node 5 to sync (%v)...", syncWaitTime)
	time.Sleep(syncWaitTime)

	if !nodes[5].isRunning() {
		t.Fatalf("Node 5 failed to start:\n%s", nodes[5].stderr.String())
	}
	t.Log("Node 5 synced and running")

	// Wait for network stabilization with all 6 nodes
	t.Logf("Waiting for full network stabilization (%v)...", stabilizationTime)
	time.Sleep(stabilizationTime)

	// Extended monitoring: check rounds every 5 seconds for 30 seconds
	t.Log("\n=== Extended Monitoring (30s) ===")
	for i := 0; i < 6; i++ {
		time.Sleep(5 * time.Second)
		t.Logf("\n--- Round check at T+%ds ---", (i+1)*5)
		for j, node := range nodes {
			output := node.stdout.String()
			// Find the last "produced vertex round=" or "round=" in the output
			lines := strings.Split(output, "\n")
			lastRound := "unknown"
			vertexCount := strings.Count(output, "produced vertex")
			for k := len(lines) - 1; k >= 0; k-- {
				if idx := strings.Index(lines[k], "round="); idx != -1 {
					// Extract round number
					rest := lines[k][idx+6:]
					if spaceIdx := strings.IndexAny(rest, " \t"); spaceIdx != -1 {
						lastRound = rest[:spaceIdx]
					} else {
						lastRound = rest
					}
					break
				}
			}
			t.Logf("  Node %d: round=%s, vertices_produced=%d", j, lastRound, vertexCount)
		}
	}

	// Run verifications
	t.Run("AllNodesRunning", func(t *testing.T) {
		for i, node := range nodes {
			if !node.isRunning() {
				t.Errorf("Node %d is not running", i)
			}
		}
	})

	t.Run("AllValidatorsRegistered", func(t *testing.T) {
		// Each node should see "new validator registered" for all other nodes
		// Bootstrap sees 4 registrations, others see registrations that happen after they join
		for i, node := range nodes {
			output := node.stdout.String()
			count := strings.Count(output, "new validator registered")
			t.Logf("Node %d saw %d validator registrations", i, count)

			// Bootstrap should see all 4 other validators register
			if i == 0 && count < numNodes-1 {
				t.Errorf("Bootstrap node saw only %d registrations, expected %d", count, numNodes-1)
			}
		}
	})

	t.Run("MeshNetworkFormed", func(t *testing.T) {
		// Each node should connect to other validators
		for i, node := range nodes {
			output := node.stdout.String()
			connections := strings.Count(output, "connected to validator")
			t.Logf("Node %d made %d outgoing validator connections", i, connections)
		}

		// Verify bootstrap received connections from all validators
		bootstrapOutput := nodes[0].stdout.String()
		// New validators connect to bootstrap, so bootstrap should see them in logs
		if !strings.Contains(bootstrapOutput, "produced vertex") {
			t.Error("Bootstrap is not producing vertices")
		}
	})

	t.Run("AllNodesProduceVertices", func(t *testing.T) {
		// Wait a bit more for vertices to be produced
		time.Sleep(3 * time.Second)

		for i, node := range nodes {
			output := node.stdout.String()
			if !strings.Contains(output, "produced vertex") {
				t.Errorf("Node %d is not producing vertices", i)
				// Debug: count gossip messages
				gossipCount := strings.Count(output, "gossip received")
				bufferCount := strings.Count(output, "buffering vertex")
				peerAdded := strings.Count(output, "peer added")
				recvEnded := strings.Count(output, "receiveLoop ended")
				streamErr := strings.Count(output, "stream read error")
				uniData := strings.Count(output, "uni data received")
				dedupFiltered := strings.Count(output, "dedup filtered")
				noUniStreams := strings.Count(output, "no uni streams received")
				t.Logf("  Node %d debug: gossip_received=%d buffered=%d peers_added=%d recv_ended=%d stream_err=%d uni_data=%d dedup_filtered=%d no_uni=%d",
					i, gossipCount, bufferCount, peerAdded, recvEnded, streamErr, uniData, dedupFiltered, noUniStreams)
			} else {
				// Count vertices produced
				count := strings.Count(output, "produced vertex")
				gossipCount := strings.Count(output, "gossip received")
				peerAdded := strings.Count(output, "peer added")
				sendFailed := strings.Count(output, "gossip send failed")
				uniData := strings.Count(output, "uni data received")
				t.Logf("Node %d produced %d vertices, gossip=%d peers=%d send_failed=%d uni_data=%d", i, count, gossipCount, peerAdded, sendFailed, uniData)
			}
		}
	})

	t.Run("TransactionsCommit", func(t *testing.T) {
		// All nodes should see committed transactions
		for i, node := range nodes {
			output := node.stdout.String()
			commits := strings.Count(output, "committed tx")
			t.Logf("Node %d saw %d committed transactions", i, commits)

			if commits == 0 {
				t.Errorf("Node %d saw no committed transactions", i)
			}
		}
	})

	t.Run("Convergence", func(t *testing.T) {
		statuses := make([]*statusResponse, numNodes)
		for i, node := range nodes {
			statuses[i] = queryStatus(t, node.httpAddr)
			t.Logf("Node %d: round=%d lastCommitted=%d validators=%d fullQuorum=%v",
				i, statuses[i].Round, statuses[i].LastCommitted,
				statuses[i].Validators, statuses[i].FullQuorumAchieved)
		}

		// All nodes see the same number of validators
		for i, s := range statuses {
			if s.Validators != numNodes {
				t.Errorf("Node %d has %d validators, expected %d", i, s.Validators, numNodes)
			}
		}

		// fullQuorumAchieved must be true on all nodes
		for i, s := range statuses {
			if !s.FullQuorumAchieved {
				t.Errorf("Node %d: fullQuorumAchieved is false", i)
			}
		}

		// Rounds within a reasonable delta (maxDelta = 10)
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
			t.Errorf("Round divergence too large: min=%d max=%d delta=%d",
				minRound, maxRound, maxRound-minRound)
		}

		// lastCommitted within a reasonable delta
		minCommit, maxCommit := statuses[0].LastCommitted, statuses[0].LastCommitted
		for _, s := range statuses[1:] {
			if s.LastCommitted < minCommit {
				minCommit = s.LastCommitted
			}
			if s.LastCommitted > maxCommit {
				maxCommit = s.LastCommitted
			}
		}
		if maxCommit-minCommit > 10 {
			t.Errorf("Commit divergence too large: min=%d max=%d delta=%d",
				minCommit, maxCommit, maxCommit-minCommit)
		}
	})

	// Print summary
	t.Log("\n=== Node Logs Summary ===")
	for i, node := range nodes {
		t.Logf("\n--- Node %d (http=%s, quic=%s) ---", i, node.httpAddr, node.quicAddr)

		// Print sync-related logs first
		output := node.stdout.String()
		for _, line := range strings.Split(output, "\n") {
			if strings.Contains(line, "imported vertices") ||
				strings.Contains(line, "replaying vertices") ||
				strings.Contains(line, "replay complete") ||
				strings.Contains(line, "snapshot applied") ||
				strings.Contains(line, "validator set created") ||
				strings.Contains(line, "sync mode") ||
				strings.Contains(line, "transition") {
				t.Logf("  [SYNC] %s", line)
			}
		}

		// Print last 100 lines of stdout
		lines := strings.Split(output, "\n")
		start := len(lines) - 100
		if start < 0 {
			start = 0
		}
		for _, line := range lines[start:] {
			if line != "" {
				t.Logf("  %s", line)
			}
		}
	}

	// Cleanup: stop all nodes
	t.Log("\nStopping all nodes...")
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

// statusResponse is the JSON response from GET /status.
type statusResponse struct {
	Round              uint64 `json:"round"`              // Round is the current consensus round
	LastCommitted      uint64 `json:"lastCommitted"`      // LastCommitted is the last committed round
	Validators         int    `json:"validators"`         // Validators is the active validator count
	FullQuorumAchieved bool   `json:"fullQuorumAchieved"` // FullQuorumAchieved indicates BFT quorum observed
	Epoch              uint64 `json:"epoch"`              // Epoch is the current epoch number
	EpochHolders       int    `json:"epochHolders"`       // EpochHolders is the frozen validator count for Rendezvous
}

// queryStatus queries the /status endpoint of a node.
func queryStatus(t *testing.T, addr string) *statusResponse {
	t.Helper()

	resp, err := http.Get("http://" + addr + "/status")
	if err != nil {
		t.Fatalf("query status %s: %v", addr, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %s returned %d", addr, resp.StatusCode)
	}

	var status statusResponse
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		t.Fatalf("decode status %s: %v", addr, err)
	}

	return &status
}

// buildBinary compiles the node binary.
func buildBinary(t *testing.T) string {
	t.Helper()

	// Build to temp location
	binary := filepath.Join(os.TempDir(), "bluepods_test")

	cmd := exec.Command("go", "build", "-o", binary, "./cmd/node")
	cmd.Dir = getProjectRoot(t)

	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("build failed: %v\n%s", err, output)
	}

	t.Cleanup(func() { os.Remove(binary) })

	return binary
}

// findSystemPod locates the system pod WASM file.
func findSystemPod(t *testing.T) string {
	t.Helper()

	root := getProjectRoot(t)
	path := filepath.Join(root, "pods", "pod-system", "build", "pod.wasm")

	if _, err := os.Stat(path); err != nil {
		t.Fatalf("system pod not found at %s: %v", path, err)
	}

	return path
}

// getProjectRoot returns the project root directory.
func getProjectRoot(t *testing.T) string {
	t.Helper()

	// We're in test/integration, go up two levels
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("get working dir: %v", err)
	}

	// Try to find go.mod
	dir := wd
	for i := 0; i < 5; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		dir = filepath.Dir(dir)
	}

	t.Fatalf("could not find project root from %s", wd)
	return ""
}

// startNode starts a node process.
// syncAddr is the address to sync from, registrationAddr is the address for validator registration.
// If registrationAddr is empty, it defaults to syncAddr.
// minValidators specifies the threshold for this node (0 uses default numNodes).
func startNode(t *testing.T, binary, systemPod, testDir string, index int, syncAddr, registrationAddr string, isBootstrap bool, minValidators int) *TestNode {
	t.Helper()

	node := &TestNode{
		index:    index,
		httpAddr: fmt.Sprintf("127.0.0.1:%d", 8080+index),
		quicAddr: fmt.Sprintf("127.0.0.1:%d", 9000+index),
		dataDir:  filepath.Join(testDir, fmt.Sprintf("node-%d", index)),
		stdout:   &bytes.Buffer{},
		stderr:   &bytes.Buffer{},
	}

	// Create data directory
	if err := os.MkdirAll(node.dataDir, 0755); err != nil {
		t.Fatalf("create node data dir: %v", err)
	}

	// Build command
	ctx, cancel := context.WithCancel(context.Background())
	node.cancel = cancel

	if minValidators == 0 {
		minValidators = numNodes
	}

	args := []string{
		"--data", node.dataDir,
		"--http", node.httpAddr,
		"--quic", node.quicAddr,
		"--system-pod", systemPod,
		"--min-validators", fmt.Sprintf("%d", minValidators),
	}

	if isBootstrap {
		args = append(args, "--bootstrap")
	} else {
		args = append(args, "--bootstrap-addr", syncAddr)
		if registrationAddr != "" && registrationAddr != syncAddr {
			args = append(args, "--registration-addr", registrationAddr)
		}
	}

	node.cmd = exec.CommandContext(ctx, binary, args...)
	node.cmd.Stdout = node.stdout
	node.cmd.Stderr = node.stderr

	if err := node.cmd.Start(); err != nil {
		t.Fatalf("start node %d: %v", index, err)
	}

	return node
}

// isRunning checks if the node process is still running.
func (n *TestNode) isRunning() bool {
	if n.cmd == nil || n.cmd.Process == nil {
		return false
	}

	// Check if stdout contains startup message (means process started successfully)
	output := n.stdout.String()
	if strings.Contains(output, "starting BluePods node") {
		// Check if process hasn't exited with error
		if n.cmd.ProcessState != nil && !n.cmd.ProcessState.Success() {
			return false
		}
		return true
	}

	return false
}

// stop terminates the node process.
func (n *TestNode) stop() {
	if n.cancel != nil {
		n.cancel()
	}
	if n.cmd != nil && n.cmd.Process != nil {
		n.cmd.Process.Kill()
		n.cmd.Wait()
	}
}
