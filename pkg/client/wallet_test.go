package client

import (
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

	var obj [32]byte
	obj[0] = 0x99
	w.TrackObject(obj)

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

	ids := loaded.ObjectIDs()
	if len(ids) != 1 || ids[0] != obj {
		t.Fatalf("object not persisted: %v", ids)
	}
}

func TestLoadWalletMissingFileIsError(t *testing.T) {
	if _, err := LoadWallet(filepath.Join(t.TempDir(), "absent.json")); err == nil {
		t.Fatal("expected error for missing wallet file")
	}
}
