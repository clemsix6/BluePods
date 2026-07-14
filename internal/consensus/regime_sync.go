package consensus

import (
	"encoding/binary"

	"BluePods/internal/storage"
)

// The regime state a sync snapshot must carry so a joiner landing AFTER the first
// epoch boundary is not left with zero epoch state (which would make HoldersForEpoch
// return false forever and wedge the commit loop — C2). It is the current epoch, the
// three holder snapshots, and the strict latch, read as ONE consistent cut and
// installed at construction, overriding any stale local epoch state (I3).

// ExportRegimeState reads the commit cursor together with the encoded regime state
// (current epoch, the three holder snapshots, the strict latch) as ONE consistent cut
// under commitMu, for a sync snapshot to carry. Pairing the cursor with the epoch state
// in a single hold is load-bearing: the commit loop advances lastCommitted and
// transitions the epoch under the same commitMu, so a boundary that would otherwise land
// between two separate reads cannot pair a pre-boundary cursor with post-boundary epoch
// state and shift the joiner one epoch ahead forever (I3). The returned cursor is the
// raw next-to-decide value; the caller converts it to the last-decided round.
func (d *DAG) ExportRegimeState() (cursor uint64, blob []byte) {
	d.commitMu.Lock()
	defer d.commitMu.Unlock()

	return d.lastCommitted, d.encodeRegimeState()
}

// encodeRegimeState serializes the regime state a sync snapshot carries: the epoch
// counter, the strict latch, the three holder snapshots, the eligibility sets, and
// the epoch settlement accumulators. The caller must hold commitMu so the encoded
// bytes pair with a consistent commit cursor.
func (d *DAG) encodeRegimeState() []byte {
	buf := binary.BigEndian.AppendUint64(nil, d.currentEpoch)
	buf = append(buf, boolByte(d.strictLatched))
	buf = binary.BigEndian.AppendUint64(buf, d.strictStartRound)
	buf = appendHolderBlob(buf, d.epochHolders)
	buf = appendHolderBlob(buf, d.prevEpochHolders)
	buf = appendHolderBlob(buf, d.nextEpochHolders)

	// Designation eligibility travels with the snapshots it was frozen with, plus
	// the live produced set, so the joiner's later freezes observe the same
	// committed production as the source.
	buf = appendMemberBlob(buf, d.producedMembers)
	buf = appendMemberBlob(buf, d.eligibleHolders)
	buf = appendMemberBlob(buf, d.prevEligibleHolders)
	buf = appendMemberBlob(buf, d.nextEligibleHolders)

	// Epoch settlement accumulators: a joiner landing mid-epoch must reach the
	// boundary with the same fees and liveness as the source, or the reward mint
	// forks (C-2). Length-prefixed, so the tail is append-only safe.
	buf = d.appendEpochAccumulators(buf)

	return buf
}

// ConsistentCut is a single atomic read of the committed frontier for a snapshot:
// the commit cursor, the encoded regime state, the vertices in the export window
// tagged with their committed flags, the object tracker entries, the validator
// set, and the supply and issuance counters — all captured under one commitMu hold
// so they pair with the same committed history. The validator set MUST come from
// this hold: read before it, a registration committed while the cut blocked on
// commitMu leaves the cursor past the registration round with the validator
// absent, and a joiner booted from that snapshot can never learn the validator
// (the round is flagged committed and never re-decided) — its committed
// projection forks forever. DBSnapshot is a storage snapshot taken inside the
// hold, from which the caller collects object, signature, and domain state at the
// identical cut; the caller MUST Close it.
type ConsistentCut struct {
	Cursor         uint64               // Cursor is the raw next-to-decide commit cursor
	Round          uint64               // Round is the current (latest) round at the cut, the vertex window's upper bound
	Regime         []byte               // Regime is the encoded epoch/latch/accumulator state
	Vertices       []VertexEntry        // Vertices are the exported vertices with committed flags
	TrackerEntries []ObjectTrackerEntry // TrackerEntries are the object versions/replication/fees
	Validators     []*ValidatorInfo     // Validators is the validator set at the cut (deep copies)
	Supply         uint64               // Supply is the protocol total supply at the cut
	IssuanceRate   uint64               // IssuanceRate is the thermostat's per-epoch rate in millionths at the cut
	DBSnapshot     *storage.Snapshot    // DBSnapshot is the consistent object/signature/domain view; caller closes it
}

