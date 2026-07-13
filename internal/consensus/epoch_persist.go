package consensus

import (
	"encoding/binary"

	"BluePods/internal/logger"
	"BluePods/internal/storage"
)

// Epoch-state metadata keys. The epoch counter and the three holder snapshots are
// persisted at every epoch boundary (atomically with the commit cursor) so a
// restarted node serves HoldersForEpoch for its decided epochs without re-running
// transitionEpoch, and never wedges on a cursor that advanced past a boundary.
var (
	currentEpochKey     = metaKey("currentEpoch")     // currentEpochKey holds the persisted epoch counter
	epochHoldersKey     = metaKey("epochHolders")     // epochHoldersKey holds the current epoch's frozen snapshot
	prevEpochHoldersKey = metaKey("prevEpochHolders") // prevEpochHoldersKey holds the grace-window previous snapshot
	nextEpochHoldersKey = metaKey("nextEpochHolders") // nextEpochHoldersKey holds the one-epoch-ahead forward proxy
)

// blsKeyLen is the byte length of a validator's BLS public key.
const blsKeyLen = 48

// metaKey builds a metadata key by appending name to the shared metadata prefix.
func metaKey(name string) []byte {
	return append(append([]byte{}, prefixMeta...), []byte(name)...)
}

// epochStateKVs returns the persisted epoch-boundary state as batch pairs: the
// epoch counter and the three holder snapshots. It is called only at a boundary,
// right after transitionEpoch has set all four, so each snapshot is non-nil.
func (d *DAG) epochStateKVs() []storage.KeyValue {
	epoch := make([]byte, 8)
	binary.BigEndian.PutUint64(epoch, d.currentEpoch)

	return []storage.KeyValue{
		{Key: currentEpochKey, Value: epoch},
		{Key: epochHoldersKey, Value: encodeHolderSnapshot(d.epochHolders)},
		{Key: prevEpochHoldersKey, Value: encodeHolderSnapshot(d.prevEpochHolders)},
		{Key: nextEpochHoldersKey, Value: encodeHolderSnapshot(d.nextEpochHolders)},
	}
}

// restoreEpochState restores the persisted epoch counter and holder snapshots at
// boot, BEFORE the commit loop can call anchorStatus or HoldersForEpoch. A node
// restarted past the first epoch boundary thus serves its decided epochs from the
// restored snapshots instead of wedging (HoldersForEpoch false for a future epoch)
// or silently using the genesis live-set fallback. A fresh node has no persisted
// state, so this is a no-op: currentEpoch stays 0 and the genesis epoch is
// untouched. It is a clean seam the sync importer (Task 0.5b) can reuse to install
// snapshot-carried epoch state on the sync path.
func (d *DAG) restoreEpochState() {
	epoch, ok := d.store.loadMetaUint64(currentEpochKey)
	if !ok {
		return
	}

	d.currentEpoch = epoch
	d.epochHolders = d.loadHolderSnapshot(epochHoldersKey)
	d.prevEpochHolders = d.loadHolderSnapshot(prevEpochHoldersKey)
	d.nextEpochHolders = d.loadHolderSnapshot(nextEpochHoldersKey)

	logger.Info("restored epoch state at boot",
		"epoch", epoch,
		"epochHolders", holderLen(d.epochHolders),
	)
}

// loadHolderSnapshot decodes a persisted holder snapshot, or nil when the key is
// absent, so a missing snapshot falls back to the genesis path rather than panicking.
func (d *DAG) loadHolderSnapshot(key []byte) *ValidatorSet {
	data := d.store.loadMetaBytes(key)
	if len(data) == 0 {
		return nil
	}

	return decodeHolderSnapshot(data)
}

// holderLen returns a snapshot's size, or 0 when it is nil (for logging).
func holderLen(set *ValidatorSet) int {
	if set == nil {
		return 0
	}
	return set.Len()
}

// encodeHolderSnapshot serializes a holder set as exactly the fields AddWithStake
// restores: per validator its pubkey, BLS key, self-stake, delegated total, jail
// flag, and QUIC address. RewardCoin is omitted because holder snapshots are rebuilt
// through AddWithStake, which does not carry it (matching snapshotEpochHolders and
// snapshotOf). A nil set encodes as an empty snapshot.
func encodeHolderSnapshot(set *ValidatorSet) []byte {
	if set == nil {
		return binary.BigEndian.AppendUint32(nil, 0)
	}

	all := set.All()
	buf := binary.BigEndian.AppendUint32(nil, uint32(len(all)))
	for _, v := range all {
		buf = appendValidatorRecord(buf, v)
	}

	return buf
}

// appendValidatorRecord appends one validator's persisted fields to buf.
func appendValidatorRecord(buf []byte, v *ValidatorInfo) []byte {
	buf = append(buf, v.Pubkey[:]...)
	buf = append(buf, v.BLSPubkey[:]...)
	buf = binary.BigEndian.AppendUint64(buf, v.SelfStake)
	buf = binary.BigEndian.AppendUint64(buf, v.DelegatedTotal)
	buf = append(buf, boolByte(v.Jailed))
	buf = binary.BigEndian.AppendUint16(buf, uint16(len(v.QUICAddr)))
	buf = append(buf, v.QUICAddr...)

	return buf
}

// decodeHolderSnapshot rebuilds a holder set from its serialized form via
// AddWithStake, so the restored set carries the same membership and stake weights
// the snapshot was frozen with. A truncated record stops the decode cleanly.
func decodeHolderSnapshot(data []byte) *ValidatorSet {
	set := NewValidatorSet(nil)
	if len(data) < 4 {
		return set
	}

	count := binary.BigEndian.Uint32(data)
	off := 4

	for i := uint32(0); i < count; i++ {
		n, ok := decodeValidatorRecord(data[off:], set)
		if !ok {
			break
		}
		off += n
	}

	return set
}

// decodeValidatorRecord decodes one validator record from data into set and returns
// the number of bytes consumed. ok is false when data is too short for the record.
func decodeValidatorRecord(data []byte, set *ValidatorSet) (int, bool) {
	const fixed = 32 + blsKeyLen + 8 + 8 + 1 + 2
	if len(data) < fixed {
		return 0, false
	}

	var pubkey Hash
	var bls [blsKeyLen]byte
	copy(pubkey[:], data[:32])
	copy(bls[:], data[32:32+blsKeyLen])

	off := 32 + blsKeyLen
	selfStake := binary.BigEndian.Uint64(data[off : off+8])
	delegated := binary.BigEndian.Uint64(data[off+8 : off+16])
	jailed := data[off+16] != 0
	addrLen := int(binary.BigEndian.Uint16(data[off+17 : off+19]))

	off += 19
	if len(data) < off+addrLen {
		return 0, false
	}
	quicAddr := string(data[off : off+addrLen])

	set.AddWithStake(pubkey, quicAddr, bls, selfStake, delegated, jailed)
	return off + addrLen, true
}

// boolByte encodes a bool as one byte (1 for true, 0 for false).
func boolByte(b bool) byte {
	if b {
		return 1
	}
	return 0
}
