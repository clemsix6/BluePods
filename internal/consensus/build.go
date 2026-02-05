package consensus

import (
	"crypto/ed25519"

	flatbuffers "github.com/google/flatbuffers/go"

	"BluePods/internal/types"
)

// buildVertex creates a new vertex with the given parameters.
func (d *DAG) buildVertex(round uint64, parents []Hash, txs [][]byte) []byte {
	builder := flatbuffers.NewBuilder(4096 + len(txs)*1024)

	// Build unsigned vertex first (includes transactions for hash)
	unsigned := d.buildUnsignedVertex(builder, round, parents, txs)

	// Compute hash and signature
	hash := hashVertex(unsigned)
	sig := ed25519.Sign(d.privKey, hash[:])

	// Rebuild with hash and signature
	builder.Reset()

	return d.buildSignedVertex(builder, round, parents, txs, hash, sig)
}

// buildUnsignedVertex creates a vertex without hash and signature.
func (d *DAG) buildUnsignedVertex(builder *flatbuffers.Builder, round uint64, parents []Hash, txs [][]byte) []byte {
	txsVec := d.buildTxVector(builder, txs)
	parentsVec := d.buildParentsVector(builder, parents)
	producerVec := builder.CreateByteVector(d.pubKey[:])

	types.VertexStart(builder)
	types.VertexAddRound(builder, round)
	types.VertexAddProducer(builder, producerVec)
	types.VertexAddParents(builder, parentsVec)
	types.VertexAddTransactions(builder, txsVec)
	types.VertexAddEpoch(builder, d.epoch)

	vertexOffset := types.VertexEnd(builder)
	builder.Finish(vertexOffset)

	return builder.FinishedBytes()
}

// buildSignedVertex creates a complete vertex with hash and signature.
func (d *DAG) buildSignedVertex(builder *flatbuffers.Builder, round uint64, parents []Hash, txs [][]byte, hash Hash, sig []byte) []byte {
	txsVec := d.buildTxVector(builder, txs)
	hashVec := builder.CreateByteVector(hash[:])
	sigVec := builder.CreateByteVector(sig)
	producerVec := builder.CreateByteVector(d.pubKey[:])
	parentsVec := d.buildParentsVector(builder, parents)

	types.VertexStart(builder)
	types.VertexAddHash(builder, hashVec)
	types.VertexAddRound(builder, round)
	types.VertexAddProducer(builder, producerVec)
	types.VertexAddSignature(builder, sigVec)
	types.VertexAddParents(builder, parentsVec)
	types.VertexAddTransactions(builder, txsVec)
	types.VertexAddEpoch(builder, d.epoch)

	vertexOffset := types.VertexEnd(builder)
	builder.Finish(vertexOffset)

	return builder.FinishedBytes()
}

// buildParentsVector creates the parents vector for a vertex.
func (d *DAG) buildParentsVector(builder *flatbuffers.Builder, parents []Hash) flatbuffers.UOffsetT {
	parentOffsets := make([]flatbuffers.UOffsetT, len(parents))
	for i, p := range parents {
		parentOffsets[i] = d.buildVertexLink(builder, p)
	}

	types.VertexStartParentsVector(builder, len(parentOffsets))
	for i := len(parentOffsets) - 1; i >= 0; i-- {
		builder.PrependUOffsetT(parentOffsets[i])
	}

	return builder.EndVector(len(parentOffsets))
}

// buildVertexLink creates a VertexLink flatbuffer.
func (d *DAG) buildVertexLink(builder *flatbuffers.Builder, hash Hash) flatbuffers.UOffsetT {
	hashVec := builder.CreateByteVector(hash[:])
	producerVec := d.getProducerForLink(builder, hash)

	types.VertexLinkStart(builder)
	types.VertexLinkAddHash(builder, hashVec)
	types.VertexLinkAddProducer(builder, producerVec)

	return types.VertexLinkEnd(builder)
}

// getProducerForLink gets the producer pubkey for a parent link.
func (d *DAG) getProducerForLink(builder *flatbuffers.Builder, hash Hash) flatbuffers.UOffsetT {
	v := d.store.get(hash)
	if v != nil {
		return builder.CreateByteVector(v.ProducerBytes())
	}

	return builder.CreateByteVector(make([]byte, 32))
}

// buildTxVector creates the transactions vector from AttestedTransaction bytes.
// Each tx in txs is a serialized AttestedTransaction that gets rebuilt in the builder.
// Invalid transactions are skipped.
func (d *DAG) buildTxVector(builder *flatbuffers.Builder, txs [][]byte) flatbuffers.UOffsetT {
	if len(txs) == 0 {
		types.VertexStartTransactionsVector(builder, 0)
		return builder.EndVector(0)
	}

	offsets := make([]flatbuffers.UOffsetT, 0, len(txs))
	for _, txBytes := range txs {
		offset, ok := d.tryRebuildAttestedTx(builder, txBytes)
		if ok {
			offsets = append(offsets, offset)
		}
	}

	types.VertexStartTransactionsVector(builder, len(offsets))
	for i := len(offsets) - 1; i >= 0; i-- {
		builder.PrependUOffsetT(offsets[i])
	}

	return builder.EndVector(len(offsets))
}