// ExportConsistentCut captures the whole snapshot cut under one commitMu hold: the
// commit cursor, the regime blob, the committed-flagged vertices of the last
// historyRounds rounds, the tracker entries, the validator set, the supply and
// issuance counters, and a storage snapshot for object/signature/domain
// collection. Holding commitMu excludes the commit loop, so every field reflects
// the same committed frontier. The vertex window's upper bound is read INSIDE the
// hold too: a bound read before it could be outrun by a commit burst the cut
// blocked behind, leaving committed vertices just below the cursor out of the
// export.
func (d *DAG) ExportConsistentCut(historyRounds uint64) ConsistentCut {
	d.commitMu.Lock()
	defer d.commitMu.Unlock()

	toRound := d.round.Load()
	fromRound := uint64(0)
	if toRound > historyRounds {
		fromRound = toRound - historyRounds
	}

	return ConsistentCut{
		Cursor:         d.lastCommitted,
		Round:          toRound,
		Regime:         d.encodeRegimeState(),
		Vertices:       d.store.ExportVertices(fromRound, toRound),
		TrackerEntries: d.tracker.Export(),
		Validators:     d.validators.All(),
		Supply:         d.TotalSupply(),
		IssuanceRate:   d.issuanceRateMicro,
		DBSnapshot:     d.store.db.Snapshot(),
	}
}

// WithSyncedRegimeState installs sync-carried regime state at construction. It runs
// in the options loop, AFTER restoreEpochState, so it overrides any stale local
// epoch keys left in the joiner's data dir: syncing must never leave stale local
// holders beside an imported cursor. An empty blob is ignored (no regime carried).
func WithSyncedRegimeState(blob []byte) Option {
	return func(d *DAG) {
		if len(blob) == 0 {
			return
		}

		d.applyRegimeState(blob)
	}
}

// applyRegimeState decodes and installs the epoch and latch state, replacing whatever
// restoreEpochState loaded. It rebuilds the committed member set during the genesis
// epoch so a node synced mid-bootstrap keeps refreezing from the full committee.
func (d *DAG) applyRegimeState(blob []byte) {
	if len(blob) < 17 {
		return
	}

	d.currentEpoch = binary.BigEndian.Uint64(blob[0:8])
	d.strictLatched = blob[8] != 0
	d.strictStartRound = binary.BigEndian.Uint64(blob[9:17])

	rest := blob[17:]
	d.epochHolders, rest = readHolderBlob(rest)
	d.prevEpochHolders, rest = readHolderBlob(rest)
	d.nextEpochHolders, rest = readHolderBlob(rest)

	d.producedMembers, rest = readMemberBlob(rest)
	d.eligibleHolders, rest = readMemberBlob(rest)
	d.prevEligibleHolders, rest = readMemberBlob(rest)
	d.nextEligibleHolders, rest = readMemberBlob(rest)

	// Install the carried settlement accumulators so the joiner mints identically at
	// the next boundary. An older, shorter blob leaves rest empty and keeps the zero
	// accumulators.
	d.readEpochAccumulators(rest)

	d.restoreCommittedMembers()
}

// appendHolderBlob appends a length-prefixed encoded holder snapshot to buf. A nil
// snapshot encodes as a count-0 record, which readHolderBlob restores back to nil.
func appendHolderBlob(buf []byte, set *ValidatorSet) []byte {
	encoded := encodeHolderSnapshot(set)
	buf = binary.BigEndian.AppendUint32(buf, uint32(len(encoded)))

	return append(buf, encoded...)
}

// appendMemberBlob appends a length-prefixed encoded member set to buf. A nil set
// encodes as a count-0 record, which readMemberBlob restores back to nil.
func appendMemberBlob(buf []byte, set map[Hash]bool) []byte {
	encoded := encodeMemberSet(set)
	buf = binary.BigEndian.AppendUint32(buf, uint32(len(encoded)))

	return append(buf, encoded...)
}

// readMemberBlob reads one length-prefixed member set and returns it with the
// remaining bytes. An absent, truncated, or count-0 record restores as nil, which
// designation reads as "no eligibility frozen" (full-snapshot fallback).
func readMemberBlob(data []byte) (map[Hash]bool, []byte) {
	if len(data) < 4 {
		return nil, nil
	}

	n := binary.BigEndian.Uint32(data)
	data = data[4:]
	if uint32(len(data)) < n {
		return nil, nil
	}

	return decodeMemberSet(data[:n]), data[n:]
}

// readHolderBlob reads one length-prefixed holder snapshot and returns it with the
// remaining bytes. An empty (count-0) snapshot restores as nil, not a non-nil empty
// set, so an absent snapshot never reads as "resolved with zero holders".
func readHolderBlob(data []byte) (*ValidatorSet, []byte) {
	if len(data) < 4 {
		return nil, nil
	}

	n := binary.BigEndian.Uint32(data)
	data = data[4:]
	if uint32(len(data)) < n {
		return nil, nil
	}

	set := decodeHolderSnapshot(data[:n])
	rest := data[n:]
	if set.Len() == 0 {
		return nil, rest
	}

	return set, rest
}
