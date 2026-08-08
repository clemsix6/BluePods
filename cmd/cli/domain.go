package main

import (
	"encoding/hex"
	"flag"
	"fmt"
	"math"
	"strconv"

	"BluePods/pkg/client"
)

// cmdDomain dispatches the domain subcommands.
func cmdDomain(e *env, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("domain requires a subcommand: register, renew, update, transfer, delete, resolve")
	}

	switch args[0] {
	case "register":
		return cmdDomainRegister(e, args[1:])
	case "renew":
		return cmdDomainRenew(e, args[1:])
	case "update":
		return cmdDomainUpdate(e, args[1:])
	case "transfer":
		return cmdDomainTransfer(e, args[1:])
	case "delete":
		return cmdDomainDelete(e, args[1:])
	case "resolve":
		return cmdDomainResolve(e, args[1:])
	default:
		return fmt.Errorf("unknown domain subcommand: %s", args[0])
	}
}

// cmdDomainRegister registers --name against an object: an existing one given
// by --object, or a brand-new one created on the spot when --replication is
// given instead — the spec §8 two-transaction saga (create, wait for commit,
// register) that pkg/client's Wallet.RegisterNewObjectDomain carries out,
// since domain_register requires the sender to already control the pointed
// object and either-ops-or-pod forbids folding the create call and the
// declared op into one transaction.
func cmdDomainRegister(e *env, args []string) error {
	fs := flag.NewFlagSet("domain register", flag.ContinueOnError)
	name := fs.String("name", "", "name to register")
	term := fs.Uint("term", 0, "rental term in epochs")
	objectHex := fs.String("object", "", "hex ID of an existing owned object to name")
	replication := fs.Uint("replication", 0, "replication factor for a brand-new object (mutually exclusive with --object)")
	content := fs.String("content", "", "initial content for a brand-new object")
	gasCoinHex := fs.String("gas-coin", "", "hex ID of an owned coin to pay gas")

	if err := fs.Parse(args); err != nil {
		return err
	}

	if *name == "" || *term == 0 {
		return fmt.Errorf("usage: domain register --name <name> --term <epochs> (--object <id-hex> | --replication N [--content STR]) --gas-coin <hex>")
	}

	// --term is flag.Uint (platform uint, no bit-size limit): reject anything
	// that would silently truncate to a different value on the uint32 narrow
	// below — including truncating to 0, which the consensus commit rejects
	// only after gas was already spent (domainExpiry in
	// internal/consensus/domainops.go). This is the same rejection domain
	// renew's strconv.ParseUint(..., 32) already gives up front.
	if *term > math.MaxUint32 {
		return fmt.Errorf("--term must be at most %d epochs", uint32(math.MaxUint32))
	}

	if (*objectHex == "") == (*replication == 0) {
		return fmt.Errorf("domain register: exactly one of --object or --replication is required")
	}

	// Same narrowing hazard as --term above, for the uint16 replication factor.
	if *replication > math.MaxUint16 {
		return fmt.Errorf("--replication must be at most %d", uint16(math.MaxUint16))
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

	if *objectHex != "" {
		return registerExistingObjectDomain(cli, w, *name, *objectHex, uint32(*term), gasCoin)
	}

	return registerNewObjectDomain(cli, w, *name, uint16(*replication), *content, uint32(*term), gasCoin)
}

// registerExistingObjectDomain runs the ordinary (non-saga) domain_register
// path: name an object the wallet already owns.
func registerExistingObjectDomain(cli *client.Client, w *client.Wallet, name, objectHex string, term uint32, gasCoin [32]byte) error {
	objectID, err := parseHash(objectHex)
	if err != nil {
		return fmt.Errorf("parse --object:\n%w", err)
	}

	if _, err := w.DomainRegister(cli, name, objectID, term, gasCoin); err != nil {
		return fmt.Errorf("register domain:\n%w", err)
	}

	fmt.Printf("registered %s -> %s\n", name, hex.EncodeToString(objectID[:]))

	return nil
}

// registerNewObjectDomain runs the spec §8 two-transaction saga
// (Wallet.RegisterNewObjectDomain): create a brand-new object, wait for that
// to commit, register name against it, then wait for the register
// transaction to commit too — the saga only prints success once both halves
// are confirmed, since domain_register can itself revert at commit (name
// taken, term over the cap, an unowned namespace) after the object was
// already created and paid for. A failure from either wait or from the
// register submission names the already-created object in its error, so the
// name can be retried with `domain register --object` instead of re-running
// the whole saga.
func registerNewObjectDomain(cli *client.Client, w *client.Wallet, name string, replication uint16, content string, term uint32, gasCoin [32]byte) error {
	objectID, _, err := w.RegisterNewObjectDomain(cli, name, replication, []byte(content), term, gasCoin)
	if err != nil {
		return fmt.Errorf("register domain on a new object:\n%w", err)
	}

	fmt.Printf("created and registered %s -> %s\n", name, hex.EncodeToString(objectID[:]))

	return nil
}

// cmdDomainRenew handles: domain renew <name> <term-epochs> <gas-coin-hex>
func cmdDomainRenew(e *env, args []string) error {
	if len(args) != 3 {
		return fmt.Errorf("usage: domain renew <name> <term-epochs> <gas-coin-hex>")
	}

	term, err := strconv.ParseUint(args[1], 10, 32)
	if err != nil {
		return fmt.Errorf("parse term-epochs:\n%w", err)
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

	if _, err := w.DomainRenew(cli, args[0], uint32(term), gasCoin); err != nil {
		return fmt.Errorf("renew domain:\n%w", err)
	}

	fmt.Printf("renewed %s for %d epochs\n", args[0], term)

	return nil
}

// cmdDomainUpdate handles: domain update <name> <object-id-hex> <gas-coin-hex>
func cmdDomainUpdate(e *env, args []string) error {
	if len(args) != 3 {
		return fmt.Errorf("usage: domain update <name> <object-id-hex> <gas-coin-hex>")
	}

	objectID, err := parseHash(args[1])
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

	if _, err := w.DomainUpdate(cli, args[0], objectID, gasCoin); err != nil {
		return fmt.Errorf("update domain:\n%w", err)
	}

	fmt.Printf("repointed %s -> %s\n", args[0], hex.EncodeToString(objectID[:]))

	return nil
}

// cmdDomainTransfer handles: domain transfer <name> <new-owner-hex> <gas-coin-hex>
func cmdDomainTransfer(e *env, args []string) error {
	if len(args) != 3 {
		return fmt.Errorf("usage: domain transfer <name> <new-owner-hex> <gas-coin-hex>")
	}

	newOwner, err := parseHash(args[1])
	if err != nil {
		return fmt.Errorf("parse new owner:\n%w", err)
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

	if _, err := w.DomainTransfer(cli, args[0], newOwner, gasCoin); err != nil {
		return fmt.Errorf("transfer domain:\n%w", err)
	}

	fmt.Printf("transferred %s -> %s\n", args[0], hex.EncodeToString(newOwner[:]))

	return nil
}

// cmdDomainDelete handles: domain delete <name> <gas-coin-hex>
func cmdDomainDelete(e *env, args []string) error {
	if len(args) != 2 {
		return fmt.Errorf("usage: domain delete <name> <gas-coin-hex>")
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

	if _, err := w.DomainDelete(cli, args[0], gasCoin); err != nil {
		return fmt.Errorf("delete domain:\n%w", err)
	}

	fmt.Printf("deleted %s\n", args[0])

	return nil
}

// cmdDomainResolve handles: domain resolve <name>. When this invocation
// holds a trust checkpoint (--checkpoint, or one persisted from an earlier
// run) it resolves through the verification library and the answer is
// proved; otherwise it reads the node's unproven word (see
// pkg/client/indexreads.go). Either way the printed line says which.
func cmdDomainResolve(e *env, args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: domain resolve <name>")
	}

	cli, err := connect(e)
	if err != nil {
		return err
	}

	w, err := wallet(e)
	if err != nil {
		return err
	}

	objectID, found, proved, err := resolveDomain(e, cli, w, args[0])
	if err != nil {
		return fmt.Errorf("resolve domain:\n%w", err)
	}

	if !found {
		fmt.Printf("%s: not registered %s\n", args[0], readLabel(proved))
		return nil
	}

	fmt.Printf("%s -> %s %s\n", args[0], hex.EncodeToString(objectID[:]), readLabel(proved))

	return nil
}

// resolveDomain resolves name through the light client when w holds a trust
// checkpoint, re-persisting whatever epoch walk that read performed before
// returning — a verification failure (including client.ErrCheckpointBehind's
// actionable "obtain a fresh checkpoint" message) is returned as-is, never
// silently retried against the unproven path, which would defeat the reason
// a checkpoint was pinned in the first place.
func resolveDomain(e *env, cli *client.Client, w *client.Wallet, name string) (objectID [32]byte, found, proved bool, err error) {
	cp, ok := w.Checkpoint()
	if !ok {
		objectID, found, err = cli.DomainResolve(name)
		return objectID, found, false, err
	}

	lc := client.NewLightClient(cli, cp)
	leaf, found, err := lc.ResolveDomain(name)

	if syncErr := syncCheckpoint(e, w, lc); syncErr != nil && err == nil {
		err = syncErr
	}

	return leaf.ObjectID, found, true, err
}
