package main

import (
	"crypto/ed25519"
	"encoding/hex"
	"fmt"
	"os"

	"BluePods/internal/logger"
)

func main() {
	logger.Init()

	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

// run is the main entry point with error handling.
func run() error {
	cfg, err := parseFlags()
	if err != nil {
		return fmt.Errorf("parse flags:\n%w", err)
	}

	if cfg.LogFormat == "json" {
		logger.UseJSON(os.Stdout)
	}

	cfg.PrivateKey, err = loadOrGenerateKey(cfg.KeyPath)
	if err != nil {
		return fmt.Errorf("load key:\n%w", err)
	}

	logger.SetNode(hex.EncodeToString(cfg.PrivateKey.Public().(ed25519.PublicKey))[:8])

	node, err := NewNode(cfg)
	if err != nil {
		return fmt.Errorf("create node:\n%w", err)
	}

	printStartupInfo(cfg)

	return node.Run()
}

// printStartupInfo displays node configuration at startup.
func printStartupInfo(cfg *Config) {
	pubKey := cfg.PrivateKey.Public().(ed25519.PublicKey)
	pubKeyHex := hex.EncodeToString(pubKey)

	logger.Info("starting BluePods node",
		"pubkey", pubKeyHex,
		"quic", cfg.QUICAddress,
		"data", cfg.DataPath,
		"bootstrap", cfg.Bootstrap,
		"listener", cfg.Listener,
	)

	if cfg.Bootstrap {
		logger.Info("genesis configuration", "initial_mint", cfg.InitialMint)
	}
}
