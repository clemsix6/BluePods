package consensus

import "encoding/binary"

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

	buf := binary.BigEndian.AppendUint64(nil, d.currentEpoch)
	buf = append(buf, boolByte(d.strictLatched))
	buf = binary.BigEndian.AppendUint64(buf, d.strictStartRound)
	buf = appendHolderBlob(buf, d.epochHolders)
	buf = appendHolderBlob(buf, d.prevEpochHolders)
	buf = appendHolderBlob(buf, d.nextEpochHolders)

	return d.lastCommitted, buf
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
	d.nextEpochHolders, _ = readHolderBlob(rest)

	d.restoreCommittedMembers()
}

// appendHolderBlob appends a length-prefixed encoded holder snapshot to buf. A nil
// snapshot encodes as a count-0 record, which readHolderBlob restores back to nil.
func appendHolderBlob(buf []byte, set *ValidatorSet) []byte {
	encoded := encodeHolderSnapshot(set)
	buf = binary.BigEndian.AppendUint32(buf, uint32(len(encoded)))

	return append(buf, encoded...)
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
