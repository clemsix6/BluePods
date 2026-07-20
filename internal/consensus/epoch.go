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
// 1. Settles the epoch that closed one boundary ago (deferred reward mint)
// 2. Applies pending removals (with churn limiting)
// 3. Snapshots validators → epochHolders
// 4. Clears epoch tracking state
// 5. Increments epoch counter
// 6. Fires epoch transition callback
//
// The reward settlement is deferred by one boundary on purpose: a vertex whose round
// sits at a boundary is committed in different batches on different nodes, so the
// bucket for that round's epoch is not final until every node has swept it up. Waiting
// one full epoch length past the epoch's close guarantees that, so all nodes settle the
// identical bucket. The MEMBERSHIP work (removals, additions, holder freeze, epoch
// increment) stays at the boundary, where it is already a pure function of committed
// history.
func (d *DAG) transitionEpoch(round uint64) {
	prevEpoch := d.currentEpoch

	// Mint and distribute the rewards of the epoch that closed one boundary ago, now
	// that its bucket is complete on every node.
	d.settleDeferredEpoch()

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

	// The validator tree freezes from the SAME snapshot that just froze
	// epochHolders (spec §4), so a light client's quorum weighing matches the
	// membership consensus itself uses from this boundary on. No-op when no
	// indexer is wired.
	if d.indexer != nil {
		d.indexer.RebuildValidators(d.ValidatorLeaves(d.epochHolders.All()))
	}

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

// settleDeferredEpoch mints and distributes the rewards of the epoch that closed one
// boundary ago (currentEpoch-1, before the pending increment). By now the commit cursor
// is a full epoch length past that epoch's last round, so every node has committed all
// of its vertices and its liveness/fee bucket is complete and identical everywhere —
// which is what makes the settlement insensitive to the per-node commit-vs-skip timing
// at the boundary. The genesis epoch (currentEpoch 0) has no earlier epoch to settle.
//
// The thermostat runs here too, so an epoch's rate adjustment and mint happen together
// with its distribution. Any share the pool could not credit anywhere folds into the
// current, still-open epoch's bucket so it stays accounted in total_supply rather than
// vanishing. Removing the settled epoch's buckets keeps the maps bounded to the one or
// two epochs still in flight. The caller holds commitMu.
func (d *DAG) settleDeferredEpoch() {
	if d.currentEpoch == 0 {
		return
	}

	epoch := d.currentEpoch - 1

	// Mint only when the settled epoch has reward weight; the rate still adjusts either
	// way, exactly as an at-boundary settlement did.
	distributable := d.totalRewardWeight(d.epochRoundsProduced[epoch]) > 0
	issuance := d.runThermostat(distributable)
	leftover := d.distributeEpochRewards(epoch, issuance)

	delete(d.epochFees, epoch)
	delete(d.epochRoundsProduced, epoch)

	if leftover > 0 {
		d.epochFees[d.currentEpoch] = safeAdd(d.epochFees[d.currentEpoch], leftover)
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
// liveness, where liveness is the validator's rounds produced in the settled epoch
// (one vertex per round, so equivocation cannot double it), read from that epoch's
// produced bucket. A jailed or zero-stake validator, or one that produced no rounds,
// has zero weight. Uses safeMul. Shares are this weight over the total weight, so no
// epoch-wide round denominator is needed.
func (d *DAG) rewardWeight(v *ValidatorInfo, produced map[Hash]uint64) uint64 {
	return safeMul(EffectiveStake(v), produced[v.Pubkey])
}

// distributeEpochRewards credits epoch E's reward pool (E's pooled fees + issuance) to
// validators in full. Each validator's share is effective_stake × E's liveness over
// the total weight; the share is split with its delegators (creditDelegators) and
// its own portion is auto-restaked and credited liquid (creditValidatorReward).
// The integer-division remainder goes to the deterministic top-weight validator so
// the whole pool lands in coins/stake (sum credited == pool, no orphaned tokens).
// It returns the leftover amount that could not be credited anywhere (no fee
// system wired, no reward weight to distribute to, or an uncreditable remainder
// recipient), so the caller carries it into an open pool instead of losing it: the
// epoch reward pool must never silently vanish. The stake is read from the live set at
// settlement time, which is network-uniform at this deterministic cursor point.
func (d *DAG) distributeEpochRewards(epoch, issuance uint64) (leftover uint64) {
	produced := d.epochRoundsProduced[epoch]
	pool := safeAdd(d.epochFees[epoch], issuance)

	if d.feeParams == nil || d.coinStore == nil {
		return pool // nowhere to credit it: carry the whole pool forward
	}

	totalWeight := d.totalRewardWeight(produced)
	if pool == 0 || totalWeight == 0 {
		return pool // no reward weight: carry the whole pool forward (harmless if 0)
	}

	vals := d.validators.All()
	weightOf := func(v *ValidatorInfo) uint64 { return d.rewardWeight(v, produced) }
	top := pickRemainderRecipient(vals, weightOf)

	var distributed uint64
	for _, v := range vals {
		share := safeMul(pool, d.rewardWeight(v, produced)) / totalWeight
		distributed += d.creditShare(v, share)
	}

	distributed += d.creditRemainder(top, pool-distributed)

	events.RewardsDistributed(epoch, pool, len(vals))

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
// its reward coin. It returns the total moved into stake/coin.
//
// A validator that designates no reward coin cannot receive a liquid credit, so
// its WHOLE portion compounds into self-stake instead of being skipped and left
// to fold into the carried-over pool. That closes the fairness/liveness gap of a
// coinless validator never being paid its share while the amount sits deferred in
// the pool indefinitely: a set member always has a self-stake to receive it, and
// it is recovered on unbond or deregistration once a coin is designated (the same
// coinless-to-stake path the deregistration boundary already uses). The decision
// is deterministic and network-uniform — every node carries the same self-stake
// and reward coin for the validator, so all nodes compound or credit alike.
func (d *DAG) creditValidatorReward(v *ValidatorInfo, amount uint64) uint64 {
	if v.RewardCoin == (Hash{}) {
		d.validators.SetSelfStake(v.Pubkey, safeAdd(v.SelfStake, amount))
		return amount
	}

	restake := safeMul(amount, d.thermostat.AutoRestakeMille) / milleMax
	liquid := amount - restake

	if restake > 0 {
		d.validators.SetSelfStake(v.Pubkey, safeAdd(v.SelfStake, restake))
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

		if d.hasBondedDelegations(pubkey) {
			continue // delegated stake still bonded: keep it in total_bonded, retry next boundary
		}

		if !d.returnDeregisteredStake(pubkey) {
			continue // bond has no coin to return to: keep it bonded, retry next boundary
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

// hasBondedDelegations reports whether a validator pending removal still carries
// delegated stake bonded behind it. Its delegated total is capital its delegators
// own, counted in total_bonded; removing the validator drops that total from the
// active set while returnDeregisteredStake only recredits the self-stake, so the
// supply identity coins_total + total_bonded + deposits + fees_in_flight ==
// total_supply would turn deflationary by exactly the delegated amount. The removal
// is therefore deferred until every delegator has withdrawn its position through
// undelegate (the normal bonded→coins path), leaving only the self-stake to
// re-home. It reads the committed DelegatedTotal, identical on every node, so the
// deferral is network-uniform — the same liveness-for-supply-safety tradeoff the
// self-stake refusal makes when a departing validator has no coin for its bond.
func (d *DAG) hasBondedDelegations(pubkey Hash) bool {
	info := d.validators.Get(pubkey)
	return info != nil && info.DelegatedTotal > 0
}

// returnDeregisteredStake returns a departing validator's self-stake principal to
// its reward coin as the boundary removes it from the active set, mirroring a full
// unbond (a coin credit plus an event) performed automatically at deregistration.
// The self-stake is the part of the validator's effective stake it bonded itself;
// the delegated remainder stays with the delegation positions their delegators own
// and is withdrawn through undelegate, so only the self-stake is re-homed here.
//
// It reports whether the removal may proceed: true when the principal is credited
// (or there is nothing to credit), false when the validator designates no reward
// coin or the credit fails, in which case the caller keeps the validator bonded so
// no bond is ever released from total_bonded without landing in coins_total. That
// keeps the protocol supply identity exact: coins_total grows by exactly the
// self-stake that leaves total_bonded.
//
// The decision is deterministic and network-uniform: every node carries the same
// reward coin and self-stake for the validator, so all nodes credit or refuse
// alike. Refusal defers the departure to a later boundary (a liveness cost paid to
// keep supply safety absolute), the same shape the reward split uses when a
// validator has no coin to receive its liquid share.
func (d *DAG) returnDeregisteredStake(pubkey Hash) bool {
	principal := releasedSelfStake(d.validators.Get(pubkey))
	if principal == 0 {
		return true // nothing left total_bonded (zero or jailed stake): remove freely
	}

	info := d.validators.Get(pubkey)
	if d.coinStore == nil || info.RewardCoin == (Hash{}) {
		logger.Warn("deregistration deferred; validator has no reward coin for its bond",
			"pubkey_prefix", pubkey[:4], "principal", principal)
		return false
	}

	if err := creditCoin(d.coinStore, info.RewardCoin, principal); err != nil {
		logger.Warn("deregistration bond credit failed; keeping validator bonded",
			"error", err)
		return false
	}

	events.StakeReleased(pubkey, info.RewardCoin, principal)
	return true
}

// releasedSelfStake returns the self-stake principal that leaves total_bonded when
// v is removed from the active set: the validator's own bonded self-stake, or zero
// for a nil or jailed validator (a jailed validator already carries zero effective
// stake, so its removal drops nothing from total_bonded and returns nothing).
func releasedSelfStake(v *ValidatorInfo) uint64 {
	if v == nil || v.Jailed {
		return 0
	}

	return v.SelfStake
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

// snapshotEpochHolders freezes the incoming epoch's holder committee. Membership is
// a pure function of COMMITTED state — the committed member set as of this boundary —
// never of the live set's transient view. The stake, address, and jail fields are
// read from the live set, which is already committed-deterministic for them (only
// genesis, committed bonds, and epoch rewards move stake). Respects maxChurnPerEpoch
// for additions: excess additions are excluded.
//
// The membership filter matters because the live set is NOT network-uniform at every
// instant: a joining node optimistically self-adds its own registration to the live
// set before that registration commits (cmd/node/registration.go selfAddToValidatorSet
// -> AddValidator). That phantom sits in the live set on the self-adding node ONLY, is
// absent from committedMembers, and — never having committed — is absent from
// epochAdditions too, so the addition filter alone would admit it and fork the frozen
// snapshot on that one node. Because the frozen snapshot then drives Rendezvous
// sharding, routed reads, and BLS quorum resolution, that fork is durable. Freezing
// from committed membership excludes the phantom on every node alike.
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

	// An empty committedMembers means a unit test built the whole committee up front
	// with no self-add to exclude (see freezeGenesis); treat every live validator as
	// committed then. In production committedMembers always carries the genesis
	// founder, so the filter is active from the first boundary on.
	tracked := len(d.committedMembers) > 0

	for _, v := range validators {
		// Never freeze an uncommitted optimistic self-add into the epoch committee.
		if tracked && !d.committedMembers[v.Pubkey] {
			continue
		}

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

// clearEpochState resets the per-boundary membership churn tracking. It does NOT
// touch the fee or liveness buckets: those are keyed by epoch and settled (then
// deleted) one boundary later by settleDeferredEpoch, so a straddling vertex committed
// just after this boundary must still find its not-yet-settled bucket.
func (d *DAG) clearEpochState() {
	d.epochAdditions = nil
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
