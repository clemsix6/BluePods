package consensus

import "BluePods/internal/storage"

// FingerprintCut is a single consistent read of every term a convergence
// fingerprint hashes or reports: the last committed round, the object tracker,
// the full validator set, the pending epoch changes, the epoch and issuance
// counters, the protocol supply terms, and a storage snapshot for the
// caller to collect domain entries and singleton objects at the identical cut.
// Every field MUST come from this one hold: reading any of them through a
// separate accessor at a later instant lets an epoch or commit boundary land
// between reads, pairing pre-boundary state with post-boundary state and
// producing a transient, non-reproducible supply mismatch. The caller MUST
// Close DBSnapshot.
type FingerprintCut struct {
	Round             uint64               // Round is the last committed round at the cut
	TrackerEntries    []ObjectTrackerEntry // TrackerEntries are the object versions/replication/fees at the cut
	Validators        []*ValidatorInfo     // Validators is the validator set at the cut (deep copies)
	PendingRemovals   []Hash               // PendingRemovals is the sorted set of validators queued for removal
	EpochAdditions    []Hash               // EpochAdditions is the sorted list of validators added this epoch
	CurrentEpoch      uint64               // CurrentEpoch is the epoch number at the cut
	IssuanceRateMicro uint64               // IssuanceRateMicro is the thermostat's per-epoch rate in millionths at the cut
	EpochFees         uint64               // EpochFees is the epoch's accumulated, undistributed consumed fees at the cut
	TotalSupply       uint64               // TotalSupply is the protocol total supply at the cut
	CoinsTotal        uint64               // CoinsTotal is the protocol sum of coin balances at the cut
	DBSnapshot        *storage.Snapshot    // DBSnapshot is the consistent object/domain view; caller closes it
}

// ExportFingerprintCut captures every term a convergence fingerprint needs
// under ONE commitMu hold, mirroring ExportConsistentCut's discipline
// (regime_sync.go). Holding commitMu excludes the commit loop, so the tracker,
// validator set, pending epoch changes, epoch/issuance counters, and supply
// terms all reflect the same committed frontier — none of them may be read
// through a separate accessor call, or a boundary landing between two reads
// would produce a fingerprint whose terms do not actually sum together.
func (d *DAG) ExportFingerprintCut() FingerprintCut {
	d.commitMu.Lock()
	defer d.commitMu.Unlock()

	return FingerprintCut{
		Round:             lastDecidedRound(d.lastCommitted),
		TrackerEntries:    d.tracker.Export(),
		Validators:        d.validators.All(),
		PendingRemovals:   sortedMemberKeys(d.pendingRemovals),
		EpochAdditions:    sortedAdditions(d.epochAdditions, len(d.epochAdditions)),
		CurrentEpoch:      d.currentEpoch,
		IssuanceRateMicro: d.issuanceRateMicro,
		EpochFees:         d.epochFees,
		TotalSupply:       d.TotalSupply(),
		CoinsTotal:        d.CoinsTotal(),
		DBSnapshot:        d.store.db.Snapshot(),
	}
}
