package client

import (
	"BluePods/internal/attest"
	"BluePods/internal/validators"
)

// HolderReport describes, for one object, the rendezvous-expected holders and the
// validators actually reporting they hold it locally.
type HolderReport struct {
	Expected map[[32]byte]bool // Expected is the rendezvous-assigned holder set
	Actual   map[[32]byte]bool // Actual is the set of validators reporting the object locally
}

// Holders computes the expected and actual holder sets for an object. It is a
// typed action usable by the CLI, the console, and a future simulator alike.
func (c *Client) Holders(objectID [32]byte) (*HolderReport, error) {
	obj, err := c.GetObject(objectID)
	if err != nil {
		return nil, err
	}

	vals, err := c.Validators()
	if err != nil {
		return nil, err
	}

	return &HolderReport{
		Expected: expectedHolders(vals, objectID, int(obj.Replication)),
		Actual:   probeHolders(vals, objectID),
	}, nil
}

// expectedHolders returns the rendezvous-assigned holder pubkeys for an object.
func expectedHolders(vals []ValidatorInfo, id [32]byte, replication int) map[[32]byte]bool {
	pubkeys := make([]validators.Hash, len(vals))
	for i, v := range vals {
		pubkeys[i] = validators.Hash(v.Pubkey)
	}

	holders := attest.ComputeHolders(validators.NewValidatorSet(pubkeys), id, replication)

	set := make(map[[32]byte]bool, len(holders))
	for _, h := range holders {
		set[[32]byte(h)] = true
	}

	return set
}

// probeHolders dials each validator and returns the pubkeys reporting the object
// locally.
func probeHolders(vals []ValidatorInfo, id [32]byte) map[[32]byte]bool {
	held := make(map[[32]byte]bool)
	for _, v := range vals {
		if v.QUICAddr == "" {
			continue
		}

		data, err := NewQUICTransport(v.QUICAddr).GetObjectLocal(id)
		if err != nil || data == nil {
			continue
		}

		held[v.Pubkey] = true
	}

	return held
}
