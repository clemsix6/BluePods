package genesis

import (
	"crypto/ed25519"

	flatbuffers "github.com/google/flatbuffers/go"
	"github.com/zeebo/blake3"

	"BluePods/internal/types"
)

// faucetSplitMaxGas is the gas budget for a faucet split. It must exceed the
// fee system's minimum gas; the small surplus covers the compute fee for a
// single-output split from the reserve coin.
const faucetSplitMaxGas uint64 = 1000

// BuildSplitTx creates a signed split transaction as an ATX that moves `amount`
// from the reserve coin to `toOwner`. The reserve coin is referenced both as a
// mutable ref (at its current version) and as the gas coin, so the faucet pays
// its own gas from the reserve. The new coin lands at created-object index 0.
func BuildSplitTx(privKey ed25519.PrivateKey, systemPod [32]byte, reserveCoinID [32]byte, reserveVersion uint64, toOwner [32]byte, amount uint64) []byte {
	args := EncodeSplitArgs(amount, toOwner)
	mutableRefs := []ObjectRefData{{ID: reserveCoinID, Version: reserveVersion}}

	pubKey := privKey.Public().(ed25519.PublicKey)
	unsignedBytes := BuildUnsignedTxBytesWithRefs(
		pubKey, systemPod, "split", args, []uint16{0}, 0, faucetSplitMaxGas, reserveCoinID[:], mutableRefs, nil,
	)
	hash := blake3.Sum256(unsignedBytes)
	sig := ed25519.Sign(privKey, hash[:])

	builder := flatbuffers.NewBuilder(1024)
	txOffset := BuildTxTableWithRefs(
		builder, pubKey, systemPod, "split", args, []uint16{0}, 0, faucetSplitMaxGas, reserveCoinID[:], hash, sig, mutableRefs, nil,
	)

	return finishAttestedTx(builder, txOffset)
}

// BuildSponsoredTx builds a doubly-signed sponsored transaction wrapped in an ATX.
// The sender and the sponsor both sign the SAME canonical body hash, which binds
// fee_payer (the sponsor's pubkey) and valid_until, so neither signature can be
// lifted onto a different body. The sponsor's coin pays the gas; the gas coin must
// be owned by the sponsor (enforced at commit). createdReps/mutableRefs/readRefs
// describe the operation exactly as a self-paid build would.
func BuildSponsoredTx(senderKey, sponsorKey ed25519.PrivateKey, pod [32]byte, funcName string, args []byte, createdReps []uint16, maxCreateDomains uint16, maxGas uint64, gasCoin [32]byte, validUntil uint64, mutableRefs, readRefs []ObjectRefData) []byte {
	senderPub := senderKey.Public().(ed25519.PublicKey)
	sponsorPub := sponsorKey.Public().(ed25519.PublicKey)

	sponsor := Sponsorship{FeePayer: sponsorPub, ValidUntil: validUntil}
	body := BuildUnsignedTxBytesSponsored(
		senderPub, pod, funcName, args, createdReps, maxCreateDomains, maxGas, gasCoin[:], mutableRefs, readRefs, sponsor, nil,
	)

	hash := blake3.Sum256(body)
	senderSig := ed25519.Sign(senderKey, hash[:])
	sponsorSig := ed25519.Sign(sponsorKey, hash[:])

	builder := flatbuffers.NewBuilder(1024)
	txOff := BuildTxTableSponsored(
		builder, senderPub, pod, funcName, args, createdReps, maxCreateDomains, maxGas, gasCoin[:], hash, senderSig, mutableRefs, readRefs, sponsor, sponsorSig, nil,
	)

	return finishAttestedTx(builder, txOff)
}

