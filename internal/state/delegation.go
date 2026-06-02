package state

import (
	"bytes"
	"encoding/binary"

	"BluePods/internal/types"
)

// delegationContentSize is the serialized length of a delegation position's
// content: validator pubkey (32) followed by the delegated amount (8 LE). It
// mirrors the consensus codec layout; the exact length is the decode's filter.
const delegationContentSize = 32 + 8

// DelegationEntry is one delegator's stake position with a validator, as
// enumerated for the epoch-boundary reward split. It is the result type of the
// consensus DelegationEnumerator contract (defined here so consensus depends on
// this data type without state ever importing consensus, which would cycle).
type DelegationEntry struct {
	Delegator [32]byte // Delegator is the position owner.
	Amount    uint64   // Amount is the delegated stake.
}

// consensusKeyPrefixes are the storage prefixes that hold non-object data and
// must be skipped when scanning for delegation-position objects. Objects use a
// bare 32-byte key, so a key is an object candidate only when it is 32 bytes
// long and carries none of these prefixes.
var consensusKeyPrefixes = [][]byte{
	[]byte("v:"), []byte("r:"), []byte("m:"), []byte("t:"), []byte("d:"), []byte("objsig:"),
}

// DelegationsFor returns every delegation position targeting validator. It scans
// the object store, decoding each 32-byte-keyed object whose content matches the
// delegation layout and whose target is validator. It satisfies the consensus
// DelegationEnumerator contract used by the epoch-boundary reward split.
// TODO: index per validator when delegation count grows.
func (s *State) DelegationsFor(validator [32]byte) []DelegationEntry {
	var entries []DelegationEntry

	_ = s.db.Iterate(func(key, value []byte) error {
		if !isObjectKey(key) {
			return nil
		}

		if entry, ok := decodeDelegationPosition(value, validator); ok {
			entries = append(entries, entry)
		}

		return nil
	})

	return entries
}

// isObjectKey reports whether a storage key addresses an object (a bare 32-byte
// key with no consensus-data prefix).
func isObjectKey(key []byte) bool {
	if len(key) != 32 {
		return false
	}

	for _, prefix := range consensusKeyPrefixes {
		if bytes.HasPrefix(key, prefix) {
			return false
		}
	}

	return true
}

// decodeDelegationPosition decodes a delegation position from serialized object
// bytes, returning its entry when the content matches the layout and targets
// validator. ok is false for any other object.
func decodeDelegationPosition(value []byte, validator [32]byte) (DelegationEntry, bool) {
	obj := types.GetRootAsObject(value, 0)

	content := obj.ContentBytes()
	if len(content) != delegationContentSize {
		return DelegationEntry{}, false
	}

	if !bytes.Equal(content[:32], validator[:]) {
		return DelegationEntry{}, false
	}

	owner := obj.OwnerBytes()
	if len(owner) != 32 {
		return DelegationEntry{}, false
	}

	var entry DelegationEntry
	copy(entry.Delegator[:], owner)
	entry.Amount = binary.LittleEndian.Uint64(content[32:40])

	return entry, true
}
