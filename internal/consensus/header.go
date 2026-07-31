package consensus

import (
	"encoding/binary"

	flatbuffers "github.com/google/flatbuffers/go"
	"github.com/zeebo/blake3"

	"BluePods/internal/logger"
	"BluePods/internal/types"
)

// headerSize is the encoded length of a vertex header. With the 64-byte signature
// a verifier needs 184 bytes per validator to check an anchor, which is what makes
// a quorum bundle cheap enough to ship to a light client.
//
// NORMATIVE wire contract for any external verifier (a light client, or a
// reimplementation of this package in another language). The header is a
// positional, fixed-width, big-endian byte string carrying no lengths and no
// separators, laid out at exactly these offsets:
//
//	offset  size  field
//	0       32    producer         Ed25519 public key of the producing validator
//	32      8     round            big-endian uint64
//	40      8     epoch            big-endian uint64
//	48      8     frontier_round   big-endian uint64
//	56      32    index_root       verifiable index root at frontier_round
//	88      32    body_hash        BLAKE3(0x02 || canonical body)
//	120           total
//
// The vertex identity is BLAKE3(0x01 || header) and the producer's Ed25519
// signature is taken over that identity, never over the header bytes themselves.
// A verifier MUST reproduce this layout byte for byte: a reordered field or an
// omitted domain tag yields a different identity, which does not fail loudly — it
// silently disagrees with every node on the network. TestVertexHeader_GoldenLayout
// pins the encoding and the identity to fixed vectors.
const headerSize = 120

// The one-byte domain tags separating the two hashes. Without them a byte string
// accepted as a header encoding and the same byte string accepted as a canonical
// body would produce the same digest, so a value proved to be one could be
// replayed as the other. They are part of the wire contract: changing a tag
// changes every vertex identity on the network.
const (
	headerDomainTag = 0x01 // headerDomainTag prefixes the header bytes before hashing
	bodyDomainTag   = 0x02 // bodyDomainTag prefixes the canonical body before hashing
)

// bodyBuilderSize is the initial capacity of the canonical body builder. Vertices
// carrying transactions grow it; the constant only avoids the first few regrowths.
const bodyBuilderSize = 4096

// minVertexSize is the shortest buffer that can hold a FlatBuffers root offset and
// a vtable. Anything shorter is not a vertex, and reading it as one panics.
const minVertexSize = 8

// malformedBody is the body hash reported for a vertex whose body cannot be read
// (a crafted FlatBuffer that panics on access). It is a domain-separated constant
// so the value is deterministic on every node, and validateVertexHash rejects it
// outright rather than comparing it, so a producer cannot declare it to smuggle an
// unreadable body past the check.
var malformedBody = blake3.Sum256([]byte("bluepods.vertex.body.malformed"))

// vertexHeader is the compact, body-independent commitment a producer signs. The
// vertex identity is BLAKE3 of its tagged encoding, so the identity commits to the
// body only through bodyHash: a verifier holding the header and the signature
// checks an anchor without downloading the body, and a full node recomputes
// bodyHash to bind the body it received back to the signed header.
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

// hash returns the vertex identity: BLAKE3 of the encoded header behind its
// domain tag.
func (h *vertexHeader) hash() Hash {
	return taggedHash(headerDomainTag, h.bytes())
}

// taggedHash returns BLAKE3(tag || data), the domain-separated hash both vertex
// commitments are built from.
func taggedHash(tag byte, data []byte) Hash {
	digest := blake3.New()
	_, _ = digest.Write([]byte{tag})
	_, _ = digest.Write(data)

	var out Hash
	copy(out[:], digest.Sum(nil))

	return out
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

// headerBytes returns the encoded header a vertex declares. The producer's
// signature is taken over BLAKE3(headerDomainTag || headerBytes(v)), so a verifier
// checks it with no access to the body.
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
// content implies, and ok=false when data is not a readable vertex at all. It is
// the exported form of vertexIdentity, for the callers outside this package that
// must derive a vertex's identity from its bytes — the only way to produce or
// check a vertex the DAG will accept.
//
// The input is untrusted (a gossiped or fetched buffer), and types.GetRootAsVertex
// panics on a short or crafted vtable, so the length guard rejects the obvious
// cases and the deferred recover contains the rest: a caller gets ok=false and
// tries the next peer instead of taking the node down.
func VertexIdentity(data []byte) (identity, bodyHash Hash, ok bool) {
	defer func() {
		if r := recover(); r != nil {
			logger.Warn("malformed vertex buffer", "panic", r)
			identity, bodyHash, ok = Hash{}, Hash{}, false
		}
	}()

	if len(data) < minVertexSize {
		return Hash{}, Hash{}, false
	}

	identity, bodyHash = vertexIdentity(types.GetRootAsVertex(data, 0))

	return identity, bodyHash, bodyHash != malformedBody
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

	return taggedHash(bodyDomainTag, bodyBytes(v))
}

// bodyBytes returns the canonical serialization of a vertex body. It rebuilds the
// body through the same builder path production uses, from the parsed vertex, so
// the producer and every receiver hash the identical bytes for identical content.
//
// CONSENSUS-CRITICAL DEPENDENCY: these bytes are whatever flatbuffers-go emits.
// The library's emission — field ordering inside the vtable, alignment, padding —
// is therefore part of the protocol: a version bump that changes one emitted byte
// changes every vertex identity on the network, and nodes on the two versions
// would reject each other's vertices as forged. TestVertexBodyHash_Golden pins the
// current emission to a fixed vector so such an upgrade fails loudly in CI. The
// header a light client verifies is hand-encoded (see headerSize) and does not
// depend on the library; only the body does.
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
