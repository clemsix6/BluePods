package consensus

// The commit regime — relaxed bootstrap versus strict BFT — is a pure function of
// committed history, never of a node's local join timing. A per-node atomic
// (transitionRound/fullQuorumAchieved) made two honest nodes classify the same
// round differently and forked the committed log; the latch below replaces that
// regime decision with strictStartRound, derived from committed STAKE (the round
// every committed member holds a committed non-zero self-stake), clamped, monotone,
// and carried in sync snapshots. Arming on stake, not on registration, keeps the
// strict 2/3 capped-stake quorum reachable the moment the regime turns strict: the
// bootstrap committee has actually bonded by then, so no stakeless snapshot wedges.

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

	// The committed member set drives the boundary committee freeze, so a change to
	// it must reach disk with the commit cursor. refreezeGenesisRegime marks the
	// regime dirty only during the genesis epoch; mark it here too so a registration
	// committed past genesis is persisted the same round it commits, closing the
	// crash window between the registration and the next epoch boundary.
	d.regimeDirty = true

	d.refreezeGenesisRegime(atRound)
}

// refreezeGenesisRegime rebuilds the epoch-0 genesis holder snapshot from the
// committed member set and fires the strict latch once minValidators committed
// members each hold a committed non-zero self-stake. It is called from both the
// registration path (a new committed member) and the bond path (a committed member
// gaining stake), so the snapshot always carries the latest committed stakes and the
// latch observes the moment the bootstrap committee is fully bonded. It is a no-op
// once latched or past the genesis epoch: after the latch the genesis committee and
// its stakes are frozen until the first epoch boundary. It marks the regime dirty so
// the change is persisted atomically with the commit cursor. The caller holds commitMu.
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

	if d.minValidators > 0 && d.committedStakedMemberCount() >= d.minValidators {
		d.latchStrictRegime(atRound)
	}
}

// committedStakedMemberCount counts committed members that hold a committed non-zero
// self-stake. Self-stake is only ever set by genesis seeding and committed bonds, so
// it is a pure function of committed history; iterating the committed member set (not
// the live validator set) keeps an uncommitted self-add out of the count, so every
// node computes the identical bootstrap-complete signal. The caller holds commitMu.
func (d *DAG) committedStakedMemberCount() int {
	count := 0

	for pubkey := range d.committedMembers {
		if v := d.validators.Get(pubkey); v != nil && v.SelfStake > 0 {
			count++
		}
	}

	return count
}

// latchStrictRegime fires the strict-regime latch once, at the committed round
// atRound whose commit brought minValidators committed members to a non-zero
// committed self-stake. strictStartRound is that round plus the grace and buffer
// windows, floored one past the crossing round: the crossing round was itself decided
// relaxed, so the strict regime can only begin strictly after it, and an
// already-relaxed-decided round is never retroactively reclassified (I8). Monotone: it
// fires once and is never lowered. The caller holds commitMu.
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

// StrictRegime reports the strict-latch state: the round strict decisions start at
// and whether the latch has armed. The relaxed bootstrap certificate is
// view-dependent under delivery skew, so callers that need view-independent
// decisions (a client gating traffic on the regime, a harness quiescing its
// bootstrap) read this to know when the strict quorum governs. Locked because the
// latch is mutated by the commit loop.
func (d *DAG) StrictRegime() (start uint64, latched bool) {
	d.commitMu.Lock()
	defer d.commitMu.Unlock()

	return d.strictStartRound, d.strictLatched
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

// restoreCommittedMembers rebuilds the committed member set at boot so a node
// restarted in ANY epoch keeps refreezing from the full committed committee rather
// than shrinking it to only the members re-seeded after the restart. It reads the
// durably persisted set first — written with the commit cursor at every boundary and
// on every committed registration — which is the network-uniform set at every epoch.
// A data dir written before that set was persisted has no key; during the genesis
// epoch epochHolders IS the committed accumulator, so it is rebuilt from there, and
// past genesis there is nothing to rebuild from (the caller keeps the empty set). The
// caller holds no lock (boot).
func (d *DAG) restoreCommittedMembers() {
	if members := decodeMemberSet(d.store.loadMetaBytes(committedMembersKey)); members != nil {
		d.committedMembers = members
		return
	}

	if d.currentEpoch != 0 || d.epochHolders == nil {
		return
	}

	d.committedMembers = make(map[Hash]bool, d.epochHolders.Len())
	for _, v := range d.epochHolders.All() {
		d.committedMembers[v.Pubkey] = true
	}
}
