package main

import (
	"crypto/ed25519"
	"encoding/hex"
	"fmt"

	"BluePods/internal/events"
	"BluePods/internal/network"
	"BluePods/internal/sync"
)

// testHooksDisabledErr is the typed refusal returned by both test-hooks
// messages when the node was not started with --test-hooks. Production
// binaries never set the flag, so these messages are inert by default.
const testHooksDisabledErr = "test hooks disabled"

// handleStateFingerprint answers a convergence-fingerprint request. Served
// only when --test-hooks is set; otherwise it returns the typed refusal and
// touches nothing.
func (n *Node) handleStateFingerprint() ([]byte, error) {
	if !n.cfg.TestHooks {
		return network.EncodeStateFingerprintResp(&network.FingerprintResponse{Err: testHooksDisabledErr}), nil
	}

	if n.dag == nil || n.state == nil {
		return network.EncodeStateFingerprintResp(&network.FingerprintResponse{Err: "consensus not available"}), nil
	}

	fp := sync.ComputeFingerprint(n.dag, n.state)

	return network.EncodeStateFingerprintResp(&network.FingerprintResponse{
		Round:        fp.Round,
		Checksum:     fp.Checksum,
		TotalSupply:  fp.TotalSupply,
		CoinsTotal:   fp.CoinsTotal,
		TotalBonded:  fp.TotalBonded,
		Deposits:     fp.Deposits,
		FeesInFlight: fp.FeesInFlight,
	}), nil
}

// handleTestControl applies a test-only network-control operation (partition
// set/clear). Served only when --test-hooks is set; otherwise it returns the
// typed refusal and changes nothing.
func (n *Node) handleTestControl(data []byte) ([]byte, error) {
	if !n.cfg.TestHooks {
		return network.EncodeTestControlResp(&network.TestControlResponse{Err: testHooksDisabledErr}), nil
	}

	req, err := network.DecodeTestControl(data)
	if err != nil {
		return nil, err
	}

	switch req.Op {
	case network.TestControlOpSetPartition:
		n.applyPartition(req.Pubkeys)
	case network.TestControlOpClearPartition:
		n.network.ClearBlocklist()
		events.PartitionCleared()
	default:
		return network.EncodeTestControlResp(&network.TestControlResponse{
			Err: fmt.Sprintf("unknown test-control op %d", req.Op),
		}), nil
	}

	return network.EncodeTestControlResp(&network.TestControlResponse{}), nil
}

// applyPartition installs blocked as the node's blocklist and emits the
// partition-applied event with the blocked pubkeys hex-encoded.
func (n *Node) applyPartition(blocked [][32]byte) {
	pubkeys := make([]ed25519.PublicKey, len(blocked))
	hexKeys := make([]string, len(blocked))

	for i, pk := range blocked {
		pubkeys[i] = ed25519.PublicKey(pk[:])
		hexKeys[i] = hex.EncodeToString(pk[:])
	}

	n.network.SetBlocklist(pubkeys)
	events.PartitionApplied(hexKeys)
}
