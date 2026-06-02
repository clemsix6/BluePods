package client

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWalletSaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wallet.json")

	w := NewWallet()
	var coin [32]byte
	coin[0] = 0x42
	w.Track(coin)

	if err := w.Save(path); err != nil {
		t.Fatalf("save: %v", err)
	}

	loaded, err := LoadWallet(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if loaded.Pubkey() != w.Pubkey() {
		t.Fatalf("pubkey mismatch after reload")
	}
	if !loaded.Knows(coin) {
		t.Fatalf("coin not persisted")
	}
}

func TestLoadWalletMissingFileIsError(t *testing.T) {
	if _, err := LoadWallet(filepath.Join(t.TempDir(), "absent.json")); err == nil {
		t.Fatal("expected error for missing wallet file")
	}
	_ = os.Remove("unused")
}
