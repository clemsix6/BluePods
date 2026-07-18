package validation

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/binary"
	"testing"

	flatbuffers "github.com/google/flatbuffers/go"
	"github.com/zeebo/blake3"

	"BluePods/internal/genesis"
	"BluePods/internal/types"
)

func TestValidateTx_Valid(t *testing.T) {
	txData := buildTestRawTx(t, nil)

	if err := ValidateTx(txData); err != nil {
		t.Fatalf("expected valid tx, got error: %v", err)
	}
}

func TestValidateTx_InvalidData(t *testing.T) {
	if err := ValidateTx([]byte("invalid")); err == nil {
		t.Fatal("expected error for malformed data")
	}
}

func TestValidateTx_EmptyData(t *testing.T) {
	if err := ValidateTx(nil); err == nil {
		t.Fatal("expected error for empty data")
	}
}

func TestValidateTx_WrongSignature(t *testing.T) {
	txData := buildTestRawTx(t, func(o *txOptions) {
		o.corruptSignature = true
	})

	if err := ValidateTx(txData); err == nil {
		t.Fatal("expected error for corrupt signature")
	}
}

func TestValidateTx_TamperedHash(t *testing.T) {
	txData := buildTestRawTx(t, func(o *txOptions) {
		o.tamperedHash = true
	})

	if err := ValidateTx(txData); err == nil {
		t.Fatal("expected error for tampered hash")
	}
}

func TestValidateTx_MissingSender(t *testing.T) {
	txData := buildTestRawTx(t, func(o *txOptions) {
		o.senderSize = 0
	})

	if err := ValidateTx(txData); err == nil {
		t.Fatal("expected error for missing sender")
	}
}

func TestValidateTx_ShortSignature(t *testing.T) {
	txData := buildTestRawTx(t, func(o *txOptions) {
		o.signatureSize = 32
	})

	if err := ValidateTx(txData); err == nil {
		t.Fatal("expected error for short signature")
	}
}

func TestValidateTx_MissingPod(t *testing.T) {
	txData := buildTestRawTx(t, func(o *txOptions) {
		o.podSize = 0
	})

	if err := ValidateTx(txData); err == nil {
		t.Fatal("expected error for missing pod")
	}
}

func TestValidateTx_EmptyFunctionName(t *testing.T) {
	txData := buildTestRawTx(t, func(o *txOptions) {
		o.functionName = ""
		o.emptyFuncName = true
	})

	if err := ValidateTx(txData); err == nil {
		t.Fatal("expected error for empty function name")
	}
}

func TestValidateTx_BadRefIDSize_Read(t *testing.T) {
	txData := buildTxWithBadRefID(t, false)

	if err := ValidateTx(txData); err == nil {
		t.Fatal("expected error for bad ref ID size in read_refs")
	}
}

func TestValidateTx_BadRefIDSize_Mutable(t *testing.T) {
	txData := buildTxWithBadRefID(t, true)

	if err := ValidateTx(txData); err == nil {
		t.Fatal("expected error for bad ref ID size in mutable_refs")
	}
}

func TestValidateTx_TooManyObjectRefs(t *testing.T) {
	txData := buildTestRawTx(t, func(o *txOptions) {
		o.mutableRefs = makeUniqueRefs(41)
	})

	if err := ValidateTx(txData); err == nil {
		t.Fatal("expected error for too many object refs")
	}
}

func TestValidateTx_DuplicateInMutableObjects(t *testing.T) {
	ref := makeUniqueRefs(1)
	txData := buildTestRawTx(t, func(o *txOptions) {
		o.mutableRefs = append(ref, ref...)
	})

	if err := ValidateTx(txData); err == nil {
		t.Fatal("expected error for duplicate mutable refs")
	}
}

func TestValidateTx_SameObjectInReadAndMutable(t *testing.T) {
	ref := makeUniqueRefs(1)
	txData := buildTestRawTx(t, func(o *txOptions) {
		o.mutableRefs = ref
		o.readRefs = ref
	})

	if err := ValidateTx(txData); err == nil {
		t.Fatal("expected error for same object in read and mutable")
	}
}

func TestValidateTx_ValidWithObjects(t *testing.T) {
	txData := buildTestRawTx(t, func(o *txOptions) {
		o.mutableRefs = makeUniqueRefsFrom(3, 1)
		o.readRefs = makeUniqueRefsFrom(2, 100)
	})

	if err := ValidateTx(txData); err != nil {
		t.Fatalf("expected valid tx, got error: %v", err)
	}
}

