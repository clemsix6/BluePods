package tui

import (
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"

	"BluePods/internal/index"
	"BluePods/pkg/client"
)

// dispatchObject routes object sub-commands.
func dispatchObject(c *client.Client, w *client.Wallet, lc *client.LightClient, cmd command) (string, [32]byte, error) {
	if len(cmd.args) == 0 {
		return "", [32]byte{}, fmt.Errorf("usage: object <create|set|transfer|reparent|delete|parent|show|holders>")
	}

	sub := cmd.args[0]
	rest := command{verb: sub, args: cmd.args[1:]}

	switch sub {
	case "create":
		return dispatchObjectCreate(c, w, rest)
	case "set":
		return dispatchObjectSet(c, w, rest)
	case "transfer":
		return dispatchObjectTransfer(c, w, rest)
	case "reparent":
		return dispatchObjectReparent(c, w, rest)
	case "delete":
		return dispatchObjectDelete(c, w, rest)
	case "parent":
		return dispatchObjectParent(c, lc, rest)
	case "show":
		return dispatchObjectShow(c, rest)
	case "holders":
		return dispatchObjectHolders(c, rest)
	default:
		return "", [32]byte{}, fmt.Errorf("unknown object subcommand: %s", sub)
	}
}

// dispatchObjectCreate handles: object create <replication> <gasCoin> [content]
func dispatchObjectCreate(c *client.Client, w *client.Wallet, cmd command) (string, [32]byte, error) {
	rep64, err := strconv.ParseUint(arg(cmd, 0), 10, 16)
	if err != nil {
		return "", [32]byte{}, fmt.Errorf("usage: object create <replication> <gasCoin-hex> [content]")
	}

	gasCoinID, err := parseHexID(arg(cmd, 1))
	if err != nil {
		return "", [32]byte{}, fmt.Errorf("usage: object create <replication> <gasCoin-hex> [content]")
	}

	content := strings.Join(cmd.args[2:], " ")

	if err := w.RefreshCoin(c, gasCoinID); err != nil {
		return "", [32]byte{}, fmt.Errorf("refresh gas coin:\n%w", err)
	}

	objectID, txHash, err := w.CreateObject(c, uint16(rep64), []byte(content), gasCoinID)
	if err != nil {
		return "", [32]byte{}, err
	}

	w.TrackObject(objectID)

	return fmt.Sprintf("object created %s (replication %d)", hex.EncodeToString(objectID[:]), rep64), txHash, nil
}

// dispatchObjects handles: objects. It reconciles the wallet's tracked object
// set against the index first — spec §10's recovery rule, ListChildren
// (pubkey) plus recursion into object-parented subtrees, MERGED into what is
// already tracked rather than replacing it. With a session LightClient (the
// wallet holds a trust checkpoint) this recovery goes through the
// verification library and is proved; otherwise it reads the node's unproven
// word (see pkg/client/indexreads.go). A recovery failure (a genuine
// verification failure included, never silently retried against the
// unproven path — including the transport's single-frame ceiling for a
// large wallet, spec §10, 6.1's as-built) does not hide the verb's whole
// output: it falls back to whatever the wallet already tracks locally, with
// a warning line, since a stale-but-present list is more useful than none.
// It then lists each object with its full hex ID and current version/owner,
// so the full ID can be copied for object show/set/transfer.
func dispatchObjects(c *client.Client, w *client.Wallet, lc *client.LightClient) (string, [32]byte, error) {
	var recoverErr error
	if lc != nil {
		_, recoverErr = w.RecoverObjects(lc)
	} else {
		_, recoverErr = w.RecoverObjects(c)
	}

	var warning string
	if recoverErr != nil {
		warning = fmt.Sprintf("warning: recover objects from index failed, showing the locally tracked set:\n  %v\n", recoverErr)
	}

	ids := w.ObjectIDs()
	if len(ids) == 0 {
		return warning + "no objects yet (use object create)", [32]byte{}, nil
	}

	var b strings.Builder
	b.WriteString(warning)
	fmt.Fprintf(&b, "objects %s (%d):", readLabel(lc != nil), len(ids))

	for _, id := range ids {
		obj, err := c.GetObject(id)
		if err != nil {
			fmt.Fprintf(&b, "\n  %s  (unavailable)", hex.EncodeToString(id[:]))
			continue
		}
		fmt.Fprintf(&b, "\n  %s  v%d  owner=%s  rep=%d",
			hex.EncodeToString(id[:]), obj.Version, hex.EncodeToString(obj.Owner[:4]), obj.Replication)
	}

	return b.String(), [32]byte{}, nil
}

