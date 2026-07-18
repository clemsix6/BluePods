package consensus

import (
	"testing"

	"BluePods/internal/types"
)

// straddleRewardCoin returns the deterministic reward-coin id newStraddleDAG assigns
// to the validator at index i, so a test can read that validator's credited balance.
func straddleRewardCoin(i int) [32]byte {
	var id [32]byte
	id[0] = 0xC0
	id[1] = byte(i + 1)
	return id
}

// newStraddleDAG builds a four-validator DAG with the thermostat on (so each settled
// epoch mints a distributable pool) and a zero-balance reward coin per validator.
// Only the background producer (index 0) is staked at genesis; the three straddlers
// stake at the first boundary, reproducing the "unstaked in epoch E, staked in
// epoch E+1" weight change the straggler diagnosis pinned as the fork trigger.
func newStraddleDAG(t *testing.T, vals []testValidator) (*DAG, *mockCoinStore) {
	t.Helper()

	store := newMockCoinStore()
	store.SetTotalSupply(1_000_000_000)

	dag := New(newTestStorage(t), NewValidatorSet(keysOf(vals)), nil, testSystemPod, 0, vals[0].privKey, nil,
		WithEpochLength(10),
		WithThermostat(testThermostatParams()),
		WithListenerMode(),
	)
	t.Cleanup(func() { dag.Close() })

	params := DefaultFeeParams()
	dag.SetFeeSystem(store, &params, nil)

	for i, v := range vals {
		coin := straddleRewardCoin(i)
		store.SetObject(buildTestCoinObject(coin, 0, v.pubKey, 0))
		dag.validators.SetRewardCoin(v.pubKey, coin)
	}

	dag.validators.SetSelfStake(vals[0].pubKey, 100)
	freezeGenesis(dag)

	return dag, store
}

// runStraddle drives one node through epochs 0 and 1 and settles both. The round-10
// batch (the three straddlers' liveness, every vertex in epoch 0) is committed as its
// own anchor batch BEFORE the round-10 boundary when early is true, or swept into a
// later batch AFTER the boundary when false. That commit-vs-skip timing is the ONLY
// difference between the two nodes; it must not change what either settles.
func runStraddle(t *testing.T, dag *DAG, vals []testValidator, early bool) {
	t.Helper()

	bg0 := addDagVertex(t, dag, vals[0], 5, nil)
	st1 := addDagVertex(t, dag, vals[1], 10, nil)
	st2 := addDagVertex(t, dag, vals[2], 10, nil)
	st3 := addDagVertex(t, dag, vals[3], 10, nil)
	bg1 := addDagVertex(t, dag, vals[0], 15, nil)

	straddle := []Hash{st1, st2, st3}

	dag.commitMu.Lock()
	defer dag.commitMu.Unlock()

	dag.applyBatch(5, []Hash{bg0})
	if early {
		dag.applyBatch(10, straddle) // committed as its own anchor, before the boundary
	}
	dag.transitionEpoch(10)

	// The straddlers stake for epoch 1 onward, so the epoch they land in changes the
	// reward weight of the whole set.
	for _, i := range []int{1, 2, 3} {
		dag.validators.SetSelfStake(vals[i].pubKey, 300)
	}

	if !early {
		dag.applyBatch(12, straddle) // skipped at round 10, swept into a later batch
	}
	dag.applyBatch(15, []Hash{bg1})

	dag.transitionEpoch(20)
	dag.transitionEpoch(30)
}

// rewardSideOf sums the value the reward pool moved to one validator: its reward-coin
// balance plus its effective stake (self + delegated, which absorbs the auto-restake).
func rewardSideOf(t *testing.T, store *mockCoinStore, dag *DAG, i int, pk Hash) uint64 {
	t.Helper()

	coin := coinBalance(t, store, straddleRewardCoin(i))
	stake := EffectiveStake(dag.validators.Get(pk))

	return coin + stake
}

