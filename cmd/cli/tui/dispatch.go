package tui

import (
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"

	"BluePods/pkg/client"
)

// dispatch executes a parsed command against the client and wallet and returns a
// one-line activity result plus, when a transaction was submitted, its hash to
// track. An empty hash means nothing to track. This is the console's adapter over
// the typed pkg/client action surface; it adds no protocol logic.
//
// Hash tracking: faucet, transfer, split, object create/set/transfer/reparent/
// delete and domain register/renew/update/transfer/delete all return a
// non-zero txHash that flows into the console's tracked map for live status
// polling.
func dispatch(c *client.Client, w *client.Wallet, cmd command) (line string, track [32]byte, err error) {
	switch cmd.verb {
	case "faucet":
		return dispatchFaucet(c, w, cmd)
	case "import":
		return dispatchImport(w, cmd)
	case "transfer":
		return dispatchTransfer(c, w, cmd)
	case "split":
		return dispatchSplit(c, w, cmd)
	case "object":
		return dispatchObject(c, w, cmd)
	case "domain":
		return dispatchDomain(c, w, cmd)
	case "validators":
		return dispatchValidators(c)
	case "balance":
		return dispatchBalance(c, w)
	case "coins":
		return dispatchCoins(c, w)
	case "objects":
		return dispatchObjects(c, w)
	case "pubkey":
		return dispatchPubkey(w)
	case "help":
		return helpText(), track, nil
	default:
		return "", track, fmt.Errorf("unknown command: %s (type help for usage)", cmd.verb)
	}
}

// dispatchFaucet handles: faucet <amount>
func dispatchFaucet(c *client.Client, w *client.Wallet, cmd command) (string, [32]byte, error) {
	amount, err := strconv.ParseUint(arg(cmd, 0), 10, 64)
	if err != nil {
		return "", [32]byte{}, fmt.Errorf("usage: faucet <amount>")
	}

	coin, txHash, err := c.Faucet(w.Pubkey(), amount)
	if err != nil {
		return "", [32]byte{}, err
	}

	w.Track(coin)

	return fmt.Sprintf("faucet %d -> coin %s", amount, hex.EncodeToString(coin[:4])), txHash, nil
}

// dispatchImport handles: import <coin-hex>
func dispatchImport(w *client.Wallet, cmd command) (string, [32]byte, error) {
	id, err := parseHexID(arg(cmd, 0))
	if err != nil {
		return "", [32]byte{}, fmt.Errorf("usage: import <coin-id-hex>")
	}

	w.Track(id)

	return "imported coin " + hex.EncodeToString(id[:4]), [32]byte{}, nil
}

// dispatchTransfer handles: transfer <coin> <to>
func dispatchTransfer(c *client.Client, w *client.Wallet, cmd command) (string, [32]byte, error) {
	id, err := parseHexID(arg(cmd, 0))
	to, err2 := parseHexID(arg(cmd, 1))
	if err != nil || err2 != nil {
		return "", [32]byte{}, fmt.Errorf("usage: transfer <coin-hex> <to-hex>")
	}

	if err := w.RefreshCoin(c, id); err != nil {
		return "", [32]byte{}, err
	}

	txHash, err := w.Transfer(c, id, to)
	if err != nil {
		return "", [32]byte{}, err
	}

	return fmt.Sprintf("transfer %s -> %s", hex.EncodeToString(id[:4]), hex.EncodeToString(to[:4])), txHash, nil
}

// dispatchSplit handles: split <coin> <amount> <to>
func dispatchSplit(c *client.Client, w *client.Wallet, cmd command) (string, [32]byte, error) {
	coinID, err := parseHexID(arg(cmd, 0))
	if err != nil {
		return "", [32]byte{}, fmt.Errorf("usage: split <coin-hex> <amount> <to-hex>")
	}

	amount, err := strconv.ParseUint(arg(cmd, 1), 10, 64)
	if err != nil {
		return "", [32]byte{}, fmt.Errorf("usage: split <coin-hex> <amount> <to-hex>")
	}

	recipient, err := parseHexID(arg(cmd, 2))
	if err != nil {
		return "", [32]byte{}, fmt.Errorf("usage: split <coin-hex> <amount> <to-hex>")
	}

	if err := w.RefreshCoin(c, coinID); err != nil {
		return "", [32]byte{}, err
	}

	newCoin, txHash, err := w.Split(c, coinID, amount, recipient)
	if err != nil {
		return "", [32]byte{}, err
	}

	return fmt.Sprintf("split %d from %s -> new coin %s to %s",
		amount, hex.EncodeToString(coinID[:4]), hex.EncodeToString(newCoin[:4]), hex.EncodeToString(recipient[:4])), txHash, nil
}

