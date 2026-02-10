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
	return BuildAttestedTx(privKey, systemPod, "mint", args, []uint16{0}, 0, 0, nil)
}

// BuildRegisterValidatorTx creates a signed register_validator transaction as ATX.
// Used for genesis transactions sent directly via HTTP (bootstrap path).
func BuildRegisterValidatorTx(privKey ed25519.PrivateKey, systemPod [32]byte, httpAddr, quicAddr string, blsPubkey []byte) []byte {
	args := encodeRegisterValidatorArgs([]byte(httpAddr), []byte(quicAddr), blsPubkey)
	return BuildAttestedTx(privKey, systemPod, "register_validator", args, []uint16{0}, 0, 0, nil)
}

// BuildRegisterValidatorRawTx creates a signed register_validator as a raw Transaction.
// Used by validators joining the network — the receiving node wraps it in ATX.
func BuildRegisterValidatorRawTx(privKey ed25519.PrivateKey, systemPod [32]byte, httpAddr, quicAddr string, blsPubkey []byte) []byte {
	args := encodeRegisterValidatorArgs([]byte(httpAddr), []byte(quicAddr), blsPubkey)
	pubKey := privKey.Public().(ed25519.PublicKey)

	unsignedBytes := BuildUnsignedTxBytes(pubKey, systemPod, "register_validator", args, []uint16{0}, 0, 0, nil)
	hash := blake3.Sum256(unsignedBytes)
	sig := ed25519.Sign(privKey, hash[:])

	builder := flatbuffers.NewBuilder(1024)
	txOffset := BuildTxTable(builder, pubKey, systemPod, "register_validator", args, []uint16{0}, 0, 0, nil, hash, sig)
	builder.Finish(txOffset)

	return builder.FinishedBytes()
}

// BuildDeregisterValidatorRawTx creates a signed deregister_validator as a raw Transaction.
// The sender pubkey identifies the validator requesting departure.
// No args are needed — the Go consensus layer handles epoch-deferred removal.
func BuildDeregisterValidatorRawTx(privKey ed25519.PrivateKey, systemPod [32]byte) []byte {
	pubKey := privKey.Public().(ed25519.PublicKey)

	unsignedBytes := BuildUnsignedTxBytes(pubKey, systemPod, "deregister_validator", nil, nil, 0, 0, nil)
	hash := blake3.Sum256(unsignedBytes)
	sig := ed25519.Sign(privKey, hash[:])

	builder := flatbuffers.NewBuilder(512)
	txOffset := BuildTxTable(builder, pubKey, systemPod, "deregister_validator", nil, nil, 0, 0, nil, hash, sig)
	builder.Finish(txOffset)

	return builder.FinishedBytes()
}

