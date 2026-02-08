package consensus

import (
	"bytes"
	"sort"

	"BluePods/internal/logger"
)

// isEpochBoundary returns true if the given round is an epoch boundary.
// Returns false if epochs are disabled (epochLength=0).
func (d *DAG) isEpochBoundary(round uint64) bool {
	if d.epochLength == 0 {
		return false
	}

	if round == 0 {
		return false
	}

	return round%d.epochLength == 0
}

// transitionEpoch handles the epoch boundary transition.
// 1. Applies pending removals (with churn limiting)
// 2. Snapshots validators → epochHolders
// 3. Clears epoch tracking state
// 4. Increments epoch counter
// 5. Fires epoch transition callback
func (d *DAG) transitionEpoch(round uint64) {
	prevEpoch := d.currentEpoch

	d.applyPendingRemovals()
	d.snapshotEpochHolders()
	d.clearEpochState()

	d.currentEpoch++

	logger.Info("epoch transition",
		"round", round,
		"epoch", d.currentEpoch,
		"prevEpoch", prevEpoch,
		"validators", d.validators.Len(),
		"epochHolders", d.epochHolders.Len(),
	)

	if d.onEpochTransition != nil {
		d.onEpochTransition(d.currentEpoch)
	}
}

// applyPendingRemovals removes validators from the active set.
// Respects maxChurnPerEpoch: excess removals are deferred to the next epoch.
// Removals are sorted by pubkey for deterministic ordering across all validators.
func (d *DAG) applyPendingRemovals() {
	if len(d.pendingRemovals) == 0 {
		return
	}

	// Sort by pubkey for deterministic order across all validators.
	// Without sorting, Go map iteration is randomized and different
	// validators would remove different sets when churn is limited.
	sorted := sortedRemovals(d.pendingRemovals)

	applied := 0

	for _, pubkey := range sorted {
		if d.maxChurnPerEpoch > 0 && applied >= d.maxChurnPerEpoch {
			break // defer remaining to next epoch
		}

		d.validators.Remove(pubkey)
		delete(d.pendingRemovals, pubkey)
		applied++

		logger.Info("validator removed at epoch boundary",
			"pubkey_prefix", pubkey[:4],
		)
	}
}

// sortedRemovals extracts pending removal keys and sorts them by pubkey bytes.
func sortedRemovals(pending map[Hash]bool) []Hash {
	keys := make([]Hash, 0, len(pending))
	for k := range pending {
		keys = append(keys, k)
	}

	sort.Slice(keys, func(i, j int) bool {
		return bytes.Compare(keys[i][:], keys[j][:]) < 0
	})

	return keys
}

// snapshotEpochHolders creates a frozen copy of the current validator set.
// This copy is used for Rendezvous hashing until the next epoch boundary.
// Respects maxChurnPerEpoch for additions: excess additions are excluded.
func (d *DAG) snapshotEpochHolders() {
	validators := d.validators.All()
	d.epochHolders = NewValidatorSet(nil)

	// If churn is unlimited or additions fit within limit, include all
	if d.maxChurnPerEpoch == 0 || len(d.epochAdditions) <= d.maxChurnPerEpoch {
		for _, v := range validators {
			d.epochHolders.Add(v.Pubkey, v.HTTPAddr, v.QUICAddr, v.BLSPubkey)
		}
		return
	}

	// Churn limited: only include maxChurnPerEpoch new additions.
	allowed := sortedAdditions(d.epochAdditions, d.maxChurnPerEpoch)
	allowedSet := make(map[Hash]bool, len(allowed))
	for _, h := range allowed {
		allowedSet[h] = true
	}

	// Build a set of this epoch's additions for quick lookup
	additionSet := make(map[Hash]bool, len(d.epochAdditions))
	for _, a := range d.epochAdditions {
		additionSet[a] = true
	}

	for _, v := range validators {
		// Include validator if it was NOT a new addition, or if it's in the allowed set
		if !additionSet[v.Pubkey] || allowedSet[v.Pubkey] {
			d.epochHolders.Add(v.Pubkey, v.HTTPAddr, v.QUICAddr, v.BLSPubkey)
		}
	}
}

// sortedAdditions sorts additions by pubkey and returns the first `limit` entries.
func sortedAdditions(additions []Hash, limit int) []Hash {
	sorted := make([]Hash, len(additions))
	copy(sorted, additions)

	sort.Slice(sorted, func(i, j int) bool {
		return bytes.Compare(sorted[i][:], sorted[j][:]) < 0
	})

	if len(sorted) > limit {
		sorted = sorted[:limit]
	}

	return sorted
}

// clearEpochState resets per-epoch tracking data.
func (d *DAG) clearEpochState() {
	d.epochAdditions = nil
}

// InitEpochHolders initializes epochHolders from the current validator set.
// Called once at startup or after snapshot import to establish the initial epoch state.
func (d *DAG) InitEpochHolders() {
	if d.epochLength == 0 {
		return
	}

	d.snapshotEpochHolders()
}
