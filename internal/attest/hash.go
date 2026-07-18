package attest

import (
	"encoding/binary"

	"github.com/zeebo/blake3"
)

// ComputeObjectHash returns the canonical attestation hash for an object.
// It is BLAKE3(content || version_u64_BE || owner), where content is the
// object's content bytes (not the full FlatBuffer object bytes) and owner is the
// object's owner public key. Binding the owner makes the quorum proof cover who
// the holders attest the object belongs to: a proof collected for one owner no
// longer verifies once the owner is rewritten, so a submitter cannot present a
// legitimately-attested object under a forged owner. The owner is the only
// variable-length trailer and the version is fixed 8 bytes, so the message
// parses unambiguously. Signer and verifier must both call this so they always
// agree on the message being signed.
func ComputeObjectHash(content []byte, version uint64, owner []byte) [32]byte {
	hasher := blake3.New()
	hasher.Write(content)

	var versionBytes [8]byte
	binary.BigEndian.PutUint64(versionBytes[:], version)
	hasher.Write(versionBytes[:])

	hasher.Write(owner)

	var hash [32]byte
	hasher.Sum(hash[:0])

	return hash
}
