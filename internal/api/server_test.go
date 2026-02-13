package api

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	flatbuffers "github.com/google/flatbuffers/go"
	"github.com/zeebo/blake3"

	"BluePods/internal/consensus"
	"BluePods/internal/genesis"
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
	server := New(":0", submitter, nil, nil, nil, nil, nil, nil, nil)

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

func TestSubmitTx_Success(t *testing.T) {
	submitter := &mockSubmitter{}
	server := New(":0", submitter, nil, nil, nil, nil, nil, nil, nil)

	txData := buildTestRawTx(t, nil)

	req := httptest.NewRequest("POST", "/tx", bytes.NewReader(txData))
	w := httptest.NewRecorder()

	server.handleSubmitTx(w, req)

	if w.Code != http.StatusAccepted {
		t.Errorf("expected status 202, got %d: %s", w.Code, w.Body.String())
	}

	if len(submitter.txs) != 1 {
		t.Errorf("expected 1 tx submitted, got %d", len(submitter.txs))
	}

	var resp map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}

	if resp["hash"] == "" {
		t.Error("expected hash in response")
	}
}

func TestSubmitTx_EmptyBody(t *testing.T) {
	submitter := &mockSubmitter{}
	server := New(":0", submitter, nil, nil, nil, nil, nil, nil, nil)

	req := httptest.NewRequest("POST", "/tx", nil)
	w := httptest.NewRecorder()

	server.handleSubmitTx(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected status 400, got %d", w.Code)
	}

	if len(submitter.txs) != 0 {
		t.Error("should not submit on error")
	}
}

func TestSubmitTx_InvalidData(t *testing.T) {
	submitter := &mockSubmitter{}
	server := New(":0", submitter, nil, nil, nil, nil, nil, nil, nil)

	req := httptest.NewRequest("POST", "/tx", bytes.NewReader([]byte("invalid")))
	w := httptest.NewRecorder()

	server.handleSubmitTx(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected status 400, got %d", w.Code)
	}
}

func TestSubmitTx_WrongSignature(t *testing.T) {
	submitter := &mockSubmitter{}
	server := New(":0", submitter, nil, nil, nil, nil, nil, nil, nil)

	// Build a valid tx, then corrupt the signature
	txData := buildTestRawTx(t, func(o *txOptions) {
		o.corruptSignature = true
	})

	req := httptest.NewRequest("POST", "/tx", bytes.NewReader(txData))
	w := httptest.NewRecorder()

	server.handleSubmitTx(w, req)

	assertRejected(t, w, submitter, "wrong signature")
}

func TestSubmitTx_TamperedHash(t *testing.T) {
	submitter := &mockSubmitter{}
	server := New(":0", submitter, nil, nil, nil, nil, nil, nil, nil)

	txData := buildTestRawTx(t, func(o *txOptions) {
		o.tamperedHash = true
	})

	req := httptest.NewRequest("POST", "/tx", bytes.NewReader(txData))
	w := httptest.NewRecorder()

	server.handleSubmitTx(w, req)

	assertRejected(t, w, submitter, "tampered hash")
}

func TestSubmitTx_MissingSender(t *testing.T) {
	submitter := &mockSubmitter{}
	server := New(":0", submitter, nil, nil, nil, nil, nil, nil, nil)

	txData := buildTestRawTx(t, func(o *txOptions) {
		o.senderSize = 0
	})

	req := httptest.NewRequest("POST", "/tx", bytes.NewReader(txData))
	w := httptest.NewRecorder()

	server.handleSubmitTx(w, req)

	assertRejected(t, w, submitter, "missing sender")
}

func TestSubmitTx_ShortSignature(t *testing.T) {
	submitter := &mockSubmitter{}
	server := New(":0", submitter, nil, nil, nil, nil, nil, nil, nil)

	txData := buildTestRawTx(t, func(o *txOptions) {
		o.signatureSize = 32
	})

	req := httptest.NewRequest("POST", "/tx", bytes.NewReader(txData))
	w := httptest.NewRecorder()

	server.handleSubmitTx(w, req)

	assertRejected(t, w, submitter, "short signature")
}

