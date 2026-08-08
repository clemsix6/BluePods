package main

import (
	"encoding/hex"
	"fmt"

	"BluePods/pkg/client"
)

// cmdObjects handles the top-level `objects [owner-or-parent-id-hex]`
// command. Both forms read through the light client and print "(proved)"
// when this invocation holds a trust checkpoint, or the node's unproven word
// otherwise (see pkg/client/indexreads.go) — proved meaning the read
// genuinely verified, never merely that the checkpointed path was attempted:
// a verification failure prints the unproven label, not a false "proved" one.
// With no argument it recovers the wallet's own object set from the index —
// spec §10's recovery rule, ListChildren(pubkey) plus recursion into
// object-parented subtrees — merging discovered IDs into whatever the wallet
// already tracks. A recovery failure (a genuine verification failure
// included, never silently retried against the unproven path — including
// the transport's single-frame ceiling for a large wallet, spec §10, 6.1's
// as-built) does not abort the command: it prints a warning and falls back
// to whatever the wallet already tracks locally, since a stale-but-present
// list is more useful than none. With an argument it enumerates a given
// owner key or object ID's subtree without touching any local wallet state
// beyond the checkpoint itself, which a proved walk still opportunistically
// advances and persists.
func cmdObjects(e *env, args []string) error {
	if len(args) > 1 {
		return fmt.Errorf("usage: objects [owner-or-parent-id-hex]")
	}

	cli, err := connect(e)
	if err != nil {
		return err
	}

	w, err := wallet(e)
	if err != nil {
		return err
	}

	if len(args) == 1 {
		root, err := parseHash(args[0])
		if err != nil {
			return fmt.Errorf("parse id:\n%w", err)
		}

		ids, proved, persistErr, err := enumerateSubtree(e, cli, w, root)
		if err != nil {
			return fmt.Errorf("enumerate objects:\n%w", err)
		}
		if persistErr != nil {
			fmt.Printf("warning: could not persist the advanced checkpoint, it will re-sync on the next invocation:\n  %v\n", persistErr)
		}

		return printObjectIDs(fmt.Sprintf("objects under %s %s", hex.EncodeToString(root[:8]), readLabel(proved)), ids)
	}

	proved, persistErr, err := recoverObjects(e, cli, w)
	if err != nil {
		fmt.Printf("warning: recover objects from index failed, showing the locally tracked set:\n  %v\n", err)
	}
	if persistErr != nil {
		fmt.Printf("warning: could not persist the advanced checkpoint, it will re-sync on the next invocation:\n  %v\n", persistErr)
	}

	return printObjectIDs("objects "+readLabel(proved), w.ObjectIDs())
}

// recoverObjects runs w.RecoverObjects through the light client when w holds
// a trust checkpoint, or through the plain client otherwise. proved reports
// whether the read actually verified: false whenever recovery itself fails —
// a forged or unreachable proof leaves the caller with nothing trustworthy
// beyond the locally tracked set, checkpoint present or not — never merely
// because the checkpointed path was attempted. persistErr is orthogonal: a
// failure to save the checkpoint a successful verified walk advanced does
// not retract the verification that already happened, so it is reported
// separately and never turns a proved read back into an unproven one.
func recoverObjects(e *env, cli *client.Client, w *client.Wallet) (proved bool, persistErr, err error) {
	cp, ok := w.Checkpoint()
	if !ok {
		_, err := w.RecoverObjects(cli)
		return false, nil, err
	}

	lc := client.NewLightClient(cli, cp)
	if _, err := w.RecoverObjects(lc); err != nil {
		return false, nil, err
	}

	return true, syncCheckpoint(e, w, lc), nil
}

// enumerateSubtree walks root's subtree through the light client when w
// holds a trust checkpoint, re-persisting whatever epoch walk that read
// performed before returning, or through the plain client otherwise — the
// same proved/unproven split every other index-reading verb makes (see
// recoverObjects). A verification failure is returned as-is, never silently
// retried against the unproven path; a failure to persist the advanced
// checkpoint is reported separately in persistErr and does not withhold the
// already-verified result.
func enumerateSubtree(e *env, cli *client.Client, w *client.Wallet, root [32]byte) (ids [][32]byte, proved bool, persistErr, err error) {
	cp, ok := w.Checkpoint()
	if !ok {
		ids, err = client.EnumerateSubtree(cli, root)
		return ids, false, nil, err
	}

	lc := client.NewLightClient(cli, cp)
	if ids, err = client.EnumerateSubtree(lc, root); err != nil {
		return nil, false, nil, err
	}

	return ids, true, syncCheckpoint(e, w, lc), nil
}

// printObjectIDs prints a labeled list of object IDs, one full hex ID per
// line so it can be copied for object show/set/transfer.
func printObjectIDs(label string, ids [][32]byte) error {
	if len(ids) == 0 {
		fmt.Printf("%s: none\n", label)
		return nil
	}

	fmt.Printf("%s (%d):\n", label, len(ids))
	for _, id := range ids {
		fmt.Printf("  %s\n", hex.EncodeToString(id[:]))
	}

	return nil
}
