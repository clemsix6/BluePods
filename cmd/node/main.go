package main

import (
	"fmt"
	"os"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

// run is the main entry point with error handling.
func run() error {
	cfg := parseFlags()

	var err error
	cfg.PrivateKey, err = loadOrGenerateKey(cfg.KeyPath)
	if err != nil {
		return fmt.Errorf("load key:\n%w", err)
	}

	node, err := NewNode(cfg)
	if err != nil {
		return fmt.Errorf("create node:\n%w", err)
	}

	printStartupInfo(cfg)

	return node.Run()
}

// printStartupInfo displays node configuration at startup.
func printStartupInfo(cfg *Config) {
	pubKey := cfg.PrivateKey.Public()
	fmt.Printf("BluePods Node\n")
	fmt.Printf("  Pubkey:    %x\n", pubKey)
	fmt.Printf("  HTTP:      %s\n", cfg.HTTPAddress)
	fmt.Printf("  QUIC:      %s\n", cfg.QUICAddress)
	fmt.Printf("  Data:      %s\n", cfg.DataPath)
	fmt.Printf("  Bootstrap: %v\n", cfg.Bootstrap)

	if cfg.Bootstrap {
		fmt.Printf("  Initial mint: %d\n", cfg.InitialMint)
	}

	fmt.Println()
}
