package client

import (
	"encoding/hex"
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

// TestWalletCheckpointRoundTrip verifies a pinned trust checkpoint survives a
// save/load cycle byte-for-byte.
func TestWalletCheckpointRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wallet.json")

	w := NewWallet()
	cp := Checkpoint{Epoch: 7, IndexRoot: [32]byte{0x11}, ValidatorSetHash: [32]byte{0x22}}
	w.SetCheckpoint(cp)

	if err := w.Save(path); err != nil {
		t.Fatalf("save: %v", err)
	}

	loaded, err := LoadWallet(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	got, ok := loaded.Checkpoint()
	if !ok {
		t.Fatal("loaded wallet reports no checkpoint")
	}
	if got != cp {
		t.Fatalf("checkpoint = %+v, want %+v", got, cp)
	}
}

// TestWalletWithoutCheckpointStaysAbsentAfterReload verifies a wallet that
// never pinned a checkpoint reports none after a save/load cycle — the
// backward-compatibility guarantee: a wallet file written before this field
// existed, or by a caller that never sets one, round-trips unaffected.
func TestWalletWithoutCheckpointStaysAbsentAfterReload(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wallet.json")

	if err := NewWallet().Save(path); err != nil {
		t.Fatalf("save: %v", err)
	}

	loaded, err := LoadWallet(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if _, ok := loaded.Checkpoint(); ok {
		t.Fatal("wallet that never set a checkpoint reports one after reload")
	}
}

// TestLoadWalletMissingCheckpointKeyIsCompatible verifies a wallet file with
// no "checkpoint" key at all — the exact shape written before this field
// existed — still loads cleanly, with Checkpoint reporting absent.
func TestLoadWalletMissingCheckpointKeyIsCompatible(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wallet.json")

	w := NewWallet()
	priv := w.privKey

	oldShape := `{"key":"` + hex.EncodeToString(priv) + `","coins":[],"objects":[]}`
	if err := os.WriteFile(path, []byte(oldShape), 0600); err != nil {
		t.Fatalf("write pre-checkpoint wallet file: %v", err)
	}

	loaded, err := LoadWallet(path)
	if err != nil {
		t.Fatalf("load pre-checkpoint wallet file: %v", err)
	}

	if _, ok := loaded.Checkpoint(); ok {
		t.Fatal("a wallet file with no checkpoint key reports one after load")
	}
}

// TestLoadWalletInvalidCheckpointHexIsError verifies a malformed persisted
// checkpoint is refused rather than silently dropped or truncated.
func TestLoadWalletInvalidCheckpointHexIsError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wallet.json")

	priv := NewWallet().privKey
	badShape := `{"key":"` + hex.EncodeToString(priv) + `","coins":[],"objects":[],` +
		`"checkpoint":{"epoch":1,"index_root":"not-hex","validator_set_hash":"` +
		hex.EncodeToString(make([]byte, 32)) + `"}}`
	if err := os.WriteFile(path, []byte(badShape), 0600); err != nil {
		t.Fatalf("write wallet file: %v", err)
	}

	if _, err := LoadWallet(path); err == nil {
		t.Fatal("expected error loading a wallet file with an invalid checkpoint root")
	}
}
