package main

import (
	"testing"

	"BluePods/internal/consensus"
	"BluePods/internal/network"
)

func TestTxStatusIndexLifecycle(t *testing.T) {
	x := newTxStatusIndex()
	var h [32]byte
	h[0] = 0xAA

	if st, _ := x.get(h); st != network.TxStateUnknown {
		t.Fatalf("unseen hash state = %d, want unknown", st)
	}

	x.markPending(h, 100)
	if st, _ := x.get(h); st != network.TxStatePending {
		t.Fatalf("state = %d, want pending", st)
	}

	x.markCommitted(consensus.CommittedTx{Hash: h, Success: false, Reason: consensus.FailVersion}, 102)
	st, reason := x.get(h)
	if st != network.TxStateFailed || reason != uint8(consensus.FailVersion) {
		t.Fatalf("state=%d reason=%d, want failed/version", st, reason)
	}

	x.prune(100 + txStatusRetentionRounds + 1)
	if st, _ := x.get(h); st != network.TxStateUnknown {
		t.Fatalf("state after prune = %d, want unknown", st)
	}
}
