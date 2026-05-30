package api

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	flatbuffers "github.com/google/flatbuffers/go"

	"BluePods/internal/consensus"
	"BluePods/internal/types"
)

// mockSubmitter captures submitted transactions.
type mockSubmitter struct {
	txs [][]byte
}

func (m *mockSubmitter) SubmitTx(tx []byte) {
	m.txs = append(m.txs, tx)
}

func TestHealthEndpoint(t *testing.T) {
	submitter := &mockSubmitter{}
	server := New(":0", submitter, nil, nil, nil, nil, nil)

	req := httptest.NewRequest("GET", "/health", nil)
	w := httptest.NewRecorder()

	server.handleHealth(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", w.Code)
	}

	var resp map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}

	if resp["status"] != "ok" {
		t.Errorf("expected status ok, got %s", resp["status"])
	}
}

// --- Mock types for API endpoint tests ---

// mockDomainResolver implements DomainResolver for testing.
type mockDomainResolver struct {
	domains map[string][32]byte
}

func (m *mockDomainResolver) ResolveDomain(name string) ([32]byte, bool) {
	id, ok := m.domains[name]
	return id, ok
}

// mockObjectQuerier implements ObjectQuerier for testing.
type mockObjectQuerier struct {
	objects map[[32]byte][]byte
}

func (m *mockObjectQuerier) GetObject(id [32]byte) []byte {
	return m.objects[id]
}

// mockStatusProvider implements StatusProvider for testing.
type mockStatusProvider struct {
	round             uint64
	lastCommitted     uint64
	validatorCount    int
	fullQuorum        bool
	epoch             uint64
	epochHoldersCount int
	validators        []*consensus.ValidatorInfo
}

func (m *mockStatusProvider) Round() uint64                              { return m.round }
func (m *mockStatusProvider) LastCommittedRound() uint64                 { return m.lastCommitted }
func (m *mockStatusProvider) ValidatorCount() int                        { return m.validatorCount }
func (m *mockStatusProvider) FullQuorumAchieved() bool                   { return m.fullQuorum }
func (m *mockStatusProvider) ValidatorsInfo() []*consensus.ValidatorInfo { return m.validators }
func (m *mockStatusProvider) Epoch() uint64                              { return m.epoch }
func (m *mockStatusProvider) EpochHoldersCount() int                     { return m.epochHoldersCount }

// --- GET /domain/{name} tests ---

func TestGetDomain_Found(t *testing.T) {
	objectID := [32]byte{0xAA, 0xBB, 0xCC}
	resolver := &mockDomainResolver{
		domains: map[string][32]byte{"example.pod": objectID},
	}

	server := New(":0", &mockSubmitter{}, nil, nil, nil, nil, resolver)

	req := httptest.NewRequest("GET", "/domain/example.pod", nil)
	w := httptest.NewRecorder()

	server.handleGetDomain(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}

	if resp["objectId"] != hex.EncodeToString(objectID[:]) {
		t.Errorf("expected objectId %s, got %s", hex.EncodeToString(objectID[:]), resp["objectId"])
	}
}

func TestGetDomain_NotFound(t *testing.T) {
	resolver := &mockDomainResolver{
		domains: map[string][32]byte{},
	}

	server := New(":0", &mockSubmitter{}, nil, nil, nil, nil, resolver)

	req := httptest.NewRequest("GET", "/domain/nonexistent.pod", nil)
	w := httptest.NewRecorder()

	server.handleGetDomain(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", w.Code)
	}
}

