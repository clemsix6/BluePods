package consensus

import (
	"github.com/zeebo/blake3"

	"BluePods/internal/types"
)

// hashVertex computes the blake3 hash of vertex data.
func hashVertex(data []byte) Hash {
	return blake3.Sum256(data)
}

// extractProducer extracts the producer hash from a vertex.
func extractProducer(v *types.Vertex) Hash {
	var h Hash
	if b := v.ProducerBytes(); len(b) == 32 {
		copy(h[:], b)
	}
	return h
}

// extractLinkHash extracts the hash from a vertex link.
func extractLinkHash(link *types.VertexLink) Hash {
	var h Hash
	if b := link.HashBytes(); len(b) == 32 {
		copy(h[:], b)
	}
	return h
}
