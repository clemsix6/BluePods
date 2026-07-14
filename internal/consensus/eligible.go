package consensus

import "encoding/binary"

// Anchor-designation eligibility: a member may be DESIGNATED only once it has at
// least one vertex in committed history. The live produced set grows as vertices
// are marked committed (strictly cursor-ordered, so it is identical on every node
// at the same cursor); the eligible set used by anchorProducerFor is frozen from
// it at exactly the events that freeze the holder snapshots (genesis freeze and
// epoch-0 refreezes, epoch boundaries, the next-epoch proxy), NEVER at evaluation
// time. While a snapshot's eligible subset is empty the full snapshot is eligible
// (bootstrap fallback). Eligibility gates designation only: quorum, support, and
// blame weights always use the full holder snapshot.

// recordProducedMember admits a member to the live produced set on its first
// committed vertex and marks the set dirty so it is persisted atomically with the
// commit cursor. It never touches a frozen eligible set: eligibility changes only
// at the freeze events. The caller holds commitMu.
func (d *DAG) recordProducedMember(pubkey Hash) {
	if d.producedMembers == nil {
		d.producedMembers = make(map[Hash]bool)
	}

	if d.producedMembers[pubkey] {
		return
	}

	d.producedMembers[pubkey] = true
	d.producedDirty = true
}

// snapshotProduced returns a frozen copy of the live produced set, taken at a
// holder-snapshot freeze so the eligible set is fixed for the epoch it serves.
// The caller holds commitMu.
func (d *DAG) snapshotProduced() map[Hash]bool {
	frozen := make(map[Hash]bool, len(d.producedMembers))
	for pubkey := range d.producedMembers {
		frozen[pubkey] = true
	}

	return frozen
}

// eligibleForEpoch returns the eligible set frozen WITH the holder snapshot
// HoldersForEpoch serves for the epoch, mirroring its selection branch for branch
// so designation always pairs a snapshot with the eligibility fixed at its freeze.
// nil means no eligibility was frozen for the slot; the caller falls back to the
// full snapshot, which is also the correct degradation for pre-rule persisted
// state. The caller holds commitMu.
func (d *DAG) eligibleForEpoch(epoch uint64) map[Hash]bool {
	if epoch == d.currentEpoch {
		return d.eligibleHolders
	}

	if d.currentEpoch > 0 && epoch == d.currentEpoch-1 {
		return d.prevEligibleHolders
	}

	// One epoch ahead: the frozen forward proxy; during the genesis epoch the
	// epoch-1 rounds resolve to the genesis snapshot, so its eligible set applies.
	if epoch == d.currentEpoch+1 {
		if d.nextEligibleHolders != nil {
			return d.nextEligibleHolders
		}
		if d.currentEpoch == 0 {
			return d.eligibleHolders
		}
	}

	return nil
}

// eligibleKeys returns the holder snapshot's sorted keys restricted to the eligible
// set. While NO member of the snapshot is eligible (bootstrap, or an eligible set
// holding only removed members) it returns the FULL sorted key set, so designation
// never goes undefined.
func eligibleKeys(holders *ValidatorSet, eligible map[Hash]bool) []Hash {
	keys := holders.SortedKeys()
	if len(eligible) == 0 {
		return keys
	}

	filtered := make([]Hash, 0, len(keys))
	for _, key := range keys {
		if eligible[key] {
			filtered = append(filtered, key)
		}
	}

	if len(filtered) == 0 {
		return keys
	}

	return filtered
}

// encodeMemberSet serializes a member set as a count followed by the pubkeys in
// ascending byte order, so the encoded bytes are canonical across nodes. A nil or
// empty set encodes as a count-0 record.
func encodeMemberSet(set map[Hash]bool) []byte {
	keys := sortedMemberKeys(set)

	buf := binary.BigEndian.AppendUint32(nil, uint32(len(keys)))
	for _, key := range keys {
		buf = append(buf, key[:]...)
	}

	return buf
}

// decodeMemberSet rebuilds a member set from its serialized form. An absent,
// truncated, or count-0 record decodes as nil, which reads as "no eligibility
// frozen" and falls back to the full snapshot.
func decodeMemberSet(data []byte) map[Hash]bool {
	if len(data) < 4 {
		return nil
	}

	count := int(binary.BigEndian.Uint32(data))
	if count == 0 || len(data) < 4+count*32 {
		return nil
	}

	set := make(map[Hash]bool, count)
	for i := 0; i < count; i++ {
		var pubkey Hash
		copy(pubkey[:], data[4+i*32:4+(i+1)*32])
		set[pubkey] = true
	}

	return set
}
