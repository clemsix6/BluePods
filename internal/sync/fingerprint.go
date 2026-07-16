package sync

import (
	"bytes"
	"sort"

	"github.com/zeebo/blake3"

	"BluePods/internal/consensus"
	"BluePods/internal/state"
	"BluePods/internal/storage"
)

// Fingerprint is the convergence digest of globally replicated state plus the
// locally evaluable supply terms. It deliberately excludes holder-scoped data
// (standard object content, the attestation signature store), the current
// round, and the per-epoch liveness counters, so it is stable across idle
// rounds and identical on every converged node.
type Fingerprint struct {
	Round        uint64   // Round is the last committed round at the cut (informational, NOT hashed)
	Checksum     [32]byte // Checksum is BLAKE3 over the canonical global state below
	TotalSupply  uint64   // TotalSupply is the protocol supply counter
	CoinsTotal   uint64   // CoinsTotal is the protocol sum-of-coin-balances counter
	TotalBonded  uint64   // TotalBonded is the sum of effective stake over the active set
	Deposits     uint64   // Deposits is the sum of locked storage deposits from the tracker
	FeesInFlight uint64   // FeesInFlight is the epoch's accumulated, undistributed consumed fees
}

// singletonEntry holds a singleton (replication == 0) object's ID and full
// serialized bytes, collected for fingerprint hashing.
type singletonEntry struct {
	id   [32]byte
	data []byte
}

// ComputeFingerprint captures one consistent cut of the dag and state, and
// digests the globally replicated portion into a single checksum. Two nodes at
// the same committed round with the same fingerprint hold the same replicated
// state. Every term hashed or reported comes from the SAME cut (the dag's
// ExportFingerprintCut, plus domains and singleton objects read from its
// storage snapshot), so nothing here can straddle an epoch or commit boundary.
func ComputeFingerprint(dag *consensus.DAG, st *state.State) Fingerprint {
	cut := dag.ExportFingerprintCut()
	defer cut.DBSnapshot.Close()

	domains := st.ExportDomainsFrom(cut.DBSnapshot)
	singletons := collectSingletons(cut.DBSnapshot, cut.TrackerEntries)

	hasher := blake3.New()
	deposits := hashTrackerEntries(hasher, cut.TrackerEntries)
	hashDomains(hasher, domains)
	totalBonded := hashValidators(hasher, cut.Validators)
	hashEpochChanges(hasher, cut.PendingRemovals, cut.EpochAdditions)
	hashEpochCounters(hasher, cut.CurrentEpoch, cut.IssuanceRateMicro)
	hashSingletons(hasher, singletons)
	hashSupplyTerms(hasher, cut.TotalSupply, cut.CoinsTotal, cut.EpochFees)

	var checksum [32]byte
	hasher.Sum(checksum[:0])

	return Fingerprint{
		Round:        cut.Round,
		Checksum:     checksum,
		TotalSupply:  cut.TotalSupply,
		CoinsTotal:   cut.CoinsTotal,
		TotalBonded:  totalBonded,
		Deposits:     deposits,
		FeesInFlight: cut.EpochFees,
	}
}

// collectSingletons reads the full bytes of every singleton (replication == 0)
// object from the cut's storage snapshot, sorted by ID. Tracker entries decide
// which IDs are singletons; the snapshot is scanned once, skipping consensus
// keys, to recover their stored bytes.
func collectSingletons(snap *storage.Snapshot, entries []consensus.ObjectTrackerEntry) []singletonEntry {
	singletonIDs := make(map[[32]byte]bool, len(entries))
	for _, e := range entries {
		if e.Replication == 0 {
			singletonIDs[e.ID] = true
		}
	}

	var out []singletonEntry

	_ = snap.Iterate(func(key, value []byte) error {
		if len(key) != objectKeySize || isConsensusKey(key) {
			return nil
		}

		var id [32]byte
		copy(id[:], key)

		if !singletonIDs[id] {
			return nil
		}

		data := make([]byte, len(value))
		copy(data, value)

		out = append(out, singletonEntry{id: id, data: data})

		return nil
	})

	sort.Slice(out, func(i, j int) bool { return bytes.Compare(out[i].id[:], out[j].id[:]) < 0 })

	return out
}
