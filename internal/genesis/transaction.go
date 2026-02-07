package genesis

import (
	"crypto/ed25519"

	flatbuffers "github.com/google/flatbuffers/go"
	"github.com/zeebo/blake3"

	"BluePods/internal/types"
)

// BuildMintTx creates a signed mint transaction wrapped in AttestedTransaction.
func BuildMintTx(privKey ed25519.PrivateKey, systemPod [32]byte, amount uint64, owner [32]byte) []byte {
	args := EncodeMintArgs(amount, owner)
	return BuildAttestedTx(privKey, systemPod, "mint", args, true)
}

// BuildRegisterValidatorTx creates a signed register_validator transaction as ATX.
// Used for genesis transactions sent directly via HTTP (bootstrap path).
func BuildRegisterValidatorTx(privKey ed25519.PrivateKey, systemPod [32]byte, httpAddr, quicAddr string, blsPubkey []byte) []byte {
	args := encodeRegisterValidatorArgs([]byte(httpAddr), []byte(quicAddr), blsPubkey)
	return BuildAttestedTx(privKey, systemPod, "register_validator", args, true)
}

// BuildRegisterValidatorRawTx creates a signed register_validator as a raw Transaction.
// Used by validators joining the network — the receiving node wraps it in ATX.
func BuildRegisterValidatorRawTx(privKey ed25519.PrivateKey, systemPod [32]byte, httpAddr, quicAddr string, blsPubkey []byte) []byte {
	args := encodeRegisterValidatorArgs([]byte(httpAddr), []byte(quicAddr), blsPubkey)
	pubKey := privKey.Public().(ed25519.PublicKey)

	unsignedBytes := BuildUnsignedTxBytes(pubKey, systemPod, "register_validator", args, true)
	hash := blake3.Sum256(unsignedBytes)
	sig := ed25519.Sign(privKey, hash[:])

	builder := flatbuffers.NewBuilder(1024)
	txOffset := BuildTxTable(builder, pubKey, systemPod, "register_validator", args, true, hash, sig)
	builder.Finish(txOffset)

	return builder.FinishedBytes()
}

// BuildAttestedTx creates a signed transaction wrapped in AttestedTransaction.
// Genesis transactions have no objects or proofs since they don't reference existing objects.
func BuildAttestedTx(privKey ed25519.PrivateKey, pod [32]byte, funcName string, args []byte, createsObjects bool) []byte {
	pubKey := privKey.Public().(ed25519.PublicKey)

	// Build unsigned tx first to compute hash
	unsignedBytes := BuildUnsignedTxBytes(pubKey, pod, funcName, args, createsObjects)
	hash := blake3.Sum256(unsignedBytes)
	sig := ed25519.Sign(privKey, hash[:])

	// Build complete AttestedTransaction with embedded Transaction
	builder := flatbuffers.NewBuilder(1024)

	// Build Transaction table first (must be done before AttestedTransaction)
	txOffset := BuildTxTable(builder, pubKey, pod, funcName, args, createsObjects, hash, sig)

	// Empty objects vector
	types.AttestedTransactionStartObjectsVector(builder, 0)
	objectsVec := builder.EndVector(0)

	// Empty proofs vector
	types.AttestedTransactionStartProofsVector(builder, 0)
	proofsVec := builder.EndVector(0)

	types.AttestedTransactionStart(builder)
	types.AttestedTransactionAddTransaction(builder, txOffset)
	types.AttestedTransactionAddObjects(builder, objectsVec)
	types.AttestedTransactionAddProofs(builder, proofsVec)
	atxOffset := types.AttestedTransactionEnd(builder)

	builder.Finish(atxOffset)

	return builder.FinishedBytes()
}

// WrapInATX wraps a raw Transaction in an AttestedTransaction with empty objects/proofs.
// Used for bootstrap/faucet paths where aggregation is not needed.
// The raw Transaction FlatBuffer is parsed and rebuilt inside the new builder.
func WrapInATX(txBytes []byte) []byte {
	tx := types.GetRootAsTransaction(txBytes, 0)

	builder := flatbuffers.NewBuilder(len(txBytes) + 256)

	// Rebuild Transaction table in the new builder
	txOffset := rebuildTxInBuilder(builder, tx)

	// Empty objects vector
	types.AttestedTransactionStartObjectsVector(builder, 0)
	objectsVec := builder.EndVector(0)

	// Empty proofs vector
	types.AttestedTransactionStartProofsVector(builder, 0)
	proofsVec := builder.EndVector(0)

	types.AttestedTransactionStart(builder)
	types.AttestedTransactionAddTransaction(builder, txOffset)
	types.AttestedTransactionAddObjects(builder, objectsVec)
	types.AttestedTransactionAddProofs(builder, proofsVec)
	atxOffset := types.AttestedTransactionEnd(builder)

	builder.Finish(atxOffset)

	return builder.FinishedBytes()
}