func TestSubmitTx_MissingPod(t *testing.T) {
	submitter := &mockSubmitter{}
	server := New(":0", submitter, nil, nil, nil, nil, nil, nil, nil)

	txData := buildTestRawTx(t, func(o *txOptions) {
		o.podSize = 0
	})

	req := httptest.NewRequest("POST", "/tx", bytes.NewReader(txData))
	w := httptest.NewRecorder()

	server.handleSubmitTx(w, req)

	assertRejected(t, w, submitter, "missing pod")
}

func TestSubmitTx_EmptyFunctionName(t *testing.T) {
	submitter := &mockSubmitter{}
	server := New(":0", submitter, nil, nil, nil, nil, nil, nil, nil)

	txData := buildTestRawTx(t, func(o *txOptions) {
		o.functionName = ""
		o.emptyFuncName = true
	})

	req := httptest.NewRequest("POST", "/tx", bytes.NewReader(txData))
	w := httptest.NewRecorder()

	server.handleSubmitTx(w, req)

	assertRejected(t, w, submitter, "empty function name")
}

func TestSubmitTx_BadRefIDSize_Read(t *testing.T) {
	submitter := &mockSubmitter{}
	server := New(":0", submitter, nil, nil, nil, nil, nil, nil, nil)

	// ObjectRef with a 16-byte ID (should be 32) triggers validation error
	txData := buildTxWithBadRefID(t, false)

	req := httptest.NewRequest("POST", "/tx", bytes.NewReader(txData))
	w := httptest.NewRecorder()

	server.handleSubmitTx(w, req)

	assertRejected(t, w, submitter, "bad ref ID size in read_refs")
}

func TestSubmitTx_BadRefIDSize_Mutable(t *testing.T) {
	submitter := &mockSubmitter{}
	server := New(":0", submitter, nil, nil, nil, nil, nil, nil, nil)

	// ObjectRef with a 16-byte ID (should be 32) triggers validation error
	txData := buildTxWithBadRefID(t, true)

	req := httptest.NewRequest("POST", "/tx", bytes.NewReader(txData))
	w := httptest.NewRecorder()

	server.handleSubmitTx(w, req)

	assertRejected(t, w, submitter, "bad ref ID size in mutable_refs")
}

func TestSubmitTx_TooManyObjectRefs(t *testing.T) {
	submitter := &mockSubmitter{}
	server := New(":0", submitter, nil, nil, nil, nil, nil, nil, nil)

	// 41 refs exceeds the 40 max
	txData := buildTestRawTx(t, func(o *txOptions) {
		o.mutableRefs = makeUniqueRefs(41)
	})

	req := httptest.NewRequest("POST", "/tx", bytes.NewReader(txData))
	w := httptest.NewRecorder()

	server.handleSubmitTx(w, req)

	assertRejected(t, w, submitter, "too many object refs")
}

func TestSubmitTx_DuplicateInMutableObjects(t *testing.T) {
	submitter := &mockSubmitter{}
	server := New(":0", submitter, nil, nil, nil, nil, nil, nil, nil)

	// Same object ID twice in mutable
	ref := makeUniqueRefs(1)
	txData := buildTestRawTx(t, func(o *txOptions) {
		o.mutableRefs = append(ref, ref...)
	})

	req := httptest.NewRequest("POST", "/tx", bytes.NewReader(txData))
	w := httptest.NewRecorder()

	server.handleSubmitTx(w, req)

	assertRejected(t, w, submitter, "duplicate in mutable_objects")
}

func TestSubmitTx_SameObjectInReadAndMutable(t *testing.T) {
	submitter := &mockSubmitter{}
	server := New(":0", submitter, nil, nil, nil, nil, nil, nil, nil)

	ref := makeUniqueRefs(1)
	txData := buildTestRawTx(t, func(o *txOptions) {
		o.mutableRefs = ref
		o.readRefs = ref
	})

	req := httptest.NewRequest("POST", "/tx", bytes.NewReader(txData))
	w := httptest.NewRecorder()

	server.handleSubmitTx(w, req)

	assertRejected(t, w, submitter, "same object in read and mutable")
}

