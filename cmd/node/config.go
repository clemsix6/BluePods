package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"flag"
	"fmt"
	"os"
)

// Config holds the node configuration.
type Config struct {
	// DataPath is the directory for persistent storage.
	DataPath string

	// HTTPAddress is the HTTP API listen address.
	HTTPAddress string

	// QUICAddress is the QUIC P2P listen address.
	QUICAddress string

	// KeyPath is the path to the Ed25519 private key file.
	KeyPath string

	// PrivateKey is the node's Ed25519 signing key.
	PrivateKey ed25519.PrivateKey

	// Bootstrap indicates this is the genesis validator.
	Bootstrap bool

	// Listener indicates this node only listens to the network without participating.
	Listener bool

	// BootstrapAddr is the address of a bootstrap node to connect to (for sync).
	BootstrapAddr string

	// RegistrationAddr is the QUIC address to use for validator registration.
	// If empty, defaults to BootstrapAddr. Useful when syncing from a non-bootstrap node.
	RegistrationAddr string

	// InitialMint is the amount of tokens to mint at genesis.
	InitialMint uint64

	// SystemPodPath is the path to the system pod WASM file.
	SystemPodPath string

	// MinValidators is the minimum number of validators before non-bootstrap nodes produce.
	// Bootstrap always produces. Others wait until this threshold is reached.
	MinValidators int
}

// parseFlags parses command-line flags into Config.
func parseFlags() *Config {
	cfg := &Config{}

	flag.StringVar(&cfg.DataPath, "data", "./data", "Data directory path")
	flag.StringVar(&cfg.HTTPAddress, "http", ":8080", "HTTP API address")
	flag.StringVar(&cfg.QUICAddress, "quic", ":9000", "QUIC P2P address")
	flag.StringVar(&cfg.KeyPath, "key", "", "Ed25519 private key path (generates new if missing)")
	flag.BoolVar(&cfg.Bootstrap, "bootstrap", false, "Bootstrap mode (genesis validator)")
	flag.BoolVar(&cfg.Listener, "listener", false, "Listener mode (observe only, no consensus)")
	flag.StringVar(&cfg.BootstrapAddr, "bootstrap-addr", "", "Bootstrap node address to connect to (for sync)")
	flag.StringVar(&cfg.RegistrationAddr, "registration-addr", "", "Address for validator registration (defaults to bootstrap-addr)")
	flag.Uint64Var(&cfg.InitialMint, "initial-mint", 1_000_000_000, "Initial token mint amount")
	flag.StringVar(&cfg.SystemPodPath, "system-pod", "./pods/pod-system/build/pod.wasm", "System pod WASM path")
	flag.IntVar(&cfg.MinValidators, "min-validators", 1, "Minimum validators before non-bootstrap nodes produce")
	flag.Parse()

	return cfg
}

// loadOrGenerateKey loads the private key from file or generates a new one.
func loadOrGenerateKey(keyPath string) (ed25519.PrivateKey, error) {
	if keyPath == "" {
		return generateNewKey()
	}

	data, err := os.ReadFile(keyPath)
	if os.IsNotExist(err) {
		return generateAndSaveKey(keyPath)
	}

	if err != nil {
		return nil, fmt.Errorf("read key file:\n%w", err)
	}

	if len(data) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("invalid key size: got %d, want %d", len(data), ed25519.PrivateKeySize)
	}

	return ed25519.PrivateKey(data), nil
}

// generateNewKey creates a new Ed25519 private key.
func generateNewKey() (ed25519.PrivateKey, error) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate key:\n%w", err)
	}

	return priv, nil
}

// generateAndSaveKey creates a new key and saves it to the given path.
func generateAndSaveKey(path string) (ed25519.PrivateKey, error) {
	priv, err := generateNewKey()
	if err != nil {
		return nil, err
	}

	if err := os.WriteFile(path, priv, 0600); err != nil {
		return nil, fmt.Errorf("save key to %s:\n%w", path, err)
	}

	return priv, nil
}
