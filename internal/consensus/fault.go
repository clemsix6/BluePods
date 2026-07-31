package consensus

import (
	"crypto/ed25519"
	"encoding/binary"
	"encoding/hex"

	"BluePods/internal/events"
	"BluePods/internal/logger"
	"BluePods/internal/types"
)

// prefixFault holds the anchor fault evidence this node has collected:
// fault/<vertex hash> -> encoded faultRecord, one entry per convicted vertex.
//
// The evidence is NODE-LOCAL and deliberately outside every convergence-checked
// structure. It is not consensus state: a lie is recorded by whichever nodes
// could verify the frontier when they committed it, so two honest nodes may
// legitimately hold different fault sets and must still agree on state. The key
// shape is what keeps it out: every scan that feeds the snapshot, the
// convergence fingerprint or the object store selects bare 32-byte keys, and a
// prefixed 38-byte key matches none of them (the committed flags under vc/ ride
// on the same property). Nothing here feeds the index either — the trees are
// built from the committed transaction stream alone.
var prefixFault = []byte("fault/")

// The offsets a fault record's fixed-width encoding is read at: the normative
// 120-byte vertex header, the producer's Ed25519 signature over that header's
// hash, and the root this node computed at the same frontier.
const (
	faultHeaderOffset    = 0                                            // faultHeaderOffset starts the 120-byte header (see headerSize)
	faultSignatureOffset = faultHeaderOffset + headerSize               // faultSignatureOffset starts the signature
	faultComputedOffset  = faultSignatureOffset + ed25519.SignatureSize // faultComputedOffset starts the 32-byte recomputed root
	faultRecordSize      = faultComputedOffset + len(Hash{})            // faultRecordSize is the total encoded length
)

// faultRecord is the self-contained proof that a producer anchored a root no
// honest node computes. It carries the producer's own signed header and this
// node's recomputation, and nothing else: producer, round, epoch, frontier and
// the claimed root are all READ OUT of the header at the offsets header.go pins
// as the normative wire contract, rather than stored a second time beside it. A
// duplicated copy could contradict the signed bytes; a derived one cannot.
//
// A third party verifies it with no access to this node: recompute
// BLAKE3(0x01 || header), check the producer's signature over it, read the
// claimed root at offset 56, and recompute the index root at frontier_round
// from the committed stream. The signature makes the claim attributable, which
// is what turns it into slashing material when slashing lands.
type faultRecord struct {
	header    []byte // header is the 120-byte normative vertex header the producer signed
	signature []byte // signature is the producer's Ed25519 signature over the header hash
	computed  Hash   // computed is the index root this node derived at the header's frontier_round
}

// encode returns the fixed-width serialization of the record.
func (r faultRecord) encode() []byte {
	out := make([]byte, 0, faultRecordSize)

	out = append(out, r.header...)
	out = append(out, r.signature...)
	out = append(out, r.computed[:]...)

	return out
}

// producer returns the convicted validator's public key, read from the header.
func (r faultRecord) producer() Hash {
	return hashFrom(r.header[:32])
}

// round returns the round the lying vertex was produced in, read from the
// header.
func (r faultRecord) round() uint64 {
	return binary.BigEndian.Uint64(r.header[32:40])
}

// claimed returns the index root the producer anchored, read from the header.
func (r faultRecord) claimed() Hash {
	return hashFrom(r.header[56:88])
}

// identity returns the vertex header the signature must verify over, decoded
// back into its struct form so a verifier reproduces the identity through the
// same encoder production uses.
func (r faultRecord) identity() vertexHeader {
	return vertexHeader{
		producer:      r.producer(),
		round:         r.round(),
		epoch:         binary.BigEndian.Uint64(r.header[40:48]),
		frontierRound: binary.BigEndian.Uint64(r.header[48:56]),
		indexRoot:     r.claimed(),
		bodyHash:      hashFrom(r.header[88:120]),
	}
}

// decodeFaultRecord parses stored evidence, returning ok=false for anything
// that is not exactly one record.
func decodeFaultRecord(data []byte) (faultRecord, bool) {
	if len(data) != faultRecordSize {
		return faultRecord{}, false
	}

	return faultRecord{
		header:    data[faultHeaderOffset:faultSignatureOffset],
		signature: data[faultSignatureOffset:faultComputedOffset],
		computed:  hashFrom(data[faultComputedOffset:]),
	}, true
}

// recordAnchorFault persists the evidence convicting a committed vertex of
// anchoring a root this node's own recomputation contradicts, and emits the
// event beside it. Exactly one record and one event per lying vertex: the write
// is skipped when the vertex is already convicted, so re-applying a batch after
// a crash, or replaying a decided round after a restart, cannot turn one lie
// into a growing pile of accusations.
//
// A vertex whose evidence does not verify produces no record at all: an
// accusation this node cannot prove convicts nobody, and storing one would put
// junk in the pile slashing is meant to read.
func (d *DAG) recordAnchorFault(v *types.Vertex, claimed, computed Hash) {
	identity := hashFrom(v.HashBytes())

	if d.store.hasFault(identity) {
		return
	}

	record, ok := verifiableEvidence(v, computed)
	if !ok {
		logger.Warn("wrong-root vertex carries no verifiable evidence: nothing recorded",
			"vertex", hex.EncodeToString(identity[:8]))
		return
	}

	d.store.putFault(identity, record.encode())

	events.AnchorFault(record.producer(), v.Round(), claimed, computed)
}

// verifiableEvidence assembles the record convicting a vertex and checks it the
// way a third party will: the producer's signature must verify over the
// identity recomputed from the header the record carries. What this node stores
// is therefore always a proof, never a claim.
func verifiableEvidence(v *types.Vertex, computed Hash) (faultRecord, bool) {
	signature := v.SignatureBytes()
	if len(signature) != ed25519.SignatureSize {
		return faultRecord{}, false
	}

	record := faultRecord{header: headerBytes(v), signature: signature, computed: computed}

	producer := record.producer()
	header := record.identity()
	identity := header.hash()

	return record, ed25519.Verify(producer[:], identity[:], signature)
}

// hasFault reports whether evidence is already stored for the vertex.
func (s *store) hasFault(hash Hash) bool {
	data, err := s.db.Get(makeFaultKey(hash))

	return err == nil && data != nil
}

// putFault persists one vertex's fault evidence.
func (s *store) putFault(hash Hash, evidence []byte) {
	if err := s.db.Set(makeFaultKey(hash), evidence); err != nil {
		logger.Error("persist anchor fault evidence", "error", err)
	}
}

// makeFaultKey creates the storage key holding a vertex's fault evidence.
func makeFaultKey(hash Hash) []byte {
	key := make([]byte, len(prefixFault)+32)
	copy(key, prefixFault)
	copy(key[len(prefixFault):], hash[:])

	return key
}
