package integration

import (
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"BluePods/client"
)

func init() {
	// Configure the default HTTP transport to aggressively reuse connections.
	// Without this, macOS ephemeral ports get exhausted during polling-heavy tests
	// because closed connections sit in TIME_WAIT for 30-60s.
	// MaxIdleConns must be >= total nodes across all tests (50+).
	http.DefaultTransport = &http.Transport{
		DialContext:         (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
		MaxIdleConns:        500,
		MaxIdleConnsPerHost: 10,
		IdleConnTimeout:     120 * time.Second,
	}

	// Set a default timeout on http.DefaultClient. The client library uses
	// http.Get/Post which use DefaultClient. Without a timeout, requests can
	// hang forever if a node's handler is slow (e.g., routing to holder nodes).
	http.DefaultClient.Timeout = 30 * time.Second
}

// drainClose fully reads and closes a response body.
// This is critical for HTTP connection reuse: Go's transport only reuses
// connections when the body is fully consumed before Close().
func drainClose(body io.ReadCloser) {
	io.Copy(io.Discard, body)
	body.Close()
}

// statusResponse is the JSON response from GET /status.
type statusResponse struct {
	Round              uint64 `json:"round"`              // Round is the current consensus round
	LastCommitted      uint64 `json:"lastCommitted"`      // LastCommitted is the last committed round
	Validators         int    `json:"validators"`         // Validators is the active validator count
	FullQuorumAchieved bool   `json:"fullQuorumAchieved"` // FullQuorumAchieved indicates BFT quorum
	Epoch              uint64 `json:"epoch"`              // Epoch is the current epoch number
	EpochHolders       int    `json:"epochHolders"`       // EpochHolders is frozen validator count
	SystemPod          string `json:"systemPod"`          // SystemPod is the hex-encoded system pod ID
}

// objectResponse is the JSON response from GET /object/{id}.
type objectResponse struct {
	ID          string `json:"id"`          // ID is the hex-encoded object ID
	Version     uint64 `json:"version"`     // Version is the current version
	Owner       string `json:"owner"`       // Owner is hex-encoded owner public key
	Replication uint16 `json:"replication"` // Replication is the replication factor
	Content     string `json:"content"`     // Content is hex-encoded raw content
}

// validatorResponse is the JSON response for a single validator.
type validatorResponse struct {
	Pubkey string `json:"pubkey"` // Pubkey is hex-encoded public key
	HTTP   string `json:"http"`   // HTTP is the HTTP API endpoint
}

// httpClient is a shared HTTP client with timeout.
var httpClient = &http.Client{Timeout: 5 * time.Second}

// QueryStatus queries GET /status and fails on error.
func QueryStatus(t *testing.T, addr string) *statusResponse {
	t.Helper()

	status := QueryStatusSafe(addr)
	if status == nil {
		t.Fatalf("query status %s: failed", addr)
	}

	return status
}

// QueryStatusSafe queries GET /status, returns nil on error.
func QueryStatusSafe(addr string) *statusResponse {
	resp, err := httpClient.Get("http://" + addr + "/status")
	if err != nil {
		return nil
	}
	defer drainClose(resp.Body)

	if resp.StatusCode != http.StatusOK {
		return nil
	}

	var status statusResponse
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		return nil
	}

	return &status
}

// QueryHealth queries GET /health and returns the response map.
func QueryHealth(t *testing.T, addr string) map[string]string {
	t.Helper()

	resp, err := httpClient.Get("http://" + addr + "/health")
	if err != nil {
		t.Fatalf("query health %s: %v", addr, err)
	}
	defer drainClose(resp.Body)

	var result map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("decode health %s: %v", addr, err)
	}

	return result
}

// QueryValidators queries GET /validators.
func QueryValidators(t *testing.T, addr string) []validatorResponse {
	t.Helper()

	resp, err := httpClient.Get("http://" + addr + "/validators")
	if err != nil {
		t.Fatalf("query validators %s: %v", addr, err)
	}
	defer drainClose(resp.Body)

	var result []validatorResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("decode validators %s: %v", addr, err)
	}

	return result
}

// QueryObject queries GET /object/{id}, returns nil if 404.
func QueryObject(t *testing.T, addr string, id [32]byte) *objectResponse {
	t.Helper()

	url := "http://" + addr + "/object/" + hex.EncodeToString(id[:])
	resp, err := httpClient.Get(url)
	if err != nil {
		t.Fatalf("query object %s: %v", addr, err)
	}
	defer drainClose(resp.Body)

	if resp.StatusCode == http.StatusNotFound {
		return nil
	}

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("query object %s: status %d", addr, resp.StatusCode)
	}

	var result objectResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("decode object %s: %v", addr, err)
	}

	return &result
}

// QueryObjectLocal queries GET /object/{id}?local=true, returns nil if not local.
func QueryObjectLocal(t *testing.T, addr string, id [32]byte) *objectResponse {
	t.Helper()

	url := "http://" + addr + "/object/" + hex.EncodeToString(id[:]) + "?local=true"
	resp, err := httpClient.Get(url)
	if err != nil {
		return nil
	}
	defer drainClose(resp.Body)

	if resp.StatusCode != http.StatusOK {
		return nil
	}

	var result objectResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil
	}

	return &result
}

