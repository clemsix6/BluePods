package integration

import (
	"encoding/binary"
	"encoding/hex"
	"sync"
	"testing"
	"time"

	"BluePods/client"
	"BluePods/internal/network"
)

// transportCache memoizes one QUIC transport per node address so polling-heavy
// tests reuse a dialer instead of constructing one per call.
var (
	transportMu    sync.Mutex
	transportCache = map[string]*client.QUICTransport{}
)

// transportFor returns a cached QUIC transport for a node address.
func transportFor(addr string) *client.QUICTransport {
	transportMu.Lock()
	defer transportMu.Unlock()

	if tr, ok := transportCache[addr]; ok {
		return tr
	}

	tr := client.NewQUICTransport(addr)
	transportCache[addr] = tr

	return tr
}

// statusResponse mirrors the operational fields the node reports over the QUIC
// status message (the former HTTP /status).
type statusResponse struct {
	Round         uint64 // Round is the current consensus round
	LastCommitted uint64 // LastCommitted is the last committed round
	Validators    int    // Validators is the active validator count
	Epoch         uint64 // Epoch is the current epoch number
	EpochHolders  int    // EpochHolders is the frozen holder-snapshot count
	SystemPod     string // SystemPod is the hex-encoded system pod ID
}

// objectResponse is a parsed object fetched over QUIC.
type objectResponse struct {
	ID          string // ID is the hex-encoded object ID
	Version     uint64 // Version is the current version
	Owner       string // Owner is the hex-encoded owner public key
	Replication uint16 // Replication is the replication factor
	Content     []byte // Content is the raw content bytes
}

// validatorResponse describes a single validator from the QUIC validator set.
type validatorResponse struct {
	Pubkey string // Pubkey is the hex-encoded public key
	QUIC   string // QUIC is the validator's QUIC endpoint
}

// QueryStatus queries a node's status over QUIC and fails on error.
func QueryStatus(t *testing.T, addr string) *statusResponse {
	t.Helper()

	status := QueryStatusSafe(addr)
	if status == nil {
		t.Fatalf("query status %s: failed", addr)
	}

	return status
}

// QueryStatusSafe queries a node's status over QUIC, returning nil on error.
func QueryStatusSafe(addr string) *statusResponse {
	resp, err := transportFor(addr).Status()
	if err != nil {
		return nil
	}

	return statusFromQUIC(resp)
}

// statusFromQUIC converts a QUIC status response into the test's status struct.
func statusFromQUIC(resp *network.StatusResponse) *statusResponse {
	return &statusResponse{
		Round:         resp.Round,
		LastCommitted: resp.LastCommitted,
		Validators:    int(resp.Validators),
		Epoch:         resp.Epoch,
		EpochHolders:  int(resp.EpochHolders),
		SystemPod:     hex.EncodeToString(resp.SystemPod[:]),
	}
}

// QueryHealth probes a node's liveness over QUIC and returns a status map.
func QueryHealth(t *testing.T, addr string) map[string]string {
	t.Helper()

	ok, err := transportFor(addr).Health()
	if err != nil {
		t.Fatalf("query health %s: %v", addr, err)
	}

	if ok {
		return map[string]string{"status": "ok"}
	}

	return map[string]string{"status": "down"}
}

// QueryValidators fetches the validator set over QUIC.
func QueryValidators(t *testing.T, addr string) []validatorResponse {
	t.Helper()

	resp, err := transportFor(addr).Validators()
	if err != nil {
		t.Fatalf("query validators %s: %v", addr, err)
	}

	result := make([]validatorResponse, len(resp.Validators))
	for i := range resp.Validators {
		v := &resp.Validators[i]
		result[i] = validatorResponse{
			Pubkey: hex.EncodeToString(v.Pubkey[:]),
			QUIC:   v.QUICAddr,
		}
	}

	return result
}

// QueryObject fetches an object over QUIC (routing to a holder if needed),
// returning nil if it is not found.
func QueryObject(t *testing.T, addr string, id [32]byte) *objectResponse {
	t.Helper()

	data, err := transportFor(addr).GetObject(id)
	if err != nil {
		t.Fatalf("query object %s: %v", addr, err)
	}

	if data == nil {
		return nil
	}

	return parseObjectResponse(data)
}

// QueryObjectLocal fetches an object only from the node's local state over QUIC,
// returning nil if the node does not hold it.
func QueryObjectLocal(t *testing.T, addr string, id [32]byte) *objectResponse {
	t.Helper()

	data, err := transportFor(addr).GetObjectLocal(id)
	if err != nil || data == nil {
		return nil
	}

	return parseObjectResponse(data)
}

// parseObjectResponse parses an Object FlatBuffer into an objectResponse.
func parseObjectResponse(data []byte) *objectResponse {
	info := client.ParseObject(data)

	return &objectResponse{
		ID:          hex.EncodeToString(info.ID[:]),
		Version:     info.Version,
		Owner:       hex.EncodeToString(info.Owner[:]),
		Replication: info.Replication,
		Content:     info.Content,
	}
}

// QueryDomain resolves a domain name over QUIC, returning the object ID and a
// found flag.
func QueryDomain(t *testing.T, addr string, name string) (string, bool) {
	t.Helper()

	id, found, err := transportFor(addr).DomainResolve(name)
	if err != nil {
		t.Fatalf("query domain %s: %v", addr, err)
	}

	if !found {
		return "", false
	}

	return hex.EncodeToString(id[:]), true
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

		if QueryObjectLocal(t, node.addr, objectID) != nil {
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

// WaitForOwner polls until an object's owner matches expected or timeout.
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

// WaitForObject polls until an object exists or timeout.
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
