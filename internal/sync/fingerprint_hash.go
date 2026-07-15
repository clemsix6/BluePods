package sync

import (
	"bytes"
	"encoding/binary"
	"sort"

	"github.com/zeebo/blake3"

	"BluePods/internal/consensus"
	"BluePods/internal/state"
)

// hashTrackerEntries writes the tracker entries sorted by object ID (id,
// version, replication, fees) and returns the sum of their fees: the running
// total of locked storage deposits.
func hashTrackerEntries(h *blake3.Hasher, entries []consensus.ObjectTrackerEntry) uint64 {
	sorted := make([]consensus.ObjectTrackerEntry, len(entries))
	copy(sorted, entries)
	sort.Slice(sorted, func(i, j int) bool { return bytes.Compare(sorted[i].ID[:], sorted[j].ID[:]) < 0 })

	var buf [8]byte
	binary.BigEndian.PutUint32(buf[:4], uint32(len(sorted)))
	h.Write(buf[:4])

	var deposits uint64

	for _, e := range sorted {
		h.Write(e.ID[:])
		binary.BigEndian.PutUint64(buf[:], e.Version)
		h.Write(buf[:])
		binary.BigEndian.PutUint16(buf[:2], e.Replication)
		h.Write(buf[:2])
		binary.BigEndian.PutUint64(buf[:], e.Fees)
		h.Write(buf[:])
		deposits += e.Fees
	}

	return deposits
}

// hashDomains writes the domain entries sorted by name.
func hashDomains(h *blake3.Hasher, entries []state.DomainEntry) {
	sorted := make([]state.DomainEntry, len(entries))
	copy(sorted, entries)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })

	var buf [4]byte
	binary.BigEndian.PutUint32(buf[:], uint32(len(sorted)))
	h.Write(buf[:])

	for _, e := range sorted {
		nameBytes := []byte(e.Name)
		binary.BigEndian.PutUint32(buf[:], uint32(len(nameBytes)))
		h.Write(buf[:])
		h.Write(nameBytes)
		h.Write(e.ObjectID[:])
	}
}

// hashValidators writes the validators sorted by pubkey (pubkey, selfStake,
// delegatedTotal, jailed) and returns the unconditional sum of self plus
// delegated stake — jailed validators are included, since jailed stake has not
// returned to coins (unlike EffectiveStake, a consensus weight that zeroes a
// jailed validator's vote).
func hashValidators(h *blake3.Hasher, validators []*consensus.ValidatorInfo) uint64 {
	sorted := make([]*consensus.ValidatorInfo, len(validators))
	copy(sorted, validators)
	sort.Slice(sorted, func(i, j int) bool { return bytes.Compare(sorted[i].Pubkey[:], sorted[j].Pubkey[:]) < 0 })

	var buf [8]byte
	binary.BigEndian.PutUint32(buf[:4], uint32(len(sorted)))
	h.Write(buf[:4])

	var totalBonded uint64

	for _, v := range sorted {
		h.Write(v.Pubkey[:])
		binary.BigEndian.PutUint64(buf[:], v.SelfStake)
		h.Write(buf[:])
		binary.BigEndian.PutUint64(buf[:], v.DelegatedTotal)
		h.Write(buf[:])

		var jailedByte [1]byte
		if v.Jailed {
			jailedByte[0] = 1
		}
		h.Write(jailedByte[:])

		totalBonded += v.SelfStake + v.DelegatedTotal
	}

	return totalBonded
}

// hashEpochChanges writes the sorted pending validator removals and the
// sorted validators added this epoch.
func hashEpochChanges(h *blake3.Hasher, pendingRemovals, epochAdditions []consensus.Hash) {
	var buf [4]byte

	binary.BigEndian.PutUint32(buf[:], uint32(len(pendingRemovals)))
	h.Write(buf[:])
	for _, pk := range pendingRemovals {
		h.Write(pk[:])
	}

	binary.BigEndian.PutUint32(buf[:], uint32(len(epochAdditions)))
	h.Write(buf[:])
	for _, pk := range epochAdditions {
		h.Write(pk[:])
	}
}

// hashEpochCounters writes the current epoch number and the thermostat's
// per-epoch issuance rate.
func hashEpochCounters(h *blake3.Hasher, currentEpoch, issuanceRateMicro uint64) {
	var buf [8]byte

	binary.BigEndian.PutUint64(buf[:], currentEpoch)
	h.Write(buf[:])

	binary.BigEndian.PutUint64(buf[:], issuanceRateMicro)
	h.Write(buf[:])
}

// hashSingletons writes the full serialized bytes of every singleton object,
// already sorted by ID.
func hashSingletons(h *blake3.Hasher, singletons []singletonEntry) {
	var buf [4]byte

	binary.BigEndian.PutUint32(buf[:], uint32(len(singletons)))
	h.Write(buf[:])

	for _, s := range singletons {
		h.Write(s.id[:])
		binary.BigEndian.PutUint32(buf[:], uint32(len(s.data)))
		h.Write(buf[:])
		h.Write(s.data)
	}
}

// hashSupplyTerms writes the protocol supply counters as big-endian u64s.
func hashSupplyTerms(h *blake3.Hasher, totalSupply, coinsTotal, feesInFlight uint64) {
	var buf [8]byte

	binary.BigEndian.PutUint64(buf[:], totalSupply)
	h.Write(buf[:])

	binary.BigEndian.PutUint64(buf[:], coinsTotal)
	h.Write(buf[:])

	binary.BigEndian.PutUint64(buf[:], feesInFlight)
	h.Write(buf[:])
}
