package main

import (
	"encoding/hex"
	"flag"
	"fmt"

	"BluePods/internal/index"
)

// cmdObject dispatches the object subcommands.
func cmdObject(e *env, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("object requires a subcommand: create, show, set, transfer, reparent, delete, parent, holders")
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
	case "reparent":
		return cmdObjectReparent(e, args[1:])
	case "delete":
		return cmdObjectDelete(e, args[1:])
	case "parent":
		return cmdObjectParent(e, args[1:])
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

	objectID, _, err := w.CreateObject(cli, uint16(*replication), []byte(*content), gasCoin)
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

	if _, err := w.SetObject(cli, id, []byte(args[1]), gasCoin); err != nil {
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

	if _, err := w.TransferObject(cli, id, recipient, gasCoin); err != nil {
		return fmt.Errorf("transfer object:\n%w", err)
	}

	fmt.Printf("transferred object %s to %s\n",
		hex.EncodeToString(id[:8]), hex.EncodeToString(recipient[:8]))

	return nil
}

// cmdObjectReparent moves an object under a new ObjectParent through the
// daemon aggregation path. The trailing gas-coin ID (an owned coin) pays the
// transaction's gas.
func cmdObjectReparent(e *env, args []string) error {
	if len(args) != 3 {
		return fmt.Errorf("usage: object reparent <id-hex> <new-parent-id-hex> <gas-coin-hex>")
	}

	id, err := parseHash(args[0])
	if err != nil {
		return fmt.Errorf("parse object id:\n%w", err)
	}

	newParent, err := parseHash(args[1])
	if err != nil {
		return fmt.Errorf("parse new parent id:\n%w", err)
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

	if _, err := w.Reparent(cli, id, newParent, gasCoin); err != nil {
		return fmt.Errorf("reparent object:\n%w", err)
	}

	fmt.Printf("reparented object %s under %s\n",
		hex.EncodeToString(id[:8]), hex.EncodeToString(newParent[:8]))

	return nil
}

// cmdObjectDelete destroys an object; it must have no remaining children. The
// trailing gas-coin ID (an owned coin) pays the transaction's gas and
// receives the storage-deposit refund.
func cmdObjectDelete(e *env, args []string) error {
	if len(args) != 2 {
		return fmt.Errorf("usage: object delete <id-hex> <gas-coin-hex>")
	}

	id, err := parseHash(args[0])
	if err != nil {
		return fmt.Errorf("parse object id:\n%w", err)
	}

	gasCoin, err := parseHash(args[1])
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

	if _, err := w.DeleteObject(cli, id, gasCoin); err != nil {
		return fmt.Errorf("delete object:\n%w", err)
	}

	fmt.Printf("deleted object %s\n", hex.EncodeToString(id[:8]))

	return nil
}

// cmdObjectParent shows an object's immediate parent edge: an owner key
// (KeyRoot) or another object (ObjectParent). It reads the node's unproven
// word (pkg/client/indexreads.go's Client.Parent) — bpctl acquires no
// trusted checkpoint to verify a proof against.
func cmdObjectParent(e *env, args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: object parent <id-hex>")
	}

	id, err := parseHash(args[0])
	if err != nil {
		return fmt.Errorf("parse object id:\n%w", err)
	}

	cli, err := connect(e)
	if err != nil {
		return err
	}

	kind, parent, hasParent, err := cli.Parent(id)
	if err != nil {
		return fmt.Errorf("get parent:\n%w", err)
	}

	if !hasParent {
		fmt.Printf("%s: no parent edge\n", hex.EncodeToString(id[:8]))
		return nil
	}

	kindName := "key"
	if kind == index.ObjectParentKind {
		kindName = "object"
	}

	fmt.Printf("%s parent (%s): %s\n", hex.EncodeToString(id[:8]), kindName, hex.EncodeToString(parent[:]))

	return nil
}
