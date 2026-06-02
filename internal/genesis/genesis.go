package genesis

import "crypto/ed25519"

// Config holds the genesis configuration for bootstrapping the network.
type Config struct {
	// PrivateKey is the bootstrap validator's Ed25519 private key.
	PrivateKey ed25519.PrivateKey

	// InitialMint is the amount of tokens to mint for the bootstrap account.
	InitialMint uint64

	// GenesisStake is the founder's bonded self-stake, locked from InitialMint so
	// the bootstrap validator has real backing for its stake-weighted quorum
	// weight. Clamped to InitialMint to keep the coin balance from underflowing.
	GenesisStake uint64

	// QUICAddress is the validator's QUIC P2P endpoint (e.g., "192.168.1.1:9000").
	QUICAddress string

	// SystemPodID is the blake3 hash of the system pod WASM.
	SystemPodID [32]byte

	// BLSPubkey is the validator's BLS public key for attestation signing.
	BLSPubkey []byte
}

