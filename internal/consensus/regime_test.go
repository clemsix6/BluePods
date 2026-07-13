package consensus

import (
	"os"
	"strings"
	"testing"
)

// keysOf returns the pubkeys of a slice of test validators.
func keysOf(vals []testValidator) []Hash {
	keys := make([]Hash, len(vals))
	for i, v := range vals {
		keys[i] = v.pubKey
	}

	return keys
}

// TestRelaxedVoteDeterminedCandidate_I1 is the I1 regression: during a relaxed
// bootstrap round two honest nodes separated by a one-vertex delivery gap decide the
// round identically. The candidate is the designated producer's vertex, selected by
// citations (verdictFromTally), NOT the lowest hash among locally-arrived vertices —
// which is view-dependent and forked two honest nodes in the reverted first attempt.
func TestRelaxedVoteDeterminedCandidate_I1(t *testing.T) {
	vals, _ := newTestValidatorSet(4)
	pubkeys := keysOf(vals)

	build := func() *DAG {
		dag := New(newTestStorage(t), NewValidatorSet(pubkeys), nil, testSystemPod, 0, vals[0].privKey, nil, WithMinValidators(4))
		t.Cleanup(func() { dag.Close() })
		setEqualStake(dag, vals, 25) // freezes the genesis committee, unlatched => relaxed
		return dag
	}

	const round = 3
	ref := build()
	if !ref.roundIsRelaxed(round) {
		t.Fatal("round must be relaxed (unlatched bootstrap)")
	}

	producer := designatedProducer(t, ref, vals, round)
	vData, v := buildRoundVertex(t, producer, round, nil) // the honest producer's single vertex

	// A decoy round-N vertex from a DIFFERENT producer: a lowest-arrived-hash rule
	// might latch onto it, but it is never the designated producer's candidate.
	decoyProducer := others(vals, producer)[0]
	wData, w := buildRoundVertex(t, decoyProducer, round, []Hash{{0x09}})

	// A single committee member cites the producer's vertex at round N+1 (relaxed
	// certificate: one supporter certifies). Identical bytes on both nodes.
	citer := others(vals, producer)[1]
	cData, c := buildRoundVertex(t, citer, round+1, []Hash{v})

	dagA := build()
	dagA.store.add(vData, v, round, producer.pubKey)
	dagA.store.add(wData, w, round, decoyProducer.pubKey) // A holds the decoy
	dagA.store.add(cData, c, round+1, citer.pubKey)

	dagB := build()
	dagB.store.add(vData, v, round, producer.pubKey)
	dagB.store.add(cData, c, round+1, citer.pubKey) // B is missing the decoy (one-vertex gap)

	decA := dagA.anchorStatus(round)
	decB := dagB.anchorStatus(round)

	if decA.kind != anchorCommit || decA.anchor != v {
		t.Fatalf("node A: expected relaxed commit of the producer's vertex, got kind=%d anchor=%x", decA.kind, decA.anchor[:4])
	}
	if decA != decB {
		t.Fatalf("a one-vertex delivery gap forked the decision: A=%+v B=%+v", decA, decB)
	}
}

