package consensus

import (
	"encoding/binary"

	flatbuffers "github.com/google/flatbuffers/go"
	"github.com/zeebo/blake3"

	"BluePods/internal/logger"
	"BluePods/internal/types"
)

// headerSize is the encoded length of a vertex header: producer (32) + round (8)
// + epoch (8) + frontier_round (8) + index_root (32) + body_hash (32). With the
// 64-byte signature a verifier needs 184 bytes per validator to check an anchor,
// which is what makes a quorum bundle cheap enough to ship to a light client.
const headerSize = 120

// bodyBuilderSize is the initial capacity of the canonical body builder. Vertices
// carrying transactions grow it; the constant only avoids the first few regrowths.
const bodyBuilderSize = 4096

// malformedBody is the body hash reported for a vertex whose body cannot be read
// (a crafted FlatBuffer that panics on access). It is a domain-separated constant
// so the value is deterministic on every node, and validateVertexHash rejects it
// outright rather than comparing it, so a producer cannot declare it to smuggle an
// unreadable body past the check.
var malformedBody = blake3.Sum256([]byte("bluepods.vertex.body.malformed"))

// vertexHeader is the compact, body-independent commitment a producer signs. The
// vertex identity is BLAKE3 of its encoding, so the identity commits to the body
// only through bodyHash: a verifier holding the header and the signature checks an
// anchor without downloading the body, and a full node recomputes bodyHash to bind
// the body it received back to the signed header.
type vertexHeader struct {
	producer      Hash   // producer is the 32-byte Ed25519 pubkey of the producing validator
	round         uint64 // round is the DAG round the vertex was produced in
	epoch         uint64 // epoch is the producer's epoch, naming the validator tree that weighs its quorum
	frontierRound uint64 // frontierRound is the committed round indexRoot anchors
	indexRoot     Hash   // indexRoot is the verifiable index root at frontierRound (zero when unanchored)
	bodyHash      Hash   // bodyHash is the BLAKE3 of the vertex body (parents, transactions, fee summary, timestamp)
}

// bytes encodes the header as a fixed-width, big-endian byte string. The encoding
// is positional and carries no lengths: every field has a constant size, so the
// 120 bytes are unambiguous and identical on every node.
func (h *vertexHeader) bytes() []byte {
	out := make([]byte, 0, headerSize)

	out = append(out, h.producer[:]...)
	out = binary.BigEndian.AppendUint64(out, h.round)
	out = binary.BigEndian.AppendUint64(out, h.epoch)
	out = binary.BigEndian.AppendUint64(out, h.frontierRound)
	out = append(out, h.indexRoot[:]...)
	out = append(out, h.bodyHash[:]...)

	return out
}

// hash returns the vertex identity: BLAKE3 of the encoded header.
func (h *vertexHeader) hash() Hash {
	return blake3.Sum256(h.bytes())
}

// headerOf reads the header a vertex declares, body hash included. It reads only
// header fields, so it works on a vertex carrying no body at all — which is what a
// light verifier holds.
func headerOf(v *types.Vertex) vertexHeader {
	return vertexHeader{
		producer:      extractProducer(v),
		round:         v.Round(),
		epoch:         v.Epoch(),
		frontierRound: v.FrontierRound(),
		indexRoot:     hashFrom(v.IndexRootBytes()),
		bodyHash:      hashFrom(v.BodyHashBytes()),
	}
}

// headerBytes returns the encoded header a vertex declares. It is the exact byte
// string the producer signed, so a verifier checks the signature over
// BLAKE3(headerBytes(v)) with no access to the body.
func headerBytes(v *types.Vertex) []byte {
	header := headerOf(v)
	return header.bytes()
}

// vertexIdentity returns the identity hash and the body hash a vertex's own
// content implies: the body is rehashed and folded into the declared header
// fields. The producer calls it over the unsigned vertex to obtain the hash it
// signs, and a receiver calls it to check that the vertex it holds is the one that
// was signed — one function, two call sites, so the two can never drift.
func vertexIdentity(v *types.Vertex) (identity, bodyHash Hash) {
	header := headerOf(v)
	header.bodyHash = computeBodyHash(v)

	return header.hash(), header.bodyHash
}

// VertexIdentity returns the header hash and body hash a serialized vertex's own
// content implies. It is the exported form of vertexIdentity, for the callers
// outside this package that must derive a vertex's identity from its bytes — the
// only way to produce or check a vertex the DAG will accept.
func VertexIdentity(data []byte) (identity, bodyHash Hash) {
	return vertexIdentity(types.GetRootAsVertex(data, 0))
}

