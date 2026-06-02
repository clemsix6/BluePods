package tui

import (
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"

	"charm.land/lipgloss/v2"

	"BluePods/pkg/client"
)

// consoleState is the immutable view the console renders. The model assembles it
// from the client, the wallet, and the tracked-tx map.
type consoleState struct {
	NodeAddr   string   // NodeAddr is the connected node address
	Connected  bool     // Connected reports node reachability
	Round      uint64   // Round is the latest consensus round
	Epoch      uint64   // Epoch is the latest epoch
	Validators int      // Validators is the active validator count
	Peers      int      // Peers is the connected mesh-peer count
	TPS        float64  // TPS is the node's transactions-per-second
	Balance    uint64   // Balance is the summed balance of known coins
	CoinCount  int      // CoinCount is the number of known coins
	Activity   []string // Activity is the scrolling action and tx-status log
	Input      string   // Input is the current command-line text
}

var (
	headerStyle = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).Padding(0, 1)
	dotOK       = lipgloss.NewStyle().Foreground(lipgloss.Color("10")).Render("connected")
	dotDown     = lipgloss.NewStyle().Foreground(lipgloss.Color("9")).Render("disconnected")
)

// renderConsole renders the header, the activity body, and the input footer. It is
// pure and unit-tested.
func renderConsole(s consoleState) string {
	conn := dotDown
	if s.Connected {
		conn = dotOK
	}

	header := fmt.Sprintf("bpctl  node %s  %s\nround %d  epoch %d  val %d  peers %d  %.1f tps\nbalance %d  (%d coins)",
		s.NodeAddr, conn, s.Round, s.Epoch, s.Validators, s.Peers, s.TPS, s.Balance, s.CoinCount)

	body := strings.Join(s.Activity, "\n")

	return headerStyle.Render(header) + "\n" + body + "\n> " + s.Input
}

// dispatch executes a parsed command against the client and wallet and returns a
// one-line activity result plus, when a transaction was submitted, its hash to
// track. An empty hash means nothing to track. This is the console's adapter over
// the typed pkg/client action surface; it adds no protocol logic.
//
// Hash tracking: pkg/client methods do not expose the tx hash directly (they
// return predicted object IDs or plain errors), so no verb currently returns a
// non-zero track hash. Tracking wiring is kept for future extension.
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
	case "validators":
		return dispatchValidators(c)
	case "balance":
		return dispatchBalance(c, w)
	case "help":
		return helpText(), track, nil
	default:
		return "", track, fmt.Errorf("unknown command: %s (type help for usage)", cmd.verb)
	}
}

// dispatchFaucet handles: faucet <amount>
func dispatchFaucet(c *client.Client, w *client.Wallet, cmd command) (string, [32]byte, error) {
	var track [32]byte

	amount, err := strconv.ParseUint(arg(cmd, 0), 10, 64)
	if err != nil {
		return "", track, fmt.Errorf("usage: faucet <amount>")
	}

	coin, err := c.Faucet(w.Pubkey(), amount)
	if err != nil {
		return "", track, err
	}

	w.Track(coin)

	return fmt.Sprintf("faucet %d -> coin %s", amount, hex.EncodeToString(coin[:4])), track, nil
}

// dispatchImport handles: import <coin-hex>
func dispatchImport(w *client.Wallet, cmd command) (string, [32]byte, error) {
	var track [32]byte

	id, err := parseHexID(arg(cmd, 0))
	if err != nil {
		return "", track, fmt.Errorf("usage: import <coin-id-hex>")
	}

	w.Track(id)

	return "imported coin " + hex.EncodeToString(id[:4]), track, nil
}