func TestSubmitTx_ValidWithObjects(t *testing.T) {
	submitter := &mockSubmitter{}
	server := New(":0", submitter, nil, nil, nil, nil, nil, nil, nil)

	txData := buildTestRawTx(t, func(o *txOptions) {
		o.mutableRefs = makeUniqueRefsFrom(3, 1)
		o.readRefs = makeUniqueRefsFrom(2, 100)
	})

	req := httptest.NewRequest("POST", "/tx", bytes.NewReader(txData))
	w := httptest.NewRecorder()

	server.handleSubmitTx(w, req)

	if w.Code != http.StatusAccepted {
		t.Errorf("expected 202, got %d: %s", w.Code, w.Body.String())
	}
}

func TestSubmitTx_MaxObjectRefs(t *testing.T) {
	submitter := &mockSubmitter{}
	server := New(":0", submitter, nil, nil, nil, nil, nil, nil, nil)

	// Exactly 40 refs = allowed
	txData := buildTestRawTx(t, func(o *txOptions) {
		o.mutableRefs = makeUniqueRefsFrom(8, 1)
		o.readRefs = makeUniqueRefsFrom(32, 100)
	})

	req := httptest.NewRequest("POST", "/tx", bytes.NewReader(txData))
	w := httptest.NewRecorder()

	server.handleSubmitTx(w, req)

	if w.Code != http.StatusAccepted {
		t.Errorf("expected 202 for exactly 40 refs, got %d: %s", w.Code, w.Body.String())
	}
}

func TestSubmitTx_SignedByDifferentKey(t *testing.T) {
	submitter := &mockSubmitter{}
	server := New(":0", submitter, nil, nil, nil, nil, nil, nil, nil)

	// Sign with one key but put a different sender pubkey
	txData := buildTestRawTx(t, func(o *txOptions) {
		o.wrongSender = true
	})

	req := httptest.NewRequest("POST", "/tx", bytes.NewReader(txData))
	w := httptest.NewRecorder()

	server.handleSubmitTx(w, req)

	assertRejected(t, w, submitter, "wrong sender pubkey")
}

// --- Test helpers ---

// assertRejected checks the tx was rejected (400) and not submitted.
func assertRejected(t *testing.T, w *httptest.ResponseRecorder, sub *mockSubmitter, label string) {
	t.Helper()

	if w.Code != http.StatusBadRequest {
		t.Errorf("[%s] expected 400, got %d: %s", label, w.Code, w.Body.String())
	}

	if len(sub.txs) != 0 {
		t.Errorf("[%s] should not submit on validation failure", label)
	}
}

// txOptions controls how buildTestRawTx constructs the transaction.
type txOptions struct {
	senderSize       int                    // override sender byte size (default 32)
	signatureSize    int                    // override signature byte size (default 64)
	podSize          int                    // override pod byte size (default 32)
	functionName     string                 // override function name (default "test")
	emptyFuncName    bool                   // force empty function name
	mutableRefs      []genesis.ObjectRefData // override mutable object refs
	readRefs         []genesis.ObjectRefData // override read object refs
	gasCoin          []byte                 // optional gas_coin bytes (nil = absent)
	corruptSignature bool                   // flip a bit in the signature
	tamperedHash     bool                   // use a wrong hash
	wrongSender      bool                   // sign with one key, put different sender
}

