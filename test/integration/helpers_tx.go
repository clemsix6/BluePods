package integration

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"io"
	"net/http"

	flatbuffers "github.com/google/flatbuffers/go"
	"github.com/zeebo/blake3"

	"BluePods/internal/genesis"
	"BluePods/internal/types"
)

// SubmitRawBytes sends arbitrary bytes to POST /tx, returns HTTP status code and body.
func SubmitRawBytes(addr string, data []byte) (int, string) {
	resp, err := http.Post("http://"+addr+"/tx", "application/octet-stream", bytes.NewReader(data))
	if err != nil {
		return 0, err.Error()
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)

	return resp.StatusCode, string(body)
}

// SubmitJSON sends JSON to a POST endpoint, returns status code and body.
func SubmitJSON(addr, path string, payload any) (int, string) {
	jsonBytes, _ := json.Marshal(payload)

	resp, err := http.Post("http://"+addr+path, "application/json", bytes.NewReader(jsonBytes))
	if err != nil {
		return 0, err.Error()
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)

	return resp.StatusCode, string(body)
}

// BuildValidTx builds a correctly signed raw Transaction.
func BuildValidTx(systemPod [32]byte, funcName string, args []byte) []byte {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	pub := priv.Public().(ed25519.PublicKey)

	unsignedBytes := genesis.BuildUnsignedTxBytesWithRefs(
		pub, systemPod, funcName, args, nil, 0, 0, nil, nil, nil,
	)
	hash := blake3.Sum256(unsignedBytes)
	sig := ed25519.Sign(priv, hash[:])

	builder := flatbuffers.NewBuilder(1024)
	txOff := genesis.BuildTxTableWithRefs(
		builder, pub, systemPod, funcName, args, nil, 0, 0, nil, hash, sig, nil, nil,
	)
	builder.Finish(txOff)

	return builder.FinishedBytes()
}

// BuildTxWithBadHash builds a tx then flips a bit in the hash.
func BuildTxWithBadHash(systemPod [32]byte) []byte {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	pub := priv.Public().(ed25519.PublicKey)

	unsignedBytes := genesis.BuildUnsignedTxBytesWithRefs(
		pub, systemPod, "test", nil, nil, 0, 0, nil, nil, nil,
	)
	hash := blake3.Sum256(unsignedBytes)
	sig := ed25519.Sign(priv, hash[:])

	// Corrupt the hash
	badHash := hash
	badHash[0] ^= 0xFF

	builder := flatbuffers.NewBuilder(1024)
	txOff := genesis.BuildTxTableWithRefs(
		builder, pub, systemPod, "test", nil, nil, 0, 0, nil, badHash, sig, nil, nil,
	)
	builder.Finish(txOff)

	return builder.FinishedBytes()
}

// BuildTxWithBadSig builds a tx then flips a bit in the signature.
func BuildTxWithBadSig(systemPod [32]byte) []byte {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	pub := priv.Public().(ed25519.PublicKey)

	unsignedBytes := genesis.BuildUnsignedTxBytesWithRefs(
		pub, systemPod, "test", nil, nil, 0, 0, nil, nil, nil,
	)
	hash := blake3.Sum256(unsignedBytes)
	sig := ed25519.Sign(priv, hash[:])

	// Corrupt the signature
	sig[0] ^= 0xFF

	builder := flatbuffers.NewBuilder(1024)
	txOff := genesis.BuildTxTableWithRefs(
		builder, pub, systemPod, "test", nil, nil, 0, 0, nil, hash, sig, nil, nil,
	)
	builder.Finish(txOff)

	return builder.FinishedBytes()
}

