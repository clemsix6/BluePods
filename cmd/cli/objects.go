package main

import (
	"encoding/hex"
	"fmt"

	"BluePods/pkg/client"
)

// cmdObjects handles the top-level `objects [owner-or-parent-id-hex]`
// command. With no argument it recovers the wallet's own object set from the
// index — spec §10's recovery rule, ListChildren(pubkey) plus recursion into
// object-parented subtrees — merging discovered IDs into whatever the wallet
// already tracks; with a trust checkpoint this recovery goes through the
// verification library and the label says "proved", otherwise it reads the
// node's unproven word (see pkg/client/indexreads.go). A recovery failure
// (a genuine verification failure included, never silently retried against
// the unproven path — including the transport's single-frame ceiling for a
// large wallet, spec §10, 6.1's as-built) does not abort the command: it
// prints a warning and falls back to whatever the wallet already tracks
// locally, since a stale-but-present list is more useful than none. With an
// argument it enumerates a given owner key or object ID's subtree without
// touching any local wallet state or checkpoint — always the node's
// unproven word, the same as before this command acquired a checkpoint at
// all.
func cmdObjects(e *env, args []string) error {
	if len(args) > 1 {
		return fmt.Errorf("usage: objects [owner-or-parent-id-hex]")
	}

	cli, err := connect(e)
	if err != nil {
		return err
	}

	if len(args) == 1 {
		root, err := parseHash(args[0])
		if err != nil {
			return fmt.Errorf("parse id:\n%w", err)
		}

		ids, err := cli.EnumerateSubtree(root)
		if err != nil {
			return fmt.Errorf("enumerate objects:\n%w", err)
		}

		return printObjectIDs(fmt.Sprintf("objects under %s", hex.EncodeToString(root[:8])), ids)
	}

	w, err := wallet(e)
	if err != nil {
		return err
	}

	proved, err := recoverObjects(e, cli, w)
	if err != nil {
		fmt.Printf("warning: recover objects from index failed, showing the locally tracked set:\n  %v\n", err)
	}

	return printObjectIDs("objects "+readLabel(proved), w.ObjectIDs())
}

// recoverObjects runs w.RecoverObjects through the light client when w holds
// a trust checkpoint, re-persisting whatever epoch walk that read performed
// before returning, or through the plain client otherwise. proved reports
// which path ran, independent of whether it succeeded, so the caller's
// warning and the printed label always agree on which guarantee was
// attempted.
func recoverObjects(e *env, cli *client.Client, w *client.Wallet) (proved bool, err error) {
	cp, ok := w.Checkpoint()
	if !ok {
		_, err := w.RecoverObjects(cli)
		return false, err
	}

	lc := client.NewLightClient(cli, cp)
	_, err = w.RecoverObjects(lc)

	if syncErr := syncCheckpoint(e, w, lc); syncErr != nil && err == nil {
		err = syncErr
	}

	return true, err
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
