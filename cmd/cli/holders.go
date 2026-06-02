package main

import (
	"encoding/hex"
	"fmt"
	"sort"

	"BluePods/internal/attest"
	"BluePods/internal/validators"
	"BluePods/pkg/client"
)

// cmdObjectHolders prints which validators actually hold an object versus the
// rendezvous-expected holder set for its replication factor.
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

	obj, err := cli.GetObject(id)
	if err != nil {
		return fmt.Errorf("get object:\n%w", err)
	}

	vals, err := cli.Validators()
	if err != nil {
		return fmt.Errorf("get validators:\n%w", err)
	}

	expected := expectedHolders(vals, id, int(obj.Replication))
	actual := probeHolders(vals, id)

	printHolders(vals, expected, actual)

	return nil
}

// expectedHolders returns the set of validator pubkeys the rendezvous hashing
// assigns to the object for its replication factor.
func expectedHolders(vals []client.ValidatorInfo, id [32]byte, replication int) map[[32]byte]bool {
	pubkeys := make([]validators.Hash, len(vals))
	for i, v := range vals {
		pubkeys[i] = validators.Hash(v.Pubkey)
	}

	vs := validators.NewValidatorSet(pubkeys)
	holders := attest.ComputeHolders(vs, id, replication)

	set := make(map[[32]byte]bool, len(holders))
	for _, h := range holders {
		set[[32]byte(h)] = true
	}

	return set
}

// probeHolders dials each validator and returns the set of pubkeys whose node
// reports holding the object locally.
func probeHolders(vals []client.ValidatorInfo, id [32]byte) map[[32]byte]bool {
	held := make(map[[32]byte]bool)

	for _, v := range vals {
		if v.QUICAddr == "" {
			continue
		}

		data, err := client.NewQUICTransport(v.QUICAddr).GetObjectLocal(id)
		if err != nil || data == nil {
			continue
		}

		held[v.Pubkey] = true
	}

	return held
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
