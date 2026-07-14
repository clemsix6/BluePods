package consensus

import (
	"bytes"
	"encoding/binary"
	"os"
	"strings"
	"testing"
	"time"
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

	_, blob := src.ExportRegimeState()

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

// TestExportRegimeStateAtomicCursorEpoch_I3 is the I3 torn-read regression: the snapshot
// export must read the commit cursor and the epoch state in ONE commitMu hold. The
// commit loop advances lastCommitted and increments the epoch together under commitMu,
// so if a boundary lands between a separate cursor read and a separate epoch-state read
// (the old two-acquisition pattern), the snapshot pairs a pre-boundary cursor with
// post-boundary epoch state; on import the joiner re-decides the boundary and transitions
// again, ending one epoch ahead of the source forever. The atomic export returns a
// mutually consistent pair whichever side of the boundary it lands on.
func TestExportRegimeStateAtomicCursorEpoch_I3(t *testing.T) {
	const epochLen = 4

	vals, vs := newTestValidatorSet(4)
	dag := New(newTestStorage(t), vs, nil, testSystemPod, 0, vals[0].privKey, nil, WithEpochLength(epochLen))
	t.Cleanup(func() { dag.Close() })
	setEqualStake(dag, vals, 25)

	// consistent reports whether a carried (rawCursor, blob) pair reconstructs one
	// committed frontier. On import WithLastCommittedRound(lastDecidedRound(rawCursor))
	// sets lastCommitted == rawCursor, and the commit loop maintains the invariant
	// currentEpoch == (lastCommitted-1)/epochLength once past round 0.
	consistent := func(rawCursor uint64, blob []byte) bool {
		epoch := binary.BigEndian.Uint64(blob[:8])
		if rawCursor == 0 {
			return epoch == 0
		}
		return epoch == (rawCursor-1)/epochLen
	}

	// Park the DAG exactly at boundary round 4: it is next-to-decide, still epoch 0.
	dag.commitMu.Lock()
	dag.lastCommitted = 4
	dag.currentEpoch = 0
	dag.epochHolders = snapshotOf(dag.validators)
	dag.commitMu.Unlock()

	// OLD two-acquisition pattern: read the cursor pre-boundary...
	tornCursor := dag.LastCommittedRound()

	// ...the commit loop crosses the boundary in the export window, advancing the cursor
	// and incrementing the epoch together under commitMu (as advanceCommitCursor does)...
	dag.commitMu.Lock()
	dag.lastCommitted = 5
	dag.currentEpoch = 1
	dag.commitMu.Unlock()

	// ...then the epoch state is read post-boundary. The pair is torn: a pre-boundary
	// cursor beside a post-boundary epoch, which would push the joiner an epoch ahead.
	_, tornBlob := dag.ExportRegimeState()
	if consistent(tornCursor, tornBlob) {
		t.Fatalf("expected the separate-read pattern to tear: cursor %d paired with epoch %d",
			tornCursor, binary.BigEndian.Uint64(tornBlob[:8]))
	}

	// NEW atomic export: cursor and epoch come from one commitMu hold and always agree.
	atomicCursor, atomicBlob := dag.ExportRegimeState()
	if !consistent(atomicCursor, atomicBlob) {
		t.Fatalf("atomic export desynced: cursor %d epoch %d",
			atomicCursor, binary.BigEndian.Uint64(atomicBlob[:8]))
	}
}

// TestExportConsistentCutValidatorsAtCursor is the joiner-fork regression found by
// the TestSimProgressiveJoining battery: the snapshot's validator set must come from
// the SAME commitMu hold as the commit cursor. The snapshot manager once read
// ValidatorsInfo() BEFORE taking the cut; ExportConsistentCut can block on commitMu
// behind a commit burst, and when that burst commits a register_validator the torn
// pair (pre-burst validators, post-burst cursor) misses a validator whose
// registration round the cursor already passed. A joiner booted from that snapshot
// never re-decides the round (it is flagged committed) and can never learn the
// validator — its committed projection forks forever (frozen at 5+self=6 in the
// sim). The cut's own (Cursor, Validators) pair must carry every validator
// registered below its cursor.
func TestExportConsistentCutValidatorsAtCursor(t *testing.T) {
	vals, vs := newTestValidatorSet(4)
	dag := New(newTestStorage(t), vs, nil, testSystemPod, 0, vals[0].privKey, nil)
	t.Cleanup(func() { dag.Close() })
	setEqualStake(dag, vals, 25)

	const regRound = 7
	joiner := Hash{0x77, 0x01}

	// Block the export behind a simulated commit burst: the burst holds commitMu,
	// commits a registration at regRound, and advances the cursor past it before
	// releasing — exactly what checkCommits does in one hold while the snapshot
	// manager's cut waits on the lock.
	dag.commitMu.Lock()

	cutCh := make(chan ConsistentCut, 1)
	go func() {
		cutCh <- dag.ExportConsistentCut(100)
	}()

	// Give the export goroutine time to perform any (buggy) pre-hold reads and
	// block on commitMu, then land the burst and release. If the goroutine has not
	// blocked yet the test can only pass trivially, never fail spuriously.
	time.Sleep(100 * time.Millisecond)
	dag.validators.Add(joiner, "127.0.0.1:9999", [48]byte{})
	dag.lastCommitted = regRound + 1
	dag.commitMu.Unlock()

	cut := <-cutCh
	defer cut.DBSnapshot.Close()

	if cut.Cursor <= regRound {
		t.Fatalf("cut cursor %d did not pass the registration round %d", cut.Cursor, regRound)
	}

	// The cut's pair must carry the validator its cursor has passed. A cut whose
	// validators were read before the hold would miss it here.
	if !containsValidator(cut.Validators, joiner) {
		t.Fatal("cut validators miss a validator registered below the cut cursor: a joiner booted from this snapshot forks its validator set forever")
	}
}

// containsValidator reports whether the validator infos include the given pubkey.
func containsValidator(infos []*ValidatorInfo, pubkey Hash) bool {
	for _, v := range infos {
		if v.Pubkey == pubkey {
			return true
		}
	}

	return false
}

// TestFreezeGenesisHoldersCanonicalEncoding is the Finding 2 regression: freezing the
// genesis holder set must iterate committed members in ascending pubkey order, so the
// frozen set's All() order — and therefore its encoded blob bytes — is canonical on
// every node rather than following Go's randomized map order. Byte-identical across two
// builds that admit the members in opposite orders, and the frozen order is sorted.
func TestFreezeGenesisHoldersCanonicalEncoding(t *testing.T) {
	vals, _ := newTestValidatorSet(4)
	pubkeys := keysOf(vals)

	freeze := func(order []int) *ValidatorSet {
		dag := New(newTestStorage(t), NewValidatorSet(pubkeys), nil, testSystemPod, 0, vals[0].privKey, nil)
		t.Cleanup(func() { dag.Close() })
		setEqualStake(dag, vals, 25)

		dag.committedMembers = make(map[Hash]bool)
		for _, i := range order {
			dag.committedMembers[vals[i].pubKey] = true
		}

		return dag.freezeGenesisHolders()
	}

	forward := freeze([]int{0, 1, 2, 3})
	reverse := freeze([]int{3, 2, 1, 0})

	if !bytes.Equal(encodeHolderSnapshot(forward), encodeHolderSnapshot(reverse)) {
		t.Fatal("frozen holder encoding is order-dependent across nodes")
	}

	members := forward.All()
	for i := 1; i < len(members); i++ {
		if bytes.Compare(members[i-1].Pubkey[:], members[i].Pubkey[:]) >= 0 {
			t.Fatalf("frozen holders are not in ascending pubkey order at index %d", i)
		}
	}
}
