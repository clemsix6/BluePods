package attest

import "testing"

// TestComputeObjectHash tests hash computation consistency.
func TestComputeObjectHash(t *testing.T) {
	content := []byte("test content for hashing")
	version := uint64(42)
	owner := []byte("owner-public-key-32-bytes-padxxx")

	hash1 := ComputeObjectHash(content, version, owner)
	hash2 := ComputeObjectHash(content, version, owner)

	// Same input should produce same hash
	if hash1 != hash2 {
		t.Error("hash not deterministic")
	}

	// Different version should produce different hash
	hash3 := ComputeObjectHash(content, version+1, owner)
	if hash1 == hash3 {
		t.Error("different version should produce different hash")
	}

	// Different content should produce different hash
	hash4 := ComputeObjectHash([]byte("different content"), version, owner)
	if hash1 == hash4 {
		t.Error("different content should produce different hash")
	}

	// Different owner should produce different hash: this is what makes a
	// quorum proof reject an object whose owner was rewritten.
	hash5 := ComputeObjectHash(content, version, []byte("another-owner-public-key-32-byte"))
	if hash1 == hash5 {
		t.Error("different owner should produce different hash")
	}
}
