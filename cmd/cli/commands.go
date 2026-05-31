package main

import (
	"encoding/hex"
	"fmt"
	"strconv"
)

// cmdStatus prints the node's round, epoch, validator count, and last commit.
func cmdStatus(e *env) error {
	cli, err := connect(e)
	if err != nil {
		return err
	}

	status, err := cli.Status()
	if err != nil {
		return fmt.Errorf("get status:\n%w", err)
	}

	fmt.Printf("node:        %s\n", e.nodeAddr)
	fmt.Printf("round:       %d\n", status.Round)
	fmt.Printf("epoch:       %d\n", status.Epoch)
	fmt.Printf("validators:  %d\n", status.Validators)
	fmt.Printf("last commit: %d\n", status.LastCommitted)

	return nil
}

// cmdValidators lists each validator's public key and QUIC address.
func cmdValidators(e *env) error {
	cli, err := connect(e)
	if err != nil {
		return err
	}

	vals, err := cli.Validators()
	if err != nil {
		return fmt.Errorf("get validators:\n%w", err)
	}

	for _, v := range vals {
		fmt.Printf("%s  %s\n", hex.EncodeToString(v.Pubkey[:]), v.QUICAddr)
	}

	return nil
}

// cmdCoin dispatches the coin subcommands.
func cmdCoin(e *env, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("coin requires a subcommand: faucet, transfer")
	}

	switch args[0] {
	case "faucet":
		return cmdCoinFaucet(e, args[1:])
	case "transfer":
		return cmdCoinTransfer(e, args[1:])
	default:
		return fmt.Errorf("unknown coin subcommand: %s", args[0])
	}
}

// cmdCoinFaucet mints a coin of the given amount to a public key.
func cmdCoinFaucet(e *env, args []string) error {
	if len(args) != 2 {
		return fmt.Errorf("usage: coin faucet <pubkey-hex> <amount>")
	}

	pubkey, err := parseHash(args[0])
	if err != nil {
		return fmt.Errorf("parse pubkey:\n%w", err)
	}

	amount, err := strconv.ParseUint(args[1], 10, 64)
	if err != nil {
		return fmt.Errorf("parse amount:\n%w", err)
	}

	cli, err := connect(e)
	if err != nil {
		return err
	}

	coinID, err := cli.Faucet(pubkey, amount)
	if err != nil {
		return fmt.Errorf("faucet:\n%w", err)
	}

	fmt.Printf("coin: %s\n", hex.EncodeToString(coinID[:]))

	return nil
}

// cmdCoinTransfer transfers a coin to a new owner using the loaded wallet.
func cmdCoinTransfer(e *env, args []string) error {
	if len(args) != 2 {
		return fmt.Errorf("usage: coin transfer <coin-id-hex> <to-pubkey-hex>")
	}

	coinID, err := parseHash(args[0])
	if err != nil {
		return fmt.Errorf("parse coin id:\n%w", err)
	}

	recipient, err := parseHash(args[1])
	if err != nil {
		return fmt.Errorf("parse recipient:\n%w", err)
	}

	cli, err := connect(e)
	if err != nil {
		return err
	}

	w, err := wallet(e)
	if err != nil {
		return err
	}

	if err := w.RefreshCoin(cli, coinID); err != nil {
		return fmt.Errorf("refresh coin:\n%w", err)
	}

	if err := w.Transfer(cli, coinID, recipient); err != nil {
		return fmt.Errorf("transfer coin:\n%w", err)
	}

	fmt.Printf("transferred coin %s to %s\n",
		hex.EncodeToString(coinID[:8]), hex.EncodeToString(recipient[:8]))

	return nil
}

// parseHash decodes a 32-byte hex string into a fixed-size array.
func parseHash(s string) ([32]byte, error) {
	var out [32]byte

	raw, err := hex.DecodeString(s)
	if err != nil {
		return out, fmt.Errorf("invalid hex:\n%w", err)
	}

	if len(raw) != 32 {
		return out, fmt.Errorf("expected 32 bytes, got %d", len(raw))
	}

	copy(out[:], raw)

	return out, nil
}

// decodeObjectContent strips the 4-byte little-endian Borsh length prefix from an
// object's serialized body and returns the raw content bytes.
func decodeObjectContent(content []byte) []byte {
	if len(content) < 4 {
		return nil
	}

	return content[4:]
}
