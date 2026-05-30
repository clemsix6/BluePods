package attest

import "testing"

// TestComputeObjectHash tests hash computation consistency.
func TestComputeObjectHash(t *testing.T) {
	content := []byte("test content for hashing")
	version := uint64(42)

	hash1 := ComputeObjectHash(content, version)
	hash2 := ComputeObjectHash(content, version)

	// Same input should produce same hash
	if hash1 != hash2 {
		t.Error("hash not deterministic")
	}

	// Different version should produce different hash
	hash3 := ComputeObjectHash(content, version+1)
	if hash1 == hash3 {
		t.Error("different version should produce different hash")
	}

	// Different content should produce different hash
	hash4 := ComputeObjectHash([]byte("different content"), version)
	if hash1 == hash4 {
		t.Error("different content should produce different hash")
	}
}
