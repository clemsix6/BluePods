package consensus

import (
	"crypto/ed25519"
	"time"

	flatbuffers "github.com/google/flatbuffers/go"

	"BluePods/internal/genesis"
	"BluePods/internal/logger"
	"BluePods/internal/types"
)

// vertexParts is the material both build passes assemble a vertex from. The
// timestamp and epoch are read once and threaded into BOTH passes with the
// identical value, so the signed fields match exactly what was hashed and signed.
type vertexParts struct {
	round         uint64   // round is the DAG round being produced
	epoch         uint64   // epoch is the producer's live epoch at production
	timestamp     uint64   // timestamp is the producer's local wall-clock (Unix nanoseconds)
	frontierRound uint64   // frontierRound is the committed round indexRoot anchors
	indexRoot     Hash     // indexRoot is the verifiable index root at frontierRound
	parents       []Hash   // parents are the round-1 vertices this vertex references
	txs           [][]byte // txs are the serialized AttestedTransactions to include
	bodyHash      Hash     // bodyHash is the body commitment (second pass only)
	hash          Hash     // hash is the header hash, the vertex identity (second pass only)
	sig           []byte   // sig is the Ed25519 signature over hash (second pass only)
}

// buildVertex creates a new vertex with the given parameters. The producer signs
// the HEADER hash: the body is hashed once into bodyHash and the identity is
// BLAKE3 over {producer, round, epoch, frontier_round, index_root, bodyHash}, so
// the signature can be checked against the 120-byte header alone.
func (d *DAG) buildVertex(round uint64, parents []Hash, txs [][]byte) []byte {
	builder := flatbuffers.NewBuilder(4096 + len(txs)*1024)

	frontierRound, indexRoot := d.committedFrontier()

	parts := vertexParts{
		round:         round,
		epoch:         d.productionEpoch(),
		timestamp:     uint64(time.Now().UnixNano()),
		frontierRound: frontierRound,
		indexRoot:     indexRoot,
		parents:       parents,
		txs:           txs,
	}

	// Build the unsigned vertex first: its body is what bodyHash commits to.
	unsigned := d.buildUnsignedVertex(builder, parts)

	parts.hash, parts.bodyHash = vertexIdentity(types.GetRootAsVertex(unsigned, 0))
	parts.sig = ed25519.Sign(d.privKey, parts.hash[:])

	// Rebuild with the header hash, body hash and signature.
	builder.Reset()

	return d.buildSignedVertex(builder, parts)
}

// committedFrontier returns the (frontier_round, index_root) pair a vertex
// produced now anchors: the indexer's most recently committed frontier, read
// as one atomic pair through the CommittedFrontier seam so a commit landing
// between two separate reads can never pair one round with another's root
// (a torn anchor stage-1 validation rejects network-wide). Zero values when
// no indexer is wired — tests, tools, and any DAG built before the index
// existed produce vertices exactly as they did before this field existed.
func (d *DAG) committedFrontier() (frontierRound uint64, indexRoot Hash) {
	if d.indexer == nil {
		return 0, Hash{}
	}

	return d.indexer.CommittedFrontier()
}

// productionEpoch returns the epoch a vertex produced now is stamped with: the
// LIVE epoch the commit path maintains, read from its lock-free mirror. The
// construction-time epoch field is a vestigial hint (every node is built with 0)
// and must never be used here — a header claiming epoch 0 forever would name the
// wrong validator tree for every quorum weighed against it.
//
// The mirror exists so this read does NOT take commitMu: production runs on the
// client-request goroutine (SubmitTx -> tryProduceVertex), and a lock there would
// put a full commit batch on the submit latency of every transaction.
func (d *DAG) productionEpoch() uint64 {
	return d.liveEpoch.Load()
}

// buildUnsignedVertex creates a vertex without hash, body hash and signature. Its
// body (parents, transactions, fee summary, timestamp) is what bodyHash is
// computed over; the header fields it carries are the ones the identity folds in.
func (d *DAG) buildUnsignedVertex(builder *flatbuffers.Builder, parts vertexParts) []byte {
	txsVec := d.buildTxVector(builder, parts.txs)
	feeSummaryOff := d.buildFeeSummary(builder, parts.txs)
	parentsVec := d.buildParentsVector(builder, parts.parents)
	producerVec := builder.CreateByteVector(d.pubKey[:])
	indexRootVec := builder.CreateByteVector(parts.indexRoot[:])

	types.VertexStart(builder)
	types.VertexAddRound(builder, parts.round)
	types.VertexAddProducer(builder, producerVec)
	types.VertexAddParents(builder, parentsVec)
	types.VertexAddTransactions(builder, txsVec)
	types.VertexAddEpoch(builder, parts.epoch)
	types.VertexAddFeeSummary(builder, feeSummaryOff)
	types.VertexAddTimestamp(builder, parts.timestamp)
	types.VertexAddFrontierRound(builder, parts.frontierRound)
	types.VertexAddIndexRoot(builder, indexRootVec)

	vertexOffset := types.VertexEnd(builder)
	builder.Finish(vertexOffset)

	return builder.FinishedBytes()
}

