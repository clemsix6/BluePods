package client

import (
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
)

// walletFile is the on-disk wallet shape: the key and the known coin IDs.
type walletFile struct {
	Key   string   `json:"key"`   // Key is the hex-encoded Ed25519 private key
	Coins []string `json:"coins"` // Coins is the hex-encoded list of known coin IDs
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

// Save writes the wallet (key and known coin IDs) to path.
func (w *Wallet) Save(path string) error {
	wf := walletFile{Key: hex.EncodeToString(w.privKey)}
	for id := range w.coins {
		wf.Coins = append(wf.Coins, hex.EncodeToString(id[:]))
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

	return w, nil
}
