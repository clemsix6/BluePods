package main

import (
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"BluePods/pkg/client"
)

// checkpointFieldSeparator splits the --checkpoint value into its three
// fields — the same separator cmd/node/checkpoint.go uses for its own
// two-field --trust-checkpoint, kept identical on purpose so the two flags
// read as the same idiom at a glance.
const checkpointFieldSeparator = ":"

// errCheckpointFlagFormat rejects a malformed --checkpoint value.
var errCheckpointFlagFormat = errors.New("malformed --checkpoint")

// parseCheckpointFlag parses --checkpoint's value into the light client's
// trust anchor: "<epoch>:<index-root hex>:<validator-set-hash hex>". This is
// ONE FIELD MORE than the node's own --trust-checkpoint (cmd/node/
// checkpoint.go), which pins only the validator set — a joining node
// rebuilds its index root locally and has nothing else to check it against,
// while a light client verifies proofs against a root it can never
// recompute itself (see pkg/client/verify.go's Checkpoint), so it must pin
// that root too. Passing the node's shorter 2-field form here is refused,
// not padded or guessed.
func parseCheckpointFlag(value string) (client.Checkpoint, error) {
	epochText, rest, found := strings.Cut(value, checkpointFieldSeparator)
	if !found {
		return client.Checkpoint{}, fmt.Errorf("%w %q: want <epoch>:<index-root hex>:<validator-set-hash hex>", errCheckpointFlagFormat, value)
	}

	indexRootText, validatorSetHashText, found := strings.Cut(rest, checkpointFieldSeparator)
	if !found {
		return client.Checkpoint{}, fmt.Errorf("%w %q: want <epoch>:<index-root hex>:<validator-set-hash hex> (3 fields; the node's --trust-checkpoint has 2)", errCheckpointFlagFormat, value)
	}

	epoch, err := strconv.ParseUint(epochText, 10, 64)
	if err != nil {
		return client.Checkpoint{}, fmt.Errorf("%w %q: epoch is not a number", errCheckpointFlagFormat, value)
	}

	indexRoot, err := parseCheckpointRoot(value, indexRootText)
	if err != nil {
		return client.Checkpoint{}, err
	}

	validatorSetHash, err := parseCheckpointRoot(value, validatorSetHashText)
	if err != nil {
		return client.Checkpoint{}, err
	}

	return client.Checkpoint{Epoch: epoch, IndexRoot: indexRoot, ValidatorSetHash: validatorSetHash}, nil
}

// parseCheckpointRoot decodes one 32-byte hex field of a --checkpoint value,
// reporting the whole original value in the error so a strict, silently-
// unenforced checkpoint never happens for want of a clear message.
func parseCheckpointRoot(value, field string) ([32]byte, error) {
	var out [32]byte

	raw, err := hex.DecodeString(field)
	if err != nil {
		return out, fmt.Errorf("%w %q: %q is not hex", errCheckpointFlagFormat, value, field)
	}

	if len(raw) != 32 {
		return out, fmt.Errorf("%w %q: %q is %d bytes, want 32", errCheckpointFlagFormat, value, field, len(raw))
	}

	copy(out[:], raw)

	return out, nil
}

// syncCheckpoint re-pins w's checkpoint to whatever epoch walk lc most
// recently attempted (LightClient.Checkpoint — nil advance included, since
// re-setting the same value costs nothing) and, when e names a wallet file,
// saves it. This is spec §10's "persisted alongside the wallet": a
// light-client read this invocation performed must not cost the next one a
// fresh out-of-band pin.
func syncCheckpoint(e *env, w *client.Wallet, lc *client.LightClient) error {
	w.SetCheckpoint(lc.Checkpoint())

	walletPath := walletFilePath(e)
	if walletPath == "" {
		return nil
	}

	if err := w.Save(walletPath); err != nil {
		return fmt.Errorf("persist advanced checkpoint:\n%w", err)
	}

	return nil
}

// readLabel discreetly tags a printed index answer with the guarantee it
// carries, so a user cannot mistake one for the other.
func readLabel(proved bool) string {
	if proved {
		return "(proved)"
	}

	return "(unproven)"
}
