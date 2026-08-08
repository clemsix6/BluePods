package client

import (
	"crypto/ed25519"
	"crypto/rand"
	"testing"

	"BluePods/internal/types"
)

// newGasTestWallet returns a wallet tracking a single owned coin so coin
// operations have a known gas coin (the operated coin itself).
func newGasTestWallet(coinID [32]byte) *Wallet {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)

	w := &Wallet{
		privKey: priv,
		pubKey:  priv.Public().(ed25519.PublicKey),
		coins:   make(map[[32]byte]*CoinInfo),
	}
	w.coins[coinID] = &CoinInfo{ID: coinID, Version: 3, Balance: 1_000_000}

	return w
}

// assertGasCoinTx asserts a built tx carries a 32-byte gas coin and a max_gas at
// or above the fee system's minimum.
func assertGasCoinTx(t *testing.T, txBytes []byte, wantGasCoin [32]byte) {
	t.Helper()

	tx := types.GetRootAsTransaction(txBytes, 0)

	gc := tx.GasCoinBytes()
	if len(gc) != 32 {
		t.Fatalf("gas coin length: got %d, want 32", len(gc))
	}

	var got [32]byte
	copy(got[:], gc)
	if got != wantGasCoin {
		t.Errorf("gas coin mismatch: got %x, want %x", got[:8], wantGasCoin[:8])
	}

	if tx.MaxGas() < clientMaxGas {
		t.Errorf("max_gas %d below client default %d", tx.MaxGas(), clientMaxGas)
	}

	if clientMaxGas < minClientGas {
		t.Fatalf("client default gas %d below MinGas %d", clientMaxGas, minClientGas)
	}
}

// TestSplitTxCarriesOperatedCoinAsGas verifies a split uses the operated coin as
// its own gas coin.
func TestSplitTxCarriesOperatedCoinAsGas(t *testing.T) {
	var coinID [32]byte
	coinID[0] = 0x11
	w := newGasTestWallet(coinID)

	var recipient [32]byte
	txBytes, _ := w.buildCoinTx(testPod(), "split", encodeSplitArgs(1000, recipient), []uint16{0}, coinID, 3)

	assertGasCoinTx(t, txBytes, coinID)
}

// TestTransferTxCarriesOperatedCoinAsGas verifies a transfer (the protocol's
// declared reparent operation, not a pod call) uses the operated coin as its
// own gas coin.
func TestTransferTxCarriesOperatedCoinAsGas(t *testing.T) {
	var coinID [32]byte
	coinID[0] = 0x22
	w := newGasTestWallet(coinID)

	var recipient [32]byte
	op := reparentOpFor(coinID, keyRootKind, recipient[:])
	txBytes, _ := w.buildOpsTx(buildMutableRef(coinID, 3), coinID, op)

	assertGasCoinTx(t, txBytes, coinID)
}

// TestCreateObjectTxUsesSuppliedGasCoin verifies an object op carries the
// caller-supplied gas coin.
func TestCreateObjectTxUsesSuppliedGasCoin(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	w := &Wallet{privKey: priv, pubKey: priv.Public().(ed25519.PublicKey), coins: make(map[[32]byte]*CoinInfo)}

	var gasCoin [32]byte
	gasCoin[0] = 0x33

	args := encodeCreateObjectArgs(w.Pubkey(), 5, []byte("meta"))
	txBytes, _ := w.buildObjectTx(testPod(), "create_object", args, []uint16{5}, nil, gasCoin)

	assertGasCoinTx(t, txBytes, gasCoin)
}

// testPod returns a deterministic system-pod ID for the gas tests.
func testPod() [32]byte {
	var pod [32]byte
	copy(pod[:], []byte("system_pod_id_for_gas_testing_99"))
	return pod
}
