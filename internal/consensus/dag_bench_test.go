package consensus

import (
	"crypto/ed25519"
	"crypto/rand"
	"os"
	"testing"

	flatbuffers "github.com/google/flatbuffers/go"

	"BluePods/internal/storage"
	"BluePods/internal/types"
)

// newBenchStorage creates a temporary storage for benchmarks.
func newBenchStorage(b *testing.B) *storage.Storage {
	b.Helper()

	dir, err := os.MkdirTemp("", "consensus_bench_*")
	if err != nil {
		b.Fatalf("failed to create temp dir: %v", err)
	}

	b.Cleanup(func() {
		os.RemoveAll(dir)
	})

	db, err := storage.New(dir)
	if err != nil {
		b.Fatalf("failed to create storage: %v", err)
	}

	b.Cleanup(func() {
		db.Close()
	})

	return db
}

func BenchmarkAddVertex(b *testing.B) {
	db := newBenchStorage(b)
	validators, vs := newBenchValidatorSet(100)
	dag := New(db, vs, nil, benchSystemPod, 1, validators[0].privKey, nil)
	defer dag.Close()

	// Pre-build vertices
	vertices := make([][]byte, b.N)
	for i := 0; i < b.N; i++ {
		v := validators[i%len(validators)]
		vertices[i] = buildBenchVertex(v, 0, nil, 1)
	}

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		_ = dag.AddVertex(vertices[i])
	}
}

func BenchmarkAddVertexParallel(b *testing.B) {
	db := newBenchStorage(b)
	validators, vs := newBenchValidatorSet(100)
	dag := New(db, vs, nil, benchSystemPod, 1, validators[0].privKey, nil)
	defer dag.Close()

	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			v := validators[i%len(validators)]
			data := buildBenchVertex(v, 0, nil, 1)
			_ = dag.AddVertex(data)
			i++
		}
	})
}

func BenchmarkHashVertex(b *testing.B) {
	validators, _ := newBenchValidatorSet(1)
	data := buildBenchVertex(validators[0], 0, nil, 1)

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		_ = hashVertex(data)
	}
}

func BenchmarkValidateSignature(b *testing.B) {
	db := newBenchStorage(b)
	validators, vs := newBenchValidatorSet(10)
	dag := New(db, vs, nil, benchSystemPod, 1, validators[0].privKey, nil)
	defer dag.Close()

	data := buildBenchVertex(validators[1], 0, nil, 1)
	vertex := types.GetRootAsVertex(data, 0)

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		_ = dag.validateSignature(vertex)
	}
}

func BenchmarkStoreAdd(b *testing.B) {
	db := newBenchStorage(b)
	s := newStore(db)
	validators, _ := newBenchValidatorSet(100)

	vertices := make([][]byte, b.N)
	hashes := make([]Hash, b.N)
	producers := make([]Hash, b.N)

	for i := 0; i < b.N; i++ {
		v := validators[i%len(validators)]
		vertices[i] = buildBenchVertex(v, uint64(i), nil, 1)
		hashes[i] = hashVertex(vertices[i])
		producers[i] = v.pubKey
	}

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		s.add(vertices[i], hashes[i], uint64(i), producers[i])
	}
}

func BenchmarkStoreGet(b *testing.B) {
	db := newBenchStorage(b)
	s := newStore(db)
	validators, _ := newBenchValidatorSet(100)

	// Pre-populate store
	hashes := make([]Hash, 10000)
	for i := 0; i < 10000; i++ {
		v := validators[i%len(validators)]
		data := buildBenchVertex(v, uint64(i), nil, 1)
		hashes[i] = hashVertex(data)
		s.add(data, hashes[i], uint64(i%100), v.pubKey)
	}

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		_ = s.get(hashes[i%len(hashes)])
	}
}

func BenchmarkStoreGetByRound(b *testing.B) {
	db := newBenchStorage(b)
	s := newStore(db)
	validators, _ := newBenchValidatorSet(100)

	// Pre-populate store with 100 rounds, 100 vertices each
	for round := uint64(0); round < 100; round++ {
		for i := 0; i < 100; i++ {
			v := validators[i%len(validators)]
			data := buildBenchVertex(v, round, nil, 1)
			hash := hashVertex(data)
			s.add(data, hash, round, v.pubKey)
		}
	}

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		_ = s.getByRound(uint64(i % 100))
	}
}

func BenchmarkValidatorSetContains(b *testing.B) {
	validators, vs := newBenchValidatorSet(1000)

	// Get a known pubkey
	pubkey := validators[500].pubKey

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		_ = vs.Contains(pubkey)
	}
}

func BenchmarkBuildVertex(b *testing.B) {
	db := newBenchStorage(b)
	validators, vs := newBenchValidatorSet(100)
	dag := New(db, vs, nil, benchSystemPod, 1, validators[0].privKey, nil)
	defer dag.Close()

	parents := make([]Hash, 67)
	for i := range parents {
		parents[i] = hashVertex([]byte{byte(i)})
	}

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		_ = dag.buildVertex(uint64(i), parents, nil)
	}
}

// benchSystemPod is a dummy system pod hash for benchmarks.
var benchSystemPod = Hash{0x01, 0x02, 0x03}

// newBenchValidatorSet creates validators for benchmarking.
func newBenchValidatorSet(n int) ([]testValidator, *ValidatorSet) {
	validators := make([]testValidator, n)
	pubkeys := make([]Hash, n)

	for i := 0; i < n; i++ {
		pub, priv, _ := ed25519.GenerateKey(rand.Reader)
		var pubHash Hash
		copy(pubHash[:], pub)

		validators[i] = testValidator{privKey: priv, pubKey: pubHash}
		pubkeys[i] = pubHash
	}

	return validators, NewValidatorSet(pubkeys)
}

// buildBenchVertex creates a vertex without t.Helper for benchmarks.
func buildBenchVertex(v testValidator, round uint64, parents []Hash, epoch uint64) []byte {
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

	types.VertexStartTransactionsVector(builder, 0)
	txsVec := builder.EndVector(0)

	producerVec := builder.CreateByteVector(v.pubKey[:])

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

	builder.Reset()

	hashVec := builder.CreateByteVector(hash[:])
	sigVec := builder.CreateByteVector(sig)
	producerVec = builder.CreateByteVector(v.pubKey[:])

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
