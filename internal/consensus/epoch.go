package consensus

import (
	"bytes"
	"encoding/hex"
	"sort"

	"BluePods/internal/events"
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
	leftover := d.distributeEpochRewards(issuance)

	// Capture epochAdditions BEFORE clearEpochState wipes it, and the pubkeys
	// applyPendingRemovals actually removed (churn-deferred ones are excluded),
	// so the epoch.transitioned event below reports the real churn. added runs
	// through the same churn filter snapshotEpochHolders applies, so the event
	// never announces an addition that churn actually excluded from membership.
	added := d.churnLimitedAdditions()
	removed := d.applyPendingRemovals()

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

	// Freeze a one-epoch-ahead snapshot so the anchor rule's forward scan can weigh
	// round-N+1 producers that fall in the next epoch when resolving a split at this
	// epoch's tail, before that epoch's own boundary has transitioned. Absent the
	// next epoch's real membership (churn is applied at its own boundary), the next
	// epoch inherits this epoch's frozen holders as the agreed forward proxy.
	d.nextEpochHolders = d.epochHolders

	// Mirror the three holder moves for designation eligibility: retain the outgoing
	// eligible set for the grace slot, freeze the new one from the produced set at
	// this boundary (a member with no committed vertex here stays ineligible for the
	// WHOLE incoming epoch), and reuse it as the forward proxy. The genesis fallback
	// mirrors snapshotOf: an epoch-0 tail that never froze eligibility retains the
	// produced set as observed now.
	if d.eligibleHolders != nil {
		d.prevEligibleHolders = d.eligibleHolders
	} else {
		d.prevEligibleHolders = d.snapshotProduced()
	}

	d.eligibleHolders = d.snapshotProduced()
	d.nextEligibleHolders = d.eligibleHolders

	d.clearEpochState()

	// clearEpochState just zeroed epochFees; re-seed it with whatever the pool
	// could not credit anywhere, so an uncreditable reward share carries into the
	// next epoch's pool instead of silently vanishing from accounted supply.
	d.epochFees = leftover

	d.currentEpoch++

	logger.Info("epoch transition",
		"round", round,
		"epoch", d.currentEpoch,
		"prevEpoch", prevEpoch,
		"validators", d.validators.Len(),
		"epochHolders", d.epochHolders.Len(),
	)

	events.EpochTransitioned(d.currentEpoch, hexSlice(added), hexSlice(removed))

	if d.onEpochTransition != nil {
		d.onEpochTransition(d.currentEpoch)
	}
}

// hexSlice hex-encodes a slice of hashes for event attributes.
func hexSlice(hashes []Hash) []string {
	out := make([]string, len(hashes))
	for i, h := range hashes {
		out[i] = hex.EncodeToString(h[:])
	}
	return out
}

// rewardWeight returns a validator's epoch reward weight: effective_stake ×
// liveness, where liveness is the validator's rounds produced this epoch (one
// vertex per round, so equivocation cannot double it). A jailed or zero-stake
// validator, or one that produced no rounds, has zero weight. Uses safeMul. Shares
// are this weight over the total weight, so no epoch-wide round denominator is needed.
func (d *DAG) rewardWeight(v *ValidatorInfo) uint64 {
	return safeMul(EffectiveStake(v), d.epochRoundsProduced[v.Pubkey])
}

// distributeEpochRewards credits the epoch reward pool (epochFees + issuance) to
// validators in full. Each validator's share is effective_stake × liveness over
// the total weight; the share is split with its delegators (creditDelegators) and
// its own portion is auto-restaked and credited liquid (creditValidatorReward).
// The integer-division remainder goes to the deterministic top-weight validator so
// the whole pool lands in coins/stake (sum credited == pool, no orphaned tokens).
// It returns the leftover amount that could not be credited anywhere (no fee
// system wired, no reward weight to distribute to, or an uncreditable remainder
// recipient), so the caller carries it into the next epoch's pool instead of
// losing it: the epoch reward pool must never silently vanish.
func (d *DAG) distributeEpochRewards(issuance uint64) (leftover uint64) {
	pool := safeAdd(d.epochFees, issuance)

	if d.feeParams == nil || d.coinStore == nil {
		return pool // nowhere to credit it: carry the whole pool forward
	}

	totalWeight := d.totalRewardWeight()
	if pool == 0 || totalWeight == 0 {
		return pool // no reward weight: carry the whole pool forward (harmless if 0)
	}

	vals := d.validators.All()
	top := pickRemainderRecipient(vals, d.rewardWeight)

	var distributed uint64
	for _, v := range vals {
		share := safeMul(pool, d.rewardWeight(v)) / totalWeight
		distributed += d.creditShare(v, share)
	}

	distributed += d.creditRemainder(top, pool-distributed)

	events.RewardsDistributed(d.currentEpoch, pool, len(vals))

	return pool - distributed
}