// BuildAttestedTx creates a signed transaction wrapped in AttestedTransaction.
// Genesis transactions have no objects or proofs since they don't reference existing objects.
func BuildAttestedTx(privKey ed25519.PrivateKey, pod [32]byte, funcName string, args []byte, createdObjectsReplication []uint16, maxCreateDomains uint16, maxGas uint64, gasCoin []byte) []byte {
	pubKey := privKey.Public().(ed25519.PublicKey)

	// Build unsigned tx first to compute hash
	unsignedBytes := BuildUnsignedTxBytes(pubKey, pod, funcName, args, createdObjectsReplication, maxCreateDomains, maxGas, gasCoin)
	hash := blake3.Sum256(unsignedBytes)
	sig := ed25519.Sign(privKey, hash[:])

	// Build complete AttestedTransaction with embedded Transaction
	builder := flatbuffers.NewBuilder(1024)

	// Build Transaction table first (must be done before AttestedTransaction)
	txOffset := BuildTxTable(builder, pubKey, pod, funcName, args, createdObjectsReplication, maxCreateDomains, maxGas, gasCoin, hash, sig)

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
	txOffset := RebuildTxInBuilder(builder, tx)

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

// RebuildTxInBuilder rebuilds a Transaction table in the given builder.
// Exported because it's needed by multiple packages.
func RebuildTxInBuilder(builder *flatbuffers.Builder, tx *types.Transaction) flatbuffers.UOffsetT {
	hashVec := builder.CreateByteVector(tx.HashBytes())
	sigVec := builder.CreateByteVector(tx.SignatureBytes())
	argsVec := builder.CreateByteVector(tx.ArgsBytes())
	senderVec := builder.CreateByteVector(tx.SenderBytes())
	podVec := builder.CreateByteVector(tx.PodBytes())
	funcNameOff := builder.CreateString(string(tx.FunctionName()))

	readRefsVec := BuildObjectRefVectorFromTx(builder, tx, false)
	mutRefsVec := BuildObjectRefVectorFromTx(builder, tx, true)

	// Rebuild created_objects_replication vector
	corVec := rebuildCreatedObjectsReplication(builder, tx)

	// Rebuild gas_coin vector
	var gasCoinVec flatbuffers.UOffsetT
	if gcBytes := tx.GasCoinBytes(); len(gcBytes) > 0 {
		gasCoinVec = builder.CreateByteVector(gcBytes)
	}

	types.TransactionStart(builder)
	types.TransactionAddHash(builder, hashVec)
	types.TransactionAddSender(builder, senderVec)
	types.TransactionAddSignature(builder, sigVec)
	types.TransactionAddPod(builder, podVec)
	types.TransactionAddFunctionName(builder, funcNameOff)
	types.TransactionAddArgs(builder, argsVec)
	types.TransactionAddMaxCreateDomains(builder, tx.MaxCreateDomains())
	types.TransactionAddMaxGas(builder, tx.MaxGas())

	if corVec != 0 {
		types.TransactionAddCreatedObjectsReplication(builder, corVec)
	}

	if gasCoinVec != 0 {
		types.TransactionAddGasCoin(builder, gasCoinVec)
	}

	if mutRefsVec != 0 {
		types.TransactionAddMutableRefs(builder, mutRefsVec)
	}

	if readRefsVec != 0 {
		types.TransactionAddReadRefs(builder, readRefsVec)
	}

	return types.TransactionEnd(builder)
}

// rebuildCreatedObjectsReplication rebuilds the created_objects_replication vector from tx.
func rebuildCreatedObjectsReplication(builder *flatbuffers.Builder, tx *types.Transaction) flatbuffers.UOffsetT {
	count := tx.CreatedObjectsReplicationLength()
	if count == 0 {
		return 0
	}

	types.TransactionStartCreatedObjectsReplicationVector(builder, count)
	for i := count - 1; i >= 0; i-- {
		builder.PrependUint16(tx.CreatedObjectsReplication(i))
	}

	return builder.EndVector(count)
}

// BuildObjectRefVectorFromTx rebuilds a read_refs or mutable_refs vector from a Transaction.
// If mutable is true, rebuilds mutable_refs; otherwise read_refs.
func BuildObjectRefVectorFromTx(builder *flatbuffers.Builder, tx *types.Transaction, mutable bool) flatbuffers.UOffsetT {
	var count int
	if mutable {
		count = tx.MutableRefsLength()
	} else {
		count = tx.ReadRefsLength()
	}

	if count == 0 {
		return 0
	}

	offsets := make([]flatbuffers.UOffsetT, count)
	var ref types.ObjectRef

	for i := 0; i < count; i++ {
		if mutable {
			tx.MutableRefs(&ref, i)
		} else {
			tx.ReadRefs(&ref, i)
		}

		offsets[i] = RebuildObjectRef(builder, &ref)
	}

	if mutable {
		types.TransactionStartMutableRefsVector(builder, count)
	} else {
		types.TransactionStartReadRefsVector(builder, count)
	}

	for i := count - 1; i >= 0; i-- {
		builder.PrependUOffsetT(offsets[i])
	}

	return builder.EndVector(count)
}

// RebuildObjectRef rebuilds a single ObjectRef in the builder.
func RebuildObjectRef(builder *flatbuffers.Builder, ref *types.ObjectRef) flatbuffers.UOffsetT {
	var idVec flatbuffers.UOffsetT
	if idBytes := ref.IdBytes(); len(idBytes) > 0 {
		idVec = builder.CreateByteVector(idBytes)
	}

	var domainOff flatbuffers.UOffsetT
	if domain := ref.Domain(); len(domain) > 0 {
		domainOff = builder.CreateString(string(domain))
	}

	types.ObjectRefStart(builder)

	if idVec != 0 {
		types.ObjectRefAddId(builder, idVec)
	}

	types.ObjectRefAddVersion(builder, ref.Version())

	if domainOff != 0 {
		types.ObjectRefAddDomain(builder, domainOff)
	}

	return types.ObjectRefEnd(builder)
}

// ObjectRefData holds the data for building an ObjectRef.
type ObjectRefData struct {
	ID      [32]byte // ID is the 32-byte object identifier
	Version uint64   // Version is the expected version
	Domain  string   // Domain is the domain name (empty for ID refs)
}

// BuildTxTable builds a Transaction table in the given builder.
func BuildTxTable(builder *flatbuffers.Builder, sender []byte, pod [32]byte, funcName string, args []byte, createdObjectsReplication []uint16, maxCreateDomains uint16, maxGas uint64, gasCoin []byte, hash [32]byte, sig []byte) flatbuffers.UOffsetT {
	return BuildTxTableWithRefs(builder, sender, pod, funcName, args, createdObjectsReplication, maxCreateDomains, maxGas, gasCoin, hash, sig, nil, nil)
}

// BuildUnsignedTxBytes creates transaction bytes without hash and signature for hashing.
func BuildUnsignedTxBytes(sender []byte, pod [32]byte, funcName string, args []byte, createdObjectsReplication []uint16, maxCreateDomains uint16, maxGas uint64, gasCoin []byte) []byte {
	return BuildUnsignedTxBytesWithRefs(sender, pod, funcName, args, createdObjectsReplication, maxCreateDomains, maxGas, gasCoin, nil, nil)
}

// BuildUnsignedTxBytesWithRefs creates transaction bytes with ObjectRef references.
func BuildUnsignedTxBytesWithRefs(sender []byte, pod [32]byte, funcName string, args []byte, createdObjectsReplication []uint16, maxCreateDomains uint16, maxGas uint64, gasCoin []byte, mutableRefs, readRefs []ObjectRefData) []byte {
	builder := flatbuffers.NewBuilder(512)

	argsVec := builder.CreateByteVector(args)
	senderVec := builder.CreateByteVector(sender)
	podVec := builder.CreateByteVector(pod[:])
	funcNameOff := builder.CreateString(funcName)

	mutRefsVec := buildObjectRefDataVector(builder, mutableRefs, true)
	readRefsVec := buildObjectRefDataVector(builder, readRefs, false)

	corVec := buildCreatedObjectsReplicationVector(builder, createdObjectsReplication)

	var gasCoinVec flatbuffers.UOffsetT
	if len(gasCoin) > 0 {
		gasCoinVec = builder.CreateByteVector(gasCoin)
	}

	types.TransactionStart(builder)
	types.TransactionAddSender(builder, senderVec)
	types.TransactionAddPod(builder, podVec)
	types.TransactionAddFunctionName(builder, funcNameOff)
	types.TransactionAddArgs(builder, argsVec)
	types.TransactionAddMaxCreateDomains(builder, maxCreateDomains)
	types.TransactionAddMaxGas(builder, maxGas)

	if corVec != 0 {
		types.TransactionAddCreatedObjectsReplication(builder, corVec)
	}

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

// BuildTxTableWithRefs builds a Transaction table with ObjectRef references in the given builder.
func BuildTxTableWithRefs(builder *flatbuffers.Builder, sender []byte, pod [32]byte, funcName string, args []byte, createdObjectsReplication []uint16, maxCreateDomains uint16, maxGas uint64, gasCoin []byte, hash [32]byte, sig []byte, mutableRefs, readRefs []ObjectRefData) flatbuffers.UOffsetT {
	hashVec := builder.CreateByteVector(hash[:])
	sigVec := builder.CreateByteVector(sig)
	argsVec := builder.CreateByteVector(args)
	senderVec := builder.CreateByteVector(sender)
	podVec := builder.CreateByteVector(pod[:])
	funcNameOff := builder.CreateString(funcName)

	mutRefsVec := buildObjectRefDataVector(builder, mutableRefs, true)
	readRefsVec := buildObjectRefDataVector(builder, readRefs, false)

	corVec := buildCreatedObjectsReplicationVector(builder, createdObjectsReplication)

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
	types.TransactionAddMaxCreateDomains(builder, maxCreateDomains)
	types.TransactionAddMaxGas(builder, maxGas)

	if corVec != 0 {
		types.TransactionAddCreatedObjectsReplication(builder, corVec)
	}

	if gasCoinVec != 0 {
		types.TransactionAddGasCoin(builder, gasCoinVec)
	}

	if mutRefsVec != 0 {
		types.TransactionAddMutableRefs(builder, mutRefsVec)
	}

	if readRefsVec != 0 {
		types.TransactionAddReadRefs(builder, readRefsVec)
	}

	return types.TransactionEnd(builder)
}

// buildCreatedObjectsReplicationVector builds the [uint16] vector for created objects replication.
func buildCreatedObjectsReplicationVector(builder *flatbuffers.Builder, replication []uint16) flatbuffers.UOffsetT {
	if len(replication) == 0 {
		return 0
	}

	types.TransactionStartCreatedObjectsReplicationVector(builder, len(replication))
	for i := len(replication) - 1; i >= 0; i-- {
		builder.PrependUint16(replication[i])
	}

	return builder.EndVector(len(replication))
}

// buildObjectRefDataVector builds an ObjectRef vector from ObjectRefData slices.
func buildObjectRefDataVector(builder *flatbuffers.Builder, refs []ObjectRefData, mutable bool) flatbuffers.UOffsetT {
	if len(refs) == 0 {
		return 0
	}

	offsets := make([]flatbuffers.UOffsetT, len(refs))

	for i, ref := range refs {
		offsets[i] = buildSingleObjectRef(builder, ref)
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

// buildSingleObjectRef builds a single ObjectRef table in the builder.
func buildSingleObjectRef(builder *flatbuffers.Builder, ref ObjectRefData) flatbuffers.UOffsetT {
	var idVec flatbuffers.UOffsetT
	if ref.ID != [32]byte{} {
		idVec = builder.CreateByteVector(ref.ID[:])
	}

	var domainOff flatbuffers.UOffsetT
	if ref.Domain != "" {
		domainOff = builder.CreateString(ref.Domain)
	}

	types.ObjectRefStart(builder)

	if idVec != 0 {
		types.ObjectRefAddId(builder, idVec)
	}

	types.ObjectRefAddVersion(builder, ref.Version)

	if domainOff != 0 {
		types.ObjectRefAddDomain(builder, domainOff)
	}

	return types.ObjectRefEnd(builder)
}