// TestValidateTx_WithOperations confirms a transaction carrying a declared
// operation validates: rebuildUnsignedTx must include the operations field so
// the recomputed hash matches the declared hash and the sender signature
// verifies.
func TestValidateTx_WithOperations(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	var pod, gasCoin, newParent [32]byte
	pod[0] = 0x11
	gasCoin[0] = 0x22
	newParent[0] = 0x33

	ops := []genesis.DeclaredOp{{
		Kind:       0,
		ObjectID:   bytes.Repeat([]byte{0xA1}, 32),
		TargetKind: 0,
		Target:     newParent[:],
	}}

	unsignedBytes := genesis.BuildUnsignedTxBytesSponsored(
		pub, pod, "noop", nil, nil, 0, 1000, gasCoin[:], nil, nil, genesis.Sponsorship{}, nil, ops,
	)
	hash := blake3.Sum256(unsignedBytes)
	sig := ed25519.Sign(priv, hash[:])

	builder := flatbuffers.NewBuilder(1024)
	txOffset := genesis.BuildTxTableSponsored(
		builder, pub, pod, "noop", nil, nil, 0, 1000, gasCoin[:], hash, sig, nil, nil, genesis.Sponsorship{}, nil, nil, ops,
	)
	builder.Finish(txOffset)

	if err := ValidateTx(builder.FinishedBytes()); err != nil {
		t.Fatalf("valid tx with operations rejected: %v", err)
	}
}

func TestValidateTx_MaxObjectRefs(t *testing.T) {
	txData := buildTestRawTx(t, func(o *txOptions) {
		o.mutableRefs = makeUniqueRefsFrom(8, 1)
		o.readRefs = makeUniqueRefsFrom(32, 100)
	})

	if err := ValidateTx(txData); err != nil {
		t.Fatalf("expected valid tx for exactly 40 refs, got error: %v", err)
	}
}

func TestValidateTx_SignedByDifferentKey(t *testing.T) {
	txData := buildTestRawTx(t, func(o *txOptions) {
		o.wrongSender = true
	})

	if err := ValidateTx(txData); err == nil {
		t.Fatal("expected error for wrong sender pubkey")
	}
}

func TestValidateTx_GasCoinInvalidSize(t *testing.T) {
	txData := buildTestRawTx(t, func(o *txOptions) {
		o.gasCoin = make([]byte, 16)
	})

	if err := ValidateTx(txData); err == nil {
		t.Fatal("expected error for invalid gas_coin size")
	}
}

func TestValidateTx_DuplicateReadRefs(t *testing.T) {
	refs := makeUniqueRefs(1)
	txData := buildTestRawTx(t, func(o *txOptions) {
		o.readRefs = append(refs, refs[0])
	})

	if err := ValidateTx(txData); err == nil {
		t.Fatal("expected error for duplicate read refs")
	}
}

func TestValidateTx_DomainRefNoID(t *testing.T) {
	txData := buildTxWithDomainRefs(t, []string{"example.pod"}, nil)

	if err := ValidateTx(txData); err != nil {
		t.Fatalf("expected valid tx for domain-only ref, got error: %v", err)
	}
}

func TestValidateTx_DuplicateDomainRef(t *testing.T) {
	txData := buildTxWithDomainRefs(t, []string{"dup.pod", "dup.pod"}, nil)

	if err := ValidateTx(txData); err == nil {
		t.Fatal("expected error for duplicate domain ref")
	}
}

func TestValidateTx_DuplicateDomainAcrossReadAndMutable(t *testing.T) {
	txData := buildTxWithDomainRefs(t, []string{"shared.pod"}, []string{"shared.pod"})

	if err := ValidateTx(txData); err == nil {
		t.Fatal("expected error for duplicate domain across read and mutable")
	}
}

func TestValidateTx_UniqueDomainRefs(t *testing.T) {
	txData := buildTxWithDomainRefs(t, []string{"alpha.pod", "beta.pod"}, nil)

	if err := ValidateTx(txData); err != nil {
		t.Fatalf("expected valid tx for unique domain refs, got error: %v", err)
	}
}

// --- Test helpers ---

// txOptions controls how buildTestRawTx constructs the transaction.
type txOptions struct {
	senderSize       int                     // override sender byte size (default 32)
	signatureSize    int                     // override signature byte size (default 64)
	podSize          int                     // override pod byte size (default 32)
	functionName     string                  // override function name (default "test")
	emptyFuncName    bool                    // force empty function name
	mutableRefs      []genesis.ObjectRefData // override mutable object refs
	readRefs         []genesis.ObjectRefData // override read object refs
	gasCoin          []byte                  // optional gas_coin bytes (nil = absent)
	corruptSignature bool                    // flip a bit in the signature
	tamperedHash     bool                    // use a wrong hash
	wrongSender      bool                    // sign with one key, put different sender
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

	unsignedBytes := genesis.BuildUnsignedTxBytesWithRefs(
		pub, pod, "test", []byte{}, nil, 0, 0, nil, nil, nil,
	)
	hash := blake3.Sum256(unsignedBytes)
	sig := ed25519.Sign(priv, hash[:])

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

// buildTxWithDomainRefs creates a signed Transaction with domain-based ObjectRefs.
func buildTxWithDomainRefs(t *testing.T, mutableDomains, readDomains []string) []byte {
	t.Helper()

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	var pod [32]byte

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