// dispatchTransfer handles: transfer <coin> <to>
func dispatchTransfer(c *client.Client, w *client.Wallet, cmd command) (string, [32]byte, error) {
	var track [32]byte

	id, err := parseHexID(arg(cmd, 0))
	to, err2 := parseHexID(arg(cmd, 1))
	if err != nil || err2 != nil {
		return "", track, fmt.Errorf("usage: transfer <coin-hex> <to-hex>")
	}

	if err := w.RefreshCoin(c, id); err != nil {
		return "", track, err
	}

	if err := w.Transfer(c, id, to); err != nil {
		return "", track, err
	}

	return fmt.Sprintf("transfer %s -> %s", hex.EncodeToString(id[:4]), hex.EncodeToString(to[:4])), track, nil
}

// dispatchSplit handles: split <coin> <amount> <to>
func dispatchSplit(c *client.Client, w *client.Wallet, cmd command) (string, [32]byte, error) {
	var track [32]byte

	coinID, err := parseHexID(arg(cmd, 0))
	if err != nil {
		return "", track, fmt.Errorf("usage: split <coin-hex> <amount> <to-hex>")
	}

	amount, err := strconv.ParseUint(arg(cmd, 1), 10, 64)
	if err != nil {
		return "", track, fmt.Errorf("usage: split <coin-hex> <amount> <to-hex>")
	}

	recipient, err := parseHexID(arg(cmd, 2))
	if err != nil {
		return "", track, fmt.Errorf("usage: split <coin-hex> <amount> <to-hex>")
	}

	if err := w.RefreshCoin(c, coinID); err != nil {
		return "", track, err
	}

	newCoin, err := w.Split(c, coinID, amount, recipient)
	if err != nil {
		return "", track, err
	}

	return fmt.Sprintf("split %d from %s -> new coin %s to %s",
		amount, hex.EncodeToString(coinID[:4]), hex.EncodeToString(newCoin[:4]), hex.EncodeToString(recipient[:4])), track, nil
}

// dispatchObject routes object sub-commands.
func dispatchObject(c *client.Client, w *client.Wallet, cmd command) (string, [32]byte, error) {
	var track [32]byte

	if len(cmd.args) == 0 {
		return "", track, fmt.Errorf("usage: object <create|set|transfer|show|holders>")
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
	case "show":
		return dispatchObjectShow(c, rest)
	case "holders":
		return dispatchObjectHolders(c, rest)
	default:
		return "", track, fmt.Errorf("unknown object subcommand: %s", sub)
	}
}

// dispatchObjectCreate handles: object create <replication> <gasCoin> [content]
func dispatchObjectCreate(c *client.Client, w *client.Wallet, cmd command) (string, [32]byte, error) {
	var track [32]byte

	rep64, err := strconv.ParseUint(arg(cmd, 0), 10, 16)
	if err != nil {
		return "", track, fmt.Errorf("usage: object create <replication> <gasCoin-hex> [content]")
	}

	gasCoinID, err := parseHexID(arg(cmd, 1))
	if err != nil {
		return "", track, fmt.Errorf("usage: object create <replication> <gasCoin-hex> [content]")
	}

	content := strings.Join(cmd.args[2:], " ")

	if err := w.RefreshCoin(c, gasCoinID); err != nil {
		return "", track, fmt.Errorf("refresh gas coin:\n%w", err)
	}

	objectID, err := w.CreateObject(c, uint16(rep64), []byte(content), gasCoinID)
	if err != nil {
		return "", track, err
	}

	return fmt.Sprintf("object created %s (replication %d)", hex.EncodeToString(objectID[:4]), rep64), track, nil
}

// dispatchObjectSet handles: object set <id> <gasCoin> <content...>
func dispatchObjectSet(c *client.Client, w *client.Wallet, cmd command) (string, [32]byte, error) {
	var track [32]byte

	objectID, err := parseHexID(arg(cmd, 0))
	if err != nil {
		return "", track, fmt.Errorf("usage: object set <id-hex> <gasCoin-hex> <content...>")
	}

	gasCoinID, err := parseHexID(arg(cmd, 1))
	if err != nil {
		return "", track, fmt.Errorf("usage: object set <id-hex> <gasCoin-hex> <content...>")
	}

	content := strings.Join(cmd.args[2:], " ")

	if err := w.RefreshCoin(c, gasCoinID); err != nil {
		return "", track, fmt.Errorf("refresh gas coin:\n%w", err)
	}

	if err := w.SetObject(c, objectID, []byte(content), gasCoinID); err != nil {
		return "", track, err
	}

	return fmt.Sprintf("object %s content updated", hex.EncodeToString(objectID[:4])), track, nil
}