// computeBodyHash returns the BLAKE3 of a vertex body: parents, transactions, fee
// summary and timestamp, canonically re-serialized. The round is deliberately
// absent — it lives in the header only.
//
// A crafted FlatBuffer can panic on field access, so the walk is recovered and
// reports the malformedBody sentinel instead of crashing the node on an
// unauthenticated gossip message.
func computeBodyHash(v *types.Vertex) (bodyHash Hash) {
	defer func() {
		if r := recover(); r != nil {
			logger.Warn("malformed vertex body", "panic", r)
			bodyHash = malformedBody
		}
	}()

	return blake3.Sum256(bodyBytes(v))
}

// bodyBytes returns the canonical serialization of a vertex body. It rebuilds the
// body through the same builder path production uses, from the parsed vertex, so
// the producer and every receiver hash the identical bytes for identical content.
func bodyBytes(v *types.Vertex) []byte {
	builder := flatbuffers.NewBuilder(bodyBuilderSize)

	txsVec := rebuildTxVector(builder, v)
	feeSummaryOff := rebuildFeeSummary(builder, v.FeeSummary(nil))
	parentsVec := rebuildParentsVector(builder, v)

	types.VertexStart(builder)
	types.VertexAddParents(builder, parentsVec)
	types.VertexAddTransactions(builder, txsVec)

	if feeSummaryOff != 0 {
		types.VertexAddFeeSummary(builder, feeSummaryOff)
	}

	types.VertexAddTimestamp(builder, v.Timestamp())
	builder.Finish(types.VertexEnd(builder))

	return builder.FinishedBytes()
}

// rebuildTxVector re-serializes a vertex's attested transactions into the builder
// and returns the vector offset.
func rebuildTxVector(builder *flatbuffers.Builder, v *types.Vertex) flatbuffers.UOffsetT {
	offsets := make([]flatbuffers.UOffsetT, 0, v.TransactionsLength())

	var atx types.AttestedTransaction
	for i := 0; i < v.TransactionsLength(); i++ {
		if !v.Transactions(&atx, i) {
			continue
		}
		offsets = append(offsets, rebuildAttestedTx(builder, &atx))
	}

	return endOffsetVector(builder, offsets, types.VertexStartTransactionsVector)
}

// rebuildParentsVector re-serializes a vertex's parent links into the builder and
// returns the vector offset.
func rebuildParentsVector(builder *flatbuffers.Builder, v *types.Vertex) flatbuffers.UOffsetT {
	offsets := make([]flatbuffers.UOffsetT, 0, v.ParentsLength())

	var link types.VertexLink
	for i := 0; i < v.ParentsLength(); i++ {
		if !v.Parents(&link, i) {
			continue
		}
		offsets = append(offsets, rebuildVertexLink(builder, &link))
	}

	return endOffsetVector(builder, offsets, types.VertexStartParentsVector)
}

// rebuildVertexLink re-serializes one parent link into the builder.
func rebuildVertexLink(builder *flatbuffers.Builder, link *types.VertexLink) flatbuffers.UOffsetT {
	hashVec := builder.CreateByteVector(link.HashBytes())
	producerVec := builder.CreateByteVector(link.ProducerBytes())

	types.VertexLinkStart(builder)
	types.VertexLinkAddHash(builder, hashVec)
	types.VertexLinkAddProducer(builder, producerVec)

	return types.VertexLinkEnd(builder)
}

// rebuildFeeSummary re-serializes a declared fee summary into the builder. It
// returns 0 when the vertex declares none, so the canonical body omits the field
// exactly as the producer's vertex does.
func rebuildFeeSummary(builder *flatbuffers.Builder, summary *types.FeeSummary) flatbuffers.UOffsetT {
	if summary == nil {
		return 0
	}

	types.FeeSummaryStart(builder)
	types.FeeSummaryAddTotalFees(builder, summary.TotalFees())
	types.FeeSummaryAddTotalBurned(builder, summary.TotalBurned())
	types.FeeSummaryAddTotalEpoch(builder, summary.TotalEpoch())

	return types.FeeSummaryEnd(builder)
}

// endOffsetVector writes offsets into a FlatBuffers vector in order, using the
// given generated vector starter, and returns the vector offset.
func endOffsetVector(builder *flatbuffers.Builder, offsets []flatbuffers.UOffsetT, start func(*flatbuffers.Builder, int) flatbuffers.UOffsetT) flatbuffers.UOffsetT {
	start(builder, len(offsets))
	for i := len(offsets) - 1; i >= 0; i-- {
		builder.PrependUOffsetT(offsets[i])
	}

	return builder.EndVector(len(offsets))
}

// hashFrom copies a 32-byte field into a Hash, returning the zero hash for any
// other length (an absent or malformed field).
func hashFrom(b []byte) Hash {
	var h Hash
	if len(b) == 32 {
		copy(h[:], b)
	}

	return h
}