// buildTestRawTx creates a properly signed raw Transaction.
// The optional modifier function can alter the txOptions before building.
func buildTestRawTx(t *testing.T, modify func(*txOptions)) []byte {
	t.Helper()

	opts := &txOptions{
		senderSize:    32,
		signatureSize: 64,
		podSize:       32,
		functionName:  "test",
	}

	if modify != nil {
		modify(opts)
	}

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	sender := pub[:opts.senderSize]
	podBytes := make([]byte, opts.podSize)
	funcName := opts.functionName

	if opts.emptyFuncName {
		funcName = ""
	}

	// Build unsigned tx for hashing
	unsignedBytes := buildUnsignedForTest(sender, podBytes, funcName, opts.mutableRefs, opts.readRefs, opts.gasCoin)
	hash := blake3.Sum256(unsignedBytes)

	if opts.tamperedHash {
		hash[0] ^= 0xFF
	}

	sig := ed25519.Sign(priv, hash[:])

	if opts.corruptSignature {
		sig[0] ^= 0xFF
	}

	if opts.wrongSender {
		otherPub, _, _ := ed25519.GenerateKey(rand.Reader)
		sender = otherPub[:opts.senderSize]
	}

	sigBytes := sig[:opts.signatureSize]

	return buildSignedForTest(sender, podBytes, funcName, hash, sigBytes, opts.mutableRefs, opts.readRefs, opts.gasCoin)
}

// buildUnsignedForTest creates unsigned Transaction bytes for hashing.
// Uses raw byte slices for sender/pod to allow testing with wrong sizes.
func buildUnsignedForTest(
	sender, pod []byte,
	funcName string,
	mutableRefs, readRefs []genesis.ObjectRefData,
	gasCoin []byte,
) []byte {
	builder := flatbuffers.NewBuilder(512)

	argsVec := builder.CreateByteVector([]byte{})
	senderVec := builder.CreateByteVector(sender)
	podVec := builder.CreateByteVector(pod)
	funcNameOff := builder.CreateString(funcName)

	var gasCoinVec flatbuffers.UOffsetT
	if len(gasCoin) > 0 {
		gasCoinVec = builder.CreateByteVector(gasCoin)
	}

	mutRefsVec := buildTestRefVector(builder, mutableRefs, true)
	readRefsVec := buildTestRefVector(builder, readRefs, false)

	types.TransactionStart(builder)
	types.TransactionAddSender(builder, senderVec)
	types.TransactionAddPod(builder, podVec)
	types.TransactionAddFunctionName(builder, funcNameOff)
	types.TransactionAddArgs(builder, argsVec)

	if gasCoinVec != 0 {
		types.TransactionAddGasCoin(builder, gasCoinVec)
	}

	if mutRefsVec != 0 {
		types.TransactionAddMutableRefs(builder, mutRefsVec)
	}

	if readRefsVec != 0 {
		types.TransactionAddReadRefs(builder, readRefsVec)
	}

	txOff := types.TransactionEnd(builder)
	builder.Finish(txOff)

	return builder.FinishedBytes()
}

// buildSignedForTest creates the final signed Transaction FlatBuffer.
func buildSignedForTest(
	sender, pod []byte,
	funcName string,
	hash [32]byte,
	sig []byte,
	mutableRefs, readRefs []genesis.ObjectRefData,
	gasCoin []byte,
) []byte {
	builder := flatbuffers.NewBuilder(1024)

	hashVec := builder.CreateByteVector(hash[:])
	sigVec := builder.CreateByteVector(sig)
	argsVec := builder.CreateByteVector([]byte{})
	senderVec := builder.CreateByteVector(sender)
	podVec := builder.CreateByteVector(pod)
	funcNameOff := builder.CreateString(funcName)

	var gasCoinVec flatbuffers.UOffsetT
	if len(gasCoin) > 0 {
		gasCoinVec = builder.CreateByteVector(gasCoin)
	}

	mutRefsVec := buildTestRefVector(builder, mutableRefs, true)
	readRefsVec := buildTestRefVector(builder, readRefs, false)

	types.TransactionStart(builder)
	types.TransactionAddHash(builder, hashVec)
	types.TransactionAddSender(builder, senderVec)
	types.TransactionAddSignature(builder, sigVec)
	types.TransactionAddPod(builder, podVec)
	types.TransactionAddFunctionName(builder, funcNameOff)
	types.TransactionAddArgs(builder, argsVec)

	if gasCoinVec != 0 {
		types.TransactionAddGasCoin(builder, gasCoinVec)
	}

	if mutRefsVec != 0 {
		types.TransactionAddMutableRefs(builder, mutRefsVec)
	}

	if readRefsVec != 0 {
		types.TransactionAddReadRefs(builder, readRefsVec)
	}

	txOff := types.TransactionEnd(builder)
	builder.Finish(txOff)

	return builder.FinishedBytes()
}