// BuildTxWithWrongSender signs with key A but puts key B as sender.
func BuildTxWithWrongSender(systemPod [32]byte) []byte {
	_, privA, _ := ed25519.GenerateKey(rand.Reader)
	pubB, _, _ := ed25519.GenerateKey(rand.Reader)

	// Hash includes pubB as sender, but signature is by privA
	unsignedBytes := genesis.BuildUnsignedTxBytesWithRefs(
		pubB, systemPod, "test", nil, nil, 0, 0, nil, nil, nil,
	)
	hash := blake3.Sum256(unsignedBytes)
	sig := ed25519.Sign(privA, hash[:])

	builder := flatbuffers.NewBuilder(1024)
	txOff := genesis.BuildTxTableWithRefs(
		builder, pubB, systemPod, "test", nil, nil, 0, 0, nil, hash, sig, nil, nil,
	)
	builder.Finish(txOff)

	return builder.FinishedBytes()
}

// BuildTxWithFieldSize builds a tx with a specific field having wrong size.
// field: "hash", "sender", "signature", "pod", "gas_coin"
func BuildTxWithFieldSize(systemPod [32]byte, field string, size int) []byte {
	hash := make([]byte, 32)
	sender := make([]byte, 32)
	sig := make([]byte, 64)
	pod := systemPod[:]
	funcName := "test"
	var gasCoin []byte

	switch field {
	case "hash":
		hash = make([]byte, size)
	case "sender":
		sender = make([]byte, size)
	case "signature":
		sig = make([]byte, size)
	case "pod":
		pod = make([]byte, size)
	case "gas_coin":
		gasCoin = make([]byte, size)
	}

	return buildRawTxBytes(hash, sender, sig, pod, funcName, nil, gasCoin)
}

// BuildTxWithEmptyFuncName builds a tx with empty function_name.
func BuildTxWithEmptyFuncName(systemPod [32]byte) []byte {
	return buildRawTxBytes(
		make([]byte, 32), make([]byte, 32), make([]byte, 64),
		systemPod[:], "", nil, nil,
	)
}

// BuildTxWithDuplicateRefs builds a tx with duplicate object refs.
// inMutable: duplicates in mutable_refs. inRead: duplicates in read_refs.
// Both true: cross-list duplicate (same ID in mutable and read).
func BuildTxWithDuplicateRefs(systemPod [32]byte, inMutable, inRead bool) []byte {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	pub := priv.Public().(ed25519.PublicKey)

	var fakeID [32]byte
	fakeID[0] = 0x42
	ref := genesis.ObjectRefData{ID: fakeID, Version: 1}

	var mutableRefs, readRefs []genesis.ObjectRefData
	if inMutable && inRead {
		mutableRefs = []genesis.ObjectRefData{ref}
		readRefs = []genesis.ObjectRefData{ref}
	} else if inMutable {
		mutableRefs = []genesis.ObjectRefData{ref, ref}
	} else if inRead {
		readRefs = []genesis.ObjectRefData{ref, ref}
	}

	unsignedBytes := genesis.BuildUnsignedTxBytesWithRefs(
		pub, systemPod, "test", nil, nil, 0, 0, nil, mutableRefs, readRefs,
	)
	hash := blake3.Sum256(unsignedBytes)
	sig := ed25519.Sign(priv, hash[:])

	builder := flatbuffers.NewBuilder(1024)
	txOff := genesis.BuildTxTableWithRefs(
		builder, pub, systemPod, "test", nil, nil, 0, 0, nil, hash, sig, mutableRefs, readRefs,
	)
	builder.Finish(txOff)

	return builder.FinishedBytes()
}

// BuildTxWithDuplicateDomainRefs builds a tx with duplicate domain references.
func BuildTxWithDuplicateDomainRefs(systemPod [32]byte, domain string) []byte {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	pub := priv.Public().(ed25519.PublicKey)

	refs := []genesis.ObjectRefData{{Domain: domain}, {Domain: domain}}

	unsignedBytes := genesis.BuildUnsignedTxBytesWithRefs(
		pub, systemPod, "test", nil, nil, 0, 0, nil, nil, refs,
	)
	hash := blake3.Sum256(unsignedBytes)
	sig := ed25519.Sign(priv, hash[:])

	builder := flatbuffers.NewBuilder(1024)
	txOff := genesis.BuildTxTableWithRefs(
		builder, pub, systemPod, "test", nil, nil, 0, 0, nil, hash, sig, nil, refs,
	)
	builder.Finish(txOff)

	return builder.FinishedBytes()
}

