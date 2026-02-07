package api

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	flatbuffers "github.com/google/flatbuffers/go"
	"github.com/zeebo/blake3"

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
	server := New(":0", submitter, nil, nil, nil, nil, nil, nil)

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
	server := New(":0", submitter, nil, nil, nil, nil, nil, nil)

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
	server := New(":0", submitter, nil, nil, nil, nil, nil, nil)

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
	server := New(":0", submitter, nil, nil, nil, nil, nil, nil)

	req := httptest.NewRequest("POST", "/tx", bytes.NewReader([]byte("invalid")))
	w := httptest.NewRecorder()

	server.handleSubmitTx(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected status 400, got %d", w.Code)
	}
}

func TestSubmitTx_WrongSignature(t *testing.T) {
	submitter := &mockSubmitter{}
	server := New(":0", submitter, nil, nil, nil, nil, nil, nil)

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
	server := New(":0", submitter, nil, nil, nil, nil, nil, nil)

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
	server := New(":0", submitter, nil, nil, nil, nil, nil, nil)

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
	server := New(":0", submitter, nil, nil, nil, nil, nil, nil)

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
	server := New(":0", submitter, nil, nil, nil, nil, nil, nil)

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
	server := New(":0", submitter, nil, nil, nil, nil, nil, nil)

	txData := buildTestRawTx(t, func(o *txOptions) {
		o.functionName = ""
		o.emptyFuncName = true
	})

	req := httptest.NewRequest("POST", "/tx", bytes.NewReader(txData))
	w := httptest.NewRecorder()

	server.handleSubmitTx(w, req)

	assertRejected(t, w, submitter, "empty function name")
}

func TestSubmitTx_MalformedReadObjects(t *testing.T) {
	submitter := &mockSubmitter{}
	server := New(":0", submitter, nil, nil, nil, nil, nil, nil)

	// 33 bytes is not a multiple of 40
	txData := buildTestRawTx(t, func(o *txOptions) {
		o.readObjects = make([]byte, 33)
	})

	req := httptest.NewRequest("POST", "/tx", bytes.NewReader(txData))
	w := httptest.NewRecorder()

	server.handleSubmitTx(w, req)

	assertRejected(t, w, submitter, "malformed read_objects")
}

func TestSubmitTx_MalformedMutableObjects(t *testing.T) {
	submitter := &mockSubmitter{}
	server := New(":0", submitter, nil, nil, nil, nil, nil, nil)

	// 17 bytes is not a multiple of 40
	txData := buildTestRawTx(t, func(o *txOptions) {
		o.mutableObjects = make([]byte, 17)
	})

	req := httptest.NewRequest("POST", "/tx", bytes.NewReader(txData))
	w := httptest.NewRecorder()

	server.handleSubmitTx(w, req)

	assertRejected(t, w, submitter, "malformed mutable_objects")
}

func TestSubmitTx_TooManyObjectRefs(t *testing.T) {
	submitter := &mockSubmitter{}
	server := New(":0", submitter, nil, nil, nil, nil, nil, nil)

	// 41 refs × 40 bytes = 1640 bytes
	txData := buildTestRawTx(t, func(o *txOptions) {
		o.mutableObjects = makeUniqueRefs(41)
	})

	req := httptest.NewRequest("POST", "/tx", bytes.NewReader(txData))
	w := httptest.NewRecorder()

	server.handleSubmitTx(w, req)

	assertRejected(t, w, submitter, "too many object refs")
}

func TestSubmitTx_DuplicateInMutableObjects(t *testing.T) {
	submitter := &mockSubmitter{}
	server := New(":0", submitter, nil, nil, nil, nil, nil, nil)

	// Same object ID twice in mutable
	ref := makeUniqueRefs(1)
	txData := buildTestRawTx(t, func(o *txOptions) {
		o.mutableObjects = append(ref, ref...)
	})

	req := httptest.NewRequest("POST", "/tx", bytes.NewReader(txData))
	w := httptest.NewRecorder()

	server.handleSubmitTx(w, req)

	assertRejected(t, w, submitter, "duplicate in mutable_objects")
}

func TestSubmitTx_SameObjectInReadAndMutable(t *testing.T) {
	submitter := &mockSubmitter{}
	server := New(":0", submitter, nil, nil, nil, nil, nil, nil)

	ref := makeUniqueRefs(1)
	txData := buildTestRawTx(t, func(o *txOptions) {
		o.mutableObjects = ref
		o.readObjects = ref
	})

	req := httptest.NewRequest("POST", "/tx", bytes.NewReader(txData))
	w := httptest.NewRecorder()

	server.handleSubmitTx(w, req)

	assertRejected(t, w, submitter, "same object in read and mutable")
}