// buildTestRefVector builds an ObjectRef vector from ObjectRefData slices for tests.
func buildTestRefVector(builder *flatbuffers.Builder, refs []genesis.ObjectRefData, mutable bool) flatbuffers.UOffsetT {
	if len(refs) == 0 {
		return 0
	}

	offsets := make([]flatbuffers.UOffsetT, len(refs))

	for i, ref := range refs {
		idVec := builder.CreateByteVector(ref.ID[:])

		types.ObjectRefStart(builder)
		types.ObjectRefAddId(builder, idVec)
		types.ObjectRefAddVersion(builder, ref.Version)
		offsets[i] = types.ObjectRefEnd(builder)
	}

	if mutable {
		types.TransactionStartMutableRefsVector(builder, len(offsets))
	} else {
		types.TransactionStartReadRefsVector(builder, len(offsets))
	}

	for i := len(offsets) - 1; i >= 0; i-- {
		builder.PrependUOffsetT(offsets[i])
	}

	return builder.EndVector(len(offsets))
}

// buildTxWithBadRefID creates a Transaction with an ObjectRef containing a 16-byte ID.
// If mutable is true, the bad ref is in mutable_refs; otherwise in read_refs.
func buildTxWithBadRefID(t *testing.T, mutable bool) []byte {
	t.Helper()

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	var pod [32]byte
	builder := flatbuffers.NewBuilder(1024)

	// Build the unsigned tx first for hashing (with no refs, since the bad ref
	// won't match anyway -- we just need a valid hash/sig pair for the outer fields)
	unsignedBytes := genesis.BuildUnsignedTxBytesWithRefs(
		pub, pod, "test", []byte{}, nil, 0, 0, nil, nil, nil,
	)
	hash := blake3.Sum256(unsignedBytes)
	sig := ed25519.Sign(priv, hash[:])

	// Create all string/vector offsets before starting any tables
	hashVec := builder.CreateByteVector(hash[:])
	sigVec := builder.CreateByteVector(sig)
	argsVec := builder.CreateByteVector([]byte{})
	senderVec := builder.CreateByteVector(pub)
	podVec := builder.CreateByteVector(pod[:])
	funcNameOff := builder.CreateString("test")

	// Build a bad ObjectRef with 16-byte ID (should be 32)
	badID := builder.CreateByteVector(make([]byte, 16))
	types.ObjectRefStart(builder)
	types.ObjectRefAddId(builder, badID)
	types.ObjectRefAddVersion(builder, 1)
	badRefOff := types.ObjectRefEnd(builder)

	// Build the ref vector
	var mutRefsVec, readRefsVec flatbuffers.UOffsetT

	if mutable {
		types.TransactionStartMutableRefsVector(builder, 1)
		builder.PrependUOffsetT(badRefOff)
		mutRefsVec = builder.EndVector(1)
	} else {
		types.TransactionStartReadRefsVector(builder, 1)
		builder.PrependUOffsetT(badRefOff)
		readRefsVec = builder.EndVector(1)
	}

	types.TransactionStart(builder)
	types.TransactionAddHash(builder, hashVec)
	types.TransactionAddSender(builder, senderVec)
	types.TransactionAddSignature(builder, sigVec)
	types.TransactionAddPod(builder, podVec)
	types.TransactionAddFunctionName(builder, funcNameOff)
	types.TransactionAddArgs(builder, argsVec)

	if mutable {
		types.TransactionAddMutableRefs(builder, mutRefsVec)
	} else {
		types.TransactionAddReadRefs(builder, readRefsVec)
	}

	txOff := types.TransactionEnd(builder)
	builder.Finish(txOff)

	return builder.FinishedBytes()
}

