package consensus

import (
	"bytes"
	"sort"

	"BluePods/internal/logger"
	"BluePods/internal/validators"
)

// EpochGraceRounds is how many rounds after an epoch boundary an attestation
// from the previous epoch is still accepted, so an ATX collected late in epoch
// E and committed shortly into E+1 verifies rather than being rejected.
const EpochGraceRounds = 50

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
// 1. Distributes epoch rewards to validators
// 2. Applies pending removals (with churn limiting)
// 3. Snapshots validators → epochHolders
// 4. Clears epoch tracking state
// 5. Increments epoch counter
// 6. Fires epoch transition callback
func (d *DAG) transitionEpoch(round uint64) {
	prevEpoch := d.currentEpoch

	// Run the thermostat (adjusts the rate every epoch, mints only when there is
	// reward weight to distribute), then distribute the pool BEFORE removals so
	// outgoing validators still get their share.
	distributable := d.totalRewardWeight() > 0
	issuance := d.runThermostat(distributable)
	d.distributeEpochRewards(issuance)

	d.applyPendingRemovals()

	// Retain the outgoing epoch's snapshot for the grace window so an ATX
	// collected late in the previous epoch still verifies shortly after the
	// boundary. snapshotEpochHolders overwrites d.epochHolders, so capture first.
	// The genesis epoch holds no frozen snapshot (it tracks the live set), so
	// freeze the current validators as its retained snapshot at this first boundary.
	if d.epochHolders != nil {
		d.prevEpochHolders = d.epochHolders
	} else {
		d.prevEpochHolders = snapshotOf(d.validators)
	}

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

// distributeEpochRewards distributes the epoch reward pool to validators. The
// pool is epochFees + issuance (the thermostat mint, added by runThermostat only
// when the pool is fully distributable). The crediting itself is wired in a later
// batch; this body still computes the proportional split and logs it. The
// epochFees==0 early-return is removed so a zero-fee, issuance-only epoch (the
// bootstrap incentive) is still distributed.
func (d *DAG) distributeEpochRewards(issuance uint64) {
	if d.feeParams == nil || d.coinStore == nil {
		return
	}

	pool := safeAdd(d.epochFees, issuance)
	if pool == 0 {
		return
	}

	// Calculate weights: stake × (rounds_produced / total_rounds).
	// With equal stake (1), weight = rounds_produced.
	var totalWeight uint64
	for _, rounds := range d.epochRoundsProduced {
		totalWeight += rounds
	}

	if totalWeight == 0 {
		return
	}

	// Distribute to each validator proportionally.
	for pubkey, rounds := range d.epochRoundsProduced {
		share := pool * rounds / totalWeight

		if share == 0 {
			continue
		}

		// TODO: credit to validator's reward_coin once ValidatorInfo stores reward_coin ObjectID (Batch 7)
		// For now, log the distribution.
		logger.Debug("epoch reward",
			"validator_prefix", pubkey[:4],
			"rounds", rounds,
			"share", share,
		)
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
			d.epochHolders.AddWithStake(v.Pubkey, v.QUICAddr, v.BLSPubkey, v.SelfStake, v.DelegatedTotal, v.Jailed)
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
			d.epochHolders.AddWithStake(v.Pubkey, v.QUICAddr, v.BLSPubkey, v.SelfStake, v.DelegatedTotal, v.Jailed)
		}
	}
}

// snapshotOf returns a frozen copy of a validator set's full membership.
func snapshotOf(vs *validators.ValidatorSet) *validators.ValidatorSet {
	frozen := NewValidatorSet(nil)
	for _, v := range vs.All() {
		frozen.AddWithStake(v.Pubkey, v.QUICAddr, v.BLSPubkey, v.SelfStake, v.DelegatedTotal, v.Jailed)
	}

	return frozen
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
	d.epochFees = 0
	d.epochRoundsProduced = make(map[Hash]uint64)
	d.epochTotalRounds = 0
}

// InitEpochHolders is a no-op during the genesis epoch on purpose.
//
// At process startup the validator set is still forming: the bootstrap knows only
// itself, and each joining node knows only the validators it has synced so far.
// A snapshot taken here would freeze that partial, per-node-divergent set as the
// epoch-0 holder set, and attestation verification would then recompute holders
// against it while the client daemon collected against the converged live set,
// so quorum proofs would never match. Leaving epochHolders nil makes
// HoldersForEpoch fall back to the live validator set for the genesis epoch,
// which converges to the same set the daemon syncs. The first real frozen
// snapshot is taken at the first epoch boundary by transitionEpoch, where the
// set is already stable.
func (d *DAG) InitEpochHolders() {}

// commitEpochForRound returns the epoch whose holder snapshot an ATX committed
// at the given round must be verified against. The boundary round R = k*epochLength
// is committed (and its ATXs verified) BEFORE transitionEpoch increments
// currentEpoch, so that round still belongs to epoch k-1. Every other round R
// maps to R/epochLength. Deterministic across all validators.
func (d *DAG) commitEpochForRound(round uint64) uint64 {
	if d.epochLength == 0 {
		return 0
	}

	epoch := round / d.epochLength

	// A nonzero multiple of epochLength is committed before the transition, so
	// it is still served by the previous epoch's holders.
	if epoch > 0 && round%d.epochLength == 0 {
		return epoch - 1
	}

	return epoch
}

// CommitEpochForRound is the exported form of commitEpochForRound, used to wire
// the ATX verifier from outside the consensus package.
func (d *DAG) CommitEpochForRound(round uint64) uint64 {
	return d.commitEpochForRound(round)
}

// HoldersForEpoch returns the holder snapshot for the given epoch, selecting the
// current snapshot for currentEpoch and the retained previous snapshot for
// currentEpoch-1. It returns false for any other epoch (too old or in the future).
func (d *DAG) HoldersForEpoch(epoch uint64) (*validators.ValidatorSet, bool) {
	if epoch == d.currentEpoch {
		if d.epochHolders != nil {
			return d.epochHolders, true
		}
		return d.validators, true
	}

	if d.currentEpoch > 0 && epoch == d.currentEpoch-1 && d.prevEpochHolders != nil {
		return d.prevEpochHolders, true
	}

	return nil, false
}
