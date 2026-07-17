package client

import (
	"encoding/binary"
	"fmt"

	"github.com/zeebo/blake3"

	"BluePods/internal/genesis"
)

// Unbond withdraws amount of self-stake from behind this wallet's validator
// identity, crediting it back to a singleton coin the wallet owns. The coin is
// both the credited coin (first mutable_ref) and the gas coin. Returns the tx
// hash.
func (w *Wallet) Unbond(c *Client, coinID [32]byte, coinVersion uint64, amount uint64) ([32]byte, error) {
	txBytes, txHash := w.buildCoinTx(c.systemPod, "unbond", encodeStakeArgs(amount), nil, coinID, coinVersion)

	if err := c.submit(txBytes); err != nil {
		return [32]byte{}, fmt.Errorf("submit unbond tx:\n%w", err)
	}

	return txHash, nil
}

// Delegate stakes amount from a singleton coin the wallet owns behind validator.
// The coin is both the debited coin (first mutable_ref) and the gas coin.
// Delegating creates a stake-position object owned by the wallet, whose
// deterministic ID this returns alongside the tx hash.
func (w *Wallet) Delegate(c *Client, coinID [32]byte, coinVersion uint64, validator [32]byte, amount uint64) ([32]byte, [32]byte, error) {
	args := encodeDelegateArgs(validator, amount)
	txBytes, txHash := w.buildCoinTx(c.systemPod, "delegate", args, nil, coinID, coinVersion)

	if err := c.submit(txBytes); err != nil {
		return [32]byte{}, [32]byte{}, fmt.Errorf("submit delegate tx:\n%w", err)
	}

	return delegationPositionID(w.Pubkey(), validator), txHash, nil
}

// Undelegate withdraws this wallet's delegation from validator, crediting the
// principal back to a singleton coin the wallet owns. The coin is both the
// credited coin (first mutable_ref) and the gas coin. Returns the tx hash.
func (w *Wallet) Undelegate(c *Client, coinID [32]byte, coinVersion uint64, validator [32]byte) ([32]byte, error) {
	args := encodeUndelegateArgs(validator)
	txBytes, txHash := w.buildCoinTx(c.systemPod, "undelegate", args, nil, coinID, coinVersion)

	if err := c.submit(txBytes); err != nil {
		return [32]byte{}, fmt.Errorf("submit undelegate tx:\n%w", err)
	}

	return txHash, nil
}

// Merge combines one or more source coins into a destination coin owned by the
// wallet. The destination coin is the transaction's first mutable_ref and also
// pays gas; it receives the combined balance, while every source coin is
// emptied to zero. Returns the tx hash.
func (w *Wallet) Merge(c *Client, destCoinID [32]byte, destCoinVersion uint64, sources []*CoinInfo) ([32]byte, error) {
	if len(sources) == 0 {
		return [32]byte{}, fmt.Errorf("merge requires at least one source coin")
	}

	mutableRefs := append(
		[]genesis.ObjectRefData{{ID: destCoinID, Version: destCoinVersion}},
		sourceRefs(sources)...,
	)

	txBytes, txHash := w.buildObjectTx(c.systemPod, "merge", nil, nil, mutableRefs, destCoinID)

	if err := c.submit(txBytes); err != nil {
		return [32]byte{}, fmt.Errorf("submit merge tx:\n%w", err)
	}

	return txHash, nil
}

// sourceRefs converts tracked source coins into object refs for a merge's
// mutable_refs, in the same order they are supplied.
func sourceRefs(sources []*CoinInfo) []genesis.ObjectRefData {
	refs := make([]genesis.ObjectRefData, len(sources))
	for i, s := range sources {
		refs[i] = genesis.ObjectRefData{ID: s.ID, Version: s.Version}
	}

	return refs
}

// encodeDelegateArgs encodes delegate function arguments in Borsh format.
// Format: [u8; 32] validator + u64 amount (LE).
func encodeDelegateArgs(validator [32]byte, amount uint64) []byte {
	buf := make([]byte, 40)
	copy(buf[:32], validator[:])
	binary.LittleEndian.PutUint64(buf[32:], amount)

	return buf
}

// encodeUndelegateArgs encodes undelegate function arguments in Borsh format.
// Format: [u8; 32] validator.
func encodeUndelegateArgs(validator [32]byte) []byte {
	buf := make([]byte, 32)
	copy(buf, validator[:])

	return buf
}

// delegationPositionID computes the deterministic ID of a delegator's stake
// position with validator. It mirrors internal/consensus.DelegationID exactly
// (a domain-separated blake3 hash of delegator || validator) but is duplicated
// here, following the same client-side convention as computeNewObjectID, so
// pkg/client does not pull in the much heavier internal/consensus package.
func delegationPositionID(delegator, validator [32]byte) [32]byte {
	h := blake3.New()
	_, _ = h.Write([]byte("bluepods/delegation/v1"))
	_, _ = h.Write(delegator[:])
	_, _ = h.Write(validator[:])

	var id [32]byte
	copy(id[:], h.Sum(nil))

	return id
}