// finishAttestedTx wraps a built Transaction table in an AttestedTransaction with
// empty objects and proofs vectors and returns the finished bytes.
func finishAttestedTx(builder *flatbuffers.Builder, txOffset flatbuffers.UOffsetT) []byte {
	types.AttestedTransactionStartObjectsVector(builder, 0)
	objectsVec := builder.EndVector(0)

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

// BuildRegisterValidatorTx creates a signed register_validator transaction as ATX.
// Used for the genesis bootstrap path. Validators register only a QUIC address.
func BuildRegisterValidatorTx(privKey ed25519.PrivateKey, systemPod [32]byte, quicAddr string, blsPubkey []byte) []byte {
	args := encodeRegisterValidatorArgs([]byte(quicAddr), blsPubkey)
	return BuildAttestedTx(privKey, systemPod, "register_validator", args, []uint16{0}, 0, 0, nil)
}

// BuildRegisterValidatorRawTx creates a signed register_validator as a raw Transaction.
// Used by validators joining the network — the receiving node wraps it in ATX.
// rewardCoin optionally designates the coin the validator's liquid epoch reward
// is credited to; a zero value encodes no designation (setRewardCoinFromArgs
// then falls back to the transaction's declared gas coin, if any, else leaves it
// zero — never a live coin-store read).
func BuildRegisterValidatorRawTx(privKey ed25519.PrivateKey, systemPod [32]byte, quicAddr string, blsPubkey []byte, rewardCoin [32]byte) []byte {
	args := EncodeRegisterValidatorArgs([]byte(quicAddr), blsPubkey, rewardCoin)
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

	// Rebuild the sponsorship vectors (absent for a self-paid tx), so wrapping a
	// gossiped sponsored Transaction into an ATX preserves its fee_payer binding.
	var feePayerVec flatbuffers.UOffsetT
	if fpBytes := tx.FeePayerBytes(); len(fpBytes) > 0 {
		feePayerVec = builder.CreateByteVector(fpBytes)
	}

	var sponsorSigVec flatbuffers.UOffsetT
	if ssBytes := tx.SponsorSignatureBytes(); len(ssBytes) > 0 {
		sponsorSigVec = builder.CreateByteVector(ssBytes)
	}

	// Preserve the declared deletion set: the execute path re-serializes the ATX
	// before running the pod, so the holder-only content removal reads its declared
	// deleted objects from here. Dropping it would leave a holder's stored content
	// behind after the commit loop already released the object's deposit.
	deletedVec := buildDeletedObjectsVector(builder, tx.DeletedObjectsBytes())

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

	if feePayerVec != 0 {
		types.TransactionAddFeePayer(builder, feePayerVec)
	}

	if sponsorSigVec != 0 {
		types.TransactionAddSponsorSignature(builder, sponsorSigVec)
	}

	if vu := tx.ValidUntil(); vu != 0 {
		types.TransactionAddValidUntil(builder, vu)
	}

	if mutRefsVec != 0 {
		types.TransactionAddMutableRefs(builder, mutRefsVec)
	}

	if readRefsVec != 0 {
		types.TransactionAddReadRefs(builder, readRefsVec)
	}

	if deletedVec != 0 {
		types.TransactionAddDeletedObjects(builder, deletedVec)
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

// Sponsorship carries the optional fee-payer binding of a sponsored transaction.
// A zero value (empty FeePayer, zero ValidUntil) is a self-paid transaction and
// contributes nothing to the canonical body, so the encoding stays byte-identical.
type Sponsorship struct {
	FeePayer   []byte // FeePayer is the 32-byte sponsor pubkey paying the gas (empty if self-paid).
	ValidUntil uint64 // ValidUntil is the last epoch the sponsored tx may commit (0 if self-paid).
}

// BuildUnsignedTxBytesWithRefs creates transaction bytes with ObjectRef references.
// It is the canonical unsigned-body primitive for a self-paid transaction; the
// sponsored variant delegates here with its Sponsorship.
func BuildUnsignedTxBytesWithRefs(sender []byte, pod [32]byte, funcName string, args []byte, createdObjectsReplication []uint16, maxCreateDomains uint16, maxGas uint64, gasCoin []byte, mutableRefs, readRefs []ObjectRefData) []byte {
	return BuildUnsignedTxBytesSponsored(sender, pod, funcName, args, createdObjectsReplication, maxCreateDomains, maxGas, gasCoin, mutableRefs, readRefs, Sponsorship{}, nil)
}

// BuildUnsignedTxBytesSponsored builds the canonical unsigned transaction body,
// optionally binding a sponsor's fee_payer and valid_until. This is the single
// site that defines the body layout; ingress validation and the commit-time
// authenticity check both reconstruct against it, so the three never drift. The
// sponsorship fields follow the same absent-when-empty rule as gas_coin, so a
// self-paid transaction (empty Sponsorship) serializes byte-identically to before.
func BuildUnsignedTxBytesSponsored(sender []byte, pod [32]byte, funcName string, args []byte, createdObjectsReplication []uint16, maxCreateDomains uint16, maxGas uint64, gasCoin []byte, mutableRefs, readRefs []ObjectRefData, sponsor Sponsorship, deletedObjects []byte) []byte {
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

	var feePayerVec flatbuffers.UOffsetT
	if len(sponsor.FeePayer) > 0 {
		feePayerVec = builder.CreateByteVector(sponsor.FeePayer)
	}

	deletedVec := buildDeletedObjectsVector(builder, deletedObjects)

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

	if feePayerVec != 0 {
		types.TransactionAddFeePayer(builder, feePayerVec)
	}

	if sponsor.ValidUntil != 0 {
		types.TransactionAddValidUntil(builder, sponsor.ValidUntil)
	}

	if mutRefsVec != 0 {
		types.TransactionAddMutableRefs(builder, mutRefsVec)
	}

	if readRefsVec != 0 {
		types.TransactionAddReadRefs(builder, readRefsVec)
	}

	if deletedVec != 0 {
		types.TransactionAddDeletedObjects(builder, deletedVec)
	}

	txOff := types.TransactionEnd(builder)
	builder.Finish(txOff)

	return builder.FinishedBytes()
}

// buildDeletedObjectsVector builds the deleted_objects byte vector (concatenated
// 32-byte IDs), or returns 0 when empty so a transaction that declares no
// deletions serializes byte-identically to before the field existed.
func buildDeletedObjectsVector(builder *flatbuffers.Builder, deletedObjects []byte) flatbuffers.UOffsetT {
	if len(deletedObjects) == 0 {
		return 0
	}

	return builder.CreateByteVector(deletedObjects)
}

// BuildTxTableWithRefs builds a Transaction table with ObjectRef references in the given builder.
func BuildTxTableWithRefs(builder *flatbuffers.Builder, sender []byte, pod [32]byte, funcName string, args []byte, createdObjectsReplication []uint16, maxCreateDomains uint16, maxGas uint64, gasCoin []byte, hash [32]byte, sig []byte, mutableRefs, readRefs []ObjectRefData) flatbuffers.UOffsetT {
	return BuildTxTableSponsored(builder, sender, pod, funcName, args, createdObjectsReplication, maxCreateDomains, maxGas, gasCoin, hash, sig, mutableRefs, readRefs, Sponsorship{}, nil, nil)
}

// BuildTxTableSponsored builds a Transaction table, optionally carrying the
// sponsor's fee_payer / valid_until (from sponsor) and the sponsor's signature
// (sponsorSig) over the same body hash. Empty sponsorship and a nil sponsorSig
// reproduce the self-paid encoding byte-for-byte. The fee_payer / valid_until
// fields are written under the SAME absent-when-empty rule the unsigned body
// uses, so the stored body hashes back to the declared hash.
func BuildTxTableSponsored(builder *flatbuffers.Builder, sender []byte, pod [32]byte, funcName string, args []byte, createdObjectsReplication []uint16, maxCreateDomains uint16, maxGas uint64, gasCoin []byte, hash [32]byte, sig []byte, mutableRefs, readRefs []ObjectRefData, sponsor Sponsorship, sponsorSig []byte, deletedObjects []byte) flatbuffers.UOffsetT {
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

	var feePayerVec flatbuffers.UOffsetT
	if len(sponsor.FeePayer) > 0 {
		feePayerVec = builder.CreateByteVector(sponsor.FeePayer)
	}

	var sponsorSigVec flatbuffers.UOffsetT
	if len(sponsorSig) > 0 {
		sponsorSigVec = builder.CreateByteVector(sponsorSig)
	}

	deletedVec := buildDeletedObjectsVector(builder, deletedObjects)

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

	if feePayerVec != 0 {
		types.TransactionAddFeePayer(builder, feePayerVec)
	}

	if sponsorSigVec != 0 {
		types.TransactionAddSponsorSignature(builder, sponsorSigVec)
	}

	if sponsor.ValidUntil != 0 {
		types.TransactionAddValidUntil(builder, sponsor.ValidUntil)
	}

	if mutRefsVec != 0 {
		types.TransactionAddMutableRefs(builder, mutRefsVec)
	}

	if readRefsVec != 0 {
		types.TransactionAddReadRefs(builder, readRefsVec)
	}

	if deletedVec != 0 {
		types.TransactionAddDeletedObjects(builder, deletedVec)
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
