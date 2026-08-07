package client

import (
	"errors"
	"fmt"
	"math"
	"math/bits"

	"BluePods/internal/index"
	"BluePods/internal/network"
)

// ErrCheckpointBehind reports a bundle whose headers all name an epoch
// outside the checkpoint's handoff window {Epoch, Epoch+1}: every header the
// node could possibly weigh has already moved past what this client's
// committee is authorized to attest. That is the weak-subjectivity boundary
// spec §5 describes, not a quorum failure — retrying gets the same refusal
// forever, and the caller's move is a fresh out-of-band checkpoint, never
// another poll.
var ErrCheckpointBehind = errors.New("checkpoint's handoff window no longer covers any served header")

// Checkpoint is a light client's trust anchor (spec §5): the weak-subjectivity
// pin it obtains out of band and then walks forward from. Holding no state of
// its own, a light client checks its first proof against IndexRoot, and the
// validator set that weighs every quorum is authenticated from it.
//
// It is NOT the joining node's checkpoint, which pins (epoch, validator set
// hash) only. A joiner cannot recompute a past frontier's index root — its
// snapshot is pinned at the source's last committed round and carries no
// versioned history (see cmd/node/checkpoint.go) — whereas a light client
// verifies a root it never has to recompute: given the four component roots of
// an answer, it checks that they combine to the pinned root and reads the
// validator component straight out of that combination. ValidatorSetHash is
// pinned too because a live node serves its CURRENT index root, which has
// usually moved past the checkpointed one by the time the client bootstraps;
// it is the same value a joiner's checkpoint carries, and the two are
// interchangeable.
type Checkpoint struct {
	Epoch            uint64   // Epoch is the epoch ValidatorSetHash describes
	IndexRoot        [32]byte // IndexRoot is the index root trusted at Epoch
	ValidatorSetHash [32]byte // ValidatorSetHash is index.ValidatorRootOf over Epoch's validator set
}

// ValidatorSet is an epoch's authenticated validator leaf set: what weighs an
// anchor quorum. It is only ever built by verifying a served set against a
// root the client already trusts, never taken from a node's word.
type ValidatorSet struct {
	Epoch  uint64                // Epoch is the epoch this set belongs to
	Leaves []index.ValidatorLeaf // Leaves are the epoch's validators with their capped voting weights
}

// VerifiedAnchor is a quorum-attested index root: the output of VerifyAnchor
// and the only value a proof may be checked against. Epoch is N+1 only when
// the N+1-epoch headers alone carry the committee's own quorum, and is N
// otherwise — never the served bundle's label, and never merely the highest
// epoch among whichever headers happened to count. A header's claimed epoch
// is that producer's word alone; attributing the anchor to N+1 on the
// strength of a minority would let one byzantine member drag a light
// client's checkpoint forward a full epoch (see VerifyAnchor).
type VerifiedAnchor struct {
	FrontierRound uint64   // FrontierRound is the committed round IndexRoot anchors
	IndexRoot     [32]byte // IndexRoot is the attested index root
	Epoch         uint64   // Epoch is the epoch whose own header subset carried the quorum
}

// VerifyAnchor checks that a GetIndexAnchor bundle carries set's capped-stake
// quorum for one (frontier, index root) pair, and returns that pair as the
// only thing a client may trust from the node that served it.
//
// Nothing outside the signed headers is authoritative: the bundle's own
// FrontierRound, IndexRoot and Epoch fields are the serving node's claim, so
// every counted header must repeat the frontier and the root it claims. The
// attested epoch is likewise never read from the response, nor taken as the
// highest epoch among whichever headers happened to count — it is N+1 only
// when the headers naming N+1 carry set's quorum BY THEMSELVES, and N
// otherwise. Weighing the two epochs' headers together for that decision
// would let one byzantine committee member's own N+1 claim relabel a genuine
// N-quorum as N+1, dragging a light client's checkpoint forward a full epoch
// on a single vote (see countedProducers and
// TestVerifyAnchor_EpochIsWhatItsOwnQuorumSays).
func VerifyAnchor(bundle *network.GetIndexAnchorResponse, set ValidatorSet) (VerifiedAnchor, error) {
	if bundle == nil || !bundle.Found {
		return VerifiedAnchor{}, fmt.Errorf("no quorate anchor bundle: the node has no attested frontier yet")
	}

	if len(set.Leaves) == 0 {
		return VerifiedAnchor{}, fmt.Errorf("empty validator set: nothing can weigh a quorum")
	}

	atCurrentEpoch, atNextEpoch := countedProducers(bundle, set)

	if attesting, total := cappedStakeOf(set, atNextEpoch); quorumReached(attesting, total) {
		return VerifiedAnchor{FrontierRound: bundle.FrontierRound, IndexRoot: bundle.IndexRoot, Epoch: set.Epoch + 1}, nil
	}

	if attesting, total := cappedStakeOf(set, atCurrentEpoch); quorumReached(attesting, total) {
		return VerifiedAnchor{FrontierRound: bundle.FrontierRound, IndexRoot: bundle.IndexRoot, Epoch: set.Epoch}, nil
	}

	return VerifiedAnchor{}, refusalError(bundle, set, atCurrentEpoch, atNextEpoch)
}

