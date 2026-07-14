package consensus

// The commit regime — relaxed bootstrap versus strict BFT — is a pure function of
// committed history, never of a node's local join timing. A per-node atomic
// (transitionRound/fullQuorumAchieved) made two honest nodes classify the same
// round differently and forked the committed log; the latch below replaces that
// regime decision with strictStartRound, derived from committed registrations,
// clamped, monotone, and carried in sync snapshots.

// roundIsRelaxed reports whether a round is decided under the relaxed bootstrap
// certificate rather than the strict BFT quorum. It reads only the persisted
// latch, so every node classifies a given round identically. With no bootstrap
// window configured (minValidators == 0) the regime is strict from the first
// round, matching the pre-latch behavior. Otherwise a round is relaxed until the
// latch fires and reaches its strict-start round.
func (d *DAG) roundIsRelaxed(round uint64) bool {
	if d.minValidators == 0 {
		return false
	}

	return !d.strictLatched || round < d.strictStartRound
}

// recordCommittedMember admits a validator to the committed member set — from a
// committed register_validator transaction or from genesis seeding — and refreshes
// the epoch-0 genesis regime. It reads committed membership only, so an optimistic
// self-add (AddValidator, still uncommitted) never enters the frozen set: a
// registering node must freeze the same set as everyone else (I7). atRound is the
// committed round driving the change. The caller holds commitMu.
func (d *DAG) recordCommittedMember(pubkey Hash, atRound uint64) {
	if d.committedMembers == nil {
		d.committedMembers = make(map[Hash]bool)
	}

	if d.committedMembers[pubkey] {
		return // already committed: no membership change
	}

	d.committedMembers[pubkey] = true
	d.refreezeGenesisRegime(atRound)
}

// refreezeGenesisRegime rebuilds the epoch-0 genesis holder snapshot from the
// committed member set and fires the strict latch once the committed count reaches
// minValidators. It is a no-op once latched or past the genesis epoch: after the
// latch the genesis committee and its stakes are frozen until the first epoch
// boundary. It marks the regime dirty so the change is persisted atomically with
// the commit cursor. The caller holds commitMu.
func (d *DAG) refreezeGenesisRegime(atRound uint64) {
	if d.strictLatched || d.currentEpoch != 0 {
		return
	}

	d.epochHolders = d.freezeGenesisHolders()

	// Freeze designation eligibility WITH the snapshot: the members holding a
	// committed vertex at this committed point. A member registered before it
	// produces is frozen ineligible and becomes designatable at the next freeze
	// that observes its first committed vertex.
	d.eligibleHolders = d.snapshotProduced()

	d.regimeDirty = true

	if d.minValidators > 0 && len(d.committedMembers) >= d.minValidators {
		d.latchStrictRegime(atRound)
	}
}

// latchStrictRegime fires the strict-regime latch once, at the committed round
// atRound whose commit crossed minValidators. strictStartRound is that round plus
// the grace and buffer windows, floored one past the crossing round: the crossing
// round was itself decided relaxed, so the strict regime can only begin strictly
// after it, and an already-relaxed-decided round is never retroactively reclassified
// (I8). Monotone: it fires once and is never lowered. The caller holds commitMu.
func (d *DAG) latchStrictRegime(atRound uint64) {
	if d.strictLatched {
		return
	}

	start := atRound + d.effectiveGrace() + d.effectiveBuffer()
	if floor := atRound + 1; start < floor {
		start = floor
	}

	d.strictStartRound = start
	d.strictLatched = true
	d.regimeDirty = true
}

// freezeGenesisHolders builds a frozen holder snapshot from the committed member
// set, reading each member's committed stake from the live validator set. Stakes
// are only ever set by genesis, committed bonds, and epoch rewards, so the snapshot
// is a pure function of committed history; the sole live-set field that could
// diverge across nodes is membership via an uncommitted self-add, which is excluded
// because committedMembers holds committed registrations only. The caller holds commitMu.
func (d *DAG) freezeGenesisHolders() *ValidatorSet {
	frozen := NewValidatorSet(nil)

	// Iterate members in ascending pubkey order so the frozen set's All() order — and
	// therefore its encoded blob bytes — is identical on every node, not Go map order.
	for _, pubkey := range sortedMemberKeys(d.committedMembers) {
		v := d.validators.Get(pubkey)

		// A crash-restart-in-place cannot rebuild the live validator set, so a member
		// restored into committedMembers (from the persisted genesis snapshot) may be
		// absent from d.validators. Fall back to its record in the snapshot being
		// refrozen — itself a pure function of committed history — rather than dropping
		// the member and shrinking the committee, which would fork the frozen set.
		if v == nil && d.epochHolders != nil {
			v = d.epochHolders.Get(pubkey)
		}

		if v == nil {
			continue
		}

		frozen.AddWithStake(v.Pubkey, v.QUICAddr, v.BLSPubkey, v.SelfStake, v.DelegatedTotal, v.Jailed)
	}

	return frozen
}

// restoreCommittedMembers rebuilds the committed member set from the restored
// epoch-0 genesis snapshot, so a node restarted mid-bootstrap keeps refreezing from
// the full committed committee rather than shrinking it to only the members
// re-seeded after the restart. It runs only during the genesis epoch, where
// epochHolders IS the committed member accumulator. The caller holds no lock (boot).
func (d *DAG) restoreCommittedMembers() {
	if d.currentEpoch != 0 || d.epochHolders == nil {
		return
	}

	d.committedMembers = make(map[Hash]bool, d.epochHolders.Len())
	for _, v := range d.epochHolders.All() {
		d.committedMembers[v.Pubkey] = true
	}
}