// buildSignedVertex creates a complete vertex with its header hash, body hash and
// signature. Every other field must carry the identical value passed to
// buildUnsignedVertex, or the vertex no longer matches what was hashed and signed.
func (d *DAG) buildSignedVertex(builder *flatbuffers.Builder, parts vertexParts) []byte {
	txsVec := d.buildTxVector(builder, parts.txs)
	feeSummaryOff := d.buildFeeSummary(builder, parts.txs)
	hashVec := builder.CreateByteVector(parts.hash[:])
	sigVec := builder.CreateByteVector(parts.sig)
	producerVec := builder.CreateByteVector(d.pubKey[:])
	parentsVec := d.buildParentsVector(builder, parts.parents)
	indexRootVec := builder.CreateByteVector(parts.indexRoot[:])
	bodyHashVec := builder.CreateByteVector(parts.bodyHash[:])

	types.VertexStart(builder)
	types.VertexAddHash(builder, hashVec)
	types.VertexAddRound(builder, parts.round)
	types.VertexAddProducer(builder, producerVec)
	types.VertexAddSignature(builder, sigVec)
	types.VertexAddParents(builder, parentsVec)
	types.VertexAddTransactions(builder, txsVec)
	types.VertexAddEpoch(builder, parts.epoch)
	types.VertexAddFeeSummary(builder, feeSummaryOff)
	types.VertexAddTimestamp(builder, parts.timestamp)
	types.VertexAddFrontierRound(builder, parts.frontierRound)
	types.VertexAddIndexRoot(builder, indexRootVec)
	types.VertexAddBodyHash(builder, bodyHashVec)

	vertexOffset := types.VertexEnd(builder)
	builder.Finish(vertexOffset)

	return builder.FinishedBytes()
}

// buildFeeSummary computes and builds the FeeSummary for a set of transactions.
// Returns the FlatBuffers offset. If fees are disabled, returns an empty summary.
func (d *DAG) buildFeeSummary(builder *flatbuffers.Builder, txs [][]byte) flatbuffers.UOffsetT {
	var totalFees, totalBurned, totalEpoch uint64

	if d.feeParams != nil {
		for _, txBytes := range txs {
			split := d.computeTxFeeSplit(txBytes)
			totalFees += split.Total
			totalBurned += split.Burned
			totalEpoch += split.Epoch
		}
	}

	types.FeeSummaryStart(builder)
	types.FeeSummaryAddTotalFees(builder, totalFees)
	types.FeeSummaryAddTotalBurned(builder, totalBurned)
	types.FeeSummaryAddTotalEpoch(builder, totalEpoch)

	return types.FeeSummaryEnd(builder)
}

