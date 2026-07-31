package consensus

import (
	"encoding/hex"
	"testing"

	"github.com/zeebo/blake3"

	"BluePods/internal/types"
)

// TestVertexHashes_AreDomainSeparated pins the one-byte domain tag in front of
// each of the two hashes. Without them a byte string accepted as a header
// encoding and the same byte string accepted as a body would produce the same
// digest, so a value proved to be one could be replayed as the other.
func TestVertexHashes_AreDomainSeparated(t *testing.T) {
	validators, _ := newTestValidatorSet(1)
	data := buildFullVertex(t, validators[0])
	vertex := types.GetRootAsVertex(data, 0)

	identity, bodyHash := vertexIdentity(vertex)

	wantIdentity := blake3.Sum256(append([]byte{0x01}, headerBytes(vertex)...))
	if identity != wantIdentity {
		t.Errorf("identity = %x, want BLAKE3(0x01 || header) = %x", identity, wantIdentity)
	}

	wantBody := blake3.Sum256(append([]byte{0x02}, bodyBytes(vertex)...))
	if bodyHash != wantBody {
		t.Errorf("body hash = %x, want BLAKE3(0x02 || body) = %x", bodyHash, wantBody)
	}

	if blake3.Sum256(headerBytes(vertex)) == identity {
		t.Error("the identity is the untagged hash of the header: the two hashes are not domain separated")
	}
}

// TestVertexHeader_GoldenLayout pins the header encoding itself: a fixed set of
// field values MUST encode to exactly these 120 bytes and hash to exactly this
// identity. It is the regression test behind the NORMATIVE wire contract in
// header.go — an external verifier reimplementing the layout reproduces this
// vector, and any change to the encoding, the field order or the domain tag fails
// here instead of forking the network silently.
func TestVertexHeader_GoldenLayout(t *testing.T) {
	header := goldenHeader()

	const wantBytes = "" +
		"0101010101010101010101010101010101010101010101010101010101010101" + // producer
		"00000000000004d2" + // round 1234
		"0000000000000005" + // epoch 5
		"00000000000004b0" + // frontier_round 1200
		"0202020202020202020202020202020202020202020202020202020202020202" + // index_root
		"0303030303030303030303030303030303030303030303030303030303030303" // body_hash

	if got := hex.EncodeToString(header.bytes()); got != wantBytes {
		t.Fatalf("header encoding =\n%s\nwant\n%s", got, wantBytes)
	}

	if n := len(header.bytes()); n != headerSize {
		t.Fatalf("header encoding = %d bytes, want %d", n, headerSize)
	}

	const wantHash = "369460b53e5d185da3b58be53018407b0683c7498b893c6ad73709a950c89f77"

	if got := hex.EncodeToString(headerHashBytes(header)); got != wantHash {
		t.Fatalf("header hash = %s, want %s", got, wantHash)
	}
}

// TestVertexBodyHash_Golden pins the body hash of a fixed vertex body. It holds
// the FlatBuffers emission itself: the canonical body is produced by
// flatbuffers-go, so a library upgrade that changes a single emitted byte changes
// every vertex identity on the network. This vector makes that a loud test
// failure rather than a silent fork.
func TestVertexBodyHash_Golden(t *testing.T) {
	validators, _ := newTestValidatorSet(1)
	vertex := types.GetRootAsVertex(buildFullVertex(t, validators[0]), 0)

	const wantBody = "2f647d359e110081d8794344e2bc4a681e4e3c93bb66b3dc402b0026a359681a"

	if got := hex.EncodeToString(bodyHashBytes(vertex)); got != wantBody {
		t.Fatalf("body hash = %s, want %s", got, wantBody)
	}
}

// goldenHeader returns the fixed header the golden vector is computed over.
func goldenHeader() vertexHeader {
	var producer, indexRoot, bodyHash Hash

	for i := range producer {
		producer[i] = 0x01
		indexRoot[i] = 0x02
		bodyHash[i] = 0x03
	}

	return vertexHeader{
		producer:      producer,
		round:         1234,
		epoch:         5,
		frontierRound: 1200,
		indexRoot:     indexRoot,
		bodyHash:      bodyHash,
	}
}

// headerHashBytes returns a header's identity as a byte slice, for hex compare.
func headerHashBytes(h vertexHeader) []byte {
	hash := h.hash()
	return hash[:]
}

// bodyHashBytes returns a vertex's computed body hash as a byte slice.
func bodyHashBytes(v *types.Vertex) []byte {
	hash := computeBodyHash(v)
	return hash[:]
}