// BuildTxWithTooManyRefs builds a tx with >40 object refs.
func BuildTxWithTooManyRefs(systemPod [32]byte) []byte {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	pub := priv.Public().(ed25519.PublicKey)

	// 41 unique refs exceed the maxObjectRefs=40 limit
	refs := make([]genesis.ObjectRefData, 41)
	for i := range refs {
		var id [32]byte
		id[0] = byte(i)
		id[1] = byte(i >> 8)
		refs[i] = genesis.ObjectRefData{ID: id, Version: 1}
	}

	unsignedBytes := genesis.BuildUnsignedTxBytesWithRefs(
		pub, systemPod, "test", nil, nil, 0, 0, nil, refs, nil,
	)
	hash := blake3.Sum256(unsignedBytes)
	sig := ed25519.Sign(priv, hash[:])

	builder := flatbuffers.NewBuilder(4096)
	txOff := genesis.BuildTxTableWithRefs(
		builder, pub, systemPod, "test", nil, nil, 0, 0, nil, hash, sig, refs, nil,
	)
	builder.Finish(txOff)

	return builder.FinishedBytes()
}

// BuildTxWithDomainRef builds a tx referencing a domain (no ID).
func BuildTxWithDomainRef(systemPod [32]byte, domain string) []byte {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	pub := priv.Public().(ed25519.PublicKey)

	refs := []genesis.ObjectRefData{{Domain: domain}}

	unsignedBytes := genesis.BuildUnsignedTxBytesWithRefs(
		pub, systemPod, "test", nil, nil, 0, 0, nil, nil, refs,
	)
	hash := blake3.Sum256(unsignedBytes)
	sig := ed25519.Sign(priv, hash[:])

	builder := flatbuffers.NewBuilder(1024)
	txOff := genesis.BuildTxTableWithRefs(
		builder, pub, systemPod, "test", nil, nil, 0, 0, nil, hash, sig, nil, refs,
	)
	builder.Finish(txOff)

	return builder.FinishedBytes()
}

// BuildTxWithBadRefIDSize builds a tx with a ref whose ID is not 32 bytes.
func BuildTxWithBadRefIDSize(systemPod [32]byte, idSize int) []byte {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	pub := priv.Public().(ed25519.PublicKey)

	// Build valid hash/sig without refs (hash won't match but ref check is first)
	unsignedBytes := genesis.BuildUnsignedTxBytesWithRefs(
		pub, systemPod, "test", nil, nil, 0, 0, nil, nil, nil,
	)
	hash := blake3.Sum256(unsignedBytes)
	sig := ed25519.Sign(priv, hash[:])

	builder := flatbuffers.NewBuilder(1024)

	// Create field vectors
	hashVec := builder.CreateByteVector(hash[:])
	sigVec := builder.CreateByteVector(sig)
	senderVec := builder.CreateByteVector(pub)
	podVec := builder.CreateByteVector(systemPod[:])
	funcNameOff := builder.CreateString("test")
	argsVec := builder.CreateByteVector(nil)

	// Build a mutable ref with wrong-sized ID
	refOff := buildRefWithCustomIDSize(builder, idSize)
	types.TransactionStartMutableRefsVector(builder, 1)
	builder.PrependUOffsetT(refOff)
	mutRefsVec := builder.EndVector(1)

	types.TransactionStart(builder)
	types.TransactionAddHash(builder, hashVec)
	types.TransactionAddSender(builder, senderVec)
	types.TransactionAddSignature(builder, sigVec)
	types.TransactionAddPod(builder, podVec)
	types.TransactionAddFunctionName(builder, funcNameOff)
	types.TransactionAddArgs(builder, argsVec)
	types.TransactionAddMutableRefs(builder, mutRefsVec)
	txOff := types.TransactionEnd(builder)
	builder.Finish(txOff)

	return builder.FinishedBytes()
}