// creditShare credits one validator's epoch share: it splits the share with the
// validator's delegators, credits each delegator's coin, then credits the
// validator's own portion (auto-restake + liquid). It returns the total moved
// into coins/stake for this validator.
func (d *DAG) creditShare(v *ValidatorInfo, share uint64) uint64 {
	dels := d.delegatorShares(v.Pubkey)
	validatorAmount, delegatorPayouts := splitValidatorReward(share, v.SelfStake, d.commissionBPS, dels)

	return d.creditDelegators(delegatorPayouts, v.Pubkey) +
		d.creditValidatorReward(v, validatorAmount)
}

// pickRemainderRecipient returns the pubkey of the validator with the greatest
// reward weight, breaking ties by the lower pubkey bytes, so the remainder is
// awarded deterministically across all nodes. It returns the zero hash when no
// validator has positive weight (the caller distributes nothing in that case).
func pickRemainderRecipient(vals []*ValidatorInfo, weightOf func(*ValidatorInfo) uint64) Hash {
	var best Hash
	var bestWeight uint64

	for _, v := range vals {
		w := weightOf(v)
		if w == 0 {
			continue
		}

		if w > bestWeight || (w == bestWeight && bytes.Compare(v.Pubkey[:], best[:]) < 0) {
			best, bestWeight = v.Pubkey, w
		}
	}

	return best
}

// creditDelegators compounds each delegator's reward into its stake position: the
// position's amount and the validator's delegated total both rise by the allotted
// reward, so it is returned with the principal on undelegate (spec §2). It returns
// the total credited. A position whose amount cannot be raised is logged and
// skipped (its tokens flow to the remainder recipient), so it never fails the
// epoch. The reward compounds into stake rather than a liquid coin because the
// protocol tracks the delegator only by its position, not by a payout coin.
func (d *DAG) creditDelegators(payouts []delegatorShare, validator Hash) uint64 {
	var credited uint64
	for _, del := range payouts {
		if !d.compoundDelegation(del.Delegator, validator, del.Amount) {
			continue
		}
		credited += del.Amount
	}

	return credited
}

// compoundDelegation raises a delegation position's amount and the validator's
// delegated total by reward, persisting the rebuilt position. It returns false
// (a no-op) when the reward is zero or the position is missing/malformed.
func (d *DAG) compoundDelegation(delegator, validator Hash, reward uint64) bool {
	if reward == 0 {
		return false
	}

	posID := DelegationID(delegator, validator)
	current, ok := d.readDelegationAmount(posID, validator)
	if !ok {
		logger.Warn("delegator reward skipped; position missing", "validator_prefix", validator[:4])
		return false
	}

	d.coinStore.SetObject(buildDelegationObject(delegator, validator, safeAdd(current, reward)))
	d.validators.AddDelegated(validator, reward)
	return true
}

// creditValidatorReward credits a validator's own portion of its share: a
// fraction is auto-restaked into its self-stake and the rest is credited liquid to
// its reward coin. It returns the total moved into stake/coin. The liquid portion
// is skipped (with a warning, so the epoch does not fail) when the validator
// designates no reward coin; the skipped amount then flows to the remainder
// recipient, preserving conservation.
func (d *DAG) creditValidatorReward(v *ValidatorInfo, amount uint64) uint64 {
	restake := safeMul(amount, d.thermostat.AutoRestakeMille) / milleMax
	liquid := amount - restake

	if restake > 0 {
		d.validators.SetSelfStake(v.Pubkey, safeAdd(v.SelfStake, restake))
	}

	if v.RewardCoin == (Hash{}) {
		logger.Warn("validator has no reward coin; liquid reward skipped", "validator_prefix", v.Pubkey[:4])
		return restake
	}

	if err := creditCoin(d.coinStore, v.RewardCoin, liquid); err != nil {
		logger.Warn("validator reward credit failed", "error", err)
		return restake
	}

	return restake + liquid
}

// creditRemainder credits the undistributed remainder to the top-weight
// validator's reward coin so the full pool lands in coins. It returns the
// credited amount: the whole remainder when credited, 0 when the remainder is
// zero or the recipient designates no reward coin — the caller carries an
// uncredited remainder forward into the next epoch's pool rather than losing it.
func (d *DAG) creditRemainder(top Hash, remainder uint64) uint64 {
	if remainder == 0 {
		return 0
	}

	info := d.validators.Get(top)
	if info == nil || info.RewardCoin == (Hash{}) {
		logger.Warn("remainder undistributable; no reward coin on top validator", "remainder", remainder)
		return 0
	}

	if err := creditCoin(d.coinStore, info.RewardCoin, remainder); err != nil {
		logger.Warn("remainder credit failed", "error", err)
		return 0
	}

	return remainder
}

// delegatorShares enumerates the delegation positions targeting a validator as
// reward-split inputs. It returns nil when no enumerator is wired (some unit
// tests) so the split treats the validator as having no delegators.
func (d *DAG) delegatorShares(validator Hash) []delegatorShare {
	if d.delegations == nil {
		return nil
	}

	entries := d.delegations.DelegationsFor(validator)
	dels := make([]delegatorShare, len(entries))
	for i, e := range entries {
		dels[i] = delegatorShare{Delegator: e.Delegator, Amount: e.Amount}
	}

	return dels
}