// dispatchValidators handles: validators
func dispatchValidators(c *client.Client) (string, [32]byte, error) {
	vals, err := c.Validators()
	if err != nil {
		return "", [32]byte{}, err
	}

	return fmt.Sprintf("validators: %d active", len(vals)), [32]byte{}, nil
}

// dispatchBalance handles: balance
func dispatchBalance(c *client.Client, w *client.Wallet) (string, [32]byte, error) {
	ids := w.CoinIDs()
	var total uint64
	for _, id := range ids {
		if err := w.RefreshCoin(c, id); err == nil {
			if ci := w.GetCoin(id); ci != nil {
				total += ci.Balance
			}
		}
	}

	return fmt.Sprintf("balance %d  (%d coins)", total, len(ids)), [32]byte{}, nil
}

// dispatchCoins handles: coins. It lists each known coin with its full hex ID and
// refreshed balance, so the full ID can be copied for use as a gas coin or
// transfer source.
func dispatchCoins(c *client.Client, w *client.Wallet) (string, [32]byte, error) {
	ids := w.CoinIDs()
	if len(ids) == 0 {
		return "no coins yet (use faucet)", [32]byte{}, nil
	}

	var b strings.Builder
	fmt.Fprintf(&b, "coins (%d):", len(ids))

	for _, id := range ids {
		var bal uint64
		if err := w.RefreshCoin(c, id); err == nil {
			if ci := w.GetCoin(id); ci != nil {
				bal = ci.Balance
			}
		}
		fmt.Fprintf(&b, "\n  %s  %d", hex.EncodeToString(id[:]), bal)
	}

	return b.String(), [32]byte{}, nil
}

// dispatchPubkey handles: pubkey
func dispatchPubkey(w *client.Wallet) (string, [32]byte, error) {
	pk := w.Pubkey()

	return "pubkey " + hex.EncodeToString(pk[:]), [32]byte{}, nil
}

// helpText returns the console command reference.
func helpText() string {
	return strings.TrimSpace(`
commands:
  faucet <amount>                        mint a coin to this wallet
  import <coin-hex>                      track an existing coin
  transfer <coin-hex> <to-hex>           transfer a coin
  split <coin-hex> <amount> <to-hex>     split a coin
  object create <rep> <gasCoin> [text]   create a replicated object
  object set <id> <gasCoin> <text>       update object content
  object transfer <id> <to> <gasCoin>    transfer object ownership
  object reparent <id> <newParent> <gasCoin>
                                          move an object under a new parent
  object delete <id> <gasCoin>           destroy an object (no children)
  object parent <id>                     show an object's immediate parent
  object show <id>                       show object info
  object holders <id>                    show holder report
  domain register <name> <objectId> <term> <gasCoin>
                                          register a name against an owned object
  domain renew <name> <term> <gasCoin>   extend a name's lease
  domain update <name> <objectId> <gasCoin>
                                          repoint a name at another owned object
  domain transfer <name> <newOwner> <gasCoin>
                                          hand a name to a new owner
  domain delete <name> <gasCoin>         remove a name from the registry
  domain resolve <name>                  resolve a name to its object ID
  validators                             list active validators
  balance                                show total balance
  coins                                  list known coins with full ids
  objects                                objects recovered from the index under
                                          this wallet's key (merged with tracked)
  pubkey                                 show this wallet's public key
  quit                                   exit the console`)
}

// arg returns the i-th argument or an empty string.
func arg(cmd command, i int) string {
	if i < len(cmd.args) {
		return cmd.args[i]
	}

	return ""
}

// parseHexID decodes a 32-byte hex ID.
func parseHexID(s string) ([32]byte, error) {
	var id [32]byte
	raw, err := hex.DecodeString(s)
	if err != nil || len(raw) != 32 {
		return id, fmt.Errorf("invalid 32-byte hex id: %q", s)
	}
	copy(id[:], raw)

	return id, nil
}