// buildRefWithCustomIDSize builds an ObjectRef with a custom-sized ID.
func buildRefWithCustomIDSize(builder *flatbuffers.Builder, idSize int) flatbuffers.UOffsetT {
	idVec := builder.CreateByteVector(make([]byte, idSize))

	types.ObjectRefStart(builder)
	types.ObjectRefAddId(builder, idVec)
	types.ObjectRefAddVersion(builder, 1)

	return types.ObjectRefEnd(builder)
}

// BuildSignedTxWithGasCoin builds a correctly signed tx with gas_coin set.
// Used to test gas coin validation (ATP 6.x, 21.15).
func BuildSignedTxWithGasCoin(
	systemPod [32]byte,
	funcName string,
	args []byte,
	gasCoin [32]byte,
	mutableRefs []genesis.ObjectRefData,
) []byte {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	pub := priv.Public().(ed25519.PublicKey)

	unsignedBytes := genesis.BuildUnsignedTxBytesWithRefs(
		pub, systemPod, funcName, args, nil, 0, 1000, gasCoin[:], mutableRefs, nil,
	)
	hash := blake3.Sum256(unsignedBytes)
	sig := ed25519.Sign(priv, hash[:])

	builder := flatbuffers.NewBuilder(1024)
	txOff := genesis.BuildTxTableWithRefs(
		builder, pub, systemPod, funcName, args, nil, 0, 1000, gasCoin[:], hash, sig, mutableRefs, nil,
	)
	builder.Finish(txOff)

	return builder.FinishedBytes()
}

// BuildNonOwnerTransferTx builds a transfer tx signed by a random key (non-owner).
// The tx references coinID at the given version as mutable ref.
func BuildNonOwnerTransferTx(systemPod [32]byte, coinID [32]byte, version uint64, newOwner [32]byte) []byte {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	pub := priv.Public().(ed25519.PublicKey)

	args := make([]byte, 32)
	copy(args, newOwner[:])

	mutableRefs := []genesis.ObjectRefData{{ID: coinID, Version: version}}

	unsignedBytes := genesis.BuildUnsignedTxBytesWithRefs(
		pub, systemPod, "transfer", args, nil, 0, 0, nil, mutableRefs, nil,
	)
	hash := blake3.Sum256(unsignedBytes)
	sig := ed25519.Sign(priv, hash[:])

	builder := flatbuffers.NewBuilder(1024)
	txOff := genesis.BuildTxTableWithRefs(
		builder, pub, systemPod, "transfer", args, nil, 0, 0, nil, hash, sig, mutableRefs, nil,
	)
	builder.Finish(txOff)

	return builder.FinishedBytes()
}

// buildRawTxBytes builds a Transaction FlatBuffer with custom-sized fields.
// Unlike genesis.BuildTxTableWithRefs, this accepts []byte for all fields,
// allowing construction of deliberately malformed transactions.
func buildRawTxBytes(hash, sender, sig, pod []byte, funcName string, args, gasCoin []byte) []byte {
	builder := flatbuffers.NewBuilder(1024)

	hashVec := builder.CreateByteVector(hash)
	sigVec := builder.CreateByteVector(sig)
	argsVec := builder.CreateByteVector(args)
	senderVec := builder.CreateByteVector(sender)
	podVec := builder.CreateByteVector(pod)
	funcNameOff := builder.CreateString(funcName)

	var gasCoinVec flatbuffers.UOffsetT
	if len(gasCoin) > 0 {
		gasCoinVec = builder.CreateByteVector(gasCoin)
	}

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

	txOff := types.TransactionEnd(builder)
	builder.Finish(txOff)

	return builder.FinishedBytes()
}