// TestStrictLatchFromCommittedHistory_I8 is the I8/I7 regression: the strict latch is
// a pure function of committed registrations. Two nodes fed the identical committed
// membership latch to the same strict-start round even when one carries an extra
// optimistic self-add in its live set — the self-add never enters the frozen genesis
// snapshot nor shifts the latch — and the crossing round itself stays relaxed.
func TestStrictLatchFromCommittedHistory_I8(t *testing.T) {
	const minV = 3
	members := make([]testValidator, minV+1) // three committed + one self-add
	for i := range members {
		members[i] = newTestValidator()
	}

	build := func(withSelfAdd bool) *DAG {
		dag := New(newTestStorage(t), NewValidatorSet(nil), nil, testSystemPod, 0, members[0].privKey, nil,
			WithMinValidators(minV), WithTransitionGrace(2), WithTransitionBuffer(1))
		t.Cleanup(func() { dag.Close() })

		for i := 0; i < minV; i++ {
			dag.validators.AddWithStake(members[i].pubKey, "", [48]byte{}, 10, 0, false)
		}
		if withSelfAdd {
			dag.validators.AddWithStake(members[minV].pubKey, "", [48]byte{}, 10, 0, false)
		}
		return dag
	}

	crossRounds := []uint64{2, 3, 5} // committed rounds the three members register at

	record := func(dag *DAG) {
		dag.commitMu.Lock()
		defer dag.commitMu.Unlock()
		for i := 0; i < minV; i++ {
			dag.recordCommittedMember(members[i].pubKey, crossRounds[i])
		}
	}

	dagA := build(false)
	dagB := build(true) // B additionally holds an uncommitted self-add in its live set
	record(dagA)
	record(dagB)

	// The latch fires when the third committed member is recorded (round 5), plus the
	// grace (2) and buffer (1) windows: strictStartRound = 5 + 2 + 1 = 8.
	if !dagA.strictLatched || dagA.strictStartRound != 8 {
		t.Fatalf("A: latched=%v start=%d, want latched=true start=8", dagA.strictLatched, dagA.strictStartRound)
	}
	if dagB.strictLatched != dagA.strictLatched || dagB.strictStartRound != dagA.strictStartRound {
		t.Fatalf("uncommitted self-add shifted the latch: A start=%d B start=%d", dagA.strictStartRound, dagB.strictStartRound)
	}

	// I7: the frozen genesis snapshot holds exactly the committed members on both.
	if dagA.epochHolders.Len() != minV || dagB.epochHolders.Len() != minV {
		t.Fatalf("genesis snapshot size A=%d B=%d, want %d (self-add excluded)", dagA.epochHolders.Len(), dagB.epochHolders.Len(), minV)
	}

	// I8 clamp: the crossing round stays relaxed; the strict regime begins strictly
	// after it (never retroactively reclassifying an already-relaxed-decided round).
	if !dagA.roundIsRelaxed(5) {
		t.Fatal("the crossing round must remain relaxed")
	}
	if dagA.roundIsRelaxed(8) {
		t.Fatal("strictStartRound must be strict")
	}
}

// TestGenesisFreezeExcludesUncommittedSelfAdd_I7 is the I7 boundary regression: two
// epoch-0-tail nodes with different in-flight (uncommitted) registration states decide
// the boundary round identically, because the anchor decision reads the committed-only
// frozen genesis snapshot, never the live set that a self-add mutates.
func TestGenesisFreezeExcludesUncommittedSelfAdd_I7(t *testing.T) {
	vals, _ := newTestValidatorSet(4)
	pubkeys := keysOf(vals)

	build := func(withSelfAdd bool) *DAG {
		dag := New(newTestStorage(t), NewValidatorSet(pubkeys), nil, testSystemPod, 0, vals[0].privKey, nil, WithEpochLength(4))
		t.Cleanup(func() { dag.Close() })
		setEqualStake(dag, vals, 25) // strict regime (minValidators 0), genesis frozen
		if withSelfAdd {
			extra := newTestValidator()
			dag.validators.AddWithStake(extra.pubKey, "", [48]byte{}, 25, 0, false) // in-flight self-add
		}
		return dag
	}

	const boundary = 4 // first epoch boundary (epochLength 4)
	ref := build(false)
	producer := designatedProducer(t, ref, vals, boundary)
	vData, v := buildRoundVertex(t, producer, boundary, nil)

	type built struct {
		data []byte
		hash Hash
		pk   Hash
	}
	var citers []built
	for _, c := range others(vals, producer)[:3] { // three of four certify the boundary anchor
		cd, ch := buildRoundVertex(t, c, boundary+1, []Hash{v})
		citers = append(citers, built{cd, ch, c.pubKey})
	}

	decide := func(withSelfAdd bool) anchorDecision {
		dag := build(withSelfAdd)
		dag.store.add(vData, v, boundary, producer.pubKey)
		for _, ct := range citers {
			dag.store.add(ct.data, ct.hash, boundary+1, ct.pk)
		}
		return dag.anchorStatus(boundary)
	}

	decClean := decide(false)
	decInFlight := decide(true)

	if decClean.kind != anchorCommit || decClean.anchor != v {
		t.Fatalf("boundary must commit the certified anchor, got kind=%d", decClean.kind)
	}
	if decClean != decInFlight {
		t.Fatalf("an in-flight registration forked the boundary decision: clean=%+v inflight=%+v", decClean, decInFlight)
	}
}

