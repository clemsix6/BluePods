package genesis

import (
	"crypto/ed25519"
	"fmt"
)

// Config holds the genesis configuration for bootstrapping the network.
type Config struct {
	// PrivateKey is the bootstrap validator's Ed25519 private key.
	PrivateKey ed25519.PrivateKey

	// InitialMint is the amount of tokens to mint for the bootstrap account.
	InitialMint uint64

	// HTTPAddress is the validator's HTTP API endpoint (e.g., "192.168.1.1:8080").
	HTTPAddress string

	// QUICAddress is the validator's QUIC P2P endpoint (e.g., "192.168.1.1:9000").
	QUICAddress string

	// SystemPodID is the blake3 hash of the system pod WASM.
	SystemPodID [32]byte
}

// BuildTransactions creates the genesis transactions for network bootstrap.
// Returns two AttestedTransaction bytes: mint (creates initial tokens) and register_validator.
func BuildTransactions(cfg Config) ([][]byte, error) {
	if cfg.PrivateKey == nil {
		return nil, fmt.Errorf("private key is required")
	}

	if len(cfg.PrivateKey) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("invalid private key size: %d", len(cfg.PrivateKey))
	}

	pubKey := cfg.PrivateKey.Public().(ed25519.PublicKey)
	var owner [32]byte
	copy(owner[:], pubKey)

	// Build mint transaction
	mintTx := BuildMintTx(cfg.PrivateKey, cfg.SystemPodID, cfg.InitialMint, owner)

	// Build register_validator transaction
	registerTx := BuildRegisterValidatorTx(
		cfg.PrivateKey,
		cfg.SystemPodID,
		cfg.HTTPAddress,
		cfg.QUICAddress,
	)

	return [][]byte{mintTx, registerTx}, nil
}