// dispatchObjectSet handles: object set <id> <gasCoin> <content...>
func dispatchObjectSet(c *client.Client, w *client.Wallet, cmd command) (string, [32]byte, error) {
	objectID, err := parseHexID(arg(cmd, 0))
	if err != nil {
		return "", [32]byte{}, fmt.Errorf("usage: object set <id-hex> <gasCoin-hex> <content...>")
	}

	gasCoinID, err := parseHexID(arg(cmd, 1))
	if err != nil {
		return "", [32]byte{}, fmt.Errorf("usage: object set <id-hex> <gasCoin-hex> <content...>")
	}

	content := strings.Join(cmd.args[2:], " ")

	if err := w.RefreshCoin(c, gasCoinID); err != nil {
		return "", [32]byte{}, fmt.Errorf("refresh gas coin:\n%w", err)
	}

	txHash, err := w.SetObject(c, objectID, []byte(content), gasCoinID)
	if err != nil {
		return "", [32]byte{}, err
	}

	return fmt.Sprintf("object %s content updated", hex.EncodeToString(objectID[:4])), txHash, nil
}

// dispatchObjectTransfer handles: object transfer <id> <to> <gasCoin>
func dispatchObjectTransfer(c *client.Client, w *client.Wallet, cmd command) (string, [32]byte, error) {
	objectID, err := parseHexID(arg(cmd, 0))
	if err != nil {
		return "", [32]byte{}, fmt.Errorf("usage: object transfer <id-hex> <to-hex> <gasCoin-hex>")
	}

	recipient, err := parseHexID(arg(cmd, 1))
	if err != nil {
		return "", [32]byte{}, fmt.Errorf("usage: object transfer <id-hex> <to-hex> <gasCoin-hex>")
	}

	gasCoinID, err := parseHexID(arg(cmd, 2))
	if err != nil {
		return "", [32]byte{}, fmt.Errorf("usage: object transfer <id-hex> <to-hex> <gasCoin-hex>")
	}

	if err := w.RefreshCoin(c, gasCoinID); err != nil {
		return "", [32]byte{}, fmt.Errorf("refresh gas coin:\n%w", err)
	}

	txHash, err := w.TransferObject(c, objectID, recipient, gasCoinID)
	if err != nil {
		return "", [32]byte{}, err
	}

	return fmt.Sprintf("object %s transferred to %s",
		hex.EncodeToString(objectID[:4]), hex.EncodeToString(recipient[:4])), txHash, nil
}

// dispatchObjectReparent handles: object reparent <id> <newParent> <gasCoin>
func dispatchObjectReparent(c *client.Client, w *client.Wallet, cmd command) (string, [32]byte, error) {
	objectID, err := parseHexID(arg(cmd, 0))
	if err != nil {
		return "", [32]byte{}, fmt.Errorf("usage: object reparent <id-hex> <newParent-hex> <gasCoin-hex>")
	}

	newParent, err := parseHexID(arg(cmd, 1))
	if err != nil {
		return "", [32]byte{}, fmt.Errorf("usage: object reparent <id-hex> <newParent-hex> <gasCoin-hex>")
	}

	gasCoinID, err := parseHexID(arg(cmd, 2))
	if err != nil {
		return "", [32]byte{}, fmt.Errorf("usage: object reparent <id-hex> <newParent-hex> <gasCoin-hex>")
	}

	if err := w.RefreshCoin(c, gasCoinID); err != nil {
		return "", [32]byte{}, fmt.Errorf("refresh gas coin:\n%w", err)
	}

	txHash, err := w.Reparent(c, objectID, newParent, gasCoinID)
	if err != nil {
		return "", [32]byte{}, err
	}

	return fmt.Sprintf("object %s reparented under %s",
		hex.EncodeToString(objectID[:4]), hex.EncodeToString(newParent[:4])), txHash, nil
}

