package main

import (
	"encoding/hex"
	"fmt"
	"sort"

	"BluePods/pkg/client"
)

// cmdObjectHolders prints which validators actually hold an object versus the
// rendezvous-expected holder set.
func cmdObjectHolders(e *env, args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: object holders <id-hex>")
	}

	id, err := parseHash(args[0])
	if err != nil {
		return fmt.Errorf("parse object id:\n%w", err)
	}

	cli, err := connect(e)
	if err != nil {
		return err
	}

	report, err := cli.Holders(id)
	if err != nil {
		return fmt.Errorf("holders:\n%w", err)
	}

	vals, err := cli.Validators()
	if err != nil {
		return fmt.Errorf("get validators:\n%w", err)
	}

	printHolders(vals, report.Expected, report.Actual)

	return nil
}

// printHolders prints one line per validator, marking actual and expected status.
func printHolders(vals []client.ValidatorInfo, expected, actual map[[32]byte]bool) {
	sorted := make([]client.ValidatorInfo, len(vals))
	copy(sorted, vals)
	sort.Slice(sorted, func(i, j int) bool {
		return hex.EncodeToString(sorted[i].Pubkey[:]) < hex.EncodeToString(sorted[j].Pubkey[:])
	})

	for _, v := range sorted {
		fmt.Printf("%s  %-21s  held=%-5t expected=%t\n",
			hex.EncodeToString(v.Pubkey[:8]), v.QUICAddr, actual[v.Pubkey], expected[v.Pubkey])
	}

	fmt.Printf("\nactual holders: %d, expected holders: %d\n", len(actual), len(expected))
}
