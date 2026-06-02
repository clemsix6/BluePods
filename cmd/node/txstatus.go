package main

import (
	"sync"

	"BluePods/internal/consensus"
	"BluePods/internal/network"
)

// txStatusRetentionRounds is how many rounds an entry is kept before pruning. It
// bounds memory: an entry whose round is older than this behind the current round
// is dropped and reads as unknown again.
const txStatusRetentionRounds = 2000

// txStatusEntry is one transaction's last-known status.
type txStatusEntry struct {
	state  uint8  // state is a network.TxState constant
	reason uint8  // reason is a consensus.FailReason code, meaningful when failed
	round  uint64 // round is when the entry was last set, for pruning
}

// txStatusIndex maps a transaction hash to its last-known outcome. It is fed at
// submission ingress (pending) and from the committed stream (finalized/failed),
// and is read by the GetTxStatus handler. It is read-only observability and is
// never consulted by consensus.
type txStatusIndex struct {
	mu      sync.RWMutex
	entries map[[32]byte]txStatusEntry
}

// newTxStatusIndex creates an empty index.
func newTxStatusIndex() *txStatusIndex {
	return &txStatusIndex{entries: make(map[[32]byte]txStatusEntry)}
}

// markPending records a freshly ingested transaction as pending, without
// downgrading a hash that already has a committed outcome.
func (x *txStatusIndex) markPending(hash [32]byte, round uint64) {
	x.mu.Lock()
	defer x.mu.Unlock()

	if _, ok := x.entries[hash]; ok {
		return
	}
	x.entries[hash] = txStatusEntry{state: network.TxStatePending, round: round}
}

// markCommitted records a transaction's final outcome from the committed stream.
// If a pending entry already exists its round is preserved, so pruning is
// anchored to submission time rather than commit time.
func (x *txStatusIndex) markCommitted(tx consensus.CommittedTx, round uint64) {
	state := network.TxStateFinalized
	if !tx.Success {
		state = network.TxStateFailed
	}

	x.mu.Lock()
	defer x.mu.Unlock()

	existing, ok := x.entries[[32]byte(tx.Hash)]

	entry := txStatusEntry{state: state, reason: uint8(tx.Reason), round: round}
	// Preserve the original round from markPending if present, so pruning uses
	// the submission round rather than the (slightly later) commit round.
	if ok && existing.round < round {
		entry.round = existing.round
	}

	x.entries[[32]byte(tx.Hash)] = entry
}

// get returns the state and reason for a hash, or unknown when absent.
func (x *txStatusIndex) get(hash [32]byte) (uint8, uint8) {
	x.mu.RLock()
	defer x.mu.RUnlock()

	e, ok := x.entries[hash]
	if !ok {
		return network.TxStateUnknown, 0
	}

	return e.state, e.reason
}

// prune drops entries older than the retention window behind currentRound.
func (x *txStatusIndex) prune(currentRound uint64) {
	if currentRound <= txStatusRetentionRounds {
		return
	}
	cutoff := currentRound - txStatusRetentionRounds

	x.mu.Lock()
	defer x.mu.Unlock()

	for h, e := range x.entries {
		if e.round < cutoff {
			delete(x.entries, h)
		}
	}
}
