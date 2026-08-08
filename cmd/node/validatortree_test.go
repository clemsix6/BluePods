package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"testing"

	"BluePods/internal/index"
	"BluePods/internal/network"
)

// TestGetValidatorTree_ServesTheCommitteeTheAnchorCommitsTo is the node side
// of the light client's epoch walk: the served leaf set must rebuild exactly
// the validator component of the anchor it travels with, and that anchor's
// four components must combine to the index root a quorum attests. Without
// both, a client has a committee it cannot tie to anything and no quorum it
// can weigh.
func TestGetValidatorTree_ServesTheCommitteeTheAnchorCommitsTo(t *testing.T) {
	dir := t.TempDir()

	_, privKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	n, db := bootstrapTestNode(t, dir, privKey)
	t.Cleanup(func() { n.dag.Close(); db.Close() })

	n.seedGenesisState()
	waitForCommit(t, n.dag, n.dag.LastCommittedRound(), 2)

	epoch := n.dag.LiveEpoch()

	resp, err := network.DecodeGetValidatorTreeResp(clientRoundTrip(t, n,
		network.EncodeGetValidatorTree(&network.GetValidatorTreeRequest{Epoch: epoch})))
	if err != nil {
		t.Fatalf("decode validator tree: %v", err)
	}

	if !resp.Found || resp.Epoch != epoch {
		t.Fatalf("node refused its own epoch: found=%v epoch=%d want %d", resp.Found, resp.Epoch, epoch)
	}

	if len(resp.Leaves) == 0 {
		t.Fatal("served an empty committee for a running chain")
	}

	leaves := make([]index.ValidatorLeaf, 0, len(resp.Leaves))
	for i, raw := range resp.Leaves {
		leaf, ok := index.DecodeValidatorLeaf(raw)
		if !ok {
			t.Fatalf("leaf %d does not decode: %x", i, raw)
		}

		leaves = append(leaves, leaf)
	}

	if root := index.ValidatorRootOf(leaves); root != resp.Anchor.ValidatorRoot {
		t.Fatalf("served committee rebuilds %x, the anchor's validator root is %x", root[:4], resp.Anchor.ValidatorRoot[:4])
	}

	assertAnchorCombines(t, resp.Anchor)
}

// TestGetValidatorTree_RefusesAnotherEpochButNamesItsOwn verifies a request
// for an epoch this node's index does not describe comes back refused rather
// than answered with the current set under the wrong label — while still
// reporting the epoch it does hold, which is what tells a client one boundary
// behind where to walk to.
func TestGetValidatorTree_RefusesAnotherEpochButNamesItsOwn(t *testing.T) {
	dir := t.TempDir()

	_, privKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	n, db := bootstrapTestNode(t, dir, privKey)
	t.Cleanup(func() { n.dag.Close(); db.Close() })

	n.seedGenesisState()

	epoch := n.dag.LiveEpoch()

	resp, err := network.DecodeGetValidatorTreeResp(clientRoundTrip(t, n,
		network.EncodeGetValidatorTree(&network.GetValidatorTreeRequest{Epoch: epoch + 7})))
	if err != nil {
		t.Fatalf("decode validator tree: %v", err)
	}

	if resp.Found || len(resp.Leaves) != 0 {
		t.Fatalf("node answered for an epoch it does not hold: found=%v leaves=%d", resp.Found, len(resp.Leaves))
	}

	if resp.Epoch != epoch {
		t.Fatalf("refusal reports epoch %d, this node holds %d", resp.Epoch, epoch)
	}
}