// makeUniqueRefsFrom creates n unique ObjectRefData entries starting from a given seed.
func makeUniqueRefsFrom(n int, startSeed int) []genesis.ObjectRefData {
	refs := make([]genesis.ObjectRefData, n)

	for i := 0; i < n; i++ {
		binary.LittleEndian.PutUint32(refs[i].ID[:], uint32(startSeed+i))
		refs[i].Version = 1
	}

	return refs
}

// makeUniqueRefs creates n unique ObjectRefData entries starting from seed 1.
func makeUniqueRefs(n int) []genesis.ObjectRefData {
	return makeUniqueRefsFrom(n, 1)
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
	round              uint64
	lastCommitted      uint64
	validatorCount     int
	fullQuorum         bool
	epoch              uint64
	epochHoldersCount  int
	validators         []*consensus.ValidatorInfo
}

func (m *mockStatusProvider) Round() uint64                            { return m.round }
func (m *mockStatusProvider) LastCommittedRound() uint64               { return m.lastCommitted }
func (m *mockStatusProvider) ValidatorCount() int                      { return m.validatorCount }
func (m *mockStatusProvider) FullQuorumAchieved() bool                 { return m.fullQuorum }
func (m *mockStatusProvider) ValidatorsInfo() []*consensus.ValidatorInfo { return m.validators }
func (m *mockStatusProvider) Epoch() uint64                            { return m.epoch }
func (m *mockStatusProvider) EpochHoldersCount() int                   { return m.epochHoldersCount }

// mockHolderRouter implements HolderRouter for testing.
type mockHolderRouter struct {
	objects map[[32]byte][]byte
}

func (m *mockHolderRouter) RouteGetObject(id [32]byte) ([]byte, error) {
	data, ok := m.objects[id]
	if !ok {
		return nil, nil
	}
	return data, nil
}

// --- GET /domain/{name} tests ---