// refusalError explains why neither epoch's own header subset carried set's
// quorum. It distinguishes the weak-subjectivity boundary — every header in
// the bundle names an epoch outside {set.Epoch, set.Epoch+1}, so nothing this
// committee is authorized to weigh even exists in the bundle — from an
// ordinary shortfall, which reads identically as "0 of N capped stake" unless
// called out on its own.
func refusalError(bundle *network.GetIndexAnchorResponse, set ValidatorSet, atCurrentEpoch, atNextEpoch map[[32]byte]bool) error {
	if len(atCurrentEpoch) == 0 && len(atNextEpoch) == 0 && len(bundle.Headers) > 0 && allHeadersOutsideWindow(bundle, set) {
		return fmt.Errorf("bundle at frontier %d: no header falls inside the handoff window {%d, %d}: obtain a fresh checkpoint:\n%w",
			bundle.FrontierRound, set.Epoch, set.Epoch+1, ErrCheckpointBehind)
	}

	currentAttesting, total := cappedStakeOf(set, atCurrentEpoch)
	nextAttesting, _ := cappedStakeOf(set, atNextEpoch)

	return fmt.Errorf("bundle at frontier %d carries %d of %d capped stake at epoch %d and %d of %d at epoch %d, short of two thirds at either epoch on its own",
		bundle.FrontierRound, currentAttesting, total, set.Epoch, nextAttesting, total, set.Epoch+1)
}

// allHeadersOutsideWindow reports whether every header the bundle carries
// names an epoch outside {set.Epoch, set.Epoch+1}, regardless of whether it
// otherwise repeats the bundle's claim or comes from a known member. That is
// what separates a checkpoint that has fallen behind the chain from a bundle
// that simply lacks a quorum.
func allHeadersOutsideWindow(bundle *network.GetIndexAnchorResponse, set ValidatorSet) bool {
	for _, record := range bundle.Headers {
		header, err := parseAnchorRecord(record)
		if err != nil {
			continue
		}

		if header.Epoch == set.Epoch || header.Epoch == set.Epoch+1 {
			return false
		}
	}

	return true
}

// cappedStakeOf sums the attesting producers' voting weight and the committee's
// whole voting weight, both from the CAPPED stake the validator leaves carry.
// The cap is what keeps a whale's weight bounded, and it is applied on both
// sides of the ratio exactly as the chain applies it (cappedStakeOf in
// internal/consensus/stake.go, over the weights index.ValidatorLeaf pins). A
// verifier weighing raw stake would hand a capped colluder more weight than
// the chain ever gives it — which is why the leaf carries the capped value and
// nothing else.
//
// A producer counts once however many records it contributed, and a jailed
// validator carries a zero weight it cannot exceed here either, both properties
// coming from the set the leaves describe rather than from the bundle.
func cappedStakeOf(set ValidatorSet, attesting map[[32]byte]bool) (attestingStake, totalStake uint64) {
	for _, leaf := range set.Leaves {
		totalStake = saturatingAdd(totalStake, leaf.CappedStake)

		if attesting[leaf.Pubkey] {
			attestingStake = saturatingAdd(attestingStake, leaf.CappedStake)
		}
	}

	return attestingStake, totalStake
}

// quorumReached reports whether a capped-stake sum meets the two-thirds BFT
// threshold, in exact integer arithmetic. A zero total is never a quorum:
// without that guard an empty or zero-weight committee would attest anything.
func quorumReached(attesting, total uint64) bool {
	if total == 0 {
		return false
	}

	return saturatingMul(3, attesting) >= saturatingMul(2, total)
}

// saturatingAdd and saturatingMul mirror the chain's own overflow discipline
// (safeAdd/safeMul in internal/consensus/fees.go): a crafted validator set
// whose weights overflow must not wrap into a small number that clears the
// threshold.
func saturatingAdd(a, b uint64) uint64 {
	sum := a + b
	if sum < a {
		return math.MaxUint64
	}

	return sum
}

// saturatingMul multiplies, clamping at the maximum instead of wrapping.
func saturatingMul(a, b uint64) uint64 {
	if a == 0 || b == 0 {
		return 0
	}

	if hi, _ := bits.Mul64(a, b); hi > 0 {
		return math.MaxUint64
	}

	return a * b
}

