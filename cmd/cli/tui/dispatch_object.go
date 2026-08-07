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
func dispatchObject(c *client.Client, w *client.Wallet, cmd command) (string, [32]byte, error) {
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
		return dispatchObjectParent(c, rest)
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
// already tracked rather than replacing it — reading the node's unproven
// word (the console holds no trusted checkpoint; see pkg/client/
// indexreads.go). It then lists each object with its full hex ID and current
// version/owner, so the full ID can be copied for object show/set/transfer.
func dispatchObjects(c *client.Client, w *client.Wallet) (string, [32]byte, error) {
	if _, err := w.RecoverObjects(c); err != nil {
		return "", [32]byte{}, fmt.Errorf("recover objects from index:\n%w", err)
	}

	ids := w.ObjectIDs()
	if len(ids) == 0 {
		return "no objects yet (use object create)", [32]byte{}, nil
	}

	var b strings.Builder
	fmt.Fprintf(&b, "objects (%d):", len(ids))

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

// dispatchObjectParent handles: object parent <id>. It reads the node's
// unproven word (the console holds no trusted checkpoint).
func dispatchObjectParent(c *client.Client, cmd command) (string, [32]byte, error) {
	objectID, err := parseHexID(arg(cmd, 0))
	if err != nil {
		return "", [32]byte{}, fmt.Errorf("usage: object parent <id-hex>")
	}

	kind, parent, hasParent, err := c.Parent(objectID)
	if err != nil {
		return "", [32]byte{}, err
	}

	if !hasParent {
		return fmt.Sprintf("object %s has no parent edge", hex.EncodeToString(objectID[:4])), [32]byte{}, nil
	}

	kindName := "key"
	if kind == index.ObjectParentKind {
		kindName = "object"
	}

	return fmt.Sprintf("object %s parent (%s): %s",
		hex.EncodeToString(objectID[:4]), kindName, hex.EncodeToString(parent[:4])), [32]byte{}, nil
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
