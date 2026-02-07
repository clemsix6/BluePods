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

// BuildRegisterValidatorTx creates a signed register_validator transaction.
// This is used both for genesis and for new validators joining the network.
func BuildRegisterValidatorTx(privKey ed25519.PrivateKey, systemPod [32]byte, httpAddr, quicAddr string) []byte {
	args := encodeRegisterValidatorArgs([]byte(httpAddr), []byte(quicAddr))
	return BuildAttestedTx(privKey, systemPod, "register_validator", args, true)
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
	return BuildUnsignedTxBytesWithMutables(sender, pod, funcName, args, createsObjects, nil)
}

// BuildUnsignedTxBytesWithMutables creates transaction bytes with MutableObjects references.
// Each mutableRef is 40 bytes: 32-byte objectID + 8-byte version LE.
func BuildUnsignedTxBytesWithMutables(sender []byte, pod [32]byte, funcName string, args []byte, createsObjects bool, mutableRefs []byte) []byte {
	builder := flatbuffers.NewBuilder(512)

	argsVec := builder.CreateByteVector(args)
	senderVec := builder.CreateByteVector(sender)
	podVec := builder.CreateByteVector(pod[:])
	funcNameOff := builder.CreateString(funcName)

	var mutObjVec flatbuffers.UOffsetT
	if len(mutableRefs) > 0 {
		mutObjVec = builder.CreateByteVector(mutableRefs)
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
	txOff := types.TransactionEnd(builder)

	builder.Finish(txOff)

	return builder.FinishedBytes()
}

// BuildTxTableWithMutables builds a Transaction table with MutableObjects in the given builder.
func BuildTxTableWithMutables(builder *flatbuffers.Builder, sender []byte, pod [32]byte, funcName string, args []byte, createsObjects bool, hash [32]byte, sig []byte, mutableRefs []byte) flatbuffers.UOffsetT {
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

	return types.TransactionEnd(builder)
}