func TestSubmitTx_ValidWithObjects(t *testing.T) {
	submitter := &mockSubmitter{}
	server := New(":0", submitter, nil, nil, nil, nil, nil, nil)

	txData := buildTestRawTx(t, func(o *txOptions) {
		o.mutableObjects = makeUniqueRefsFrom(3, 1)
		o.readObjects = makeUniqueRefsFrom(2, 100)
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
	server := New(":0", submitter, nil, nil, nil, nil, nil, nil)

	// Exactly 40 refs = allowed
	txData := buildTestRawTx(t, func(o *txOptions) {
		o.mutableObjects = makeUniqueRefsFrom(8, 1)
		o.readObjects = makeUniqueRefsFrom(32, 100)
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
	server := New(":0", submitter, nil, nil, nil, nil, nil, nil)

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
	senderSize       int    // override sender byte size (default 32)
	signatureSize    int    // override signature byte size (default 64)
	podSize          int    // override pod byte size (default 32)
	functionName     string // override function name (default "test")
	emptyFuncName    bool   // force empty function name
	mutableObjects   []byte // override mutable objects bytes
	readObjects      []byte // override read objects bytes
	corruptSignature bool   // flip a bit in the signature
	tamperedHash     bool   // use a wrong hash
	wrongSender      bool   // sign with one key, put different sender
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

	pod := make([]byte, opts.podSize)
	sender := pub[:opts.senderSize]
	funcName := opts.functionName

	if opts.emptyFuncName {
		funcName = ""
	}

	// Build unsigned tx bytes (same construction as client)
	unsignedBytes := buildUnsignedForTest(sender, pod, funcName, []byte{},
		false, opts.mutableObjects, opts.readObjects)
	hash := blake3.Sum256(unsignedBytes)

	if opts.tamperedHash {
		hash[0] ^= 0xFF
	}

	sig := ed25519.Sign(priv, hash[:])

	if opts.corruptSignature {
		sig[0] ^= 0xFF
	}

	if opts.wrongSender {
		// Generate a different keypair for sender field
		otherPub, _, _ := ed25519.GenerateKey(rand.Reader)
		sender = otherPub[:opts.senderSize]
	}

	sigBytes := sig[:opts.signatureSize]

	return buildTxFlatBufferForTest(sender, pod, funcName, []byte{},
		false, opts.mutableObjects, opts.readObjects, hash, sigBytes)
}

// buildUnsignedForTest mirrors the client's buildUnsignedTxBytes.
func buildUnsignedForTest(
	sender, pod []byte,
	funcName string,
	args []byte,
	createsObjects bool,
	mutableRefs, readRefs []byte,
) []byte {
	builder := flatbuffers.NewBuilder(512)

	argsVec := builder.CreateByteVector(args)
	senderVec := builder.CreateByteVector(sender)
	podVec := builder.CreateByteVector(pod)
	funcNameOff := builder.CreateString(funcName)

	var mutObjVec flatbuffers.UOffsetT
	if len(mutableRefs) > 0 {
		mutObjVec = builder.CreateByteVector(mutableRefs)
	}

	var readObjVec flatbuffers.UOffsetT
	if len(readRefs) > 0 {
		readObjVec = builder.CreateByteVector(readRefs)
	}

	types.TransactionStart(builder)
	types.TransactionAddSender(builder, senderVec)
	types.TransactionAddPod(builder, podVec)
	types.TransactionAddFunctionName(builder, funcNameOff)
	types.TransactionAddArgs(builder, argsVec)
	types.TransactionAddCreatesObjects(builder, createsObjects)

	if len(mutableRefs) > 0 {
		types.TransactionAddMutableObjects(builder, mutObjVec)
	}

	if len(readRefs) > 0 {
		types.TransactionAddReadObjects(builder, readObjVec)
	}

	txOff := types.TransactionEnd(builder)
	builder.Finish(txOff)

	return builder.FinishedBytes()
}

// buildTxFlatBufferForTest creates the final Transaction FlatBuffer with hash and signature.
func buildTxFlatBufferForTest(
	sender, pod []byte,
	funcName string,
	args []byte,
	createsObjects bool,
	mutableRefs, readRefs []byte,
	hash [32]byte,
	sig []byte,
) []byte {
	builder := flatbuffers.NewBuilder(1024)

	hashVec := builder.CreateByteVector(hash[:])
	sigVec := builder.CreateByteVector(sig)
	argsVec := builder.CreateByteVector(args)
	senderVec := builder.CreateByteVector(sender)
	podVec := builder.CreateByteVector(pod)
	funcNameOff := builder.CreateString(funcName)

	var mutObjVec flatbuffers.UOffsetT
	if len(mutableRefs) > 0 {
		mutObjVec = builder.CreateByteVector(mutableRefs)
	}

	var readObjVec flatbuffers.UOffsetT
	if len(readRefs) > 0 {
		readObjVec = builder.CreateByteVector(readRefs)
	}

	types.TransactionStart(builder)
	types.TransactionAddHash(builder, hashVec)
	types.TransactionAddSender(builder, senderVec)
	types.TransactionAddSignature(builder, sigVec)
	types.TransactionAddPod(builder, podVec)
	types.TransactionAddFunctionName(builder, funcNameOff)
	types.TransactionAddArgs(builder, argsVec)
	types.TransactionAddCreatesObjects(builder, createsObjects)

	if len(mutableRefs) > 0 {
		types.TransactionAddMutableObjects(builder, mutObjVec)
	}

	if len(readRefs) > 0 {
		types.TransactionAddReadObjects(builder, readObjVec)
	}

	txOff := types.TransactionEnd(builder)
	builder.Finish(txOff)

	return builder.FinishedBytes()
}

// makeUniqueRefs creates n unique 40-byte object references starting from a given seed.
func makeUniqueRefsFrom(n int, startSeed int) []byte {
	refs := make([]byte, n*40)

	for i := 0; i < n; i++ {
		offset := i * 40
		binary.LittleEndian.PutUint32(refs[offset:], uint32(startSeed+i))
		binary.LittleEndian.PutUint64(refs[offset+32:], 1)
	}

	return refs
}

// makeUniqueRefs creates n unique 40-byte object references starting from seed 1.
func makeUniqueRefs(n int) []byte {
	return makeUniqueRefsFrom(n, 1)
}
