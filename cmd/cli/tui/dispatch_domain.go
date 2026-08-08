package tui

import (
	"encoding/hex"
	"fmt"
	"strconv"

	"BluePods/pkg/client"
)

// dispatchDomain routes domain sub-commands: register, renew, update,
// transfer, delete, resolve.
func dispatchDomain(c *client.Client, w *client.Wallet, lc *client.LightClient, cmd command) (string, [32]byte, error) {
	if len(cmd.args) == 0 {
		return "", [32]byte{}, fmt.Errorf("usage: domain <register|renew|update|transfer|delete|resolve>")
	}

	sub := cmd.args[0]
	rest := command{verb: sub, args: cmd.args[1:]}

	switch sub {
	case "register":
		return dispatchDomainRegister(c, w, rest)
	case "renew":
		return dispatchDomainRenew(c, w, rest)
	case "update":
		return dispatchDomainUpdate(c, w, rest)
	case "transfer":
		return dispatchDomainTransfer(c, w, rest)
	case "delete":
		return dispatchDomainDelete(c, w, rest)
	case "resolve":
		return dispatchDomainResolve(c, lc, rest)
	default:
		return "", [32]byte{}, fmt.Errorf("unknown domain subcommand: %s", sub)
	}
}

// dispatchDomainRegister handles: domain register <name> <objectId> <term> <gasCoin>.
// It names an object the wallet already owns. The spec §8 two-transaction
// create-then-register saga (pkg/client's Wallet.RegisterNewObjectDomain) is
// not exposed here: the console takes only positional arguments, and the
// saga's replication/content parameters do not fit that shape — use bpctl's
// `domain register --replication N` for a brand-new object.
func dispatchDomainRegister(c *client.Client, w *client.Wallet, cmd command) (string, [32]byte, error) {
	name := arg(cmd, 0)

	objectID, err := parseHexID(arg(cmd, 1))
	if err != nil || name == "" {
		return "", [32]byte{}, fmt.Errorf("usage: domain register <name> <objectId-hex> <term-epochs> <gasCoin-hex>")
	}

	term, err := strconv.ParseUint(arg(cmd, 2), 10, 32)
	if err != nil {
		return "", [32]byte{}, fmt.Errorf("usage: domain register <name> <objectId-hex> <term-epochs> <gasCoin-hex>")
	}

	gasCoinID, err := parseHexID(arg(cmd, 3))
	if err != nil {
		return "", [32]byte{}, fmt.Errorf("usage: domain register <name> <objectId-hex> <term-epochs> <gasCoin-hex>")
	}

	if err := w.RefreshCoin(c, gasCoinID); err != nil {
		return "", [32]byte{}, fmt.Errorf("refresh gas coin:\n%w", err)
	}

	txHash, err := w.DomainRegister(c, name, objectID, uint32(term), gasCoinID)
	if err != nil {
		return "", [32]byte{}, err
	}

	return fmt.Sprintf("domain %s -> %s registered", name, hex.EncodeToString(objectID[:4])), txHash, nil
}

// dispatchDomainRenew handles: domain renew <name> <term> <gasCoin>
func dispatchDomainRenew(c *client.Client, w *client.Wallet, cmd command) (string, [32]byte, error) {
	name := arg(cmd, 0)

	term, err := strconv.ParseUint(arg(cmd, 1), 10, 32)
	if err != nil || name == "" {
		return "", [32]byte{}, fmt.Errorf("usage: domain renew <name> <term-epochs> <gasCoin-hex>")
	}

	gasCoinID, err := parseHexID(arg(cmd, 2))
	if err != nil {
		return "", [32]byte{}, fmt.Errorf("usage: domain renew <name> <term-epochs> <gasCoin-hex>")
	}

	if err := w.RefreshCoin(c, gasCoinID); err != nil {
		return "", [32]byte{}, fmt.Errorf("refresh gas coin:\n%w", err)
	}

	txHash, err := w.DomainRenew(c, name, uint32(term), gasCoinID)
	if err != nil {
		return "", [32]byte{}, err
	}

	return fmt.Sprintf("domain %s renewed for %d epochs", name, term), txHash, nil
}

