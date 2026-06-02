package genesis

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"testing"

	"BluePods/internal/types"
)

// TestBuildSplitTx confirms the faucet split ATX targets the reserve coin as a
// mutable ref and as its gas coin, calls the system pod's "split" function, and
// carries a 32-byte gas coin reference.
func TestBuildSplitTx(t *testing.T) {
	_, privKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	var systemPod [32]byte
	systemPod[0] = 0x11

	var reserveCoinID [32]byte
	reserveCoinID[0] = 0x22

	var toOwner [32]byte
	toOwner[0] = 0x33

	atxBytes := BuildSplitTx(privKey, systemPod, reserveCoinID, 3, toOwner, 500)

	atx := types.GetRootAsAttestedTransaction(atxBytes, 0)
	tx := atx.Transaction(nil)
	if tx == nil {
		t.Fatal("missing transaction")
	}

	if string(tx.FunctionName()) != "split" {
		t.Errorf("function name: got %q, want \"split\"", tx.FunctionName())
	}

	if gc := tx.GasCoinBytes(); len(gc) != 32 || !bytes.Equal(gc, reserveCoinID[:]) {
		t.Errorf("gas coin: got %x, want %x", gc, reserveCoinID[:])
	}

	if tx.MutableRefsLength() != 1 {
		t.Fatalf("mutable refs: got %d, want 1", tx.MutableRefsLength())
	}

	var ref types.ObjectRef
	tx.MutableRefs(&ref, 0)
	if !bytes.Equal(ref.IdBytes(), reserveCoinID[:]) {
		t.Errorf("mutable ref id: got %x, want %x", ref.IdBytes(), reserveCoinID[:])
	}

	if ref.Version() != 3 {
		t.Errorf("mutable ref version: got %d, want 3", ref.Version())
	}
}
