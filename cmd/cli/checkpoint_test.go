package main

import (
	"encoding/hex"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"BluePods/pkg/client"
)

// checkpointFlagValue builds a well-formed --checkpoint flag value for the
// given epoch, with a recognizable leading byte in each root so a test can
// tell them apart after a round trip.
func checkpointFlagValue(epoch uint64, indexRootByte, validatorSetHashByte byte) string {
	var indexRoot, validatorSetHash [32]byte
	indexRoot[0] = indexRootByte
	validatorSetHash[0] = validatorSetHashByte

	return strings.Join([]string{
		strconv.FormatUint(epoch, 10),
		hex.EncodeToString(indexRoot[:]),
		hex.EncodeToString(validatorSetHash[:]),
	}, ":")
}

// TestParseCheckpointFlagValid verifies the 3-component <epoch>:<indexRoot
// hex>:<validatorSetHash hex> shape parses into the matching Checkpoint.
func TestParseCheckpointFlagValid(t *testing.T) {
	value := checkpointFlagValue(42, 0xAA, 0xBB)

	cp, err := parseCheckpointFlag(value)
	if err != nil {
		t.Fatalf("parse %q: %v", value, err)
	}

	if cp.Epoch != 42 {
		t.Errorf("epoch = %d, want 42", cp.Epoch)
	}
	if cp.IndexRoot[0] != 0xAA {
		t.Errorf("index root[0] = %x, want AA", cp.IndexRoot[0])
	}
	if cp.ValidatorSetHash[0] != 0xBB {
		t.Errorf("validator set hash[0] = %x, want BB", cp.ValidatorSetHash[0])
	}
}

// TestParseCheckpointFlagRejectsTwoComponents verifies the joiner's own
// <epoch>:<root hex> shape (see cmd/node/checkpoint.go) is refused: a light
// client checkpoint carries three fields, not two, and accepting the shorter
// form silently would leave the index root unpinned.
func TestParseCheckpointFlagRejectsTwoComponents(t *testing.T) {
	value := "42:" + hex.EncodeToString(make([]byte, 32))

	if _, err := parseCheckpointFlag(value); err == nil {
		t.Fatalf("parse %q: expected an error for a 2-component value", value)
	}
}

// TestParseCheckpointFlagRejectsNonNumericEpoch verifies a non-numeric epoch
// field is refused rather than silently truncated.
func TestParseCheckpointFlagRejectsNonNumericEpoch(t *testing.T) {
	value := "not-a-number:" + hex.EncodeToString(make([]byte, 32)) + ":" + hex.EncodeToString(make([]byte, 32))

	if _, err := parseCheckpointFlag(value); err == nil {
		t.Fatal("expected an error for a non-numeric epoch")
	}
}

// TestParseCheckpointFlagRejectsInvalidHex verifies a non-hex root is
// refused.
func TestParseCheckpointFlagRejectsInvalidHex(t *testing.T) {
	value := "1:not-hex:" + hex.EncodeToString(make([]byte, 32))

	if _, err := parseCheckpointFlag(value); err == nil {
		t.Fatal("expected an error for a non-hex index root")
	}
}

// TestParseCheckpointFlagRejectsWrongLength verifies a root that is not
// exactly 32 bytes is refused instead of silently zero-padded or truncated.
func TestParseCheckpointFlagRejectsWrongLength(t *testing.T) {
	value := "1:aabb:" + hex.EncodeToString(make([]byte, 32))

	if _, err := parseCheckpointFlag(value); err == nil {
		t.Fatal("expected an error for a short index root")
	}
}

// TestLoadOrBuildWalletAbsentCheckpoint verifies a fresh key with no wallet
// file and no --checkpoint flag builds a wallet reporting no checkpoint —
// the ordinary case, unaffected by this feature.
func TestLoadOrBuildWalletAbsentCheckpoint(t *testing.T) {
	dir := t.TempDir()
	e := &env{keyPath: filepath.Join(dir, "key")}

	w, err := loadOrBuildWallet(e, walletFilePath(e))
	if err != nil {
		t.Fatalf("loadOrBuildWallet: %v", err)
	}

	if _, ok := w.Checkpoint(); ok {
		t.Fatal("fresh wallet with no flag reports a checkpoint")
	}
}