// dispatchDomainUpdate handles: domain update <name> <objectId> <gasCoin>
func dispatchDomainUpdate(c *client.Client, w *client.Wallet, cmd command) (string, [32]byte, error) {
	name := arg(cmd, 0)

	objectID, err := parseHexID(arg(cmd, 1))
	if err != nil || name == "" {
		return "", [32]byte{}, fmt.Errorf("usage: domain update <name> <objectId-hex> <gasCoin-hex>")
	}

	gasCoinID, err := parseHexID(arg(cmd, 2))
	if err != nil {
		return "", [32]byte{}, fmt.Errorf("usage: domain update <name> <objectId-hex> <gasCoin-hex>")
	}

	if err := w.RefreshCoin(c, gasCoinID); err != nil {
		return "", [32]byte{}, fmt.Errorf("refresh gas coin:\n%w", err)
	}

	txHash, err := w.DomainUpdate(c, name, objectID, gasCoinID)
	if err != nil {
		return "", [32]byte{}, err
	}

	return fmt.Sprintf("domain %s repointed to %s", name, hex.EncodeToString(objectID[:4])), txHash, nil
}

// dispatchDomainTransfer handles: domain transfer <name> <newOwner> <gasCoin>
func dispatchDomainTransfer(c *client.Client, w *client.Wallet, cmd command) (string, [32]byte, error) {
	name := arg(cmd, 0)

	newOwner, err := parseHexID(arg(cmd, 1))
	if err != nil || name == "" {
		return "", [32]byte{}, fmt.Errorf("usage: domain transfer <name> <newOwner-hex> <gasCoin-hex>")
	}

	gasCoinID, err := parseHexID(arg(cmd, 2))
	if err != nil {
		return "", [32]byte{}, fmt.Errorf("usage: domain transfer <name> <newOwner-hex> <gasCoin-hex>")
	}

	if err := w.RefreshCoin(c, gasCoinID); err != nil {
		return "", [32]byte{}, fmt.Errorf("refresh gas coin:\n%w", err)
	}

	txHash, err := w.DomainTransfer(c, name, newOwner, gasCoinID)
	if err != nil {
		return "", [32]byte{}, err
	}

	return fmt.Sprintf("domain %s transferred to %s", name, hex.EncodeToString(newOwner[:4])), txHash, nil
}

// dispatchDomainDelete handles: domain delete <name> <gasCoin>
func dispatchDomainDelete(c *client.Client, w *client.Wallet, cmd command) (string, [32]byte, error) {
	name := arg(cmd, 0)
	if name == "" {
		return "", [32]byte{}, fmt.Errorf("usage: domain delete <name> <gasCoin-hex>")
	}

	gasCoinID, err := parseHexID(arg(cmd, 1))
	if err != nil {
		return "", [32]byte{}, fmt.Errorf("usage: domain delete <name> <gasCoin-hex>")
	}

	if err := w.RefreshCoin(c, gasCoinID); err != nil {
		return "", [32]byte{}, fmt.Errorf("refresh gas coin:\n%w", err)
	}

	txHash, err := w.DomainDelete(c, name, gasCoinID)
	if err != nil {
		return "", [32]byte{}, err
	}

	return fmt.Sprintf("domain %s deleted", name), txHash, nil
}

// dispatchDomainResolve handles: domain resolve <name>. With a session
// LightClient (the wallet holds a trust checkpoint) it resolves through the
// verification library and the answer is proved; otherwise it reads the
// node's unproven word (see pkg/client/indexreads.go). Either way the
// returned line says which.
func dispatchDomainResolve(c *client.Client, lc *client.LightClient, cmd command) (string, [32]byte, error) {
	name := arg(cmd, 0)
	if name == "" {
		return "", [32]byte{}, fmt.Errorf("usage: domain resolve <name>")
	}

	objectID, found, proved, err := resolveDomain(c, lc, name)
	if err != nil {
		return "", [32]byte{}, err
	}

	if !found {
		return fmt.Sprintf("domain %s: not registered %s", name, readLabel(proved)), [32]byte{}, nil
	}

	return fmt.Sprintf("domain %s -> %s %s", name, hex.EncodeToString(objectID[:4]), readLabel(proved)), [32]byte{}, nil
}

// resolveDomain reads through lc when it is non-nil (the wallet holds a
// trust checkpoint), or through the plain client otherwise. A verification
// failure — including client.ErrCheckpointBehind's actionable "obtain a
// fresh checkpoint" message — is returned as-is, never silently retried
// against the unproven path.
func resolveDomain(c *client.Client, lc *client.LightClient, name string) (objectID [32]byte, found, proved bool, err error) {
	if lc == nil {
		objectID, found, err = c.DomainResolve(name)
		return objectID, found, false, err
	}

	leaf, found, err := lc.ResolveDomain(name)

	return leaf.ObjectID, found, true, err
}
