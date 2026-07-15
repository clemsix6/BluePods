package client

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/binary"
	"testing"

	"BluePods/internal/genesis"
	"BluePods/internal/types"
)

// TestBondTxUsesCoinAsMutableRefAndGas verifies that a bond built through
// buildCoinTx (what Wallet.Bond calls) carries the "bond" function name, the
// Borsh-encoded amount, and uses the staked coin as both its first mutable_ref
// and its gas coin.
func TestBondTxUsesCoinAsMutableRefAndGas(t *testing.T) {
	var coinID [32]byte
	coinID[0] = 0x44
	w := newGasTestWallet(coinID)

	const amount = 5000

	txBytes, _ := w.buildCoinTx(testPod(), "bond", encodeStakeArgs(amount), nil, coinID, 3)

	tx := types.GetRootAsTransaction(txBytes, 0)

	if string(tx.FunctionName()) != "bond" {
		t.Errorf("func name: got %q, want %q", tx.FunctionName(), "bond")
	}

	if got := binary.LittleEndian.Uint64(tx.ArgsBytes()); got != amount {
		t.Errorf("amount: got %d, want %d", got, uint64(amount))
	}

	if tx.MutableRefsLength() != 1 {
		t.Fatalf("mutable refs: got %d, want 1", tx.MutableRefsLength())
	}

	var ref types.ObjectRef
	tx.MutableRefs(&ref, 0)

	var gotID [32]byte
	copy(gotID[:], ref.IdBytes())
	if gotID != coinID {
		t.Errorf("mutable ref id mismatch: got %x, want %x", gotID[:8], coinID[:8])
	}

	assertGasCoinTx(t, txBytes, coinID)
}

// TestRegisterValidatorTxCarriesRewardCoinAndNoGas verifies that
// Wallet.buildRegisterValidatorTx (what RegisterValidator submits) builds a
// register_validator transaction carrying the designated reward coin, with no
// gas coin (it is a raw, gas-free transaction).
func TestRegisterValidatorTxCarriesRewardCoinAndNoGas(t *testing.T) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	w := &Wallet{privKey: priv, pubKey: priv.Public().(ed25519.PublicKey), coins: make(map[[32]byte]*CoinInfo)}

	var rewardCoin [32]byte
	rewardCoin[0] = 0x55

	txBytes := w.buildRegisterValidatorTx(testPod(), "127.0.0.1:9000", make([]byte, 48), rewardCoin)

	tx := types.GetRootAsTransaction(txBytes, 0)

	if string(tx.FunctionName()) != "register_validator" {
		t.Errorf("func name: got %q, want %q", tx.FunctionName(), "register_validator")
	}

	if len(tx.GasCoinBytes()) != 0 {
		t.Errorf("gas coin: got %x, want none (raw gas-free tx)", tx.GasCoinBytes())
	}

	got, ok := genesis.DecodeRegisterValidatorRewardCoin(tx.ArgsBytes())
	if !ok {
		t.Fatal("reward coin not present in args")
	}
	if got != rewardCoin {
		t.Errorf("reward coin: got %x, want %x", got[:8], rewardCoin[:8])
	}
}

// TestRegisterValidatorTxWithoutRewardCoin verifies a zero reward coin encodes
// as "no designation", matching genesis.DecodeRegisterValidatorRewardCoin's
// ok=false absence case.
func TestRegisterValidatorTxWithoutRewardCoin(t *testing.T) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	w := &Wallet{privKey: priv, pubKey: priv.Public().(ed25519.PublicKey), coins: make(map[[32]byte]*CoinInfo)}

	txBytes := w.buildRegisterValidatorTx(testPod(), "127.0.0.1:9000", make([]byte, 48), [32]byte{})

	tx := types.GetRootAsTransaction(txBytes, 0)

	if _, ok := genesis.DecodeRegisterValidatorRewardCoin(tx.ArgsBytes()); ok {
		t.Error("expected no reward coin designation for a zero rewardCoin")
	}
}
