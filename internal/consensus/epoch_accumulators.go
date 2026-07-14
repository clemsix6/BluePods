package consensus

import (
	"bytes"
	"encoding/binary"
	"sort"

	"BluePods/internal/storage"
)

// Epoch settlement accumulators are the in-epoch state distributeEpochRewards mints
// from at the boundary: the pooled consumed fees, per-validator liveness, this
// epoch's validator additions (churn limiting), and the pending removals. Every
// committed round advances them, and committed flags stop a restarted or freshly
// synced node from re-deriving them by replay. So they are persisted atomically with
// the commit cursor (each round) and carried in the sync regime blob; without this a
// mid-epoch restart or join reaches the boundary with near-zero accumulators and
// forks the reward mint deterministically (C-2).
var (
	epochFeesKey           = metaKey("epochFees")           // epochFeesKey holds the pooled consumed fees this epoch
	epochRoundsProducedKey = metaKey("epochRoundsProduced") // epochRoundsProducedKey holds per-validator liveness this epoch
	epochAdditionsKey      = metaKey("epochAdditions")      // epochAdditionsKey holds this epoch's validator additions
	pendingRemovalsKey     = metaKey("pendingRemovals")     // pendingRemovalsKey holds validators queued for removal at the next boundary
)

// accumulatorKVs returns the settlement accumulators as batch pairs. It is the
// subset of the persisted epoch state that changes on EVERY committed round, so the
// common commit path rides it on the cursor batch without rewriting the holder
// snapshots. The caller holds commitMu.
func (d *DAG) accumulatorKVs() []storage.KeyValue {
	fees := make([]byte, 8)
	binary.BigEndian.PutUint64(fees, d.epochFees)

	return []storage.KeyValue{
		{Key: epochFeesKey, Value: fees},
		{Key: epochRoundsProducedKey, Value: encodeCounterSet(d.epochRoundsProduced)},
		{Key: epochAdditionsKey, Value: encodeHashList(d.epochAdditions)},
		{Key: pendingRemovalsKey, Value: encodeMemberSet(d.pendingRemovals)},
	}
}

// restoreAccumulators restores the settlement accumulators at boot. Absent keys (a
// fresh node, or one that committed no round this epoch) decode to the zero
// accumulators, which equal the post-boundary reset, so the restore is safe whether
// or not the node ever persisted them. The caller holds no lock (boot).
func (d *DAG) restoreAccumulators() {
	if v, ok := d.store.loadMetaUint64(epochFeesKey); ok {
		d.epochFees = v
	}

	if produced := decodeCounterSet(d.store.loadMetaBytes(epochRoundsProducedKey)); produced != nil {
		d.epochRoundsProduced = produced
	}

	if additions := decodeHashList(d.store.loadMetaBytes(epochAdditionsKey)); additions != nil {
		d.epochAdditions = additions
	}

	if removals := decodeMemberSet(d.store.loadMetaBytes(pendingRemovalsKey)); removals != nil {
		d.pendingRemovals = removals
	}
}

// appendEpochAccumulators appends the settlement accumulators to a regime blob, each
// length-prefixed so the tail stays self-delimiting and append-only safe. A joiner
// landing mid-epoch thus reaches the boundary with the same fees, liveness, additions,
// and removals as the source. The caller holds commitMu.
func (d *DAG) appendEpochAccumulators(buf []byte) []byte {
	buf = binary.BigEndian.AppendUint64(buf, d.epochFees)
	buf = appendLenPrefixed(buf, encodeCounterSet(d.epochRoundsProduced))
	buf = appendLenPrefixed(buf, encodeHashList(d.epochAdditions))
	buf = appendLenPrefixed(buf, encodeMemberSet(d.pendingRemovals))

	return buf
}