// tryRebuildAttestedTx parses an AttestedTransaction and rebuilds it in the builder.
// Returns (offset, true) on success, (0, false) if data is invalid.
func (d *DAG) tryRebuildAttestedTx(builder *flatbuffers.Builder, data []byte) (flatbuffers.UOffsetT, bool) {
	if len(data) < 8 {
		return 0, false
	}

	atx := types.GetRootAsAttestedTransaction(data, 0)
	if atx.Transaction(nil) == nil {
		return 0, false
	}

	// Rebuild inner Transaction
	txOffset := d.rebuildTransaction(builder, atx.Transaction(nil))

	// Rebuild Objects vector
	objOffsets := make([]flatbuffers.UOffsetT, atx.ObjectsLength())
	for i := 0; i < atx.ObjectsLength(); i++ {
		var obj types.Object
		atx.Objects(&obj, i)
		objOffsets[i] = d.rebuildObject(builder, &obj)
	}

	types.AttestedTransactionStartObjectsVector(builder, len(objOffsets))
	for i := len(objOffsets) - 1; i >= 0; i-- {
		builder.PrependUOffsetT(objOffsets[i])
	}
	objectsVec := builder.EndVector(len(objOffsets))

	// Rebuild Proofs vector
	proofOffsets := make([]flatbuffers.UOffsetT, atx.ProofsLength())
	for i := 0; i < atx.ProofsLength(); i++ {
		var proof types.QuorumProof
		atx.Proofs(&proof, i)
		proofOffsets[i] = d.rebuildQuorumProof(builder, &proof)
	}

	types.AttestedTransactionStartProofsVector(builder, len(proofOffsets))
	for i := len(proofOffsets) - 1; i >= 0; i-- {
		builder.PrependUOffsetT(proofOffsets[i])
	}
	proofsVec := builder.EndVector(len(proofOffsets))

	types.AttestedTransactionStart(builder)
	types.AttestedTransactionAddTransaction(builder, txOffset)
	types.AttestedTransactionAddObjects(builder, objectsVec)
	types.AttestedTransactionAddProofs(builder, proofsVec)

	return types.AttestedTransactionEnd(builder), true
}

// rebuildTransaction rebuilds a Transaction in the builder.
func (d *DAG) rebuildTransaction(builder *flatbuffers.Builder, tx *types.Transaction) flatbuffers.UOffsetT {
	if tx == nil {
		types.TransactionStart(builder)
		return types.TransactionEnd(builder)
	}

	hashVec := builder.CreateByteVector(tx.HashBytes())
	readObjVec := builder.CreateByteVector(tx.ReadObjectsBytes())
	mutObjVec := builder.CreateByteVector(tx.MutableObjectsBytes())
	senderVec := builder.CreateByteVector(tx.SenderBytes())
	sigVec := builder.CreateByteVector(tx.SignatureBytes())
	podVec := builder.CreateByteVector(tx.PodBytes())
	funcNameOff := builder.CreateString(string(tx.FunctionName()))
	argsVec := builder.CreateByteVector(tx.ArgsBytes())

	types.TransactionStart(builder)
	types.TransactionAddHash(builder, hashVec)
	types.TransactionAddReadObjects(builder, readObjVec)
	types.TransactionAddMutableObjects(builder, mutObjVec)
	types.TransactionAddCreatesObjects(builder, tx.CreatesObjects())
	types.TransactionAddSender(builder, senderVec)
	types.TransactionAddSignature(builder, sigVec)
	types.TransactionAddPod(builder, podVec)
	types.TransactionAddFunctionName(builder, funcNameOff)
	types.TransactionAddArgs(builder, argsVec)

	return types.TransactionEnd(builder)
}

// rebuildObject rebuilds an Object in the builder.
func (d *DAG) rebuildObject(builder *flatbuffers.Builder, obj *types.Object) flatbuffers.UOffsetT {
	idVec := builder.CreateByteVector(obj.IdBytes())
	ownerVec := builder.CreateByteVector(obj.OwnerBytes())
	contentVec := builder.CreateByteVector(obj.ContentBytes())

	types.ObjectStart(builder)
	types.ObjectAddId(builder, idVec)
	types.ObjectAddVersion(builder, obj.Version())
	types.ObjectAddOwner(builder, ownerVec)
	types.ObjectAddReplication(builder, obj.Replication())
	types.ObjectAddContent(builder, contentVec)

	return types.ObjectEnd(builder)
}

// rebuildQuorumProof rebuilds a QuorumProof in the builder.
func (d *DAG) rebuildQuorumProof(builder *flatbuffers.Builder, proof *types.QuorumProof) flatbuffers.UOffsetT {
	objIdVec := builder.CreateByteVector(proof.ObjectIdBytes())
	blsSigVec := builder.CreateByteVector(proof.BlsSignatureBytes())
	bitmapVec := builder.CreateByteVector(proof.SignerBitmapBytes())

	types.QuorumProofStart(builder)
	types.QuorumProofAddObjectId(builder, objIdVec)
	types.QuorumProofAddBlsSignature(builder, blsSigVec)
	types.QuorumProofAddSignerBitmap(builder, bitmapVec)

	return types.QuorumProofEnd(builder)
}
