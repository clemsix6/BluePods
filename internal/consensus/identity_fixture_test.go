package consensus

import (
	"crypto/ed25519"
	"testing"

	flatbuffers "github.com/google/flatbuffers/go"

	"BluePods/internal/types"
)

// The fixture's fixed field values. Every one is non-default on purpose:
// FlatBuffers omits a scalar equal to its default from the buffer, and an omitted
// field cannot be mutated, so a zero here would silently skip a mutation case.
const (
	fixtureRound         = 25                  // fixtureRound is the round the fixture vertex claims
	fixtureEpoch         = 2                   // fixtureEpoch is the epoch the fixture header claims
	fixtureFrontierRound = 7                   // fixtureFrontierRound is the anchored committed round
	fixtureTimestamp     = 1700000000000000000 // fixtureTimestamp is the producer's wall-clock reading
	fixtureElements      = 2                   // fixtureElements is the length of every vector in the fixture
)

// buildFullVertex returns a signed vertex in which every field of every table the
// identity covers is present and non-default, with two elements in every vector.
// It is the fixture the per-field mutation matrix flips one field at a time in.
func buildFullVertex(t *testing.T, v testValidator) []byte {
	t.Helper()

	unsigned := writeFullVertex(flatbuffers.NewBuilder(4096), v.pubKey, vertexParts{
		round:     fixtureRound,
		epoch:     fixtureEpoch,
		timestamp: fixtureTimestamp,
	})

	parts := vertexParts{
		round:     fixtureRound,
		epoch:     fixtureEpoch,
		timestamp: fixtureTimestamp,
	}

	parts.hash, parts.bodyHash = vertexIdentity(types.GetRootAsVertex(unsigned, 0))
	parts.sig = ed25519.Sign(v.privKey, parts.hash[:])

	return writeFullVertex(flatbuffers.NewBuilder(4096), v.pubKey, parts)
}

// writeFullVertex writes every Vertex field into the builder and returns the
// finished buffer. The hash, body hash and signature are written only when parts
// carries them, so the same writer produces the unsigned pass the identity is
// derived from and the signed vertex that ships.
func writeFullVertex(builder *flatbuffers.Builder, producer Hash, parts vertexParts) []byte {
	txsVec := writeFixtureTxVector(builder)
	parentsVec := writeFixtureParents(builder)
	feeSummaryOff := writeFixtureFeeSummary(builder)

	producerVec := builder.CreateByteVector(producer[:])
	indexRootVec := builder.CreateByteVector(fixtureBytes(0xA0, 32))

	var hashVec, sigVec, bodyHashVec flatbuffers.UOffsetT
	if parts.sig != nil {
		hashVec = builder.CreateByteVector(parts.hash[:])
		sigVec = builder.CreateByteVector(parts.sig)
		bodyHashVec = builder.CreateByteVector(parts.bodyHash[:])
	}

	types.VertexStart(builder)
	types.VertexAddRound(builder, parts.round)
	types.VertexAddProducer(builder, producerVec)
	types.VertexAddParents(builder, parentsVec)
	types.VertexAddTransactions(builder, txsVec)
	types.VertexAddEpoch(builder, parts.epoch)
	types.VertexAddFeeSummary(builder, feeSummaryOff)
	types.VertexAddTimestamp(builder, parts.timestamp)
	types.VertexAddFrontierRound(builder, fixtureFrontierRound)
	types.VertexAddIndexRoot(builder, indexRootVec)

	if parts.sig != nil {
		types.VertexAddHash(builder, hashVec)
		types.VertexAddSignature(builder, sigVec)
		types.VertexAddBodyHash(builder, bodyHashVec)
	}

	builder.Finish(types.VertexEnd(builder))

	return builder.FinishedBytes()
}

// writeFixtureParents writes the fixture's parent links and returns the vector.
func writeFixtureParents(builder *flatbuffers.Builder) flatbuffers.UOffsetT {
	offsets := make([]flatbuffers.UOffsetT, fixtureElements)

	for i := range offsets {
		hashVec := builder.CreateByteVector(fixtureBytes(byte(0x11+i), 32))
		producerVec := builder.CreateByteVector(fixtureBytes(byte(0x21+i), 32))

		types.VertexLinkStart(builder)
		types.VertexLinkAddHash(builder, hashVec)
		types.VertexLinkAddProducer(builder, producerVec)
		offsets[i] = types.VertexLinkEnd(builder)
	}

	return endOffsetVector(builder, offsets, types.VertexStartParentsVector)
}

// writeFixtureFeeSummary writes the fixture's fee summary and returns its offset.
func writeFixtureFeeSummary(builder *flatbuffers.Builder) flatbuffers.UOffsetT {
	types.FeeSummaryStart(builder)
	types.FeeSummaryAddTotalFees(builder, 111)
	types.FeeSummaryAddTotalBurned(builder, 222)
	types.FeeSummaryAddTotalEpoch(builder, 333)

	return types.FeeSummaryEnd(builder)
}