// TestSyncCarriesRegimeStatePastBoundary_C2 is the C2 regression: a node that syncs
// AFTER the first epoch boundary imports the latch and epoch state and commits past
// the boundary, whereas a node landing there with zero epoch state cannot resolve the
// epoch and WAITs forever.
func TestSyncCarriesRegimeStatePastBoundary_C2(t *testing.T) {
	vals, vs := newTestValidatorSet(4)
	pubkeys := keysOf(vals)

	// A source node already past the first epoch boundary (epoch 1, latched).
	src := New(newTestStorage(t), vs, nil, testSystemPod, 0, vals[0].privKey, nil, WithEpochLength(4))
	t.Cleanup(func() { src.Close() })
	setEqualStake(src, vals, 25)
	src.commitMu.Lock()
	src.currentEpoch = 1
	src.epochHolders = snapshotOf(src.validators)
	src.prevEpochHolders = snapshotOf(src.validators)
	src.strictLatched = true
	src.strictStartRound = 2
	src.commitMu.Unlock()

	blob := src.ExportRegimeState()

	const round = 6 // an epoch-1 round to decide
	producer := designatedProducer(t, src, vals, round)
	vData, v := buildRoundVertex(t, producer, round, nil)

	type built struct {
		data []byte
		hash Hash
		pk   Hash
	}
	var citers []built
	for _, c := range others(vals, producer)[:3] {
		cd, ch := buildRoundVertex(t, c, round+1, []Hash{v})
		citers = append(citers, built{cd, ch, c.pubKey})
	}

	addFrontier := func(dag *DAG) {
		dag.store.add(vData, v, round, producer.pubKey)
		for _, ct := range citers {
			dag.store.add(ct.data, ct.hash, round+1, ct.pk)
		}
	}

	// Joiner WITH the synced regime state commits past the boundary.
	joiner := New(newTestStorage(t), NewValidatorSet(pubkeys), nil, testSystemPod, 0, vals[0].privKey, nil,
		WithEpochLength(4), WithSyncedRegimeState(blob), WithLastCommittedRound(round-1))
	t.Cleanup(func() { joiner.Close() })
	if joiner.currentEpoch != 1 || !joiner.strictLatched {
		t.Fatalf("regime state not imported: epoch=%d latched=%v", joiner.currentEpoch, joiner.strictLatched)
	}
	addFrontier(joiner)
	if dec := joiner.anchorStatus(round); dec.kind != anchorCommit || dec.anchor != v {
		t.Fatalf("joiner with synced regime state did not commit past the boundary: kind=%d", dec.kind)
	}

	// Negative control: a joiner with ZERO epoch state cannot resolve epoch 1 and WAITs.
	bare := New(newTestStorage(t), NewValidatorSet(pubkeys), nil, testSystemPod, 0, vals[0].privKey, nil,
		WithEpochLength(4), WithLastCommittedRound(round-1))
	t.Cleanup(func() { bare.Close() })
	addFrontier(bare)
	if dec := bare.anchorStatus(round); dec.kind != anchorWait {
		t.Fatalf("joiner with zero epoch state must WAIT, got kind=%d", dec.kind)
	}
}