// dispatchObjectDelete handles: object delete <id> <gasCoin>
func dispatchObjectDelete(c *client.Client, w *client.Wallet, cmd command) (string, [32]byte, error) {
	objectID, err := parseHexID(arg(cmd, 0))
	if err != nil {
		return "", [32]byte{}, fmt.Errorf("usage: object delete <id-hex> <gasCoin-hex>")
	}

	gasCoinID, err := parseHexID(arg(cmd, 1))
	if err != nil {
		return "", [32]byte{}, fmt.Errorf("usage: object delete <id-hex> <gasCoin-hex>")
	}

	if err := w.RefreshCoin(c, gasCoinID); err != nil {
		return "", [32]byte{}, fmt.Errorf("refresh gas coin:\n%w", err)
	}

	txHash, err := w.DeleteObject(c, objectID, gasCoinID)
	if err != nil {
		return "", [32]byte{}, err
	}

	return fmt.Sprintf("object %s deleted", hex.EncodeToString(objectID[:4])), txHash, nil
}

// dispatchObjectParent handles: object parent <id>. With a session
// LightClient (the wallet holds a trust checkpoint) it reads through the
// verification library and the edge is proved; otherwise it reads the
// node's unproven word.
func dispatchObjectParent(c *client.Client, lc *client.LightClient, cmd command) (string, [32]byte, error) {
	objectID, err := parseHexID(arg(cmd, 0))
	if err != nil {
		return "", [32]byte{}, fmt.Errorf("usage: object parent <id-hex>")
	}

	kind, parent, hasParent, err := objectParent(c, lc, objectID)
	if err != nil {
		return "", [32]byte{}, err
	}

	proved := lc != nil

	if !hasParent {
		return fmt.Sprintf("object %s has no parent edge %s", hex.EncodeToString(objectID[:4]), readLabel(proved)), [32]byte{}, nil
	}

	kindName := "key"
	if kind == index.ObjectParentKind {
		kindName = "object"
	}

	return fmt.Sprintf("object %s parent (%s): %s %s",
		hex.EncodeToString(objectID[:4]), kindName, hex.EncodeToString(parent[:4]), readLabel(proved)), [32]byte{}, nil
}

// objectParent reads objectID's immediate parent edge through lc when it is
// non-nil (the wallet holds a trust checkpoint), or through the plain client
// otherwise. LightClient exposes only the full ancestry walk (Ancestors), so
// the immediate edge this command reports is that walk's first hop.
func objectParent(c *client.Client, lc *client.LightClient, objectID [32]byte) (kind byte, parent [32]byte, hasParent bool, err error) {
	if lc == nil {
		return c.Parent(objectID)
	}

	chain, err := lc.Ancestors(objectID)
	if err != nil || len(chain) == 0 {
		return 0, [32]byte{}, false, err
	}

	return chain[0].ParentKind, chain[0].Parent, true, nil
}

// dispatchObjectShow handles: object show <id>
func dispatchObjectShow(c *client.Client, cmd command) (string, [32]byte, error) {
	objectID, err := parseHexID(arg(cmd, 0))
	if err != nil {
		return "", [32]byte{}, fmt.Errorf("usage: object show <id-hex>")
	}

	obj, err := c.GetObject(objectID)
	if err != nil {
		return "", [32]byte{}, err
	}

	line := fmt.Sprintf("object %s  owner=%s  v%d  rep=%d  content=%q",
		hex.EncodeToString(objectID[:4]),
		hex.EncodeToString(obj.Owner[:4]),
		obj.Version,
		obj.Replication,
		obj.Content)

	return line, [32]byte{}, nil
}

// dispatchObjectHolders handles: object holders <id>
func dispatchObjectHolders(c *client.Client, cmd command) (string, [32]byte, error) {
	objectID, err := parseHexID(arg(cmd, 0))
	if err != nil {
		return "", [32]byte{}, fmt.Errorf("usage: object holders <id-hex>")
	}

	report, err := c.Holders(objectID)
	if err != nil {
		return "", [32]byte{}, err
	}

	line := fmt.Sprintf("object %s  expected=%d actual=%d",
		hex.EncodeToString(objectID[:4]), len(report.Expected), len(report.Actual))

	return line, [32]byte{}, nil
}
