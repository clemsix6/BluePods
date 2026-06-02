package consensus

import (
	"encoding/binary"

	flatbuffers "github.com/google/flatbuffers/go"
	"github.com/zeebo/blake3"

	"BluePods/internal/types"
)

// delegationContentSize is the serialized length of a delegation position's
// content: validator pubkey (32) followed by the delegated amount (8 LE).
const delegationContentSize = 32 + 8

// DelegationID derives the deterministic ID of a delegator's stake position with
// a validator. There is no per-position counter: a delegator holds at most one
// position per validator, so the ID is a domain-separated hash of the pair.
func DelegationID(delegator, validator [32]byte) [32]byte {
	h := blake3.New()
	_, _ = h.Write([]byte("bluepods/delegation/v1"))
	_, _ = h.Write(delegator[:])
	_, _ = h.Write(validator[:])

	var id [32]byte
	copy(id[:], h.Sum(nil))

	return id
}

// encodeDelegationContent serializes a delegation position's content:
// validator(32) followed by amount(8, little-endian).
func encodeDelegationContent(validator [32]byte, amount uint64) []byte {
	content := make([]byte, delegationContentSize)
	copy(content[:32], validator[:])
	binary.LittleEndian.PutUint64(content[32:], amount)

	return content
}

// decodeDelegationContent parses a delegation position's content into its
// validator and amount. ok is false when the content is not exactly the
// expected length (so non-delegation objects are filtered out cleanly).
func decodeDelegationContent(content []byte) (validator [32]byte, amount uint64, ok bool) {
	if len(content) != delegationContentSize {
		return validator, 0, false
	}

	copy(validator[:], content[:32])
	amount = binary.LittleEndian.Uint64(content[32:])

	return validator, amount, true
}

// buildDelegationObject constructs the serialized stake-position Object owned by
// the delegator. Its ID is deterministic in the (delegator, validator) pair and
// its content carries the target validator and the delegated amount. Version is
// 0: the position is protocol-managed, not version-tracked like a coin.
func buildDelegationObject(delegator, validator [32]byte, amount uint64) []byte {
	id := DelegationID(delegator, validator)
	content := encodeDelegationContent(validator, amount)

	b := flatbuffers.NewBuilder(256)
	idVec := b.CreateByteVector(id[:])
	ownerVec := b.CreateByteVector(delegator[:])
	contentVec := b.CreateByteVector(content)

	types.ObjectStart(b)
	types.ObjectAddId(b, idVec)
	types.ObjectAddVersion(b, 0)
	types.ObjectAddOwner(b, ownerVec)
	types.ObjectAddReplication(b, 0)
	types.ObjectAddContent(b, contentVec)
	types.ObjectAddFees(b, 0)
	b.Finish(types.ObjectEnd(b))

	return b.FinishedBytes()
}
