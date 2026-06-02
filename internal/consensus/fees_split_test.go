package consensus

import (
	"testing"

	"BluePods/internal/types"
)

// TestCalculateTxFeeSplit_SeparatesStorage confirms calculateTxFeeSplit returns
// the storage component matching StorageDeposit over the created objects and a
// consumed component equal to the full fee minus that storage.
func TestCalculateTxFeeSplit_SeparatesStorage(t *testing.T) {
	db := newTestStorage(t)
	validators, vs := newTestValidatorSet(4)
	mock := &mockBroadcaster{}

	dag := New(db, vs, mock, testSystemPod, 0, validators[0].privKey, nil)
	defer dag.Close()

	coinStore := newMockCoinStore()
	params := DefaultFeeParams()
	dag.SetFeeSystem(coinStore, &params, nil)

	sender := Hash{0x01}
	gasCoinID := Hash{0xCC}
	coinStore.SetObject(buildTestCoinObject(gasCoinID, 1_000_000, sender, 0))

	// A create tx with one singleton-replication created object.
	atxBytes := buildFeeTestATX(t, sender, gasCoinID, 1000, []uint16{0})
	atx := types.GetRootAsAttestedTransaction(atxBytes, 0)
	tx := atx.Transaction(nil)

	consumed, storage := dag.calculateTxFeeSplit(tx, atx)

	wantStorage := StorageDeposit(0, dag.validators.Len(), params.StorageFee)
	if storage != wantStorage {
		t.Errorf("storage component: got %d, want %d", storage, wantStorage)
	}

	wantTotal := dag.calculateTxFee(tx, atx)
	if consumed+storage != wantTotal {
		t.Errorf("consumed+storage: got %d, want full fee %d", consumed+storage, wantTotal)
	}

	if consumed == 0 {
		t.Error("consumed component should be non-zero for a create tx with max_gas")
	}
}

// TestCalculateTxFeeSplit_StorageTracksLiveCount confirms the storage component
// is computed against the live validator count and stays equal to StorageDeposit
// as the set grows AND shrinks. This is the count-match guarantee: state stamps
// the deposit from the same live count, so debit==stamp across both directions.
func TestCalculateTxFeeSplit_StorageTracksLiveCount(t *testing.T) {
	db := newTestStorage(t)
	validators, vs := newTestValidatorSet(4)
	mock := &mockBroadcaster{}

	dag := New(db, vs, mock, testSystemPod, 0, validators[0].privKey, nil)
	defer dag.Close()

	params := DefaultFeeParams()
	dag.SetFeeSystem(newMockCoinStore(), &params, nil)

	sender := Hash{0x01}
	gasCoinID := Hash{0xCC}
	atxBytes := buildFeeTestATX(t, sender, gasCoinID, 1000, []uint16{2})
	atx := types.GetRootAsAttestedTransaction(atxBytes, 0)
	tx := atx.Transaction(nil)

	assertStorageMatchesLiveCount := func(label string) {
		_, storage := dag.calculateTxFeeSplit(tx, atx)
		want := StorageDeposit(2, dag.validators.Len(), params.StorageFee)
		if storage != want {
			t.Errorf("%s: storage %d != StorageDeposit at len %d (%d)",
				label, storage, dag.validators.Len(), want)
		}
	}

	assertStorageMatchesLiveCount("initial 4 validators")

	// Grow the set.
	dag.validators.Add(Hash{0xA1}, "quic://a:1", [48]byte{})
	dag.validators.Add(Hash{0xA2}, "quic://a:2", [48]byte{})
	assertStorageMatchesLiveCount("grown to 6 validators")

	// Shrink the set below the original size.
	dag.validators.Remove(Hash{0xA1})
	dag.validators.Remove(Hash{0xA2})
	dag.validators.Remove(validators[3].pubKey)
	assertStorageMatchesLiveCount("shrunk to 3 validators")
}

// TestDeductFees_PoolsConsumedLocksStorage confirms deductFees debits
// consumed+storage from the gas coin but the returned split pools only the
// consumed portion; the storage portion is locked, never pooled.
func TestDeductFees_PoolsConsumedLocksStorage(t *testing.T) {
	db := newTestStorage(t)
	validators, vs := newTestValidatorSet(4)
	mock := &mockBroadcaster{}

	dag := New(db, vs, mock, testSystemPod, 0, validators[0].privKey, nil)
	defer dag.Close()

	coinStore := newMockCoinStore()
	params := DefaultFeeParams()
	dag.SetFeeSystem(coinStore, &params, nil)

	sender := Hash{0x01}
	gasCoinID := Hash{0xCC}
	const startBalance = 1_000_000
	coinStore.SetObject(buildTestCoinObject(gasCoinID, startBalance, sender, 0))

	atxBytes := buildFeeTestATX(t, sender, gasCoinID, 1000, []uint16{0})
	atx := types.GetRootAsAttestedTransaction(atxBytes, 0)
	tx := atx.Transaction(nil)

	consumed, storage := dag.calculateTxFeeSplit(tx, atx)

	split, proceed := dag.deductFees(tx, atx, validators[0].pubKey)
	if !proceed {
		t.Fatal("deductFees should proceed for a funded gas coin")
	}

	// Pool (epoch) reflects only the consumed portion (BurnBPS default split).
	wantEpoch := SplitFee(consumed, params).Epoch
	if split.Epoch != wantEpoch {
		t.Errorf("pooled epoch: got %d, want %d (consumed only)", split.Epoch, wantEpoch)
	}

	// The coin was debited by the full consumed+storage.
	balance, _ := readCoinBalance(coinStore.GetObject(gasCoinID))
	if want := uint64(startBalance) - (consumed + storage); balance != want {
		t.Errorf("coin balance: got %d, want %d (debited consumed+storage)", balance, want)
	}
}