// rebuildTxInBuilder rebuilds a Transaction table in the given builder.
func rebuildTxInBuilder(builder *flatbuffers.Builder, tx *types.Transaction) flatbuffers.UOffsetT {
	hashVec := builder.CreateByteVector(tx.HashBytes())
	sigVec := builder.CreateByteVector(tx.SignatureBytes())
	argsVec := builder.CreateByteVector(tx.ArgsBytes())
	senderVec := builder.CreateByteVector(tx.SenderBytes())
	podVec := builder.CreateByteVector(tx.PodBytes())
	funcNameOff := builder.CreateString(string(tx.FunctionName()))
	mutObjVec := builder.CreateByteVector(tx.MutableObjectsBytes())
	readObjVec := builder.CreateByteVector(tx.ReadObjectsBytes())

	types.TransactionStart(builder)
	types.TransactionAddHash(builder, hashVec)
	types.TransactionAddSender(builder, senderVec)
	types.TransactionAddSignature(builder, sigVec)
	types.TransactionAddPod(builder, podVec)
	types.TransactionAddFunctionName(builder, funcNameOff)
	types.TransactionAddArgs(builder, argsVec)
	types.TransactionAddCreatesObjects(builder, tx.CreatesObjects())
	types.TransactionAddMutableObjects(builder, mutObjVec)
	types.TransactionAddReadObjects(builder, readObjVec)

	return types.TransactionEnd(builder)
}

// BuildTxTable builds a Transaction table in the given builder.
func BuildTxTable(builder *flatbuffers.Builder, sender []byte, pod [32]byte, funcName string, args []byte, createsObjects bool, hash [32]byte, sig []byte) flatbuffers.UOffsetT {
	hashVec := builder.CreateByteVector(hash[:])
	sigVec := builder.CreateByteVector(sig)
	argsVec := builder.CreateByteVector(args)
	senderVec := builder.CreateByteVector(sender)
	podVec := builder.CreateByteVector(pod[:])
	funcNameOff := builder.CreateString(funcName)

	types.TransactionStart(builder)
	types.TransactionAddHash(builder, hashVec)
	types.TransactionAddSender(builder, senderVec)
	types.TransactionAddSignature(builder, sigVec)
	types.TransactionAddPod(builder, podVec)
	types.TransactionAddFunctionName(builder, funcNameOff)
	types.TransactionAddArgs(builder, argsVec)
	types.TransactionAddCreatesObjects(builder, createsObjects)

	return types.TransactionEnd(builder)
}

// BuildUnsignedTxBytes creates transaction bytes without hash and signature for hashing.
func BuildUnsignedTxBytes(sender []byte, pod [32]byte, funcName string, args []byte, createsObjects bool) []byte {
	return BuildUnsignedTxBytesWithMutables(sender, pod, funcName, args, createsObjects, nil, nil)
}

// BuildUnsignedTxBytesWithMutables creates transaction bytes with MutableObjects and ReadObjects references.
// Each mutableRef/readRef is 40 bytes: 32-byte objectID + 8-byte version LE.
func BuildUnsignedTxBytesWithMutables(sender []byte, pod [32]byte, funcName string, args []byte, createsObjects bool, mutableRefs []byte, readRefs []byte) []byte {
	builder := flatbuffers.NewBuilder(512)

	argsVec := builder.CreateByteVector(args)
	senderVec := builder.CreateByteVector(sender)
	podVec := builder.CreateByteVector(pod[:])
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

// BuildTxTableWithMutables builds a Transaction table with MutableObjects and ReadObjects in the given builder.
func BuildTxTableWithMutables(builder *flatbuffers.Builder, sender []byte, pod [32]byte, funcName string, args []byte, createsObjects bool, hash [32]byte, sig []byte, mutableRefs []byte, readRefs []byte) flatbuffers.UOffsetT {
	hashVec := builder.CreateByteVector(hash[:])
	sigVec := builder.CreateByteVector(sig)
	argsVec := builder.CreateByteVector(args)
	senderVec := builder.CreateByteVector(sender)
	podVec := builder.CreateByteVector(pod[:])
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

	return types.TransactionEnd(builder)
}