func TestGetDomain_EmptyName(t *testing.T) {
	resolver := &mockDomainResolver{
		domains: map[string][32]byte{},
	}

	server := New(":0", &mockSubmitter{}, nil, nil, nil, nil, resolver)

	req := httptest.NewRequest("GET", "/domain/", nil)
	w := httptest.NewRecorder()

	server.handleGetDomain(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

func TestGetDomain_NilResolver(t *testing.T) {
	server := New(":0", &mockSubmitter{}, nil, nil, nil, nil, nil)

	req := httptest.NewRequest("GET", "/domain/test.pod", nil)
	w := httptest.NewRecorder()

	server.handleGetDomain(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got %d", w.Code)
	}
}

// --- GET /object/{id} tests ---

func TestGetObject_Found(t *testing.T) {
	objectID := [32]byte{0x01, 0x02, 0x03}
	objectData := buildTestObjectForAPI(objectID, 5, []byte("hello"))

	querier := &mockObjectQuerier{
		objects: map[[32]byte][]byte{objectID: objectData},
	}

	server := New(":0", &mockSubmitter{}, nil, nil, querier, nil, nil)

	idHex := hex.EncodeToString(objectID[:])
	req := httptest.NewRequest("GET", "/object/"+idHex, nil)
	w := httptest.NewRecorder()

	server.handleGetObject(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}

	if resp["version"].(float64) != 5 {
		t.Errorf("expected version 5, got %v", resp["version"])
	}
}

func TestGetObject_NotFound(t *testing.T) {
	querier := &mockObjectQuerier{
		objects: map[[32]byte][]byte{},
	}

	server := New(":0", &mockSubmitter{}, nil, nil, querier, nil, nil)

	idHex := hex.EncodeToString(make([]byte, 32))
	req := httptest.NewRequest("GET", "/object/"+idHex, nil)
	w := httptest.NewRecorder()

	server.handleGetObject(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", w.Code)
	}
}

func TestGetObject_InvalidID(t *testing.T) {
	querier := &mockObjectQuerier{
		objects: map[[32]byte][]byte{},
	}

	server := New(":0", &mockSubmitter{}, nil, nil, querier, nil, nil)

	req := httptest.NewRequest("GET", "/object/notahex", nil)
	w := httptest.NewRecorder()

	server.handleGetObject(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

// --- GET /status tests ---

func TestStatus_Success(t *testing.T) {
	status := &mockStatusProvider{
		round:             100,
		lastCommitted:     98,
		validatorCount:    4,
		fullQuorum:        true,
		epoch:             3,
		epochHoldersCount: 4,
	}

	server := New(":0", &mockSubmitter{}, nil, status, nil, nil, nil)

	req := httptest.NewRequest("GET", "/status", nil)
	w := httptest.NewRecorder()

	server.handleStatus(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}

	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}

	if resp["round"].(float64) != 100 {
		t.Errorf("expected round 100, got %v", resp["round"])
	}

	if resp["epoch"].(float64) != 3 {
		t.Errorf("expected epoch 3, got %v", resp["epoch"])
	}

	if resp["epochHolders"].(float64) != 4 {
		t.Errorf("expected epochHolders 4, got %v", resp["epochHolders"])
	}
}

func TestStatus_NilProvider(t *testing.T) {
	server := New(":0", &mockSubmitter{}, nil, nil, nil, nil, nil)

	req := httptest.NewRequest("GET", "/status", nil)
	w := httptest.NewRecorder()

	server.handleStatus(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got %d", w.Code)
	}
}

// --- GET /validators tests ---

func TestValidators_Success(t *testing.T) {
	pubkey := [32]byte{0x01, 0x02}
	status := &mockStatusProvider{
		validators: []*consensus.ValidatorInfo{
			{Pubkey: pubkey, HTTPAddr: "http://localhost:8080"},
		},
	}

	server := New(":0", &mockSubmitter{}, nil, status, nil, nil, nil)

	req := httptest.NewRequest("GET", "/validators", nil)
	w := httptest.NewRecorder()

	server.handleValidators(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}

	var resp []map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}

	if len(resp) != 1 {
		t.Fatalf("expected 1 validator, got %d", len(resp))
	}

	if resp[0]["http"] != "http://localhost:8080" {
		t.Errorf("expected http addr, got %s", resp[0]["http"])
	}
}

// =============================================================================
// Faucet Tests (ATP 1.32-1.34)
// =============================================================================

func TestFaucet_Valid(t *testing.T) {
	submitter := &mockSubmitter{}

	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	var systemPod [32]byte
	copy(systemPod[:], []byte("test_system_pod_id_1234567890ab"))

	faucetCfg := &FaucetConfig{
		PrivKey:   priv,
		SystemPod: systemPod,
	}

	server := New(":0", submitter, nil, nil, nil, faucetCfg, nil)

	body := `{"pubkey":"` + hex.EncodeToString(make([]byte, 32)) + `","amount":1000}`
	req := httptest.NewRequest("POST", "/faucet", strings.NewReader(body))
	w := httptest.NewRecorder()

	server.handleFaucet(w, req)

	if w.Code != http.StatusAccepted {
		t.Errorf("expected 202, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}

	if resp["hash"] == "" {
		t.Error("expected hash in response")
	}

	if resp["coinID"] == "" {
		t.Error("expected coinID in response")
	}

	if len(submitter.txs) != 1 {
		t.Errorf("expected 1 submitted tx, got %d", len(submitter.txs))
	}
}

func TestFaucet_ZeroAmount(t *testing.T) {
	submitter := &mockSubmitter{}

	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	faucetCfg := &FaucetConfig{PrivKey: priv}

	server := New(":0", submitter, nil, nil, nil, faucetCfg, nil)

	body := `{"pubkey":"` + hex.EncodeToString(make([]byte, 32)) + `","amount":0}`
	req := httptest.NewRequest("POST", "/faucet", strings.NewReader(body))
	w := httptest.NewRecorder()

	server.handleFaucet(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for zero amount, got %d: %s", w.Code, w.Body.String())
	}

	if len(submitter.txs) != 0 {
		t.Error("should not submit on zero amount")
	}
}

func TestFaucet_InvalidPubkey(t *testing.T) {
	submitter := &mockSubmitter{}

	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	faucetCfg := &FaucetConfig{PrivKey: priv}

	server := New(":0", submitter, nil, nil, nil, faucetCfg, nil)

	body := `{"pubkey":"deadbeef","amount":1000}`
	req := httptest.NewRequest("POST", "/faucet", strings.NewReader(body))
	w := httptest.NewRecorder()

	server.handleFaucet(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for invalid pubkey, got %d: %s", w.Code, w.Body.String())
	}

	if len(submitter.txs) != 0 {
		t.Error("should not submit on invalid pubkey")
	}
}

// buildTestObjectForAPI creates a serialized Object for API test mocks.
func buildTestObjectForAPI(id [32]byte, version uint64, content []byte) []byte {
	builder := flatbuffers.NewBuilder(256)

	idVec := builder.CreateByteVector(id[:])
	ownerVec := builder.CreateByteVector(make([]byte, 32))
	contentVec := builder.CreateByteVector(content)

	types.ObjectStart(builder)
	types.ObjectAddId(builder, idVec)
	types.ObjectAddVersion(builder, version)
	types.ObjectAddOwner(builder, ownerVec)
	types.ObjectAddReplication(builder, 10)
	types.ObjectAddContent(builder, contentVec)
	offset := types.ObjectEnd(builder)

	builder.Finish(offset)

	return builder.FinishedBytes()
}
