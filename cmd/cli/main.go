package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"flag"
	"fmt"
	"os"

	"golang.org/x/term"

	"BluePods/cmd/cli/tui"
	"BluePods/pkg/client"
)

// defaultNodeAddr is the QUIC address used when --node is not given.
const defaultNodeAddr = "127.0.0.1:9000"

// usage prints the top-level command help to stderr.
func usage() {
	fmt.Fprint(os.Stderr, `bpctl - BluePods control CLI

Usage:
  bpctl [--node <quicaddr>] [--key <path>] <command> [args]

Global flags:
  --node <quicaddr>   Node QUIC address (default 127.0.0.1:9000)
  --key  <path>       Ed25519 key file (generated if missing; ephemeral if unset)

Commands:
  status                                 Show round, epoch, validator count, last commit
  validators                             List validators (pubkey hex, QUIC addr)
  coin faucet <pubkey-hex> <amount>      Mint a coin to a public key
  coin transfer <coin-id-hex> <to-hex>   Transfer a coin to a public key
  object create --replication N [--content STRING]
                                         Create a replicated object, print its ID hex
  object show <id-hex>                   Show owner, version, replication, content
  object set <id-hex> <STRING>           Overwrite content via set_object
  object transfer <id-hex> <to-hex>      Transfer ownership
  object holders <id-hex>                Show actual vs rendezvous-expected holders
`)
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// run parses global flags, then dispatches to the named subcommand.
func run(args []string) error {
	fs := flag.NewFlagSet("bpctl", flag.ContinueOnError)
	fs.Usage = usage

	nodeAddr := fs.String("node", defaultNodeAddr, "node QUIC address")
	keyPath := fs.String("key", "", "Ed25519 key file path")

	if err := fs.Parse(args); err != nil {
		return err
	}

	rest := fs.Args()

	env := &env{nodeAddr: *nodeAddr, keyPath: *keyPath}

	if len(rest) == 0 {
		if term.IsTerminal(int(os.Stdout.Fd())) {
			return runConsole(env)
		}
		usage()
		return errors.New("no command given")
	}

	return dispatch(env, rest[0], rest[1:])
}

// runConsole connects, loads or creates the wallet, and opens the interactive console.
func runConsole(e *env) error {
	cli, err := connect(e)
	if err != nil {
		return err
	}

	w, err := wallet(e)
	if err != nil {
		return err
	}

	return tui.RunConsole(cli, w, e.nodeAddr)
}

// env holds the resolved global flags shared across subcommands.
type env struct {
	nodeAddr string // nodeAddr is the node's QUIC address
	keyPath  string // keyPath is the Ed25519 key file path (empty = ephemeral)
}

// dispatch routes a subcommand name to its handler.
func dispatch(e *env, cmd string, args []string) error {
	switch cmd {
	case "status":
		return cmdStatus(e)
	case "validators":
		return cmdValidators(e)
	case "coin":
		return cmdCoin(e, args)
	case "object":
		return cmdObject(e, args)
	default:
		usage()
		return fmt.Errorf("unknown command: %s", cmd)
	}
}

// loadOrGenerateKey loads an Ed25519 key from the path, generating and saving a
// new one if the file is missing. An empty path yields an ephemeral key.
func loadOrGenerateKey(keyPath string) (ed25519.PrivateKey, error) {
	if keyPath == "" {
		_, priv, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			return nil, fmt.Errorf("generate ephemeral key:\n%w", err)
		}

		return priv, nil
	}

	data, err := os.ReadFile(keyPath)
	if os.IsNotExist(err) {
		return generateAndSaveKey(keyPath)
	}

	if err != nil {
		return nil, fmt.Errorf("read key file:\n%w", err)
	}

	if len(data) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("invalid key size: got %d, want %d", len(data), ed25519.PrivateKeySize)
	}

	return ed25519.PrivateKey(data), nil
}

// generateAndSaveKey creates a new Ed25519 key and writes it to path.
func generateAndSaveKey(path string) (ed25519.PrivateKey, error) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate key:\n%w", err)
	}

	if err := os.WriteFile(path, priv, 0600); err != nil {
		return nil, fmt.Errorf("save key to %s:\n%w", path, err)
	}

	return priv, nil
}

// connect dials the node and returns a client. The system pod ID is read from
// the node's status response, so no local WASM path is needed.
func connect(e *env) (*client.Client, error) {
	t := client.NewQUICTransport(e.nodeAddr)

	status, err := t.Status()
	if err != nil {
		return nil, fmt.Errorf("query status from %s:\n%w", e.nodeAddr, err)
	}

	cli, err := client.NewClient(e.nodeAddr, status.SystemPod)
	if err != nil {
		return nil, fmt.Errorf("connect to node:\n%w", err)
	}

	return cli, nil
}

// wallet loads the wallet from the resolved key path.
func wallet(e *env) (*client.Wallet, error) {
	priv, err := loadOrGenerateKey(e.keyPath)
	if err != nil {
		return nil, err
	}

	return client.NewWalletFromKey(priv), nil
}