// dispatchObjectTransfer handles: object transfer <id> <to> <gasCoin>
func dispatchObjectTransfer(c *client.Client, w *client.Wallet, cmd command) (string, [32]byte, error) {
	var track [32]byte

	objectID, err := parseHexID(arg(cmd, 0))
	if err != nil {
		return "", track, fmt.Errorf("usage: object transfer <id-hex> <to-hex> <gasCoin-hex>")
	}

	recipient, err := parseHexID(arg(cmd, 1))
	if err != nil {
		return "", track, fmt.Errorf("usage: object transfer <id-hex> <to-hex> <gasCoin-hex>")
	}

	gasCoinID, err := parseHexID(arg(cmd, 2))
	if err != nil {
		return "", track, fmt.Errorf("usage: object transfer <id-hex> <to-hex> <gasCoin-hex>")
	}

	if err := w.RefreshCoin(c, gasCoinID); err != nil {
		return "", track, fmt.Errorf("refresh gas coin:\n%w", err)
	}

	if err := w.TransferObject(c, objectID, recipient, gasCoinID); err != nil {
		return "", track, err
	}

	return fmt.Sprintf("object %s transferred to %s",
		hex.EncodeToString(objectID[:4]), hex.EncodeToString(recipient[:4])), track, nil
}

// dispatchObjectShow handles: object show <id>
func dispatchObjectShow(c *client.Client, cmd command) (string, [32]byte, error) {
	var track [32]byte

	objectID, err := parseHexID(arg(cmd, 0))
	if err != nil {
		return "", track, fmt.Errorf("usage: object show <id-hex>")
	}

	obj, err := c.GetObject(objectID)
	if err != nil {
		return "", track, err
	}

	line := fmt.Sprintf("object %s  owner=%s  v%d  rep=%d  content=%q",
		hex.EncodeToString(objectID[:4]),
		hex.EncodeToString(obj.Owner[:4]),
		obj.Version,
		obj.Replication,
		obj.Content)

	return line, track, nil
}

// dispatchObjectHolders handles: object holders <id>
func dispatchObjectHolders(c *client.Client, cmd command) (string, [32]byte, error) {
	var track [32]byte

	objectID, err := parseHexID(arg(cmd, 0))
	if err != nil {
		return "", track, fmt.Errorf("usage: object holders <id-hex>")
	}

	report, err := c.Holders(objectID)
	if err != nil {
		return "", track, err
	}

	line := fmt.Sprintf("object %s  expected=%d actual=%d",
		hex.EncodeToString(objectID[:4]), len(report.Expected), len(report.Actual))

	return line, track, nil
}

// dispatchValidators handles: validators
func dispatchValidators(c *client.Client) (string, [32]byte, error) {
	var track [32]byte

	vals, err := c.Validators()
	if err != nil {
		return "", track, err
	}

	return fmt.Sprintf("validators: %d active", len(vals)), track, nil
}

// dispatchBalance handles: balance
func dispatchBalance(c *client.Client, w *client.Wallet) (string, [32]byte, error) {
	var track [32]byte

	ids := w.CoinIDs()
	var total uint64
	for _, id := range ids {
		if err := w.RefreshCoin(c, id); err == nil {
			if ci := w.GetCoin(id); ci != nil {
				total += ci.Balance
			}
		}
	}

	return fmt.Sprintf("balance %d  (%d coins)", total, len(ids)), track, nil
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
  object show <id>                       show object info
  object holders <id>                    show holder report
  validators                             list active validators
  balance                                show total balance
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
