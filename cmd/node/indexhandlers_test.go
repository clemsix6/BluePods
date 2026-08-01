package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"testing"

	"BluePods/internal/network"
)

// TestHandleGetIndexAnchor_NoIndexerNotFound covers a node whose consensus is
// not constructed yet, so nothing has wired the index seam: GetIndexAnchor must
// answer Found=false, never panic on the nil dag/idxManager guard.
func TestHandleGetIndexAnchor_NoIndexerNotFound(t *testing.T) {
	n := &Node{}

	respBytes, err := n.handleGetIndexAnchor()
	if err != nil {
		t.Fatalf("handleGetIndexAnchor: %v", err)
	}

	resp, err := network.DecodeGetIndexAnchorResp(respBytes)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}

	if resp.Found {
		t.Fatal("expected Found=false with no dag/indexer wired")
	}
}

// TestHandleGetIndexAnchor_ServesAndCachesUntilFrontierAdvances drives a real
// single-validator bootstrap node (the founder trivially reaches its own
// 1-of-1 quorum) through the actual message-tag route, and checks the served
// frontier never goes backwards across repeated polls and does eventually
// move once the node's own committed frontier moves further — the cache
// invalidation the plan requires, without pinning to an exact frontier value
// a live background commit loop makes inherently racy to predict.
func TestHandleGetIndexAnchor_ServesAndCachesUntilFrontierAdvances(t *testing.T) {
	dir := t.TempDir()
	_, privKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	n, db := bootstrapTestNode(t, dir, privKey)
	t.Cleanup(func() { n.dag.Close(); db.Close() })

	n.seedGenesisState()

	waitForCommit(t, n.dag, n.dag.LastCommittedRound(), 2)

	if !network.IsClientMessage(network.EncodeGetIndexAnchor()) {
		t.Fatal("get-index-anchor request must classify as a client message, or it never routes")
	}

	respBytes, err := n.handleClientMessage(network.EncodeGetIndexAnchor())
	if err != nil {
		t.Fatalf("handleClientMessage: %v", err)
	}

	first, err := network.DecodeGetIndexAnchorResp(respBytes)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}

	if !first.Found {
		t.Fatal("the founder's own vertex trivially reaches 1-of-1 quorum and must be served")
	}

	if len(first.Headers) != 1 {
		t.Fatalf("bundle carries %d headers, want 1 (the sole validator)", len(first.Headers))
	}

	respBytes2, err := n.handleClientMessage(network.EncodeGetIndexAnchor())
	if err != nil {
		t.Fatalf("second handleClientMessage: %v", err)
	}

	second, err := network.DecodeGetIndexAnchorResp(respBytes2)
	if err != nil {
		t.Fatalf("decode second: %v", err)
	}

	if !second.Found || second.FrontierRound < first.FrontierRound {
		t.Fatalf("served frontier went backwards: %d then %d", first.FrontierRound, second.FrontierRound)
	}

	// Let the node's own committed frontier advance further, then confirm the
	// cache picks it up rather than serving the first answer forever.
	waitForCommit(t, n.dag, n.dag.LastCommittedRound(), 2)

	respBytes3, err := n.handleClientMessage(network.EncodeGetIndexAnchor())
	if err != nil {
		t.Fatalf("third handleClientMessage: %v", err)
	}

	third, err := network.DecodeGetIndexAnchorResp(respBytes3)
	if err != nil {
		t.Fatalf("decode third: %v", err)
	}

	if !third.Found || third.FrontierRound <= second.FrontierRound {
		t.Fatalf("cache never advanced past frontier %d after the commit cursor moved further", second.FrontierRound)
	}
}