func TestGetDomain_Found(t *testing.T) {
	objectID := [32]byte{0xAA, 0xBB, 0xCC}
	resolver := &mockDomainResolver{
		domains: map[string][32]byte{"example.pod": objectID},
	}

	server := New(":0", &mockSubmitter{}, nil, nil, nil, nil, nil, nil, resolver)

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

	server := New(":0", &mockSubmitter{}, nil, nil, nil, nil, nil, nil, resolver)

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

	server := New(":0", &mockSubmitter{}, nil, nil, nil, nil, nil, nil, resolver)

	req := httptest.NewRequest("GET", "/domain/", nil)
	w := httptest.NewRecorder()

	server.handleGetDomain(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

func TestGetDomain_NilResolver(t *testing.T) {
	server := New(":0", &mockSubmitter{}, nil, nil, nil, nil, nil, nil, nil)

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

	server := New(":0", &mockSubmitter{}, nil, nil, querier, nil, nil, nil, nil)

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

	server := New(":0", &mockSubmitter{}, nil, nil, querier, nil, nil, nil, nil)

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

	server := New(":0", &mockSubmitter{}, nil, nil, querier, nil, nil, nil, nil)

	req := httptest.NewRequest("GET", "/object/notahex", nil)
	w := httptest.NewRecorder()

	server.handleGetObject(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

func TestGetObject_RoutesToHolder(t *testing.T) {
	objectID := [32]byte{0x10, 0x20}
	objectData := buildTestObjectForAPI(objectID, 3, []byte("remote"))

	// Not in local store
	querier := &mockObjectQuerier{
		objects: map[[32]byte][]byte{},
	}

	// Available via remote holder
	router := &mockHolderRouter{
		objects: map[[32]byte][]byte{objectID: objectData},
	}

	server := New(":0", &mockSubmitter{}, nil, nil, querier, nil, nil, router, nil)

	idHex := hex.EncodeToString(objectID[:])
	req := httptest.NewRequest("GET", "/object/"+idHex, nil)
	w := httptest.NewRecorder()

	server.handleGetObject(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200 from routing, got %d: %s", w.Code, w.Body.String())
	}
}

func TestGetObject_LocalOnlySkipsRouter(t *testing.T) {
	objectID := [32]byte{0x30, 0x40}
	objectData := buildTestObjectForAPI(objectID, 1, []byte("remote"))

	querier := &mockObjectQuerier{
		objects: map[[32]byte][]byte{},
	}
	router := &mockHolderRouter{
		objects: map[[32]byte][]byte{objectID: objectData},
	}

	server := New(":0", &mockSubmitter{}, nil, nil, querier, nil, nil, router, nil)

	idHex := hex.EncodeToString(objectID[:])
	req := httptest.NewRequest("GET", "/object/"+idHex+"?local=true", nil)
	w := httptest.NewRecorder()

	server.handleGetObject(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404 with local=true, got %d", w.Code)
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

	server := New(":0", &mockSubmitter{}, nil, status, nil, nil, nil, nil, nil)

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
	server := New(":0", &mockSubmitter{}, nil, nil, nil, nil, nil, nil, nil)

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

	server := New(":0", &mockSubmitter{}, nil, status, nil, nil, nil, nil, nil)

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
// Gas Coin Validation Tests (ATP 1.6)
// =============================================================================

func TestSubmitTx_GasCoinInvalidSize(t *testing.T) {
	submitter := &mockSubmitter{}
	server := New(":0", submitter, nil, nil, nil, nil, nil, nil, nil)

	// gas_coin = 16 bytes (should be 0 or 32)
	txData := buildTestRawTx(t, func(o *txOptions) {
		o.gasCoin = make([]byte, 16)
	})

	req := httptest.NewRequest("POST", "/tx", bytes.NewReader(txData))
	w := httptest.NewRecorder()

	server.handleSubmitTx(w, req)

	assertRejected(t, w, submitter, "invalid gas_coin size")
}

// =============================================================================
// Duplicate Read Refs Test (ATP 1.12)
// =============================================================================

func TestSubmitTx_DuplicateReadRefs(t *testing.T) {
	submitter := &mockSubmitter{}
	server := New(":0", submitter, nil, nil, nil, nil, nil, nil, nil)

	// Same object ID twice in read_refs
	refs := makeUniqueRefs(1)
	txData := buildTestRawTx(t, func(o *txOptions) {
		o.readRefs = append(refs, refs[0])
	})

	req := httptest.NewRequest("POST", "/tx", bytes.NewReader(txData))
	w := httptest.NewRecorder()

	server.handleSubmitTx(w, req)

	assertRejected(t, w, submitter, "duplicate in read_refs")
}

// =============================================================================
// Domain Ref With No ID Test (ATP 1.17)
// =============================================================================

func TestSubmitTx_DomainRefNoID(t *testing.T) {
	submitter := &mockSubmitter{}
	server := New(":0", submitter, nil, nil, nil, nil, nil, nil, nil)

	// Domain-only ref (no object ID) should be accepted
	txData := buildTxWithDomainRefs(t, []string{"example.pod"}, nil)

	req := httptest.NewRequest("POST", "/tx", bytes.NewReader(txData))
	w := httptest.NewRecorder()

	server.handleSubmitTx(w, req)

	if w.Code != http.StatusAccepted {
		t.Errorf("expected 202 for domain-only ref, got %d: %s", w.Code, w.Body.String())
	}
}

// =============================================================================
// Body Too Large Test (ATP 1.20)
// =============================================================================

func TestSubmitTx_BodyTooLarge(t *testing.T) {
	submitter := &mockSubmitter{}
	server := New(":0", submitter, nil, nil, nil, nil, nil, nil, nil)

	// Build a body larger than 1MB. The server reads at most maxTxSize (1MB),
	// so a 2MB body is truncated and results in malformed FlatBuffer.
	largeBody := make([]byte, 2*1024*1024)

	req := httptest.NewRequest("POST", "/tx", bytes.NewReader(largeBody))
	w := httptest.NewRecorder()

	server.handleSubmitTx(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for oversized body, got %d", w.Code)
	}

	if len(submitter.txs) != 0 {
		t.Error("should not submit oversized body")
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

	server := New(":0", submitter, nil, nil, nil, faucetCfg, nil, nil, nil)

	body := `{"pubkey":"` + hex.EncodeToString(make([]byte, 32)) + `","amount":1000}`
	req := httptest.NewRequest("POST", "/faucet", bytes.NewReader([]byte(body)))
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

	server := New(":0", submitter, nil, nil, nil, faucetCfg, nil, nil, nil)

	body := `{"pubkey":"` + hex.EncodeToString(make([]byte, 32)) + `","amount":0}`
	req := httptest.NewRequest("POST", "/faucet", bytes.NewReader([]byte(body)))
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

	server := New(":0", submitter, nil, nil, nil, faucetCfg, nil, nil, nil)

	body := `{"pubkey":"deadbeef","amount":1000}`
	req := httptest.NewRequest("POST", "/faucet", bytes.NewReader([]byte(body)))
	w := httptest.NewRecorder()

	server.handleFaucet(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for invalid pubkey, got %d: %s", w.Code, w.Body.String())
	}

	if len(submitter.txs) != 0 {
		t.Error("should not submit on invalid pubkey")
	}
}

// --- Domain ref duplicate validation tests ---

func TestSubmitTx_DuplicateDomainRef(t *testing.T) {
	submitter := &mockSubmitter{}
	server := New(":0", submitter, nil, nil, nil, nil, nil, nil, nil)

	// Two mutable refs with the same domain name
	txData := buildTxWithDomainRefs(t, []string{"dup.pod", "dup.pod"}, nil)

	req := httptest.NewRequest("POST", "/tx", bytes.NewReader(txData))
	w := httptest.NewRecorder()

	server.handleSubmitTx(w, req)

	assertRejected(t, w, submitter, "duplicate domain ref")
}

func TestSubmitTx_DuplicateDomainAcrossReadAndMutable(t *testing.T) {
	submitter := &mockSubmitter{}
	server := New(":0", submitter, nil, nil, nil, nil, nil, nil, nil)

	// Same domain in mutable and read refs
	txData := buildTxWithDomainRefs(t, []string{"shared.pod"}, []string{"shared.pod"})

	req := httptest.NewRequest("POST", "/tx", bytes.NewReader(txData))
	w := httptest.NewRecorder()

	server.handleSubmitTx(w, req)

	assertRejected(t, w, submitter, "duplicate domain across read and mutable")
}

func TestSubmitTx_UniqueDomainRefs(t *testing.T) {
	submitter := &mockSubmitter{}
	server := New(":0", submitter, nil, nil, nil, nil, nil, nil, nil)

	// Different domain names in mutable refs
	txData := buildTxWithDomainRefs(t, []string{"alpha.pod", "beta.pod"}, nil)

	req := httptest.NewRequest("POST", "/tx", bytes.NewReader(txData))
	w := httptest.NewRecorder()

	server.handleSubmitTx(w, req)

	if w.Code != http.StatusAccepted {
		t.Errorf("expected 202 for unique domain refs, got %d: %s", w.Code, w.Body.String())
	}
}

// buildTxWithDomainRefs creates a signed Transaction with domain-based ObjectRefs.
func buildTxWithDomainRefs(t *testing.T, mutableDomains, readDomains []string) []byte {
	t.Helper()

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	var pod [32]byte

	// Build mutable and read ref data
	var mutableRefs, readRefs []genesis.ObjectRefData
	for _, d := range mutableDomains {
		mutableRefs = append(mutableRefs, genesis.ObjectRefData{Domain: d})
	}
	for _, d := range readDomains {
		readRefs = append(readRefs, genesis.ObjectRefData{Domain: d})
	}

	unsignedBytes := genesis.BuildUnsignedTxBytesWithRefs(pub, pod, "test", nil, nil, 0, 0, nil, mutableRefs, readRefs)
	hash := blake3.Sum256(unsignedBytes)
	sig := ed25519.Sign(priv, hash[:])

	builder := flatbuffers.NewBuilder(1024)
	txOffset := genesis.BuildTxTableWithRefs(builder, pub, pod, "test", nil, nil, 0, 0, nil, hash, sig, mutableRefs, readRefs)
	builder.Finish(txOffset)

	return builder.FinishedBytes()
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