// TestStrictQuorumReachableUnderStakeConcentration is the capped-denominator
// regression: a strict quorum divides the capped supporter sum by the CAPPED total,
// so a whale capped in the numerator is also capped in the denominator and unanimous
// support reaches the quorum. A raw-total denominator would make it unreachable and
// wedge the chain.
func TestStrictQuorumReachableUnderStakeConcentration(t *testing.T) {
	vals, vs := newTestValidatorSet(4)
	dag := New(newTestStorage(t), vs, nil, testSystemPod, 0, vals[0].privKey, nil)
	t.Cleanup(func() { dag.Close() })

	dag.validators.SetSelfStake(vals[0].pubKey, 1000) // whale
	for i := 1; i < 4; i++ {
		dag.validators.SetSelfStake(vals[i].pubKey, 10) // minnows
	}
	freezeGenesis(dag)

	set, ok := dag.HoldersForEpoch(0)
	if !ok {
		t.Fatal("genesis snapshot not frozen")
	}

	all := map[Hash]bool{}
	for _, v := range vals {
		all[v.pubKey] = true
	}

	// Under the capped denominator, unanimous support reaches the strict quorum.
	if !dag.reachesStrictQuorum(set, all) {
		t.Fatal("strict quorum unreachable under unanimous support (capped-denominator regression)")
	}

	// Sanity-check the fixture: a RAW-total denominator would NOT reach the quorum,
	// which is exactly the wedge the capped denominator fixes.
	cappedSum, cappedTotal := dag.cappedStakeOf(set, all)
	var rawTotal uint64
	for _, v := range set.All() {
		rawTotal = safeAdd(rawTotal, EffectiveStake(v))
	}
	if quorumReached(cappedSum, rawTotal) {
		t.Fatalf("fixture invalid: raw-total denominator (%d over %d) should NOT reach quorum", cappedSum, rawTotal)
	}
	if cappedSum != cappedTotal {
		t.Fatalf("unanimous support should sum to the capped total: sum=%d total=%d", cappedSum, cappedTotal)
	}
}

// TestAnchorPathNeverReadsLiveSet asserts the anchor path never falls back to the
// live validator set: with no frozen genesis snapshot epoch 0 is unresolved and the
// caller WAITs, and the anchor-path source files carry no live-set fallback.
func TestAnchorPathNeverReadsLiveSet(t *testing.T) {
	vals, vs := newTestValidatorSet(4)
	dag := New(newTestStorage(t), vs, nil, testSystemPod, 0, vals[0].privKey, nil)
	t.Cleanup(func() { dag.Close() })
	for _, v := range vals {
		dag.validators.SetSelfStake(v.pubKey, 25)
	}
	// Deliberately do NOT freeze the genesis snapshot.

	if _, ok := dag.HoldersForEpoch(0); ok {
		t.Fatal("HoldersForEpoch(0) must be false with no frozen snapshot (no live-set fallback)")
	}
	if _, ok := dag.anchorProducerFor(3); ok {
		t.Fatal("anchorProducerFor must be false with no genesis snapshot")
	}

	data, h := buildRoundVertex(t, vals[0], 3, nil)
	dag.store.add(data, h, 3, vals[0].pubKey)
	if dec := dag.anchorStatus(3); dec.kind != anchorWait {
		t.Fatalf("expected WAIT with no genesis snapshot, got kind=%d", dec.kind)
	}
}

// TestNoLiveSetFallbackInAnchorPath is the grep-level assertion the brief requires:
// the anchor-path source must not read the live validator set. anchor.go and
// anchor_decision.go must not mention d.validators at all, and HoldersForEpoch must
// carry no `return d.validators` fallback.
func TestNoLiveSetFallbackInAnchorPath(t *testing.T) {
	forbidden := map[string]string{
		"anchor.go":          "d.validators",
		"anchor_decision.go": "d.validators",
		"epoch.go":           "return d.validators",
	}

	for file, needle := range forbidden {
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		if strings.Contains(string(data), needle) {
			t.Fatalf("%s contains %q: the anchor path must never read the live validator set", file, needle)
		}
	}
}