// QueryDomain queries GET /domain/{name}, returns objectID and found flag.
func QueryDomain(t *testing.T, addr string, name string) (string, bool) {
	t.Helper()

	resp, err := httpClient.Get("http://" + addr + "/domain/" + name)
	if err != nil {
		t.Fatalf("query domain %s: %v", addr, err)
	}
	defer drainClose(resp.Body)

	if resp.StatusCode == http.StatusNotFound {
		return "", false
	}

	var result struct {
		ObjectID string `json:"objectId"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", false
	}

	return result.ObjectID, true
}

// AssertObjectExists verifies an object exists on the network.
func AssertObjectExists(t *testing.T, cli *client.Client, id [32]byte) {
	t.Helper()

	_, err := cli.GetObject(id)
	if err != nil {
		t.Fatalf("object %s does not exist: %v", hex.EncodeToString(id[:8]), err)
	}
}

// AssertObjectOwner verifies an object's owner matches expected.
func AssertObjectOwner(t *testing.T, cli *client.Client, id, expectedOwner [32]byte) {
	t.Helper()

	obj, err := cli.GetObject(id)
	if err != nil {
		t.Fatalf("get object %s: %v", hex.EncodeToString(id[:8]), err)
	}

	if obj.Owner != expectedOwner {
		t.Fatalf("object owner: got %s, want %s",
			hex.EncodeToString(obj.Owner[:8]), hex.EncodeToString(expectedOwner[:8]))
	}
}

// AssertBalance verifies a coin's balance.
func AssertBalance(t *testing.T, cli *client.Client, coinID [32]byte, expected uint64) {
	t.Helper()

	obj, err := cli.GetObject(coinID)
	if err != nil {
		t.Fatalf("get coin %s: %v", hex.EncodeToString(coinID[:8]), err)
	}

	balance := ReadBalance(obj.Content)
	if balance != expected {
		t.Fatalf("balance: got %d, want %d", balance, expected)
	}
}

// ReadBalance extracts the u64 balance from coin content bytes (little-endian).
func ReadBalance(content []byte) uint64 {
	if len(content) < 8 {
		return 0
	}

	return binary.LittleEndian.Uint64(content[:8])
}

// CountHolders counts how many nodes hold an object locally.
func CountHolders(t *testing.T, nodes []*Node, objectID [32]byte) int {
	t.Helper()

	holders := 0

	for _, node := range nodes {
		if node == nil {
			continue
		}

		if QueryObjectLocal(t, node.httpAddr, objectID) != nil {
			holders++
		}
	}

	return holders
}

// WaitForHolders polls until at least minHolders nodes hold an object locally.
func WaitForHolders(t *testing.T, nodes []*Node, objectID [32]byte, minHolders int, timeout time.Duration) {
	t.Helper()

	deadline := time.Now().Add(timeout)

	for time.Now().Before(deadline) {
		holders := CountHolders(t, nodes, objectID)
		if holders >= minHolders {
			t.Logf("Object %s has %d holders (need %d)",
				hex.EncodeToString(objectID[:8]), holders, minHolders)
			return
		}

		time.Sleep(2 * time.Second)
	}

	t.Fatalf("timeout waiting for %d holders of object %s",
		minHolders, hex.EncodeToString(objectID[:8]))
}

// FaucetAndWait mints a coin and polls until the object appears.
func FaucetAndWait(t *testing.T, cli *client.Client, w *client.Wallet, amount uint64, timeout time.Duration) [32]byte {
	t.Helper()

	pk := w.Pubkey()

	coinID, err := cli.Faucet(pk, amount)
	if err != nil {
		t.Fatalf("faucet: %v", err)
	}

	WaitForObject(t, cli, coinID, timeout)

	return coinID
}

// WaitForOwner polls GET /object/{id} until the owner matches expected or timeout.
func WaitForOwner(t *testing.T, cli *client.Client, id [32]byte, expectedOwner [32]byte, timeout time.Duration) {
	t.Helper()

	deadline := time.Now().Add(timeout)

	for time.Now().Before(deadline) {
		obj, err := cli.GetObject(id)
		if err == nil && obj.Owner == expectedOwner {
			return
		}

		time.Sleep(time.Second)
	}

	t.Fatalf("timeout waiting for owner change on object %s", hex.EncodeToString(id[:8]))
}

// WaitForObject polls GET /object/{id} until it exists or timeout.
func WaitForObject(t *testing.T, cli *client.Client, id [32]byte, timeout time.Duration) {
	t.Helper()

	deadline := time.Now().Add(timeout)

	for time.Now().Before(deadline) {
		_, err := cli.GetObject(id)
		if err == nil {
			return
		}

		time.Sleep(time.Second)
	}

	t.Fatalf("timeout waiting for object %s", hex.EncodeToString(id[:8]))
}
