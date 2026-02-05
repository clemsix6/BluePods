package aggregation

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"testing"
	"time"

	"BluePods/internal/consensus"
	"BluePods/internal/network"
	"BluePods/internal/types"

	flatbuffers "github.com/google/flatbuffers/go"
)

// buildTestTransaction creates a test transaction with the given object IDs.
func buildTestTransaction(readObjects, mutableObjects [][32]byte) []byte {
	builder := flatbuffers.NewBuilder(512)

	hash := [32]byte{0x01, 0x02, 0x03}
	hashOffset := builder.CreateByteVector(hash[:])

	var readBytesOffset flatbuffers.UOffsetT

	if len(readObjects) > 0 {
		readBytes := make([]byte, len(readObjects)*32)

		for i, id := range readObjects {
			copy(readBytes[i*32:], id[:])
		}

		readBytesOffset = builder.CreateByteVector(readBytes)
	}

	var mutableBytesOffset flatbuffers.UOffsetT

	if len(mutableObjects) > 0 {
		mutableBytes := make([]byte, len(mutableObjects)*32)

		for i, id := range mutableObjects {
			copy(mutableBytes[i*32:], id[:])
		}

		mutableBytesOffset = builder.CreateByteVector(mutableBytes)
	}

	sender := [32]byte{0xAA}
	senderOffset := builder.CreateByteVector(sender[:])

	sig := [64]byte{0xBB}
	sigOffset := builder.CreateByteVector(sig[:])

	pod := [32]byte{0xCC}
	podOffset := builder.CreateByteVector(pod[:])

	funcOffset := builder.CreateString("test_func")
	argsOffset := builder.CreateByteVector([]byte{0x01, 0x02})

	types.TransactionStart(builder)
	types.TransactionAddHash(builder, hashOffset)

	if readBytesOffset != 0 {
		types.TransactionAddReadObjects(builder, readBytesOffset)
	}

	if mutableBytesOffset != 0 {
		types.TransactionAddMutableObjects(builder, mutableBytesOffset)
	}

	types.TransactionAddCreatesObjects(builder, false)
	types.TransactionAddSender(builder, senderOffset)
	types.TransactionAddSignature(builder, sigOffset)
	types.TransactionAddPod(builder, podOffset)
	types.TransactionAddFunctionName(builder, funcOffset)
	types.TransactionAddArgs(builder, argsOffset)

	txOffset := types.TransactionEnd(builder)
	builder.Finish(txOffset)

	return builder.FinishedBytes()
}

