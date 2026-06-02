package daemon

import (
	flatbuffers "github.com/google/flatbuffers/go"

	"BluePods/internal/types"
)

// rebuildTx rebuilds a Transaction table in the given builder. It mirrors the
// node's rebuild so the daemon stays free of any heavy package; the daemon
// shares with the node only the pure wire types.
func rebuildTx(builder *flatbuffers.Builder, tx *types.Transaction) flatbuffers.UOffsetT {
	hashVec := builder.CreateByteVector(tx.HashBytes())
	sigVec := builder.CreateByteVector(tx.SignatureBytes())
	argsVec := builder.CreateByteVector(tx.ArgsBytes())
	senderVec := builder.CreateByteVector(tx.SenderBytes())
	podVec := builder.CreateByteVector(tx.PodBytes())
	funcNameOff := builder.CreateString(string(tx.FunctionName()))

	readRefsVec := rebuildRefs(builder, tx, false)
	mutRefsVec := rebuildRefs(builder, tx, true)
	corVec := rebuildCreatedReplication(builder, tx)

	var gasCoinVec flatbuffers.UOffsetT
	if gc := tx.GasCoinBytes(); len(gc) > 0 {
		gasCoinVec = builder.CreateByteVector(gc)
	}

	// Carry the sponsorship fields (absent for a self-paid tx) so a sponsored
	// transaction whose ATX is assembled here keeps its fee_payer binding and
	// sponsor signature; otherwise the recomputed body hash would no longer match
	// the declared hash and the tx would be rejected at commit.
	var feePayerVec flatbuffers.UOffsetT
	if fp := tx.FeePayerBytes(); len(fp) > 0 {
		feePayerVec = builder.CreateByteVector(fp)
	}

	var sponsorSigVec flatbuffers.UOffsetT
	if ss := tx.SponsorSignatureBytes(); len(ss) > 0 {
		sponsorSigVec = builder.CreateByteVector(ss)
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

	return types.TransactionEnd(builder)
}

// rebuildRefs rebuilds the mutable_refs or read_refs vector of a transaction.
func rebuildRefs(builder *flatbuffers.Builder, tx *types.Transaction, mutable bool) flatbuffers.UOffsetT {
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
		offsets[i] = rebuildRef(builder, &ref)
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

// rebuildRef rebuilds a single ObjectRef in the builder.
func rebuildRef(builder *flatbuffers.Builder, ref *types.ObjectRef) flatbuffers.UOffsetT {
	var idVec flatbuffers.UOffsetT
	if id := ref.IdBytes(); len(id) > 0 {
		idVec = builder.CreateByteVector(id)
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

// rebuildCreatedReplication rebuilds the created_objects_replication vector.
func rebuildCreatedReplication(builder *flatbuffers.Builder, tx *types.Transaction) flatbuffers.UOffsetT {
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
