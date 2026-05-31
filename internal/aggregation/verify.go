package aggregation

import (
	"bytes"
	"fmt"

	"BluePods/internal/attest"
	"BluePods/internal/types"
	"BluePods/internal/validators"
)

// EpochResolver supplies the epoch-boundary information the ATX verifier needs:
// the holder snapshot for a given epoch, the commit epoch a round belongs to,
// the per-epoch round count, and the grace window. The consensus DAG implements
// it; injecting it keeps aggregation free of a consensus import.
type EpochResolver struct {
	// HoldersForEpoch returns the holder snapshot for an epoch, or false when the
	// epoch is too old or in the future.
	HoldersForEpoch func(epoch uint64) (*validators.ValidatorSet, bool)

	// CommitEpochForRound returns the epoch an ATX committed at a round belongs to.
	CommitEpochForRound func(round uint64) uint64

	// EpochLength is the number of rounds per epoch (0 when epochs are disabled).
	EpochLength uint64

	// GraceRounds is the number of rounds after a boundary that the previous
	// epoch's attestations are still accepted.
	GraceRounds uint64
}

// ATXVerifier verifies the BLS quorum proofs carried in an attested transaction.
// It selects the holder snapshot from the epoch the attestations were collected
// in and validates that epoch against the commit round (same epoch, or the
// previous epoch within the grace window).
type ATXVerifier struct {
	// epoch resolves epoch-boundary information from the consensus layer.
	epoch EpochResolver
}

// NewATXVerifier creates an ATXVerifier driven by the given epoch resolver.
func NewATXVerifier(epoch EpochResolver) *ATXVerifier {
	return &ATXVerifier{epoch: epoch}
}

// Verify checks every QuorumProof in the ATX against its included object and the
// holder snapshot of the attestation epoch carried on the wire. It validates the
// attestation epoch against the deterministic commit epoch and returns an error
// on the first invalid proof.
//
// It is a thin wrapper over VerifyBatch for a single ATX, so the per-ATX verdict
// is identical whether an ATX is verified alone or as part of a round's batch.
func (v *ATXVerifier) Verify(atx *types.AttestedTransaction, commitRound uint64) error {
	return v.VerifyBatch([]*types.AttestedTransaction{atx}, commitRound)[0]
}

// VerifyBatch verifies the BLS proofs of every ATX in a committed round in one
// parallel pass and returns one error per ATX, in input order: results[i] is nil
// when atxs[i] passes and the same error Verify would return otherwise.
//
// It runs in two phases. The prepare phase is cheap and fully deterministic: for
// each proof it resolves the holder snapshot, looks up the object, recomputes the
// canonical hash, derives the signer keys, and checks the quorum, recording
// either an early rejection or an attest.AggCheck. The signature phase verifies
// every collected AggCheck with attest.VerifyAggregatedBatch across cores. Each
// AggCheck is a pure function of its own ATX, so parallelism only changes when
// the signatures are checked, never which ATXs pass or the order of the results.
func (v *ATXVerifier) VerifyBatch(atxs []*types.AttestedTransaction, commitRound uint64) []error {
	results := make([]error, len(atxs))

	plan := v.prepareBatch(atxs, commitRound, results)

	verdicts := attest.VerifyAggregatedBatch(plan.checks)

	applyBatchVerdicts(plan, verdicts, results)

	return results
}

// batchPlan holds the AggChecks gathered across a round's ATXs and, for each
// check, which ATX it belongs to and the proof index within that ATX.
type batchPlan struct {
	checks  []attest.AggCheck // checks are the signature verifications to run in parallel
	atxOf   []int             // atxOf[k] is the ATX index that check k belongs to
	proofOf []int             // proofOf[k] is the proof index within that ATX
}

// prepareBatch runs the cheap deterministic phase for every ATX. It writes an
// early rejection into results for any ATX that fails before the signature check
// and collects the surviving proofs into AggChecks for the parallel phase. The
// first failing proof of an ATX wins, exactly as the sequential loop does.
func (v *ATXVerifier) prepareBatch(atxs []*types.AttestedTransaction, commitRound uint64, results []error) batchPlan {
	var plan batchPlan

	for atxIdx, atx := range atxs {
		results[atxIdx] = v.collectATXChecks(atx, atxIdx, commitRound, &plan)
	}

	return plan
}

// collectATXChecks resolves the holder snapshot for one ATX and appends an
// AggCheck for each proof. It returns the first non-signature error encountered
// (matching Verify's first-failure semantics); on such an error no AggChecks for
// later proofs of this ATX are added, so a deterministic signature failure can
// never override an earlier deterministic rejection.
func (v *ATXVerifier) collectATXChecks(atx *types.AttestedTransaction, atxIdx int, commitRound uint64, plan *batchPlan) error {
	vs, err := v.resolveHolders(atx.AttestationEpoch(), commitRound)
	if err != nil {
		return err
	}

	var proof types.QuorumProof

	for i := 0; i < atx.ProofsLength(); i++ {
		if !atx.Proofs(&proof, i) {
			return fmt.Errorf("cannot read proof %d", i)
		}

		check, err := v.prepareSingleProof(atx, &proof, vs)
		if err != nil {
			return fmt.Errorf("proof %d:\n%w", i, err)
		}

		plan.checks = append(plan.checks, check)
		plan.atxOf = append(plan.atxOf, atxIdx)
		plan.proofOf = append(plan.proofOf, i)
	}

	return nil
}