// applyPendingRemovals removes validators from the active set and returns the
// pubkeys actually removed (respecting maxChurnPerEpoch; deferred removals are
// not included). Removals are sorted by pubkey for deterministic ordering
// across all validators.
func (d *DAG) applyPendingRemovals() []Hash {
	if len(d.pendingRemovals) == 0 {
		return nil
	}

	// Sort by pubkey for deterministic order across all validators.
	// Without sorting, Go map iteration is randomized and different
	// validators would remove different sets when churn is limited.
	sorted := sortedMemberKeys(d.pendingRemovals)

	var removed []Hash

	for _, pubkey := range sorted {
		if d.maxChurnPerEpoch > 0 && len(removed) >= d.maxChurnPerEpoch {
			break // defer remaining to next epoch
		}

		d.validators.Remove(pubkey)
		delete(d.pendingRemovals, pubkey)
		removed = append(removed, pubkey)

		logger.Info("validator removed at epoch boundary",
			"pubkey_prefix", pubkey[:4],
		)
	}

	return removed
}

// sortedMemberKeys extracts the keys of a hash set and sorts them by pubkey bytes
// ascending, giving a deterministic iteration order across all nodes regardless of
// Go's randomized map order.
func sortedMemberKeys(set map[Hash]bool) []Hash {
	keys := make([]Hash, 0, len(set))
	for k := range set {
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

	allowedSet := make(map[Hash]bool)
	for _, h := range d.churnLimitedAdditions() {
		allowedSet[h] = true
	}

	// Build a set of this epoch's additions for quick lookup
	additionSet := make(map[Hash]bool, len(d.epochAdditions))
	for _, a := range d.epochAdditions {
		additionSet[a] = true
	}

	for _, v := range validators {
		// Include validator if it was NOT a new addition, or if churn allowed it in.
		if !additionSet[v.Pubkey] || allowedSet[v.Pubkey] {
			d.epochHolders.AddWithStake(v.Pubkey, v.QUICAddr, v.BLSPubkey, v.SelfStake, v.DelegatedTotal, v.Jailed)
		}
	}
}

// churnLimitedAdditions returns this epoch's additions actually admitted:
// every addition when churn is unlimited or under cap, otherwise the
// churn-limited subset sortedAdditions selects. snapshotEpochHolders (the
// actual membership filter) and the epoch.transitioned event's "added"
// attribute must agree on this set, or the event announces an addition churn
// silently excluded from membership.
func (d *DAG) churnLimitedAdditions() []Hash {
	if d.maxChurnPerEpoch == 0 || len(d.epochAdditions) <= d.maxChurnPerEpoch {
		return append([]Hash(nil), d.epochAdditions...)
	}

	return sortedAdditions(d.epochAdditions, d.maxChurnPerEpoch)
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
}

// InitEpochHolders is a no-op: the genesis epoch's holder snapshot is not taken
// here.
//
// The epoch-0 holder set is the frozen GENESIS snapshot, built from committed
// registrations only (genesis seeding plus committed register_validator
// transactions) and refrozen as that committed set grows, then fixed at the strict
// latch until the first epoch boundary. Freezing it here from the live validator
// set would capture a partial, per-node-divergent set (an optimistic self-add is
// not yet committed), so the freeze is driven by the commit path instead. Until it
// is frozen, HoldersForEpoch reports epoch 0 unresolved and the commit loop waits;
// it never reads the live, mutating set on the anchor path.
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
// currentEpoch-1. It NEVER falls back to the live, mutating validator set on the
// anchor path: a missing snapshot returns false so the commit loop WAITs. During
// the genesis epoch the current snapshot is the frozen genesis holder set (frozen
// from committed registrations at the strict latch, refrozen as the committed set
// grows before it); until it is frozen, epoch 0 is unresolved and the loop waits.
func (d *DAG) HoldersForEpoch(epoch uint64) (*validators.ValidatorSet, bool) {
	if epoch == d.currentEpoch {
		if d.epochHolders != nil {
			return d.epochHolders, true
		}
		return nil, false
	}

	if d.currentEpoch > 0 && epoch == d.currentEpoch-1 && d.prevEpochHolders != nil {
		return d.prevEpochHolders, true
	}

	// One epoch ahead: resolve to the frozen forward proxy so the anchor rule can
	// cross an epoch tail. The proxy is frozen at the first transition; during the
	// genesis epoch (currentEpoch 0, no proxy yet) the round-N+1 citers of the epoch-0
	// tail fall in epoch 1, so resolve them to the frozen GENESIS committee (not the
	// live set). Without this the first epoch boundary is undecidable and the commit
	// loop wedges on it before any transition can freeze a proxy.
	if epoch == d.currentEpoch+1 {
		if d.nextEpochHolders != nil {
			return d.nextEpochHolders, true
		}
		if d.currentEpoch == 0 && d.epochHolders != nil {
			return d.epochHolders, true
		}
	}

	return nil, false
}
