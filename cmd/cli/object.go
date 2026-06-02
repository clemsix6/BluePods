package main

import (
	"encoding/hex"
	"flag"
	"fmt"
)

// cmdObject dispatches the object subcommands.
func cmdObject(e *env, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("object requires a subcommand: create, show, set, transfer, holders")
	}

	switch args[0] {
	case "create":
		return cmdObjectCreate(e, args[1:])
	case "show":
		return cmdObjectShow(e, args[1:])
	case "set":
		return cmdObjectSet(e, args[1:])
	case "transfer":
		return cmdObjectTransfer(e, args[1:])
	case "holders":
		return cmdObjectHolders(e, args[1:])
	default:
		return fmt.Errorf("unknown object subcommand: %s", args[0])
	}
}

// cmdObjectCreate creates a replicated object and prints its ID.
func cmdObjectCreate(e *env, args []string) error {
	fs := flag.NewFlagSet("object create", flag.ContinueOnError)
	replication := fs.Uint("replication", 0, "replication factor (number of holders)")
	content := fs.String("content", "", "initial content string")
	gasCoinHex := fs.String("gas-coin", "", "hex ID of an owned coin to pay gas")

	if err := fs.Parse(args); err != nil {
		return err
	}

	if *replication == 0 {
		return fmt.Errorf("--replication must be > 0 for a replicated object")
	}

	gasCoin, err := parseHash(*gasCoinHex)
	if err != nil {
		return fmt.Errorf("parse --gas-coin:\n%w", err)
	}

	cli, err := connect(e)
	if err != nil {
		return err
	}

	w, err := wallet(e)
	if err != nil {
		return err
	}

	objectID, err := w.CreateObject(cli, uint16(*replication), []byte(*content), gasCoin)
	if err != nil {
		return fmt.Errorf("create object:\n%w", err)
	}

	fmt.Printf("object: %s\n", hex.EncodeToString(objectID[:]))

	return nil
}

// cmdObjectShow prints an object's owner, version, replication, and content.
func cmdObjectShow(e *env, args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: object show <id-hex>")
	}

	id, err := parseHash(args[0])
	if err != nil {
		return fmt.Errorf("parse object id:\n%w", err)
	}

	cli, err := connect(e)
	if err != nil {
		return err
	}

	obj, err := cli.GetObject(id)
	if err != nil {
		return fmt.Errorf("get object:\n%w", err)
	}

	fmt.Printf("id:          %s\n", hex.EncodeToString(obj.ID[:]))
	fmt.Printf("owner:       %s\n", hex.EncodeToString(obj.Owner[:]))
	fmt.Printf("version:     %d\n", obj.Version)
	fmt.Printf("replication: %d\n", obj.Replication)
	fmt.Printf("content:     %s\n", string(decodeObjectContent(obj.Content)))

	return nil
}

// cmdObjectSet overwrites an object's content through the daemon aggregation path.
// The first two positional args are the object ID and the new content; a trailing
// gas-coin ID (an owned coin) pays the transaction's gas.
func cmdObjectSet(e *env, args []string) error {
	if len(args) != 3 {
		return fmt.Errorf("usage: object set <id-hex> <STRING> <gas-coin-hex>")
	}

	id, err := parseHash(args[0])
	if err != nil {
		return fmt.Errorf("parse object id:\n%w", err)
	}

	gasCoin, err := parseHash(args[2])
	if err != nil {
		return fmt.Errorf("parse gas-coin id:\n%w", err)
	}

	cli, err := connect(e)
	if err != nil {
		return err
	}

	w, err := wallet(e)
	if err != nil {
		return err
	}

	if err := w.SetObject(cli, id, []byte(args[1]), gasCoin); err != nil {
		return fmt.Errorf("set object:\n%w", err)
	}

	fmt.Printf("set content of object %s\n", hex.EncodeToString(id[:8]))

	return nil
}

// cmdObjectTransfer transfers object ownership through the daemon aggregation path.
// The trailing gas-coin ID (an owned coin) pays the transaction's gas.
func cmdObjectTransfer(e *env, args []string) error {
	if len(args) != 3 {
		return fmt.Errorf("usage: object transfer <id-hex> <to-pubkey-hex> <gas-coin-hex>")
	}

	id, err := parseHash(args[0])
	if err != nil {
		return fmt.Errorf("parse object id:\n%w", err)
	}

	recipient, err := parseHash(args[1])
	if err != nil {
		return fmt.Errorf("parse recipient:\n%w", err)
	}

	gasCoin, err := parseHash(args[2])
	if err != nil {
		return fmt.Errorf("parse gas-coin id:\n%w", err)
	}

	cli, err := connect(e)
	if err != nil {
		return err
	}

	w, err := wallet(e)
	if err != nil {
		return err
	}

	if err := w.TransferObject(cli, id, recipient, gasCoin); err != nil {
		return fmt.Errorf("transfer object:\n%w", err)
	}

	fmt.Printf("transferred object %s to %s\n",
		hex.EncodeToString(id[:8]), hex.EncodeToString(recipient[:8]))

	return nil
}