// readEpochAccumulators decodes the settlement accumulators from the tail of a regime
// blob into d. A truncated or absent tail leaves the zero accumulators (an older blob,
// or a genesis-epoch source that carried none). The caller holds commitMu.
func (d *DAG) readEpochAccumulators(data []byte) {
	if len(data) < 8 {
		return
	}

	d.epochFees = binary.BigEndian.Uint64(data[:8])
	data = data[8:]

	var raw []byte
	raw, data = readLenPrefixed(data)
	if produced := decodeCounterSet(raw); produced != nil {
		d.epochRoundsProduced = produced
	}

	raw, data = readLenPrefixed(data)
	if additions := decodeHashList(raw); additions != nil {
		d.epochAdditions = additions
	}

	raw, _ = readLenPrefixed(data)
	if removals := decodeMemberSet(raw); removals != nil {
		d.pendingRemovals = removals
	}
}

// encodeCounterSet serializes a map[Hash]uint64 as a count followed by (pubkey,
// value) pairs in ascending pubkey order, canonical across nodes. A nil or empty map
// encodes as a count-0 record.
func encodeCounterSet(set map[Hash]uint64) []byte {
	keys := make([]Hash, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}

	sort.Slice(keys, func(i, j int) bool {
		return bytes.Compare(keys[i][:], keys[j][:]) < 0
	})

	buf := binary.BigEndian.AppendUint32(nil, uint32(len(keys)))
	for _, k := range keys {
		buf = append(buf, k[:]...)
		buf = binary.BigEndian.AppendUint64(buf, set[k])
	}

	return buf
}

// decodeCounterSet rebuilds a map[Hash]uint64. An absent, truncated, or count-0
// record decodes as nil (the empty accumulator).
func decodeCounterSet(data []byte) map[Hash]uint64 {
	const recordSize = 32 + 8

	if len(data) < 4 {
		return nil
	}

	count := int(binary.BigEndian.Uint32(data))
	if count == 0 || len(data) < 4+count*recordSize {
		return nil
	}

	set := make(map[Hash]uint64, count)
	off := 4
	for i := 0; i < count; i++ {
		var k Hash
		copy(k[:], data[off:off+32])
		set[k] = binary.BigEndian.Uint64(data[off+32 : off+recordSize])
		off += recordSize
	}

	return set
}

// encodeHashList serializes a []Hash as a count followed by the hashes in ascending
// byte order. Order is not semantically meaningful (additions are consumed sorted),
// so canonical order keeps the encoded bytes identical across nodes. Empty encodes as
// a count-0 record.
func encodeHashList(list []Hash) []byte {
	sorted := append([]Hash(nil), list...)
	sort.Slice(sorted, func(i, j int) bool {
		return bytes.Compare(sorted[i][:], sorted[j][:]) < 0
	})

	buf := binary.BigEndian.AppendUint32(nil, uint32(len(sorted)))
	for _, h := range sorted {
		buf = append(buf, h[:]...)
	}

	return buf
}

// decodeHashList rebuilds a []Hash. An absent, truncated, or count-0 record decodes
// as nil.
func decodeHashList(data []byte) []Hash {
	if len(data) < 4 {
		return nil
	}

	count := int(binary.BigEndian.Uint32(data))
	if count == 0 || len(data) < 4+count*32 {
		return nil
	}

	list := make([]Hash, count)
	for i := 0; i < count; i++ {
		copy(list[i][:], data[4+i*32:4+(i+1)*32])
	}

	return list
}

// appendLenPrefixed appends b to buf behind a uint32 big-endian length prefix.
func appendLenPrefixed(buf, b []byte) []byte {
	buf = binary.BigEndian.AppendUint32(buf, uint32(len(b)))

	return append(buf, b...)
}

// readLenPrefixed reads one length-prefixed byte slice, returning it and the
// remaining bytes. A truncated prefix or body returns nil and consumes the input.
func readLenPrefixed(data []byte) (b, rest []byte) {
	if len(data) < 4 {
		return nil, nil
	}

	n := int(binary.BigEndian.Uint32(data))
	data = data[4:]
	if len(data) < n {
		return nil, nil
	}

	return data[:n], data[n:]
}
