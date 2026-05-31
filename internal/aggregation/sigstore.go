package aggregation

import (
	"encoding/binary"

	"BluePods/internal/storage"
)

// sigKeyPrefix is the Pebble key prefix for stored object signatures.
const sigKeyPrefix = "objsig:"

// SigKeyPrefix is the exported key prefix for stored object signatures.
// Snapshot collection uses it to scan signature entries by prefix.
const SigKeyPrefix = sigKeyPrefix

// sigValueSize is the length of a stored signature value: version (8) + BLS signature (96).
const sigValueSize = 8 + BLSSignatureSize

// sigKey builds the storage key for an object's signature: "objsig:" || id[32].
func sigKey(id [32]byte) []byte {
	k := make([]byte, len(sigKeyPrefix)+32)
	copy(k, sigKeyPrefix)
	copy(k[len(sigKeyPrefix):], id[:])

	return k
}

// PutObjectSig stores the BLS signature for an object at the given version.
// Keying by object ID means a version advance overwrites the entry, so only
// the current-version signature is ever retained.
func PutObjectSig(db *storage.Storage, id [32]byte, version uint64, sig []byte) error {
	val := make([]byte, 8+len(sig))
	binary.BigEndian.PutUint64(val[:8], version)
	copy(val[8:], sig)

	return db.Set(sigKey(id), val)
}

// GetObjectSig returns the stored version, a fresh copy of the BLS signature,
// and true if a well-formed entry exists. The returned signature is copied out
// of the storage-owned buffer so callers may mutate it safely.
func GetObjectSig(db *storage.Storage, id [32]byte) (uint64, []byte, bool) {
	val, err := db.Get(sigKey(id))
	if err != nil || len(val) != sigValueSize {
		return 0, nil, false
	}

	out := make([]byte, BLSSignatureSize)
	copy(out, val[8:])

	return binary.BigEndian.Uint64(val[:8]), out, true
}
