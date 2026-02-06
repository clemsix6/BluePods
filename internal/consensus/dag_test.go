package consensus

import (
	"crypto/ed25519"
	"crypto/rand"
	"os"
	"testing"
	"time"

	flatbuffers "github.com/google/flatbuffers/go"

	"BluePods/internal/storage"
	"BluePods/internal/types"
)

// newTestStorage creates a temporary storage for testing.
func newTestStorage(t *testing.T) *storage.Storage {
	t.Helper()

	dir, err := os.MkdirTemp("", "consensus_test_*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}

	t.Cleanup(func() {
		os.RemoveAll(dir)
	})

	db, err := storage.New(dir)
	if err != nil {
		t.Fatalf("failed to create storage: %v", err)
	}

	t.Cleanup(func() {
		db.Close()
	})

	return db
}

// testValidator holds a keypair for testing.
type testValidator struct {
	privKey ed25519.PrivateKey
	pubKey  Hash
}

// newTestValidator creates a new test validator with random keys.
func newTestValidator() testValidator {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	var pubHash Hash
	copy(pubHash[:], pub)

	return testValidator{privKey: priv, pubKey: pubHash}
}

// testSystemPod is a dummy system pod hash for testing.
var testSystemPod = Hash{0x01, 0x02, 0x03}

// newTestValidatorSet creates n test validators and returns their set.
func newTestValidatorSet(n int) ([]testValidator, *ValidatorSet) {
	validators := make([]testValidator, n)
	pubkeys := make([]Hash, n)

	for i := 0; i < n; i++ {
		validators[i] = newTestValidator()
		pubkeys[i] = validators[i].pubKey
	}

	return validators, NewValidatorSet(pubkeys)
}

func TestNewDAG(t *testing.T) {
	db := newTestStorage(t)
	validators, vs := newTestValidatorSet(4)

	dag := New(db, vs, nil, testSystemPod, 1, validators[0].privKey, nil)
	defer dag.Close()

	if dag.Round() != 0 {
		t.Errorf("expected round 0, got %d", dag.Round())
	}
}

func TestValidatorSet(t *testing.T) {
	_, vs := newTestValidatorSet(10)

	if vs.Len() != 10 {
		t.Errorf("expected 10 validators, got %d", vs.Len())
	}

	// BFT quorum = 2n/3 + 1 = 7
	if vs.QuorumSize() != 7 {
		t.Errorf("expected quorum 7, got %d", vs.QuorumSize())
	}
}

func TestQuorumSize(t *testing.T) {
	tests := []struct {
		validators int
		quorum     int
	}{
		{1, 1},
		{3, 3},
		{4, 3},
		{5, 4},
		{10, 7},
		{100, 67},
	}

	for _, tt := range tests {
		_, vs := newTestValidatorSet(tt.validators)
		if vs.QuorumSize() != tt.quorum {
			t.Errorf("validators=%d: expected quorum %d, got %d",
				tt.validators, tt.quorum, vs.QuorumSize())
		}
	}
}

func TestAddVertex(t *testing.T) {
	db := newTestStorage(t)
	validators, vs := newTestValidatorSet(4)

	dag := New(db, vs, nil, testSystemPod, 1, validators[0].privKey, nil)
	defer dag.Close()

	// Build a valid round 0 vertex
	data := buildTestVertex(t, validators[1], 0, nil, 1)

	if !dag.AddVertex(data) {
		t.Fatal("AddVertex should return true for new valid vertex")
	}
}

func TestAddVertexUnknownProducer(t *testing.T) {
	db := newTestStorage(t)
	validators, vs := newTestValidatorSet(4)
	unknown := newTestValidator()

	dag := New(db, vs, nil, testSystemPod, 1, validators[0].privKey, nil)
	defer dag.Close()

	data := buildTestVertex(t, unknown, 0, nil, 1)

	if dag.AddVertex(data) {
		t.Error("AddVertex should return false for unknown producer")
	}
}

func TestAddVertexDuplicate(t *testing.T) {
	db := newTestStorage(t)
	validators, vs := newTestValidatorSet(4)

	dag := New(db, vs, nil, testSystemPod, 1, validators[0].privKey, nil)
	defer dag.Close()

	data := buildTestVertex(t, validators[1], 0, nil, 1)

	if !dag.AddVertex(data) {
		t.Fatal("first AddVertex should return true")
	}

	// Adding same vertex again should return false (duplicate)
	if dag.AddVertex(data) {
		t.Error("duplicate AddVertex should return false")
	}
}

// mockBroadcaster captures vertices for testing.
type mockBroadcaster struct {
	vertices [][]byte
}

func (m *mockBroadcaster) Gossip(data []byte, fanout int) error {
	m.vertices = append(m.vertices, data)
	return nil
}

func TestProduceVertex(t *testing.T) {
	db := newTestStorage(t)
	validators, vs := newTestValidatorSet(4)
	mock := &mockBroadcaster{}

	dag := New(db, vs, mock, testSystemPod, 1, validators[0].privKey, nil)
	defer dag.Close()

	// Submit a transaction to trigger vertex production (round 0 has no parents)
	dag.SubmitTx([]byte("test tx"))

	// Wait for vertex production
	time.Sleep(50 * time.Millisecond)

	if len(mock.vertices) == 0 {
		t.Fatal("no vertex was broadcast")
	}

	vertex := types.GetRootAsVertex(mock.vertices[0], 0)
	if vertex.Round() != 0 {
		t.Errorf("expected round 0, got %d", vertex.Round())
	}
}

func TestRoundProgression(t *testing.T) {
	db := newTestStorage(t)
	validators, vs := newTestValidatorSet(4)

	dag := New(db, vs, nil, testSystemPod, 1, validators[0].privKey, nil)
	defer dag.Close()

	// Add round 0 vertices from quorum of validators (not including validator 0)
	for i := 1; i < 4; i++ {
		data := buildTestVertex(t, validators[i], 0, nil, 1)
		if !dag.AddVertex(data) {
			t.Fatalf("AddVertex failed for validator %d", i)
		}
	}

	// With quorum from round 0, DAG can produce round 1 vertex
	// So round should have advanced
	if dag.Round() < 1 {
		t.Errorf("expected round >= 1, got %d", dag.Round())
	}
}

// buildTestVertex creates a signed vertex for testing.
func buildTestVertex(t *testing.T, v testValidator, round uint64, parents []Hash, epoch uint64) []byte {
	t.Helper()

	builder := flatbuffers.NewBuilder(1024)

	// Build parents
	parentOffsets := make([]flatbuffers.UOffsetT, len(parents))
	for i, p := range parents {
		hashVec := builder.CreateByteVector(p[:])
		prodVec := builder.CreateByteVector(make([]byte, 32))

		types.VertexLinkStart(builder)
		types.VertexLinkAddHash(builder, hashVec)
		types.VertexLinkAddProducer(builder, prodVec)
		parentOffsets[i] = types.VertexLinkEnd(builder)
	}

	types.VertexStartParentsVector(builder, len(parentOffsets))
	for i := len(parentOffsets) - 1; i >= 0; i-- {
		builder.PrependUOffsetT(parentOffsets[i])
	}
	parentsVec := builder.EndVector(len(parentOffsets))

	// Empty transactions
	types.VertexStartTransactionsVector(builder, 0)
	txsVec := builder.EndVector(0)

	producerVec := builder.CreateByteVector(v.pubKey[:])

	// Build unsigned vertex first
	types.VertexStart(builder)
	types.VertexAddRound(builder, round)
	types.VertexAddProducer(builder, producerVec)
	types.VertexAddParents(builder, parentsVec)
	types.VertexAddTransactions(builder, txsVec)
	types.VertexAddEpoch(builder, epoch)
	vertexOffset := types.VertexEnd(builder)
	builder.Finish(vertexOffset)

	unsigned := builder.FinishedBytes()
	hash := hashVertex(unsigned)
	sig := ed25519.Sign(v.privKey, hash[:])

	// Rebuild with hash and signature
	builder.Reset()

	hashVec := builder.CreateByteVector(hash[:])
	sigVec := builder.CreateByteVector(sig)
	producerVec = builder.CreateByteVector(v.pubKey[:])

	// Rebuild parents
	parentOffsets = make([]flatbuffers.UOffsetT, len(parents))
	for i, p := range parents {
		hVec := builder.CreateByteVector(p[:])
		pVec := builder.CreateByteVector(make([]byte, 32))

		types.VertexLinkStart(builder)
		types.VertexLinkAddHash(builder, hVec)
		types.VertexLinkAddProducer(builder, pVec)
		parentOffsets[i] = types.VertexLinkEnd(builder)
	}

	types.VertexStartParentsVector(builder, len(parentOffsets))
	for i := len(parentOffsets) - 1; i >= 0; i-- {
		builder.PrependUOffsetT(parentOffsets[i])
	}
	parentsVec = builder.EndVector(len(parentOffsets))

	types.VertexStartTransactionsVector(builder, 0)
	txsVec = builder.EndVector(0)

	types.VertexStart(builder)
	types.VertexAddHash(builder, hashVec)
	types.VertexAddRound(builder, round)
	types.VertexAddProducer(builder, producerVec)
	types.VertexAddSignature(builder, sigVec)
	types.VertexAddParents(builder, parentsVec)
	types.VertexAddTransactions(builder, txsVec)
	types.VertexAddEpoch(builder, epoch)
	vertexOffset = types.VertexEnd(builder)

	builder.Finish(vertexOffset)

	return builder.FinishedBytes()
}