// TestBoundaryStraddleSettlesIdentically proves epoch settlement is insensitive to
// the per-node commit-vs-skip timing of a boundary-straddling round. Two nodes commit
// the SAME round-10 vertices — one as its own anchor batch before the round-10
// boundary, the other in a later batch after it — and must credit that liveness to the
// same epoch and settle byte-identical rewards on every validator. Keying liveness and
// fees by the vertex's round (not the epoch current when its batch commits) and
// deferring settlement one boundary past the epoch's close makes the two paths
// converge; crediting the current epoch and settling at the boundary forks them,
// because the late-committed path lands the liveness in the next epoch's pool.
func TestBoundaryStraddleSettlesIdentically(t *testing.T) {
	vals, _ := newTestValidatorSet(straddleValidators)

	early, earlyStore := newStraddleDAG(t, vals)
	late, lateStore := newStraddleDAG(t, vals)

	runStraddle(t, early, vals, true)
	runStraddle(t, late, vals, false)

	for i, v := range vals {
		earlyValue := rewardSideOf(t, earlyStore, early, i, v.pubKey)
		lateValue := rewardSideOf(t, lateStore, late, i, v.pubKey)

		if earlyValue != lateValue {
			t.Errorf("validator %d reward depends on commit timing: early=%d late=%d",
				i, earlyValue, lateValue)
		}
	}
}

// straddleValidators is the boundary-straddle scenario's validator count: one
// background producer plus the three straddlers the diagnosis observed forking.
const straddleValidators = 4

// TestStraddleCreditsRoundEpochBucket proves both the liveness and fee credits of a
// committed vertex land in the bucket of the epoch its ROUND belongs to, not the epoch
// the node happens to be in when the batch commits. Committing a boundary-round vertex
// (round 10, epoch 0) while the node is already several epochs ahead — the late-sweep
// case that forked the split — must still credit epoch 0's buckets, never the current
// epoch's.
func TestStraddleCreditsRoundEpochBucket(t *testing.T) {
	db := newTestStorage(t)
	vals, vs := newTestValidatorSet(1)
	dag := New(db, vs, nil, testSystemPod, 0, vals[0].privKey, nil, WithEpochLength(10))
	defer dag.Close()
	disableTxAuth(dag)

	store := newMockCoinStore()
	store.SetTotalSupply(1_000_000)
	params := DefaultFeeParams()
	dag.SetFeeSystem(store, &params, nil)

	sender := vals[0].pubKey
	gasCoin := Hash{0xCC}
	store.SetObject(buildTestCoinObject(gasCoin, 100_000, sender, 0))

	// A fee-bearing vertex at the round-10 boundary (commitEpochForRound(10) == 0),
	// swept into a batch long after the node has moved on to epoch 7.
	atx := buildFeeTestATX(t, sender, gasCoin, 500, []uint16{0})
	data := buildTestVertexWithTx(t, vals[0], 10, nil, 0, atx)
	var h Hash
	copy(h[:], types.GetRootAsVertex(data, 0).HashBytes())

	dag.commitMu.Lock()
	dag.currentEpoch = 7
	dag.store.add(data, h, 10, sender)
	dag.applyBatch(10, []Hash{h})
	dag.commitMu.Unlock()

	const roundEpoch = 0 // commitEpochForRound(10) with epochLength 10

	if got := dag.epochRoundsProduced[roundEpoch][sender]; got != 1 {
		t.Fatalf("liveness credited %d to the round's epoch bucket, want 1: %v", got, dag.epochRoundsProduced)
	}
	if got := len(dag.epochRoundsProduced[dag.currentEpoch]); got != 0 {
		t.Fatalf("liveness leaked into the current epoch's bucket: %v", dag.epochRoundsProduced[dag.currentEpoch])
	}
	if dag.epochFees[roundEpoch] == 0 {
		t.Fatalf("fee not pooled into the round's epoch bucket: %v", dag.epochFees)
	}
	if got := dag.epochFees[dag.currentEpoch]; got != 0 {
		t.Fatalf("fee leaked into the current epoch's bucket: %d", got)
	}
}
