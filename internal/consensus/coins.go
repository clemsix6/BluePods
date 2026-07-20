package consensus

import (
	"encoding/binary"
	"fmt"

	flatbuffers "github.com/google/flatbuffers/go"

	"BluePods/internal/state"
	"BluePods/internal/types"
)

// CoinStore provides protocol-level access to coin objects and the supply counter.
// Used for implicit fee deduction and aggregator credits without going through pod execution.
type CoinStore interface {
	// GetObject retrieves a serialized object by ID, or nil if not found.
	GetObject(id [32]byte) []byte
	// SetObject stores a serialized object.
	SetObject(data []byte)
	// DeleteObject removes an object by ID (used to destroy a delegation position
	// on undelegate, a protocol mutation outside pod execution).
	DeleteObject(id [32]byte)
	// TotalSupply returns the protocol-maintained total token supply.
	TotalSupply() uint64
	// SetTotalSupply overwrites the total supply (genesis seeding, snapshot restore).
	SetTotalSupply(supply uint64)
	// AddSupply increases the total supply (protocol issuance).
	AddSupply(amount uint64)
	// SubSupply decreases the total supply (deletion burn, future slashing).
	SubSupply(amount uint64)

	// CoinsTotal returns the protocol-maintained sum of every coin's balance.
	// Coins carry no type tag, so this cannot be recomputed by scanning objects;
	// it is maintained incrementally at every protocol coin flow instead.
	CoinsTotal() uint64
	// SetCoinsTotal overwrites coins_total (genesis seeding, snapshot restore).
	SetCoinsTotal(total uint64)
	// AddCoins increases coins_total (a credit into a coin: unbond, undelegate,
	// rewards, remainder, refunds).
	AddCoins(amount uint64)
	// SubCoins decreases coins_total (a debit out of a coin: fees/deposits, bond,
	// delegate).
	SubCoins(amount uint64)
}

// DelegationEnumerator returns the delegation positions targeting a validator.
// It is a narrow read-only contract so consensus never needs general object
// iteration on CoinStore; *state.State implements it (the consensus test stub
// implements it too, so unit tests inject a mock rather than a *state.State).
// The entry type lives in state, the package that owns object storage, so
// consensus can name the result without state importing consensus (a cycle).
type DelegationEnumerator interface {
	// DelegationsFor returns every delegation position targeting validator.
	DelegationsFor(validator [32]byte) []state.DelegationEntry
}

// readCoinBalance reads the balance from a serialized Coin object.
// Coin content is Borsh: first 8 bytes = uint64 balance (little-endian).
func readCoinBalance(data []byte) (uint64, error) {
	obj := types.GetRootAsObject(data, 0)

	content := obj.ContentBytes()
	if len(content) < 8 {
		return 0, fmt.Errorf("coin content too short: %d bytes", len(content))
	}

	return binary.LittleEndian.Uint64(content[:8]), nil
}

// readCoinOwner extracts the owner pubkey from a serialized Coin object.
func readCoinOwner(data []byte) ([32]byte, error) {
	obj := types.GetRootAsObject(data, 0)

	ownerBytes := obj.OwnerBytes()
	if len(ownerBytes) != 32 {
		return [32]byte{}, fmt.Errorf("invalid owner length: %d", len(ownerBytes))
	}

	var owner [32]byte
	copy(owner[:], ownerBytes)

	return owner, nil
}

// readCoinReplication extracts the replication from a serialized Coin object.
func readCoinReplication(data []byte) uint16 {
	obj := types.GetRootAsObject(data, 0)

	return obj.Replication()
}

