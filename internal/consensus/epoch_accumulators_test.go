package consensus

import (
	"os"
	"reflect"
	"testing"

	"BluePods/internal/storage"
)

// seedRewardCoins gives each validator a zero-balance singleton reward coin in the
// coin store and designates it, so distributeEpochRewards credits the liquid reward
// somewhere and the whole pool lands in coins (AutoRestake is 0 with no thermostat).
func seedRewardCoins(dag *DAG, coinStore *mockCoinStore, vals []testValidator) map[Hash][32]byte {
	coins := make(map[Hash][32]byte, len(vals))

	for i, v := range vals {
		var coinID [32]byte
		coinID[0] = 0xC0
		coinID[1] = byte(i + 1)

		coinStore.SetObject(buildTestCoinObject(coinID, 0, v.pubKey, 0))
		dag.validators.SetRewardCoin(v.pubKey, coinID)
		coins[v.pubKey] = coinID
	}

	return coins
}

// sumRewardBalances totals the reward coins' balances, to measure how much a
// distribution credited.
func sumRewardBalances(coinStore *mockCoinStore, coins map[Hash][32]byte) uint64 {
	var total uint64
	for _, id := range coins {
		data := coinStore.GetObject(id)
		if data == nil {
			continue
		}
		bal, _ := readCoinBalance(data)
		total += bal
	}
	return total
}

// TestSettlementAccumulatorsSurviveRestart is the C-2 restart regression WITH FEES
// WIRED (the existing restart tests use feeParams==nil, so distributeEpochRewards
// no-ops and never exercises the accumulators). A node accrues mid-epoch fees and
// liveness, persists them on the cursor batch, crashes, and reopens. The accumulators
// must be restored exactly, and a boundary reward mint on the reopened node must
// credit the full restored pool — not near-zero, which would fork the mint.
func TestSettlementAccumulatorsSurviveRestart(t *testing.T) {
	vals, _ := seededValidatorSet(4)

	dir, err := os.MkdirTemp("", "consensus_accumulators_restart_*")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	defer os.RemoveAll(dir)

	const feePool uint64 = 9000

	// Pre-crash node: accrue fees + liveness, persist on the cursor batch, crash.
	db1, err := storage.New(dir)
	if err != nil {
		t.Fatalf("open storage: %v", err)
	}

	_, vs1 := reuseSet(vals)
	dag1 := New(db1, vs1, nil, testSystemPod, 0, vals[0].privKey, nil, WithListenerMode())
	setEqualStake(dag1, vals, 25)

	dag1.commitMu.Lock()
	dag1.lastCommitted = 3
	dag1.epochFees = map[uint64]uint64{0: feePool}
	dag1.epochRoundsProduced = map[uint64]map[Hash]uint64{
		0: {vals[0].pubKey: 3, vals[1].pubKey: 2, vals[2].pubKey: 1, vals[3].pubKey: 1},
	}
	dag1.epochAdditions = []Hash{vals[2].pubKey}
	dag1.pendingRemovals = map[Hash]bool{vals[3].pubKey: true}
	dag1.store.saveCommitCursorBatch(dag1.lastCommitted, dag1.accumulatorKVs())
	wantProduced := dag1.epochRoundsProduced
	dag1.commitMu.Unlock()

	dag1.Close()
	db1.Close()

	// Reopen: the accumulators are restored from disk at construction.
	db2, err := storage.New(dir)
	if err != nil {
		t.Fatalf("reopen storage: %v", err)
	}
	defer db2.Close()

	_, vs2 := reuseSet(vals)
	dag2 := New(db2, vs2, nil, testSystemPod, 0, vals[0].privKey, nil, WithListenerMode())
	defer dag2.Close()
	setEqualStake(dag2, vals, 25)

	if dag2.epochFees[0] != feePool {
		t.Fatalf("epochFees not restored: got %d, want %d", dag2.epochFees[0], feePool)
	}
	if !reflect.DeepEqual(dag2.epochRoundsProduced, wantProduced) {
		t.Fatalf("epochRoundsProduced not restored: got %v, want %v", dag2.epochRoundsProduced, wantProduced)
	}
	if len(dag2.epochAdditions) != 1 || dag2.epochAdditions[0] != vals[2].pubKey {
		t.Fatalf("epochAdditions not restored: got %v", dag2.epochAdditions)
	}
	if !dag2.pendingRemovals[vals[3].pubKey] {
		t.Fatalf("pendingRemovals not restored: got %v", dag2.pendingRemovals)
	}

	// Fees WIRED: the reward mint on the reopened node credits the full restored pool.
	coinStore := newMockCoinStore()
	params := DefaultFeeParams()
	dag2.SetFeeSystem(coinStore, &params, nil)
	coins := seedRewardCoins(dag2, coinStore, vals)

	dag2.commitMu.Lock()
	dag2.distributeEpochRewards(0, 0)
	dag2.commitMu.Unlock()

	credited := sumRewardBalances(coinStore, coins)
	if credited != feePool {
		t.Fatalf("restored fee pool not fully minted: credited %d, want %d", credited, feePool)
	}
}

