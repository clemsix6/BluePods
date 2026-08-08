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
  --checkpoint <epoch>:<index-root-hex>:<validator-set-hash-hex>
                      Trust checkpoint for proved index reads (domain
                      resolve, objects, object parent go through the
                      verification library instead of an honest unproven
                      read). Persisted alongside --key's wallet file and
                      reloaded on later runs; passing it again overwrites the
                      stored one. Three colon-separated fields, unlike the
                      node's --trust-checkpoint <epoch>:<validator-root hex>:
                      a light client also pins the index root it verifies
                      proofs against.

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
  object reparent <id-hex> <new-parent-hex> <gas-coin-hex>
                                         Move an object under a new ObjectParent
  object delete <id-hex> <gas-coin-hex>  Destroy an object (must have no children)
  object parent <id-hex>                 Show an object's immediate parent edge
  object holders <id-hex>                Show actual vs rendezvous-expected holders
  objects [owner-or-parent-id-hex]       List objects: recovered from the index
                                         under this wallet's key, or under the
                                         given owner key / object ID
  domain register --name <name> --term <epochs> --gas-coin <hex>
                   (--object <id-hex> | --replication N [--content STRING])
                                         Register a name against an existing or
                                         brand-new object
  domain renew <name> <term-epochs> <gas-coin-hex>
                                         Extend a name's lease
  domain update <name> <object-id-hex> <gas-coin-hex>
                                         Repoint a name at another owned object
  domain transfer <name> <new-owner-hex> <gas-coin-hex>
                                         Hand a name to a new owner
  domain delete <name> <gas-coin-hex>    Remove a name from the registry
  domain resolve <name>                  Resolve a name to its object ID
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
	checkpointHex := fs.String("checkpoint", "", "trust checkpoint for proved index reads: <epoch>:<index-root hex>:<validator-set-hash hex>")

	if err := fs.Parse(args); err != nil {
		return err
	}

	rest := fs.Args()

	var checkpointFlag *client.Checkpoint
	if *checkpointHex != "" {
		cp, err := parseCheckpointFlag(*checkpointHex)
		if err != nil {
			return err
		}
		checkpointFlag = &cp
	}

	env := &env{nodeAddr: *nodeAddr, keyPath: *keyPath, checkpointFlag: checkpointFlag}

	if len(rest) == 0 {
		if term.IsTerminal(int(os.Stdout.Fd())) {
			return runConsole(env)
		}
		usage()
		return errors.New("no command given")
	}

	return dispatch(env, rest[0], rest[1:])
}

// runConsole connects, loads or creates the wallet, opens the interactive console,
// and persists the wallet on exit when a key path is set.
func runConsole(e *env) error {
	cli, err := connect(e)
	if err != nil {
		return err
	}

	walletPath := walletFilePath(e)

	w, err := loadOrBuildWallet(e, walletPath)
	if err != nil {
		return err
	}

	runErr := tui.RunConsole(cli, w, e.nodeAddr)

	if walletPath != "" {
		if saveErr := w.Save(walletPath); saveErr != nil {
			fmt.Fprintf(os.Stderr, "warning: save wallet: %v\n", saveErr)
		}
	}

	return runErr
}

// loadOrBuildWallet loads a wallet from walletPath when the file exists, or
// builds a fresh wallet from the key otherwise. A --checkpoint flag given on
// this invocation then overwrites whatever checkpoint the wallet carries —
// spec §10's deliberate rotation, never merged with the stored one.
func loadOrBuildWallet(e *env, walletPath string) (*client.Wallet, error) {
	w, err := existingOrFreshWallet(e, walletPath)
	if err != nil {
		return nil, err
	}

	if e.checkpointFlag != nil {
		w.SetCheckpoint(*e.checkpointFlag)
	}

	return w, nil
}

// existingOrFreshWallet loads a wallet from walletPath when the file exists,
// or builds a fresh one from the key otherwise.
func existingOrFreshWallet(e *env, walletPath string) (*client.Wallet, error) {
	if walletPath != "" {
		if _, err := os.Stat(walletPath); err == nil {
			w, err := client.LoadWallet(walletPath)
			if err != nil {
				return nil, fmt.Errorf("load wallet:\n%w", err)
			}

			return w, nil
		}
	}

	return freshWallet(e)
}

// walletFilePath returns the JSON wallet file a --key path implies, or ""
// when no key path was given: an ephemeral key carries no file to persist a
// checkpoint (or anything else) into.
func walletFilePath(e *env) string {
	if e.keyPath == "" {
		return ""
	}

	return e.keyPath + ".wallet"
}

// env holds the resolved global flags shared across subcommands.
type env struct {
	nodeAddr       string             // nodeAddr is the node's QUIC address
	keyPath        string             // keyPath is the Ed25519 key file path (empty = ephemeral)
	checkpointFlag *client.Checkpoint // checkpointFlag is --checkpoint, parsed; nil when not given
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
	case "objects":
		return cmdObjects(e, args)
	case "domain":
		return cmdDomain(e, args)
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

// freshWallet builds a wallet straight from the resolved key path, with none
// of a wallet file's persisted state (coins, objects, trust checkpoint) —
// existingOrFreshWallet's fallback when no such file exists yet.
func freshWallet(e *env) (*client.Wallet, error) {
	priv, err := loadOrGenerateKey(e.keyPath)
	if err != nil {
		return nil, err
	}

	return client.NewWalletFromKey(priv), nil
}

// wallet loads the wallet a one-shot command runs against, the same way the
// console does: from --key's wallet file when one exists (a persisted trust
// checkpoint included), overwritten by --checkpoint when given. A one-shot
// invocation is otherwise the console's session compressed to a single
// command, and the checkpoint's persistence has to survive between the two
// just the same.
func wallet(e *env) (*client.Wallet, error) {
	return loadOrBuildWallet(e, walletFilePath(e))
}