// writeFixtureTxVector writes the fixture's attested transactions and returns the
// vector.
func writeFixtureTxVector(builder *flatbuffers.Builder) flatbuffers.UOffsetT {
	offsets := make([]flatbuffers.UOffsetT, fixtureElements)
	for i := range offsets {
		offsets[i] = writeFixtureATX(builder, i)
	}

	return endOffsetVector(builder, offsets, types.VertexStartTransactionsVector)
}

// writeFixtureATX writes one attested transaction, objects and proofs included.
func writeFixtureATX(builder *flatbuffers.Builder, n int) flatbuffers.UOffsetT {
	txOff := writeFixtureTx(builder, n)

	objOffsets := make([]flatbuffers.UOffsetT, fixtureElements)
	for i := range objOffsets {
		objOffsets[i] = writeFixtureObject(builder, i)
	}

	objectsVec := endOffsetVector(builder, objOffsets, types.AttestedTransactionStartObjectsVector)

	proofOffsets := make([]flatbuffers.UOffsetT, fixtureElements)
	for i := range proofOffsets {
		proofOffsets[i] = writeFixtureProof(builder, i)
	}

	proofsVec := endOffsetVector(builder, proofOffsets, types.AttestedTransactionStartProofsVector)

	types.AttestedTransactionStart(builder)
	types.AttestedTransactionAddTransaction(builder, txOff)
	types.AttestedTransactionAddObjects(builder, objectsVec)
	types.AttestedTransactionAddProofs(builder, proofsVec)
	types.AttestedTransactionAddAttestationEpoch(builder, uint64(3+n))

	return types.AttestedTransactionEnd(builder)
}

// writeFixtureObject writes one object with every field of the table set.
func writeFixtureObject(builder *flatbuffers.Builder, n int) flatbuffers.UOffsetT {
	idVec := builder.CreateByteVector(fixtureBytes(byte(0x31+n), 32))
	ownerVec := builder.CreateByteVector(fixtureBytes(byte(0x41+n), 32))
	contentVec := builder.CreateByteVector(fixtureBytes(byte(0x51+n), 16))

	types.ObjectStart(builder)
	types.ObjectAddId(builder, idVec)
	types.ObjectAddVersion(builder, uint64(7+n))
	types.ObjectAddOwner(builder, ownerVec)
	types.ObjectAddReplication(builder, uint16(10+n))
	types.ObjectAddContent(builder, contentVec)
	types.ObjectAddFees(builder, uint64(500+n))
	types.ObjectAddParentKind(builder, 1)

	return types.ObjectEnd(builder)
}

// writeFixtureProof writes one quorum proof with every field of the table set.
func writeFixtureProof(builder *flatbuffers.Builder, n int) flatbuffers.UOffsetT {
	objIDVec := builder.CreateByteVector(fixtureBytes(byte(0x61+n), 32))
	blsSigVec := builder.CreateByteVector(fixtureBytes(byte(0x71+n), 96))
	bitmapVec := builder.CreateByteVector(fixtureBytes(byte(0x81+n), 4))

	types.QuorumProofStart(builder)
	types.QuorumProofAddObjectId(builder, objIDVec)
	types.QuorumProofAddBlsSignature(builder, blsSigVec)
	types.QuorumProofAddSignerBitmap(builder, bitmapVec)

	return types.QuorumProofEnd(builder)
}

// writeFixtureTx writes the inner transaction of a fixture ATX. Only the fields
// the mutation matrix reaches are set: the Transaction table's own field coverage
// belongs to genesis.RebuildTxInBuilder, which canonically rebuilds it.
func writeFixtureTx(builder *flatbuffers.Builder, n int) flatbuffers.UOffsetT {
	hashVec := builder.CreateByteVector(fixtureBytes(byte(0x91+n), 32))
	senderVec := builder.CreateByteVector(fixtureBytes(byte(0xA1+n), 32))
	podVec := builder.CreateByteVector(fixtureBytes(byte(0xB1+n), 32))
	funcNameOff := builder.CreateString("fixture_fn")

	types.TransactionStart(builder)
	types.TransactionAddHash(builder, hashVec)
	types.TransactionAddSender(builder, senderVec)
	types.TransactionAddPod(builder, podVec)
	types.TransactionAddFunctionName(builder, funcNameOff)

	return types.TransactionEnd(builder)
}

// fixtureBytes returns n bytes filled with b.
func fixtureBytes(b byte, n int) []byte {
	out := make([]byte, n)
	for i := range out {
		out[i] = b
	}

	return out
}