// writeCoinBalance rebuilds a Coin object with a new balance.
// Preserves all other fields (ID, version, owner, replication, fees).
// Version is NOT incremented — implicit protocol modification.
func writeCoinBalance(data []byte, newBalance uint64) []byte {
	obj := types.GetRootAsObject(data, 0)
	builder := flatbuffers.NewBuilder(256)

	// Rebuild content with new balance
	content := obj.ContentBytes()
	newContent := make([]byte, len(content))
	copy(newContent, content)

	if len(newContent) < 8 {
		newContent = make([]byte, 8)
	}
	binary.LittleEndian.PutUint64(newContent[:8], newBalance)

	// Build vectors
	idVec := builder.CreateByteVector(obj.IdBytes())
	ownerVec := builder.CreateByteVector(obj.OwnerBytes())
	contentVec := builder.CreateByteVector(newContent)

	types.ObjectStart(builder)
	types.ObjectAddId(builder, idVec)
	types.ObjectAddVersion(builder, obj.Version())
	types.ObjectAddOwner(builder, ownerVec)
	types.ObjectAddReplication(builder, obj.Replication())
	types.ObjectAddContent(builder, contentVec)
	types.ObjectAddFees(builder, obj.Fees())

	offset := types.ObjectEnd(builder)
	builder.Finish(offset)

	return builder.FinishedBytes()
}

// writeObjectOwner rebuilds an object with a new owner (its parent bytes), a
// new parent kind, and the tracker's already-bumped version, preserving every
// other field (ID, replication, content, fees). A reparent rewrites the
// stored body's owner to mirror the tracker's new parent reference, restoring
// the invariant that body owner bytes equal the current parent bytes; the
// version is stamped from the caller (the tracker's post-checkAndUpdate
// version) so the held copy GetObject serves never falls behind the
// version-conflict check a follow-up mutation is validated against.
func writeObjectOwner(data []byte, newOwner Hash, newParentKind byte, newVersion uint64) []byte {
	obj := types.GetRootAsObject(data, 0)
	builder := flatbuffers.NewBuilder(256)

	idVec := builder.CreateByteVector(obj.IdBytes())
	ownerVec := builder.CreateByteVector(newOwner[:])
	contentVec := builder.CreateByteVector(obj.ContentBytes())

	types.ObjectStart(builder)
	types.ObjectAddId(builder, idVec)
	types.ObjectAddVersion(builder, newVersion)
	types.ObjectAddOwner(builder, ownerVec)
	types.ObjectAddReplication(builder, obj.Replication())
	types.ObjectAddContent(builder, contentVec)
	types.ObjectAddFees(builder, obj.Fees())
	types.ObjectAddParentKind(builder, newParentKind)

	offset := types.ObjectEnd(builder)
	builder.Finish(offset)

	return builder.FinishedBytes()
}

// deductCoinFee reads a coin, deducts the fee, and writes back.
// Returns the actual amount deducted and whether the full fee was covered.
// If balance < fee, the entire balance is taken and fullyCovered = false.
func deductCoinFee(store CoinStore, coinID [32]byte, fee uint64) (deducted uint64, fullyCovered bool, err error) {
	data := store.GetObject(coinID)
	if data == nil {
		return 0, false, fmt.Errorf("gas coin not found: %x", coinID[:8])
	}

	balance, err := readCoinBalance(data)
	if err != nil {
		return 0, false, fmt.Errorf("read gas coin balance:\n%w", err)
	}

	if balance >= fee {
		newData := writeCoinBalance(data, balance-fee)
		store.SetObject(newData)
		store.SubCoins(fee)

		return fee, true, nil
	}

	// Insufficient: take remaining balance. It still left the coin, so coins_total
	// falls by exactly what was taken, matching the pooled amount (Task 3).
	newData := writeCoinBalance(data, 0)
	store.SetObject(newData)
	store.SubCoins(balance)

	return balance, false, nil
}

// creditCoin adds amount to a coin's balance.
// Used for aggregator credits. Version is NOT incremented.
func creditCoin(store CoinStore, coinID [32]byte, amount uint64) error {
	if amount == 0 {
		return nil
	}

	data := store.GetObject(coinID)
	if data == nil {
		return fmt.Errorf("credit coin not found: %x", coinID[:8])
	}

	balance, err := readCoinBalance(data)
	if err != nil {
		return fmt.Errorf("read coin balance:\n%w", err)
	}

	// Overflow check: balance + amount must not wrap
	newBalance := balance + amount
	if newBalance < balance {
		return fmt.Errorf("credit overflow: balance=%d + amount=%d wraps", balance, amount)
	}

	newData := writeCoinBalance(data, newBalance)
	store.SetObject(newData)
	store.AddCoins(amount)

	return nil
}
