package consensus

import (
	"BluePods/internal/types"
)

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