// TestAggregatorNoObjects tests aggregation with no objects.
func TestAggregatorNoObjects(t *testing.T) {
	nodes := setupTestNetwork(t, 4)
	defer cleanupTestNetwork(nodes)

	validators := make([]consensus.Hash, len(nodes)-1)
	for i := 1; i < len(nodes); i++ {
		copy(validators[i-1][:], nodes[i].key.Public().(ed25519.PublicKey))
	}

	vs := consensus.NewValidatorSet(validators)
	agg := NewAggregator(nodes[0].node, vs, nil)

	txData := buildTestTransaction(nil, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	result, err := agg.Aggregate(ctx, txData)
	if err != nil {
		t.Fatalf("aggregate: %v", err)
	}

	if len(result) == 0 {
		t.Error("expected non-empty result")
	}

	attested := types.GetRootAsAttestedTransaction(result, 0)

	if attested.ObjectsLength() != 0 {
		t.Errorf("expected 0 objects, got %d", attested.ObjectsLength())
	}

	if attested.ProofsLength() != 0 {
		t.Errorf("expected 0 proofs, got %d", attested.ProofsLength())
	}
}

// TestAggregatorWithObjects tests aggregation with standard objects.
func TestAggregatorWithObjects(t *testing.T) {
	nodes := setupTestNetwork(t, 6)
	defer cleanupTestNetwork(nodes)

	validators := make([]consensus.Hash, len(nodes)-1)
	for i := 1; i < len(nodes); i++ {
		copy(validators[i-1][:], nodes[i].key.Public().(ed25519.PublicKey))
	}

	vs := consensus.NewValidatorSet(validators)
	agg := NewAggregator(nodes[0].node, vs, nil)

	for i := 1; i < len(nodes); i++ {
		idx := i

		nodes[i].node.OnRequest(func(p *network.Peer, data []byte) ([]byte, error) {
			req, err := DecodeRequest(data)
			if err != nil {
				return nil, err
			}

			seed := make([]byte, 32)
			copy(seed, nodes[idx].key.Public().(ed25519.PublicKey)[:32])
			blsKey, _ := GenerateBLSKeyFromSeed(seed)

			resp := &PositiveResponse{
				Hash:      [32]byte{0x42},
				Signature: blsKey.Sign(req.ObjectID[:]),
			}

			if req.WantFull {
				resp.Data = buildTestObject(req.ObjectID, 3) // replication=3
			}

			return EncodePositiveResponse(resp), nil
		})
	}

	objID := [32]byte{0x01, 0x02, 0x03}
	txData := buildTestTransaction([][32]byte{objID}, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	result, err := agg.Aggregate(ctx, txData)
	if err != nil {
		t.Fatalf("aggregate: %v", err)
	}

	attested := types.GetRootAsAttestedTransaction(result, 0)

	if attested.ObjectsLength() != 1 {
		t.Errorf("expected 1 object, got %d", attested.ObjectsLength())
	}

	if attested.ProofsLength() != 1 {
		t.Errorf("expected 1 proof, got %d", attested.ProofsLength())
	}

	var proof types.QuorumProof

	if attested.Proofs(&proof, 0) {
		sigLen := proof.BlsSignatureLength()
		if sigLen != BLSSignatureSize {
			t.Errorf("expected signature length %d, got %d", BLSSignatureSize, sigLen)
		}
	} else {
		t.Error("failed to get proof")
	}
}

// TestAggregatorSingleton tests that singletons are skipped in output.
func TestAggregatorSingleton(t *testing.T) {
	nodes := setupTestNetwork(t, 4)
	defer cleanupTestNetwork(nodes)

	validators := make([]consensus.Hash, len(nodes)-1)
	for i := 1; i < len(nodes); i++ {
		copy(validators[i-1][:], nodes[i].key.Public().(ed25519.PublicKey))
	}

	vs := consensus.NewValidatorSet(validators)
	agg := NewAggregator(nodes[0].node, vs, nil)

	for i := 1; i < len(nodes); i++ {
		idx := i

		nodes[i].node.OnRequest(func(p *network.Peer, data []byte) ([]byte, error) {
			req, err := DecodeRequest(data)
			if err != nil {
				return nil, err
			}

			seed := make([]byte, 32)
			copy(seed, nodes[idx].key.Public().(ed25519.PublicKey)[:32])
			blsKey, _ := GenerateBLSKeyFromSeed(seed)

			resp := &PositiveResponse{
				Hash:      [32]byte{0x42},
				Signature: blsKey.Sign(req.ObjectID[:]),
			}

			if req.WantFull {
				resp.Data = buildTestObject(req.ObjectID, 0) // singleton!
			}

			return EncodePositiveResponse(resp), nil
		})
	}

	objID := [32]byte{0x01}
	txData := buildTestTransaction([][32]byte{objID}, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	result, err := agg.Aggregate(ctx, txData)
	if err != nil {
		t.Fatalf("aggregate: %v", err)
	}

	attested := types.GetRootAsAttestedTransaction(result, 0)

	// Singleton should NOT appear in objects/proofs
	if attested.ObjectsLength() != 0 {
		t.Errorf("singleton should not be in objects, got %d", attested.ObjectsLength())
	}

	if attested.ProofsLength() != 0 {
		t.Errorf("singleton should not have proof, got %d", attested.ProofsLength())
	}
}

// TestAggregatorMixedObjects tests aggregation with both standard and singleton objects.
func TestAggregatorMixedObjects(t *testing.T) {
	nodes := setupTestNetwork(t, 6)
	defer cleanupTestNetwork(nodes)

	validators := make([]consensus.Hash, len(nodes)-1)
	for i := 1; i < len(nodes); i++ {
		copy(validators[i-1][:], nodes[i].key.Public().(ed25519.PublicKey))
	}

	vs := consensus.NewValidatorSet(validators)
	agg := NewAggregator(nodes[0].node, vs, nil)

	standardObj := [32]byte{0x01}
	singletonObj := [32]byte{0x02}

	for i := 1; i < len(nodes); i++ {
		idx := i

		nodes[i].node.OnRequest(func(p *network.Peer, data []byte) ([]byte, error) {
			req, err := DecodeRequest(data)
			if err != nil {
				return nil, err
			}

			seed := make([]byte, 32)
			copy(seed, nodes[idx].key.Public().(ed25519.PublicKey)[:32])
			blsKey, _ := GenerateBLSKeyFromSeed(seed)

			resp := &PositiveResponse{
				Hash:      [32]byte{req.ObjectID[0]},
				Signature: blsKey.Sign(req.ObjectID[:]),
			}

			if req.WantFull {
				// First object is standard, second is singleton
				if req.ObjectID == standardObj {
					resp.Data = buildTestObject(req.ObjectID, 3)
				} else {
					resp.Data = buildTestObject(req.ObjectID, 0) // singleton
				}
			}

			return EncodePositiveResponse(resp), nil
		})
	}

	txData := buildTestTransaction([][32]byte{standardObj}, [][32]byte{singletonObj})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	result, err := agg.Aggregate(ctx, txData)
	if err != nil {
		t.Fatalf("aggregate: %v", err)
	}

	attested := types.GetRootAsAttestedTransaction(result, 0)

	// Only standard object should appear
	if attested.ObjectsLength() != 1 {
		t.Errorf("expected 1 object (standard only), got %d", attested.ObjectsLength())
	}

	if attested.ProofsLength() != 1 {
		t.Errorf("expected 1 proof (standard only), got %d", attested.ProofsLength())
	}
}

// TestExtractObjectRefs tests object reference extraction.
func TestExtractObjectRefs(t *testing.T) {
	vs := consensus.NewValidatorSet(nil)
	agg := &Aggregator{validators: vs}

	obj1 := [32]byte{0x01}
	obj2 := [32]byte{0x02}
	obj3 := [32]byte{0x03}

	txData := buildTestTransaction([][32]byte{obj1}, [][32]byte{obj2, obj3})
	tx := types.GetRootAsTransaction(txData, 0)

	refs := agg.extractObjectRefs(tx)

	if len(refs) != 3 {
		t.Fatalf("expected 3 refs, got %d", len(refs))
	}

	if refs[0].ID != obj1 {
		t.Error("first ref mismatch")
	}

	if refs[1].ID != obj2 {
		t.Error("second ref mismatch")
	}

	if refs[2].ID != obj3 {
		t.Error("third ref mismatch")
	}
}

// TestBuildAttestedTransactionEmpty tests building with no results.
func TestBuildAttestedTransactionEmpty(t *testing.T) {
	vs := consensus.NewValidatorSet(nil)
	agg := &Aggregator{validators: vs}

	txData := buildTestTransaction(nil, nil)

	result, err := agg.buildAttestedTransaction(txData, nil)
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	attested := types.GetRootAsAttestedTransaction(result, 0)

	if attested.ObjectsLength() != 0 {
		t.Error("expected empty objects")
	}

	if attested.ProofsLength() != 0 {
		t.Error("expected empty proofs")
	}
}

// generateTestKey generates a random ed25519 key pair for testing.
func generateTestKey(t *testing.T) ed25519.PrivateKey {
	t.Helper()

	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	return priv
}
