package client

import (
	"fmt"
	"math"
	"math/bits"

	"BluePods/internal/index"
	"BluePods/internal/network"
)

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
// and the only value a proof may be checked against. Epoch is the highest
// epoch among the headers that counted, which is the epoch the attested root
// is attributed to — under the spec §5 handoff rule a set at epoch N may weigh
// headers at N+1, and that attested root is what then proves the N+1 set.
type VerifiedAnchor struct {
	FrontierRound uint64   // FrontierRound is the committed round IndexRoot anchors
	IndexRoot     [32]byte // IndexRoot is the attested index root
	Epoch         uint64   // Epoch is the highest epoch among the counted headers
}

// VerifyAnchor checks that a GetIndexAnchor bundle carries set's capped-stake
// quorum for one (frontier, index root) pair, and returns that pair as the
// only thing a client may trust from the node that served it.
//
// Nothing outside the signed headers is authoritative: the bundle's own
// FrontierRound, IndexRoot and Epoch fields are the serving node's claim, so
// every counted header must repeat the frontier and the root it claims, and
// the epoch is read from the headers rather than from the response.
func VerifyAnchor(bundle *network.GetIndexAnchorResponse, set ValidatorSet) (VerifiedAnchor, error) {
	if bundle == nil || !bundle.Found {
		return VerifiedAnchor{}, fmt.Errorf("no quorate anchor bundle: the node has no attested frontier yet")
	}

	if len(set.Leaves) == 0 {
		return VerifiedAnchor{}, fmt.Errorf("empty validator set: nothing can weigh a quorum")
	}

	counted, epoch := countedProducers(bundle, set)

	attesting, total := cappedStakeOf(set, counted)
	if !quorumReached(attesting, total) {
		return VerifiedAnchor{}, fmt.Errorf("bundle at frontier %d carries %d of %d capped stake across %d of %d validators, short of two thirds",
			bundle.FrontierRound, attesting, total, len(counted), len(set.Leaves))
	}

	return VerifiedAnchor{FrontierRound: bundle.FrontierRound, IndexRoot: bundle.IndexRoot, Epoch: epoch}, nil
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

// countedProducers returns the distinct set members whose own signed header
// attests the bundle's claimed pair, together with the highest epoch among
// them. A record that fails any check is skipped rather than fatal: a bundle
// may legitimately carry a header from a producer this set does not know (one
// that joined at the boundary), and the quorum test below decides whether what
// remains is enough.
func countedProducers(bundle *network.GetIndexAnchorResponse, set ValidatorSet) (map[[32]byte]bool, uint64) {
	members := membersByPubkey(set)
	counted := make(map[[32]byte]bool, len(bundle.Headers))

	var epoch uint64

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

		counted[header.Producer] = true

		if header.Epoch > epoch {
			epoch = header.Epoch
		}
	}

	return counted, epoch
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
