package genesis

import (
	"crypto/ed25519"

	flatbuffers "github.com/google/flatbuffers/go"
	"github.com/zeebo/blake3"

	"BluePods/internal/types"
)

// buildMintTx creates a signed mint transaction wrapped in AttestedTransaction.
func buildMintTx(privKey ed25519.PrivateKey, systemPod [32]byte, amount uint64, owner [32]byte) []byte {
	args := encodeMintArgs(amount, owner)
	return buildAttestedTx(privKey, systemPod, "mint", args, true)
}

// buildRegisterValidatorTx creates a signed register_validator transaction.
func buildRegisterValidatorTx(privKey ed25519.PrivateKey, systemPod [32]byte, httpAddr, quicAddr []byte) []byte {
	args := encodeRegisterValidatorArgs(httpAddr, quicAddr)
	return buildAttestedTx(privKey, systemPod, "register_validator", args, true)
}

// buildAttestedTx creates a signed transaction wrapped in AttestedTransaction.
// Genesis transactions have no objects or proofs since they don't reference existing objects.
func buildAttestedTx(privKey ed25519.PrivateKey, pod [32]byte, funcName string, args []byte, createsObjects bool) []byte {
	pubKey := privKey.Public().(ed25519.PublicKey)

	// Build unsigned tx first to compute hash
	unsignedBytes := buildUnsignedTxBytes(pubKey, pod, funcName, args, createsObjects)
	hash := blake3.Sum256(unsignedBytes)
	sig := ed25519.Sign(privKey, hash[:])

	// Build complete AttestedTransaction with embedded Transaction
	builder := flatbuffers.NewBuilder(1024)

	// Build Transaction table first (must be done before AttestedTransaction)
	txOffset := buildTxTable(builder, pubKey, pod, funcName, args, createsObjects, hash, sig)

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

// buildTxTable builds a Transaction table in the given builder.
func buildTxTable(builder *flatbuffers.Builder, sender []byte, pod [32]byte, funcName string, args []byte, createsObjects bool, hash [32]byte, sig []byte) flatbuffers.UOffsetT {
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

// buildUnsignedTxBytes creates transaction bytes without hash and signature for hashing.
func buildUnsignedTxBytes(sender []byte, pod [32]byte, funcName string, args []byte, createsObjects bool) []byte {
	builder := flatbuffers.NewBuilder(512)

	argsVec := builder.CreateByteVector(args)
	senderVec := builder.CreateByteVector(sender)
	podVec := builder.CreateByteVector(pod[:])
	funcNameOff := builder.CreateString(funcName)

	types.TransactionStart(builder)
	types.TransactionAddSender(builder, senderVec)
	types.TransactionAddPod(builder, podVec)
	types.TransactionAddFunctionName(builder, funcNameOff)
	types.TransactionAddArgs(builder, argsVec)
	types.TransactionAddCreatesObjects(builder, createsObjects)
	txOff := types.TransactionEnd(builder)

	builder.Finish(txOff)

	return builder.FinishedBytes()
}