// TestLoadOrBuildWalletReloadsStoredCheckpoint verifies a checkpoint saved on
// one invocation is present on the next without the flag being repeated.
func TestLoadOrBuildWalletReloadsStoredCheckpoint(t *testing.T) {
	dir := t.TempDir()
	e := &env{keyPath: filepath.Join(dir, "key")}
	walletPath := walletFilePath(e)

	stored := client.Checkpoint{Epoch: 3, IndexRoot: [32]byte{0x01}, ValidatorSetHash: [32]byte{0x02}}
	w := client.NewWallet()
	w.SetCheckpoint(stored)
	if err := w.Save(walletPath); err != nil {
		t.Fatalf("save wallet: %v", err)
	}

	reloaded, err := loadOrBuildWallet(e, walletPath)
	if err != nil {
		t.Fatalf("loadOrBuildWallet: %v", err)
	}

	got, ok := reloaded.Checkpoint()
	if !ok || got != stored {
		t.Fatalf("checkpoint = %+v, ok=%v, want %+v", got, ok, stored)
	}
}

// TestLoadOrBuildWalletFlagOverridesStored verifies a --checkpoint flag given
// on this invocation overwrites whatever checkpoint the wallet file already
// carries — the deliberate rotation the spec calls for, never a merge.
func TestLoadOrBuildWalletFlagOverridesStored(t *testing.T) {
	dir := t.TempDir()
	e := &env{keyPath: filepath.Join(dir, "key")}
	walletPath := walletFilePath(e)

	stored := client.Checkpoint{Epoch: 3, IndexRoot: [32]byte{0x01}, ValidatorSetHash: [32]byte{0x02}}
	w := client.NewWallet()
	w.SetCheckpoint(stored)
	if err := w.Save(walletPath); err != nil {
		t.Fatalf("save wallet: %v", err)
	}

	fresh := client.Checkpoint{Epoch: 9, IndexRoot: [32]byte{0x03}, ValidatorSetHash: [32]byte{0x04}}
	e.checkpointFlag = &fresh

	reloaded, err := loadOrBuildWallet(e, walletPath)
	if err != nil {
		t.Fatalf("loadOrBuildWallet: %v", err)
	}

	got, ok := reloaded.Checkpoint()
	if !ok || got != fresh {
		t.Fatalf("checkpoint = %+v, ok=%v, want the flag value %+v (stored was %+v)", got, ok, fresh, stored)
	}
}

// TestSyncCheckpointPersistsAdvance verifies the checkpoint a LightClient
// re-pinned is written back to the wallet file, so the pin advances with the
// chain rather than staying fixed at whatever the flag first supplied.
func TestSyncCheckpointPersistsAdvance(t *testing.T) {
	dir := t.TempDir()
	e := &env{keyPath: filepath.Join(dir, "key")}
	walletPath := walletFilePath(e)

	w := client.NewWallet()
	advanced := client.Checkpoint{Epoch: 11, IndexRoot: [32]byte{0x05}, ValidatorSetHash: [32]byte{0x06}}
	lc := client.NewLightClient(&client.Client{}, advanced)

	if err := syncCheckpoint(e, w, lc); err != nil {
		t.Fatalf("syncCheckpoint: %v", err)
	}

	if got, ok := w.Checkpoint(); !ok || got != advanced {
		t.Fatalf("in-memory checkpoint = %+v, ok=%v, want %+v", got, ok, advanced)
	}

	reloaded, err := client.LoadWallet(walletPath)
	if err != nil {
		t.Fatalf("load persisted wallet: %v", err)
	}
	if got, ok := reloaded.Checkpoint(); !ok || got != advanced {
		t.Fatalf("persisted checkpoint = %+v, ok=%v, want %+v", got, ok, advanced)
	}
}

// TestSyncCheckpointSkipsSaveWithoutAKeyPath verifies an ephemeral wallet (no
// --key given) still gets its in-memory checkpoint synced, without erroring
// over the absent file path.
func TestSyncCheckpointSkipsSaveWithoutAKeyPath(t *testing.T) {
	e := &env{}
	w := client.NewWallet()
	cp := client.Checkpoint{Epoch: 1, IndexRoot: [32]byte{0x07}, ValidatorSetHash: [32]byte{0x08}}
	lc := client.NewLightClient(&client.Client{}, cp)

	if err := syncCheckpoint(e, w, lc); err != nil {
		t.Fatalf("syncCheckpoint: %v", err)
	}

	if got, ok := w.Checkpoint(); !ok || got != cp {
		t.Fatalf("checkpoint = %+v, ok=%v, want %+v", got, ok, cp)
	}
}