// computeTxFeeSplit calculates the fee split for a single AttestedTransaction.
// The summary covers only the consumed portion (compute+transit+domain): the
// storage deposit is locked in the object, never pooled, so it is not summarized.
func (d *DAG) computeTxFeeSplit(txBytes []byte) FeeSplit {
	if len(txBytes) < 8 {
		return FeeSplit{}
	}

	atx := types.GetRootAsAttestedTransaction(txBytes, 0)
	tx := atx.Transaction(nil)
	if tx == nil {
		return FeeSplit{}
	}

	// Skip if no gas_coin (genesis/bootstrap tx)
	if len(tx.GasCoinBytes()) != 32 {
		return FeeSplit{}
	}

	consumed, _ := d.calculateTxFeeSplit(tx, atx)

	return SplitFee(consumed, *d.feeParams)
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
// Uses defer/recover to handle FlatBuffer panics on corrupted data.
func (d *DAG) tryRebuildAttestedTx(builder *flatbuffers.Builder, data []byte) (offset flatbuffers.UOffsetT, ok bool) {
	defer func() {
		if r := recover(); r != nil {
			logger.Warn("malformed ATX skipped", "panic", r)
			offset = 0
			ok = false
		}
	}()

	if len(data) < 8 {
		return 0, false
	}

	atx := types.GetRootAsAttestedTransaction(data, 0)
	if atx.Transaction(nil) == nil {
		return 0, false
	}

	return rebuildAttestedTx(builder, atx), true
}

// rebuildAttestedTx re-serializes a parsed AttestedTransaction into the builder
// and returns its offset. It is the single canonical rewrap: vertex production
// runs it over submitted transactions, and the body hash runs it over the parsed
// vertex, so producer and receiver serialize identical content identically.
func rebuildAttestedTx(builder *flatbuffers.Builder, atx *types.AttestedTransaction) flatbuffers.UOffsetT {
	// Rebuild inner Transaction
	txOffset := rebuildTransaction(builder, atx.Transaction(nil))

	// Rebuild Objects vector
	objOffsets := make([]flatbuffers.UOffsetT, atx.ObjectsLength())
	for i := 0; i < atx.ObjectsLength(); i++ {
		var obj types.Object
		atx.Objects(&obj, i)
		objOffsets[i] = rebuildObject(builder, &obj)
	}

	objectsVec := endOffsetVector(builder, objOffsets, types.AttestedTransactionStartObjectsVector)

	// Rebuild Proofs vector
	proofOffsets := make([]flatbuffers.UOffsetT, atx.ProofsLength())
	for i := 0; i < atx.ProofsLength(); i++ {
		var proof types.QuorumProof
		atx.Proofs(&proof, i)
		proofOffsets[i] = rebuildQuorumProof(builder, &proof)
	}

	proofsVec := endOffsetVector(builder, proofOffsets, types.AttestedTransactionStartProofsVector)

	types.AttestedTransactionStart(builder)
	types.AttestedTransactionAddTransaction(builder, txOffset)
	types.AttestedTransactionAddObjects(builder, objectsVec)
	types.AttestedTransactionAddProofs(builder, proofsVec)
	types.AttestedTransactionAddAttestationEpoch(builder, atx.AttestationEpoch())

	return types.AttestedTransactionEnd(builder)
}

// serializeAttestedTx re-serializes a parsed AttestedTransaction as a standalone
// buffer, which the execute path needs because atx.Table().Bytes returns the whole
// parent vertex buffer. It runs the SAME rebuild the body hash runs, so what a pod
// executes is byte-for-byte what the producer's signature covers.
func serializeAttestedTx(atx *types.AttestedTransaction) []byte {
	builder := flatbuffers.NewBuilder(1024)
	builder.Finish(rebuildAttestedTx(builder, atx))

	return builder.FinishedBytes()
}

// rebuildTransaction rebuilds a Transaction in the builder.
func rebuildTransaction(builder *flatbuffers.Builder, tx *types.Transaction) flatbuffers.UOffsetT {
	if tx == nil {
		types.TransactionStart(builder)
		return types.TransactionEnd(builder)
	}

	return genesis.RebuildTxInBuilder(builder, tx)
}

// rebuildObject rebuilds an Object in the builder. Every field the schema
// declares is written: a field left out here is covered by neither the body hash
// nor the BLS quorum proof (which signs content||version||owner only), so a
// relaying peer could rewrite it and the vertex would keep its identity and its
// signature. TestVertexIdentity_MutationMatrixMatchesSchema holds this in lockstep
// with types/object.fbs.
func rebuildObject(builder *flatbuffers.Builder, obj *types.Object) flatbuffers.UOffsetT {
	idVec := builder.CreateByteVector(obj.IdBytes())
	ownerVec := builder.CreateByteVector(obj.OwnerBytes())
	contentVec := builder.CreateByteVector(obj.ContentBytes())

	types.ObjectStart(builder)
	types.ObjectAddId(builder, idVec)
	types.ObjectAddVersion(builder, obj.Version())
	types.ObjectAddOwner(builder, ownerVec)
	types.ObjectAddReplication(builder, obj.Replication())
	types.ObjectAddContent(builder, contentVec)
	types.ObjectAddFees(builder, obj.Fees())
	types.ObjectAddParentKind(builder, obj.ParentKind())

	return types.ObjectEnd(builder)
}

// rebuildQuorumProof rebuilds a QuorumProof in the builder.
func rebuildQuorumProof(builder *flatbuffers.Builder, proof *types.QuorumProof) flatbuffers.UOffsetT {
	objIdVec := builder.CreateByteVector(proof.ObjectIdBytes())
	blsSigVec := builder.CreateByteVector(proof.BlsSignatureBytes())
	bitmapVec := builder.CreateByteVector(proof.SignerBitmapBytes())

	types.QuorumProofStart(builder)
	types.QuorumProofAddObjectId(builder, objIdVec)
	types.QuorumProofAddBlsSignature(builder, blsSigVec)
	types.QuorumProofAddSignerBitmap(builder, bitmapVec)

	return types.QuorumProofEnd(builder)
}
