package attest

import (
	"encoding/binary"

	"github.com/zeebo/blake3"
)

// ComputeObjectHash returns the canonical attestation hash for an object.
// It is BLAKE3(content || version_u64_BE), where content is the object's
// content bytes (not the full FlatBuffer object bytes). Signer and verifier
// must both call this so they always agree on the message being signed.
func ComputeObjectHash(content []byte, version uint64) [32]byte {
	hasher := blake3.New()
	hasher.Write(content)

	var versionBytes [8]byte
	binary.BigEndian.PutUint64(versionBytes[:], version)
	hasher.Write(versionBytes[:])

	var hash [32]byte
	hasher.Sum(hash[:0])

	return hash
}