// countedProducers partitions the distinct set members whose own signed
// header attests the bundle's claimed pair into the two epoch buckets the
// handoff window allows: those naming set.Epoch and those naming
// set.Epoch+1. A record that fails any check is skipped rather than fatal: a
// bundle may legitimately carry a header from a producer this set does not
// know (one that joined at the boundary), and the quorum test in VerifyAnchor
// decides whether what remains, PER BUCKET, is enough.
//
// The two buckets are returned separately, never merged into one member set,
// because VerifyAnchor must test each epoch's quorum against its own headers
// only — see VerifyAnchor for why.
func countedProducers(bundle *network.GetIndexAnchorResponse, set ValidatorSet) (atCurrentEpoch, atNextEpoch map[[32]byte]bool) {
	members := membersByPubkey(set)
	atCurrentEpoch = make(map[[32]byte]bool, len(bundle.Headers))
	atNextEpoch = make(map[[32]byte]bool, len(bundle.Headers))

	for _, record := range bundle.Headers {
		header, err := parseAnchorRecord(record)
		if err != nil {
			continue
		}

		if !attestsBundle(header, bundle, set) {
			continue
		}

		if _, member := members[header.Producer]; !member {
			continue
		}

		// attestsBundle already restricted header.Epoch to {set.Epoch,
		// set.Epoch+1}, so anything not the next epoch is the current one.
		if header.Epoch == set.Epoch+1 {
			atNextEpoch[header.Producer] = true
		} else {
			atCurrentEpoch[header.Producer] = true
		}
	}

	return atCurrentEpoch, atNextEpoch
}

// attestsBundle reports whether a header's own signed fields back the bundle's
// claim: the same frontier, the same index root, and an epoch inside the
// handoff window of spec §5. That window is exactly {set.Epoch, set.Epoch+1}:
// headers whose epoch is one past the set's are weighed by that set because
// validator churn is capped per epoch and the two sets overlap by
// construction, while a header two epochs ahead is refused — a client that far
// behind must walk its checkpoint forward or obtain a fresh one.
func attestsBundle(header anchorHeader, bundle *network.GetIndexAnchorResponse, set ValidatorSet) bool {
	if header.FrontierRound != bundle.FrontierRound || header.IndexRoot != bundle.IndexRoot {
		return false
	}

	return header.Epoch == set.Epoch || header.Epoch == set.Epoch+1
}

// membersByPubkey indexes a set's leaves by validator pubkey.
func membersByPubkey(set ValidatorSet) map[[32]byte]index.ValidatorLeaf {
	members := make(map[[32]byte]index.ValidatorLeaf, len(set.Leaves))
	for _, leaf := range set.Leaves {
		members[leaf.Pubkey] = leaf
	}

	return members
}

// authenticate returns the committee a served validator tree carries, checked
// against this checkpoint by whichever of its two links holds.
//
// The strong link is the index root: when the serving node's answer still
// describes the checkpointed root, the committee is authenticated BY that root
// — the four component roots combine to it and the validator component is the
// one the leaves rebuild. This is what a light client can close and a joining
// node cannot (see Checkpoint).
//
// The fallback is the pinned committee hash, for the ordinary case where the
// node's index has moved on since the checkpoint was cut. It authenticates the
// same bytes through the same leaf encoding; what it does not do is tie them
// to the checkpointed index root, which is why the strong link is tried first.
func (cp Checkpoint) authenticate(resp *network.GetValidatorTreeResponse) (ValidatorSet, error) {
	if resp == nil || !resp.Found {
		served := uint64(0)
		if resp != nil {
			served = resp.Epoch
		}

		return ValidatorSet{}, fmt.Errorf("checkpoint names epoch %d, the node serves epoch %d: obtain a fresh checkpoint",
			cp.Epoch, served)
	}

	if resp.Epoch != cp.Epoch {
		return ValidatorSet{}, fmt.Errorf("checkpoint names epoch %d, the answer carries epoch %d", cp.Epoch, resp.Epoch)
	}

	if set, err := (VerifiedAnchor{IndexRoot: cp.IndexRoot}).VerifyValidatorSet(resp); err == nil {
		return set, nil
	}

	leaves, err := decodeValidatorLeaves(resp.Leaves)
	if err != nil {
		return ValidatorSet{}, fmt.Errorf("validator set of epoch %d:\n%w", resp.Epoch, err)
	}

	if root := index.ValidatorRootOf(leaves); root != cp.ValidatorSetHash {
		return ValidatorSet{}, fmt.Errorf("epoch %d's served committee rebuilds root %x, the checkpoint pins %x",
			resp.Epoch, root[:8], cp.ValidatorSetHash[:8])
	}

	return ValidatorSet{Epoch: resp.Epoch, Leaves: leaves}, nil
}
