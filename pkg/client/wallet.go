package client

import (
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
)

// walletFile is the on-disk wallet shape: the key, known coin IDs, created
// object IDs, and an optional trust checkpoint. Checkpoint is a pointer so it
// is simply absent from the JSON (omitempty) on a wallet that never held one
// — an old wallet file with no such key still loads, and a wallet this
// package writes without ever calling SetCheckpoint is byte-for-byte what it
// would have been before this field existed.
type walletFile struct {
	Key        string            `json:"key"`                  // Key is the hex-encoded Ed25519 private key
	Coins      []string          `json:"coins"`                // Coins is the hex-encoded list of known coin IDs
	Objects    []string          `json:"objects"`              // Objects is the hex-encoded list of created object IDs
	Checkpoint *walletCheckpoint `json:"checkpoint,omitempty"` // Checkpoint is the persisted trust checkpoint, absent when none was ever set
}

// walletCheckpoint is Checkpoint's on-disk encoding: the epoch as a number,
// the two roots hex-encoded like every other 32-byte value in this file.
type walletCheckpoint struct {
	Epoch            uint64 `json:"epoch"`              // Epoch is Checkpoint.Epoch
	IndexRoot        string `json:"index_root"`         // IndexRoot is the hex-encoded Checkpoint.IndexRoot
	ValidatorSetHash string `json:"validator_set_hash"` // ValidatorSetHash is the hex-encoded Checkpoint.ValidatorSetHash
}

// Track records a coin ID as owned so the wallet includes it in balance queries.
func (w *Wallet) Track(id [32]byte) {
	if w.coins[id] == nil {
		w.coins[id] = &CoinInfo{ID: id}
	}
}

// Knows reports whether the wallet tracks the given coin ID.
func (w *Wallet) Knows(id [32]byte) bool {
	return w.coins[id] != nil
}

// CoinIDs returns the known coin IDs.
func (w *Wallet) CoinIDs() [][32]byte {
	ids := make([][32]byte, 0, len(w.coins))
	for id := range w.coins {
		ids = append(ids, id)
	}

	return ids
}

// TrackObject records a created object's ID so the wallet can list it later.
func (w *Wallet) TrackObject(id [32]byte) {
	w.objects[id] = true
}

// ObjectIDs returns the tracked object IDs.
func (w *Wallet) ObjectIDs() [][32]byte {
	ids := make([][32]byte, 0, len(w.objects))
	for id := range w.objects {
		ids = append(ids, id)
	}

	return ids
}

// Save writes the wallet (key, known coin IDs, and trust checkpoint when set)
// to path.
func (w *Wallet) Save(path string) error {
	wf := walletFile{Key: hex.EncodeToString(w.privKey)}
	for id := range w.coins {
		wf.Coins = append(wf.Coins, hex.EncodeToString(id[:]))
	}
	for id := range w.objects {
		wf.Objects = append(wf.Objects, hex.EncodeToString(id[:]))
	}
	if w.checkpoint != nil {
		wf.Checkpoint = &walletCheckpoint{
			Epoch:            w.checkpoint.Epoch,
			IndexRoot:        hex.EncodeToString(w.checkpoint.IndexRoot[:]),
			ValidatorSetHash: hex.EncodeToString(w.checkpoint.ValidatorSetHash[:]),
		}
	}

	data, err := json.MarshalIndent(wf, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal wallet:\n%w", err)
	}

	if err := os.WriteFile(path, data, 0600); err != nil {
		return fmt.Errorf("write wallet %s:\n%w", path, err)
	}

	return nil
}

// LoadWallet reads a wallet (key and known coin IDs) from path.
func LoadWallet(path string) (*Wallet, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read wallet %s:\n%w", path, err)
	}

	var wf walletFile
	if err := json.Unmarshal(data, &wf); err != nil {
		return nil, fmt.Errorf("parse wallet:\n%w", err)
	}

	rawKey, err := hex.DecodeString(wf.Key)
	if err != nil || len(rawKey) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("invalid wallet key")
	}

	w := NewWalletFromKey(ed25519.PrivateKey(rawKey))
	for _, c := range wf.Coins {
		raw, err := hex.DecodeString(c)
		if err != nil || len(raw) != 32 {
			return nil, fmt.Errorf("invalid coin id %q", c)
		}

		var id [32]byte
		copy(id[:], raw)
		w.Track(id)
	}

	for _, o := range wf.Objects {
		raw, err := hex.DecodeString(o)
		if err != nil || len(raw) != 32 {
			return nil, fmt.Errorf("invalid object id %q", o)
		}

		var id [32]byte
		copy(id[:], raw)
		w.TrackObject(id)
	}

	if wf.Checkpoint != nil {
		cp, err := decodeWalletCheckpoint(*wf.Checkpoint)
		if err != nil {
			return nil, fmt.Errorf("invalid checkpoint:\n%w", err)
		}
		w.SetCheckpoint(cp)
	}

	return w, nil
}

// decodeWalletCheckpoint parses a walletCheckpoint's hex-encoded roots back
// into a Checkpoint.
func decodeWalletCheckpoint(wc walletCheckpoint) (Checkpoint, error) {
	indexRoot, err := decodeRoot(wc.IndexRoot)
	if err != nil {
		return Checkpoint{}, fmt.Errorf("index root:\n%w", err)
	}

	validatorSetHash, err := decodeRoot(wc.ValidatorSetHash)
	if err != nil {
		return Checkpoint{}, fmt.Errorf("validator set hash:\n%w", err)
	}

	return Checkpoint{Epoch: wc.Epoch, IndexRoot: indexRoot, ValidatorSetHash: validatorSetHash}, nil
}

// decodeRoot decodes a 32-byte hex-encoded root, the same shape every other
// hash in this file is stored as.
func decodeRoot(s string) ([32]byte, error) {
	var out [32]byte

	raw, err := hex.DecodeString(s)
	if err != nil || len(raw) != 32 {
		return out, fmt.Errorf("invalid 32-byte hex %q", s)
	}

	copy(out[:], raw)

	return out, nil
}
