package client

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/binary"
	"testing"

	"BluePods/internal/consensus"
	"BluePods/internal/genesis"
	"BluePods/internal/types"
)

// =============================================================================
// Borsh Encoding Tests
// =============================================================================

// TestEncodeDelegateArgs verifies delegate args encode to 40 bytes:
// 32-byte validator followed by u64 LE amount.
func TestEncodeDelegateArgs(t *testing.T) {
	var validator [32]byte
	for i := range validator {
		validator[i] = byte(i + 1)
	}

	args := encodeDelegateArgs(validator, 7500)

	if len(args) != 40 {
		t.Fatalf("expected 40 bytes, got %d", len(args))
	}

	var decoded [32]byte
	copy(decoded[:], args[:32])
	if decoded != validator {
		t.Error("validator mismatch in encoded args")
	}

	amount := binary.LittleEndian.Uint64(args[32:])
	if amount != 7500 {
		t.Errorf("expected amount 7500, got %d", amount)
	}
}

// TestEncodeUndelegateArgs verifies undelegate args encode to 32 bytes = validator pubkey.
func TestEncodeUndelegateArgs(t *testing.T) {
	var validator [32]byte
	for i := range validator {
		validator[i] = byte(0xBB + i)
	}

	args := encodeUndelegateArgs(validator)

	if len(args) != 32 {
		t.Fatalf("expected 32 bytes, got %d", len(args))
	}

	var decoded [32]byte
	copy(decoded[:], args)
	if decoded != validator {
		t.Error("validator mismatch in encoded args")
	}
}

// TestDelegationPositionIDMatchesConsensus verifies the client's local
// delegationPositionID reproduces internal/consensus.DelegationID exactly, so
// a Delegate's returned position ID always matches what commit.go derives
// Go-side. The two must never drift.
func TestDelegationPositionIDMatchesConsensus(t *testing.T) {
	var delegator, validator [32]byte
	for i := range delegator {
		delegator[i] = byte(i + 1)
		validator[i] = byte(255 - i)
	}

	got := delegationPositionID(delegator, validator)
	want := consensus.DelegationID(delegator, validator)

	if got != want {
		t.Errorf("delegationPositionID mismatch: got %x, want %x", got[:8], want[:8])
	}
}

// =============================================================================
// Transaction Construction Tests
// =============================================================================

// TestUnbondTxUsesCoinAsMutableRefAndGas verifies that an unbond built through
// buildCoinTx (what Wallet.Unbond calls) carries the "unbond" function name,
// the Borsh-encoded amount, and uses the credited coin as both its first
// mutable_ref and its gas coin.
func TestUnbondTxUsesCoinAsMutableRefAndGas(t *testing.T) {
	var coinID [32]byte
	coinID[0] = 0x66
	w := newGasTestWallet(coinID)

	const amount = 2500

	txBytes, _ := w.buildCoinTx(testPod(), "unbond", encodeStakeArgs(amount), nil, coinID, 4)

	tx := types.GetRootAsTransaction(txBytes, 0)

	if string(tx.FunctionName()) != "unbond" {
		t.Errorf("func name: got %q, want %q", tx.FunctionName(), "unbond")
	}

	if got := binary.LittleEndian.Uint64(tx.ArgsBytes()); got != amount {
		t.Errorf("amount: got %d, want %d", got, uint64(amount))
	}

	assertGasCoinTx(t, txBytes, coinID)
}

// TestDelegateTxUsesCoinAsMutableRefAndGas verifies that a delegate built
// through buildCoinTx (what Wallet.Delegate calls) carries the "delegate"
// function name, the Borsh-encoded validator and amount, and uses the debited
// coin as both its first mutable_ref and its gas coin.
func TestDelegateTxUsesCoinAsMutableRefAndGas(t *testing.T) {
	var coinID, validator [32]byte
	coinID[0] = 0x77
	validator[0] = 0x88
	w := newGasTestWallet(coinID)

	txBytes, _ := w.buildCoinTx(testPod(), "delegate", encodeDelegateArgs(validator, 3000), nil, coinID, 2)

	tx := types.GetRootAsTransaction(txBytes, 0)

	if string(tx.FunctionName()) != "delegate" {
		t.Errorf("func name: got %q, want %q", tx.FunctionName(), "delegate")
	}

	var gotValidator [32]byte
	copy(gotValidator[:], tx.ArgsBytes()[:32])
	if gotValidator != validator {
		t.Errorf("validator mismatch: got %x, want %x", gotValidator[:8], validator[:8])
	}

	assertGasCoinTx(t, txBytes, coinID)
}

