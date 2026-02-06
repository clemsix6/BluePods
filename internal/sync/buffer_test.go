package sync

import (
	"testing"

	flatbuffers "github.com/google/flatbuffers/go"

	"BluePods/internal/types"
)

// buildTestVertex creates a minimal vertex for testing.
func buildTestVertex(round uint64) []byte {
	builder := flatbuffers.NewBuilder(256)

	// Empty vectors
	hashOffset := builder.CreateByteVector(make([]byte, 32))
	producerOffset := builder.CreateByteVector(make([]byte, 32))
	sigOffset := builder.CreateByteVector(make([]byte, 64))

	types.VertexStart(builder)
	types.VertexAddHash(builder, hashOffset)
	types.VertexAddRound(builder, round)
	types.VertexAddProducer(builder, producerOffset)
	types.VertexAddSignature(builder, sigOffset)
	types.VertexAddEpoch(builder, 0)
	offset := types.VertexEnd(builder)
	builder.Finish(offset)

	return builder.FinishedBytes()
}

func TestVertexBuffer_Add(t *testing.T) {
	buf := NewVertexBuffer()

	v1 := buildTestVertex(1)
	v2 := buildTestVertex(2)

	// First add should succeed
	if !buf.Add(v1) {
		t.Error("first add should return true")
	}

	// Second add of same vertex should return false (duplicate)
	if buf.Add(v1) {
		t.Error("duplicate add should return false")
	}

	// Different vertex should succeed
	if !buf.Add(v2) {
		t.Error("different vertex add should return true")
	}

	if buf.Len() != 2 {
		t.Errorf("len = %d, want 2", buf.Len())
	}
}

func TestVertexBuffer_GetSince(t *testing.T) {
	buf := NewVertexBuffer()

	// Add vertices for rounds 5, 10, 15, 20
	buf.Add(buildTestVertex(10))
	buf.Add(buildTestVertex(5))
	buf.Add(buildTestVertex(20))
	buf.Add(buildTestVertex(15))

	// Get since round 10
	vertices := buf.GetSince(10)

	if len(vertices) != 3 {
		t.Fatalf("got %d vertices, want 3", len(vertices))
	}

	// Should be ordered by round
	rounds := make([]uint64, len(vertices))
	for i, data := range vertices {
		v := types.GetRootAsVertex(data, 0)
		rounds[i] = v.Round()
	}

	expected := []uint64{10, 15, 20}
	for i, r := range rounds {
		if r != expected[i] {
			t.Errorf("vertex %d: round = %d, want %d", i, r, expected[i])
		}
	}
}

func TestVertexBuffer_GetAll(t *testing.T) {
	buf := NewVertexBuffer()

	buf.Add(buildTestVertex(3))
	buf.Add(buildTestVertex(1))
	buf.Add(buildTestVertex(2))

	vertices := buf.GetAll()

	if len(vertices) != 3 {
		t.Fatalf("got %d vertices, want 3", len(vertices))
	}

	// Should be ordered
	for i, data := range vertices {
		v := types.GetRootAsVertex(data, 0)
		if v.Round() != uint64(i+1) {
			t.Errorf("vertex %d: round = %d, want %d", i, v.Round(), i+1)
		}
	}
}

func TestVertexBuffer_Clear(t *testing.T) {
	buf := NewVertexBuffer()

	buf.Add(buildTestVertex(1))
	buf.Add(buildTestVertex(2))

	if buf.Len() != 2 {
		t.Fatalf("len before clear = %d, want 2", buf.Len())
	}

	buf.Clear()

	if buf.Len() != 0 {
		t.Errorf("len after clear = %d, want 0", buf.Len())
	}
}

func TestVertexBuffer_MinMaxRound(t *testing.T) {
	buf := NewVertexBuffer()

	// Empty buffer
	if buf.MinRound() != 0 {
		t.Errorf("empty min = %d, want 0", buf.MinRound())
	}
	if buf.MaxRound() != 0 {
		t.Errorf("empty max = %d, want 0", buf.MaxRound())
	}

	buf.Add(buildTestVertex(10))
	buf.Add(buildTestVertex(5))
	buf.Add(buildTestVertex(20))

	if buf.MinRound() != 5 {
		t.Errorf("min = %d, want 5", buf.MinRound())
	}
	if buf.MaxRound() != 20 {
		t.Errorf("max = %d, want 20", buf.MaxRound())
	}
}

func TestVertexBuffer_ConcurrentAccess(t *testing.T) {
	buf := NewVertexBuffer()
	done := make(chan bool)

	// Writer goroutine
	go func() {
		for i := 0; i < 100; i++ {
			buf.Add(buildTestVertex(uint64(i)))
		}
		done <- true
	}()

	// Reader goroutine
	go func() {
		for i := 0; i < 100; i++ {
			_ = buf.Len()
			_ = buf.GetAll()
		}
		done <- true
	}()

	<-done
	<-done

	// Should not panic and have all vertices
	if buf.Len() != 100 {
		t.Errorf("len = %d, want 100", buf.Len())
	}
}
