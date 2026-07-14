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

// freezeGenesis freezes the epoch-0 genesis holder snapshot from the current
// validator set, treating every current validator as a committed member. It is the
// test stand-in for the committed-registration freeze the production commit path
// performs (SeedGenesis + handleRegisterValidator): a unit test constructs the DAG
// with its whole committee up front, so all of them are "committed" here and no
// optimistic self-add exists to exclude. Call it after stakes are set so the frozen
// snapshot carries the intended weights.
func freezeGenesis(dag *DAG) {
	dag.commitMu.Lock()
	defer dag.commitMu.Unlock()

	dag.committedMembers = make(map[Hash]bool)
	for _, v := range dag.validators.All() {
		dag.committedMembers[v.Pubkey] = true
	}

	dag.epochHolders = dag.freezeGenesisHolders()
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

// disableTxAuth turns off commit-time authenticity on a DAG for tests that drive
// executeTx/commitRound with synthetic (unsigned, zero-hash) transactions to
// isolate downstream logic (version tracking, fees, ownership, validator-set
// changes). Production always runs the real verifyTxAuthenticity; these fixtures
// were never signed, so they would be rejected at the commit-time gate before the
// logic under test runs. Tests that assert authenticity itself (forged-tx
// rejection) leave it on and must not call this.
func disableTxAuth(dag *DAG) {
	dag.verifyTxAuth = func(*types.Transaction) error { return nil }
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

// TestHasQuorumFromRound_StakeWeighted verifies the production quorum is
// stake-weighted against the SAME holder snapshot the committer uses
// (HoldersForEpoch(commitEpochForRound)). A single supermajority-stake producer
// reaches quorum; a single minority-stake producer does not. Distinguishes
// stake-weighting from counting: with two validators the old count rule
// (QuorumSize=2) would reject any single producer.
func TestHasQuorumFromRound_StakeWeighted(t *testing.T) {
	db := newTestStorage(t)
	validators, vs := newTestValidatorSet(2)

	dag := New(db, vs, nil, testSystemPod, 1, validators[0].privKey, nil, WithVotingCapMille(900))
	defer dag.Close()

	dag.validators.SetSelfStake(validators[0].pubKey, 90) // big
	dag.validators.SetSelfStake(validators[1].pubKey, 10) // small

	// Only the SMALL (10%) producer at the round → minority stake, no quorum.
	const r1 = 4
	storeRoundVertex(t, dag, validators[1], r1)
	if dag.hasQuorumFromRound(r1) {
		t.Fatal("hasQuorumFromRound true on minority-stake (10%) producer, expected false")
	}

	// Only the BIG (90%) producer at the round → supermajority stake, quorum with
	// a SINGLE producer (would fail the old count rule of QuorumSize=2).
	const r2 = 6
	storeRoundVertex(t, dag, validators[0], r2)
	if !dag.hasQuorumFromRound(r2) {
		t.Fatal("hasQuorumFromRound false on single supermajority-stake (90%) producer, expected true")
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
