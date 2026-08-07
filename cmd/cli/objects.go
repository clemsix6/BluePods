package main

import (
	"encoding/hex"
	"fmt"
)

// cmdObjects handles the top-level `objects [owner-or-parent-id-hex]`
// command. With no argument it recovers the wallet's own object set from the
// index — spec §10's recovery rule, ListChildren(pubkey) plus recursion into
// object-parented subtrees — merging discovered IDs into whatever the wallet
// already tracks. A recovery failure (including the transport's single-frame
// ceiling for a large wallet — spec §10, 6.1's as-built) does not abort the
// command: it prints a warning and falls back to whatever the wallet already
// tracks locally, since a stale-but-present list is more useful than none.
// With an argument it enumerates a given owner key or object ID's subtree
// without touching any local wallet state. Both read the node's unproven
// word: bpctl acquires no trusted checkpoint (see pkg/client/indexreads.go).
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

	if _, err := w.RecoverObjects(cli); err != nil {
		fmt.Printf("warning: recover objects from index failed, showing the locally tracked set:\n  %v\n", err)
	}

	return printObjectIDs("objects", w.ObjectIDs())
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