// TestSyncBlobCarriesSettlementAccumulators is the C-2 mid-epoch-join regression WITH
// FEES WIRED. A source accrues mid-epoch fees and liveness; a joiner installs the sync
// regime blob and must reconstruct the identical accumulators and mint the full pool
// at the boundary. A joiner WITHOUT the blob reaches the boundary with a zero pool —
// the deterministic fork the carried accumulators prevent.
func TestSyncBlobCarriesSettlementAccumulators(t *testing.T) {
	vals, vs := newTestValidatorSet(4)
	pubkeys := keysOf(vals)

	const feePool uint64 = 12000

	src := New(newTestStorage(t), vs, nil, testSystemPod, 0, vals[0].privKey, nil)
	t.Cleanup(func() { src.Close() })
	setEqualStake(src, vals, 25)

	src.commitMu.Lock()
	src.epochFees = map[uint64]uint64{0: feePool}
	src.epochRoundsProduced = map[uint64]map[Hash]uint64{
		0: {vals[0].pubKey: 4, vals[1].pubKey: 3, vals[2].pubKey: 2, vals[3].pubKey: 1},
	}
	wantProduced := src.epochRoundsProduced
	src.commitMu.Unlock()

	_, blob := src.ExportRegimeState()

	// Joiner installs the blob: accumulators carried, epochs resolved.
	joiner := New(newTestStorage(t), NewValidatorSet(pubkeys), nil, testSystemPod, 0, vals[0].privKey, nil,
		WithSyncedRegimeState(blob))
	t.Cleanup(func() { joiner.Close() })

	if joiner.epochFees[0] != feePool {
		t.Fatalf("joiner epochFees not carried: got %d, want %d", joiner.epochFees[0], feePool)
	}
	if !reflect.DeepEqual(joiner.epochRoundsProduced, wantProduced) {
		t.Fatalf("joiner epochRoundsProduced not carried: got %v, want %v", joiner.epochRoundsProduced, wantProduced)
	}

	// Fees WIRED: the joiner mints the full carried pool at the boundary.
	coinStore := newMockCoinStore()
	params := DefaultFeeParams()
	joiner.SetFeeSystem(coinStore, &params, nil)
	for _, v := range vals {
		joiner.validators.SetSelfStake(v.pubKey, 25)
	}
	coins := seedRewardCoins(joiner, coinStore, vals)

	joiner.commitMu.Lock()
	joiner.distributeEpochRewards(0, 0)
	joiner.commitMu.Unlock()

	if credited := sumRewardBalances(coinStore, coins); credited != feePool {
		t.Fatalf("joiner did not mint the carried fee pool: credited %d, want %d", credited, feePool)
	}

	// Negative control: a bare joiner without the blob reaches the boundary with a
	// zero pool — exactly the near-zero fork the carried accumulators prevent.
	bare := New(newTestStorage(t), NewValidatorSet(pubkeys), nil, testSystemPod, 0, vals[0].privKey, nil)
	t.Cleanup(func() { bare.Close() })
	if bare.totalEpochFees() != 0 {
		t.Fatalf("bare joiner unexpectedly has fees: %d", bare.totalEpochFees())
	}
}
