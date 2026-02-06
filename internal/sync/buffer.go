package sync

import (
	"sort"
	"sync"

	"github.com/zeebo/blake3"

	"BluePods/internal/types"
)

// VertexBuffer stores vertices received during sync.
// It deduplicates by hash and allows retrieval ordered by round.
type VertexBuffer struct {
	mu       sync.RWMutex
	vertices map[[32]byte]vertexEntry // hash -> entry
}

// vertexEntry holds a vertex and its metadata.
type vertexEntry struct {
	data  []byte
	round uint64
}

// NewVertexBuffer creates a new vertex buffer.
func NewVertexBuffer() *VertexBuffer {
	return &VertexBuffer{
		vertices: make(map[[32]byte]vertexEntry),
	}
}

// Add stores a vertex in the buffer.
// Returns true if the vertex was new, false if it was a duplicate.
func (b *VertexBuffer) Add(data []byte) bool {
	hash := blake3.Sum256(data)

	b.mu.Lock()
	defer b.mu.Unlock()

	if _, exists := b.vertices[hash]; exists {
		return false
	}

	// Parse to get round
	vertex := types.GetRootAsVertex(data, 0)
	round := vertex.Round()

	// Copy data to avoid external mutation
	dataCopy := make([]byte, len(data))
	copy(dataCopy, data)

	b.vertices[hash] = vertexEntry{
		data:  dataCopy,
		round: round,
	}

	return true
}

// Len returns the number of vertices in the buffer.
func (b *VertexBuffer) Len() int {
	b.mu.RLock()
	defer b.mu.RUnlock()

	return len(b.vertices)
}

// GetSince returns all vertices with round >= minRound, ordered by round.
func (b *VertexBuffer) GetSince(minRound uint64) [][]byte {
	b.mu.RLock()
	defer b.mu.RUnlock()

	// Collect matching vertices
	var entries []vertexEntry
	for _, entry := range b.vertices {
		if entry.round >= minRound {
			entries = append(entries, entry)
		}
	}

	// Sort by round
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].round < entries[j].round
	})

	// Extract data
	result := make([][]byte, len(entries))
	for i, entry := range entries {
		result[i] = entry.data
	}

	return result
}

// GetAll returns all vertices ordered by round.
func (b *VertexBuffer) GetAll() [][]byte {
	return b.GetSince(0)
}

// Clear removes all vertices from the buffer.
func (b *VertexBuffer) Clear() {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.vertices = make(map[[32]byte]vertexEntry)
}

// MinRound returns the lowest round in the buffer, or 0 if empty.
func (b *VertexBuffer) MinRound() uint64 {
	b.mu.RLock()
	defer b.mu.RUnlock()

	if len(b.vertices) == 0 {
		return 0
	}

	min := uint64(^uint64(0)) // max uint64
	for _, entry := range b.vertices {
		if entry.round < min {
			min = entry.round
		}
	}

	return min
}

// MaxRound returns the highest round in the buffer, or 0 if empty.
func (b *VertexBuffer) MaxRound() uint64 {
	b.mu.RLock()
	defer b.mu.RUnlock()

	var max uint64
	for _, entry := range b.vertices {
		if entry.round > max {
			max = entry.round
		}
	}

	return max
}