// applyBatchVerdicts folds the parallel signature verdicts back into the per-ATX
// results. A false verdict rejects its ATX with the same error verifySingleProof
// would return; the first failing proof of an ATX wins, and an ATX already
// rejected in the prepare phase is left untouched.
func applyBatchVerdicts(plan batchPlan, verdicts []bool, results []error) {
	for k, ok := range verdicts {
		if ok {
			continue
		}

		atxIdx := plan.atxOf[k]
		if results[atxIdx] != nil {
			continue
		}

		results[atxIdx] = fmt.Errorf("proof %d:\n%w", plan.proofOf[k], errSignatureInvalid)
	}
}

// errSignatureInvalid is the error a failing aggregated BLS verification yields,
// matching the message verifySingleProof returned inline.
var errSignatureInvalid = fmt.Errorf("aggregated BLS signature invalid")

// resolveHolders validates the attestation epoch against the commit round and
// returns the holder snapshot to recompute the quorum against. It accepts the
// commit epoch, or the previous epoch when the commit round is within the grace
// window of the boundary.
func (v *ATXVerifier) resolveHolders(attestationEpoch, commitRound uint64) (*validators.ValidatorSet, error) {
	commitEpoch := v.epoch.CommitEpochForRound(commitRound)

	switch {
	case attestationEpoch == commitEpoch:
		// Current epoch: always valid.
	case commitEpoch > 0 && attestationEpoch == commitEpoch-1:
		if !v.withinGrace(commitRound) {
			return nil, fmt.Errorf("attestation epoch %d past grace at round %d", attestationEpoch, commitRound)
		}
	default:
		return nil, fmt.Errorf("attestation epoch %d invalid for commit epoch %d", attestationEpoch, commitEpoch)
	}

	vs, ok := v.epoch.HoldersForEpoch(attestationEpoch)
	if !ok {
		return nil, fmt.Errorf("no holder snapshot for epoch %d", attestationEpoch)
	}

	return vs, nil
}

// withinGrace reports whether a commit round is still within the grace window of
// the boundary that separates the commit epoch from the previous one. That
// boundary is round commitEpoch*epochLength (the round where the previous epoch
// transitioned into the commit epoch).
func (v *ATXVerifier) withinGrace(commitRound uint64) bool {
	if v.epoch.EpochLength == 0 {
		return false
	}

	commitEpoch := v.epoch.CommitEpochForRound(commitRound)
	boundary := commitEpoch * v.epoch.EpochLength

	if commitRound < boundary {
		return false
	}

	return commitRound-boundary < v.epoch.GraceRounds
}

// prepareSingleProof runs every deterministic check for one QuorumProof except
// the aggregated BLS verification, and returns the AggCheck that defers that
// verification to the parallel phase. The checks and their order are identical to
// the inline verifySingleProof they replace, so a proof rejected here is rejected
// exactly as before; only the final signature check is deferred.
func (v *ATXVerifier) prepareSingleProof(atx *types.AttestedTransaction, proof *types.QuorumProof, vs *validators.ValidatorSet) (attest.AggCheck, error) {
	objIdx := findATXObjectIndex(atx, proof.ObjectIdBytes())
	if objIdx < 0 {
		return attest.AggCheck{}, fmt.Errorf("object not found in ATX")
	}

	var obj types.Object
	if !atx.Objects(&obj, objIdx) {
		return attest.AggCheck{}, fmt.Errorf("cannot read object at index %d", objIdx)
	}

	// Recompute the canonical hash from the object's content bytes.
	hash := attest.ComputeObjectHash(obj.ContentBytes(), obj.Version())

	var objectID [32]byte
	copy(objectID[:], proof.ObjectIdBytes())
	holders := attest.ComputeHolders(vs, objectID, int(obj.Replication()))

	blsKeys, signerCount := extractSignerBLSKeys(proof.SignerBitmapBytes(), holders, vs)
	if signerCount == 0 {
		return attest.AggCheck{}, fmt.Errorf("no signers in bitmap")
	}

	quorum := attest.QuorumSize(len(holders))
	if signerCount < quorum {
		return attest.AggCheck{}, fmt.Errorf("insufficient signers: got %d, need %d", signerCount, quorum)
	}

	// The flatbuffer fields below are owned by the parent buffer, which is read
	// only during commit, so the worker pool reads them concurrently without
	// copying. hash is a local array; take a stable slice over it.
	return attest.AggCheck{
		Signature:  proof.BlsSignatureBytes(),
		Message:    hash[:],
		PublicKeys: blsKeys,
	}, nil
}

// findATXObjectIndex returns the index of the object with the given ID in the ATX, or -1.
func findATXObjectIndex(atx *types.AttestedTransaction, objectID []byte) int {
	var obj types.Object

	for i := 0; i < atx.ObjectsLength(); i++ {
		if !atx.Objects(&obj, i) {
			continue
		}

		if bytes.Equal(obj.IdBytes(), objectID) {
			return i
		}
	}

	return -1
}

// extractSignerBLSKeys maps a signer bitmap to BLS public keys via rendezvous holders.
// Returns the BLS keys and the number of signers.
func extractSignerBLSKeys(bitmap []byte, holders []validators.Hash, vs *validators.ValidatorSet) ([][]byte, int) {
	indices := attest.ParseSignerBitmap(bitmap)
	var keys [][]byte

	for _, idx := range indices {
		if idx >= len(holders) {
			continue
		}

		info := vs.Get(holders[idx])
		if info == nil || info.BLSPubkey == [48]byte{} {
			continue
		}

		keys = append(keys, info.BLSPubkey[:])
	}

	return keys, len(keys)
}