// TestUndelegateTxUsesCoinAsMutableRefAndGas verifies that an undelegate built
// through buildCoinTx (what Wallet.Undelegate calls) carries the "undelegate"
// function name, the Borsh-encoded validator, and uses the credited coin as
// both its first mutable_ref and its gas coin.
func TestUndelegateTxUsesCoinAsMutableRefAndGas(t *testing.T) {
	var coinID, validator [32]byte
	coinID[0] = 0x99
	validator[0] = 0xAA
	w := newGasTestWallet(coinID)

	txBytes, _ := w.buildCoinTx(testPod(), "undelegate", encodeUndelegateArgs(validator), nil, coinID, 1)

	tx := types.GetRootAsTransaction(txBytes, 0)

	if string(tx.FunctionName()) != "undelegate" {
		t.Errorf("func name: got %q, want %q", tx.FunctionName(), "undelegate")
	}

	var gotValidator [32]byte
	copy(gotValidator[:], tx.ArgsBytes())
	if gotValidator != validator {
		t.Errorf("validator mismatch: got %x, want %x", gotValidator[:8], validator[:8])
	}

	assertGasCoinTx(t, txBytes, coinID)
}

// TestMergeTxOrdersDestinationFirstAndUsesItAsGas verifies that a merge built
// through buildObjectTx (what Wallet.Merge calls) carries the "merge" function
// name, places the destination coin as the first mutable_ref followed by the
// source coins in order, and uses the destination coin as gas.
func TestMergeTxOrdersDestinationFirstAndUsesItAsGas(t *testing.T) {
	var dest, src1, src2 [32]byte
	dest[0] = 0x10
	src1[0] = 0x20
	src2[0] = 0x30

	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	mutableRefs := []genesis.ObjectRefData{
		{ID: dest, Version: 9},
		{ID: src1, Version: 1},
		{ID: src2, Version: 4},
	}

	txBytes, _ := buildSignedGasTx(priv, testPod(), "merge", nil, nil, mutableRefs, nil, dest)

	tx := types.GetRootAsTransaction(txBytes, 0)

	if string(tx.FunctionName()) != "merge" {
		t.Errorf("func name: got %q, want %q", tx.FunctionName(), "merge")
	}

	if tx.MutableRefsLength() != 3 {
		t.Fatalf("mutable refs: got %d, want 3", tx.MutableRefsLength())
	}

	wantOrder := [][32]byte{dest, src1, src2}
	var ref types.ObjectRef
	for i, want := range wantOrder {
		tx.MutableRefs(&ref, i)

		var got [32]byte
		copy(got[:], ref.IdBytes())
		if got != want {
			t.Errorf("mutable ref %d: got %x, want %x", i, got[:8], want[:8])
		}
	}

	assertGasCoinTx(t, txBytes, dest)
}

// TestSourceRefsPreservesOrder verifies sourceRefs converts tracked coins into
// object refs in the same order they were supplied.
func TestSourceRefsPreservesOrder(t *testing.T) {
	var id1, id2 [32]byte
	id1[0] = 0x01
	id2[0] = 0x02

	sources := []*CoinInfo{
		{ID: id1, Version: 3},
		{ID: id2, Version: 5},
	}

	refs := sourceRefs(sources)

	if len(refs) != 2 {
		t.Fatalf("expected 2 refs, got %d", len(refs))
	}
	if refs[0].ID != id1 || refs[0].Version != 3 {
		t.Errorf("ref 0 mismatch: %+v", refs[0])
	}
	if refs[1].ID != id2 || refs[1].Version != 5 {
		t.Errorf("ref 1 mismatch: %+v", refs[1])
	}
}

// TestMergeRejectsNoSources verifies Merge refuses to build a transaction with
// zero source coins, without needing a live *Client (the guard runs first).
func TestMergeRejectsNoSources(t *testing.T) {
	var coinID [32]byte
	coinID[0] = 0x40
	w := newGasTestWallet(coinID)

	if _, err := w.Merge(nil, coinID, 1, nil); err == nil {
		t.Error("expected error for merge with no source coins")
	}
}
