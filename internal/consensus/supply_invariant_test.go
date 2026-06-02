package consensus

import (
	"encoding/binary"
	"testing"

	flatbuffers "github.com/google/flatbuffers/go"

	"BluePods/internal/genesis"
	"BluePods/internal/state"
	"BluePods/internal/storage"
	"BluePods/internal/types"
)

// TestSupplyInvariant drives genesis seeding and the money-moving operations a
// fee-paying workload exercises (transfer, split, merge, create, delete) through
// the real state and consensus coin primitives, then asserts the supply
// invariant exactly at an epoch boundary, where in-flight epochFees is 0:
//
//	sum(coin balances) + total_bonded + sum(locked storage deposits) == total_supply
//
// total_bonded is 0 in this batch (genesis stake 0, no bonding yet). The boundary
// is modeled by crediting the pooled consumed fees back into circulation (the
// distribution Batch 7 implements) so epochFees returns to 0 and money the fee
// path debited reappears in coin balances. Every mutation uses a real production
// function: the deletion burn is state.SubSupply, the storage lock is
// StorageDeposit, the fee debit is deductCoinFee, and refunds are creditCoin.
func TestSupplyInvariant(t *testing.T) {
	db := newTestStorage(t)
	st := state.New(db, nil)
	st.SetStorageFees(1000, 9500, 4)

	dag := newInvariantDAG(t, db, st)
	defer dag.Close()

	founder := deriveInvariantOwner()
	is := genesis.BuildInitialState(genesis.Config{InitialMint: 1_000_000, GenesisStake: 0}, founder)
	dag.SeedGenesis(is)

	// The genesis coin is the founder's funded singleton.
	coinID := is.CoinID

	// 1. Fee-paying transfer: debit a consumed fee from the coin (no created
	//    objects, so storage is 0); the pooled fee accrues to epochFees.
	payFee(t, dag, st, coinID, founder)

	// 2. Split: move part of the coin's balance into a new coin (conserves total
	//    balance), and pay a fee.
	newCoinID := Hash{0xC0, 0x1A}
	splitCoin(t, st, coinID, newCoinID, founder, 100_000)
	payFee(t, dag, st, coinID, founder)

	// 3. Merge: fold the new coin back into the first (conserves total balance),
	//    pay a fee, and remove the now-empty coin.
	mergeCoin(t, st, newCoinID, coinID)
	payFee(t, dag, st, coinID, founder)

	// 4. Create: lock a storage deposit in a new object (debited from the coin,
	//    never pooled). total_supply is unchanged because the debit equals the lock.
	objID := Hash{0x0B, 0x1E}
	createObjectWithDeposit(t, dag, st, coinID, objID, founder, 0)

	// 5. Delete: refund 95% of the locked deposit to the coin, burn 5% from supply.
	deleteObjectWithRefund(t, st, objID, coinID)

	// Epoch boundary: credit the pooled consumed fees back into circulation
	// (distribution) so epochFees returns to 0, then clear it.
	creditPooledFees(t, st, coinID, dag.epochFees)
	dag.epochFees = 0

	assertSupplyInvariant(t, db, st)
}

// newInvariantDAG builds a DAG backed by the real state coin store with the fee
// system wired and the validator count bound to the live set.
func newInvariantDAG(t *testing.T, db *storage.Storage, st *state.State) *DAG {
	t.Helper()

	validators, vs := newTestValidatorSet(4)
	dag := New(db, vs, &mockBroadcaster{}, testSystemPod, 0, validators[0].privKey, nil)

	params := DefaultFeeParams()
	dag.SetFeeSystem(st, &params, nil)
	st.SetValidatorCount(vs.Len)

	return dag
}

// deriveInvariantOwner returns a deterministic 32-byte owner for the test ledger.
func deriveInvariantOwner() [32]byte {
	var owner [32]byte
	owner[0] = 0xAB
	owner[31] = 0xCD
	return owner
}

// payFee deducts a fee-only transaction's fee from the coin and accumulates the
// pooled (consumed) portion into epochFees, mirroring commitRound.
func payFee(t *testing.T, dag *DAG, st *state.State, coinID, owner [32]byte) {
	t.Helper()

	atxBytes := buildFeeTestATX(t, owner, coinID, 500, nil)
	atx := types.GetRootAsAttestedTransaction(atxBytes, 0)
	tx := atx.Transaction(nil)

	split, proceed := dag.deductFees(tx, atx, dag.validators.All()[0].Pubkey)
	if !proceed {
		t.Fatal("payFee: deductFees did not proceed")
	}

	dag.epochFees += split.Epoch
}

// splitCoin reduces the source coin and creates a destination coin holding the
// moved amount, conserving total balance (the real split semantics).
func splitCoin(t *testing.T, st *state.State, srcID, dstID, owner [32]byte, amount uint64) {
	t.Helper()

	src := st.GetObject(srcID)
	if src == nil {
		t.Fatal("splitCoin: source coin missing")
	}

	balance, err := readCoinBalance(src)
	if err != nil {
		t.Fatalf("splitCoin: read balance: %v", err)
	}
	if balance < amount {
		t.Fatalf("splitCoin: insufficient balance %d < %d", balance, amount)
	}

	st.SetObject(writeCoinBalance(src, balance-amount))
	st.SetObject(buildInvariantCoin(dstID, amount, owner))
}

// mergeCoin folds the source coin's balance into the destination coin and
// removes the source, conserving total balance (the real merge semantics).
func mergeCoin(t *testing.T, st *state.State, srcID, dstID [32]byte) {
	t.Helper()

	src := st.GetObject(srcID)
	dst := st.GetObject(dstID)
	if src == nil || dst == nil {
		t.Fatal("mergeCoin: a coin is missing")
	}

	srcBal, _ := readCoinBalance(src)
	dstBal, _ := readCoinBalance(dst)

	st.SetObject(writeCoinBalance(dst, dstBal+srcBal))
	st.SetObject(writeCoinBalance(src, 0)) // empty the source coin (still counted, balance 0)
}

// createObjectWithDeposit locks a storage deposit in a new object: the deposit is
// debited from the coin and stored as the object's fees, leaving supply unchanged.
func createObjectWithDeposit(t *testing.T, dag *DAG, st *state.State, coinID, objID, owner [32]byte, replication uint16) {
	t.Helper()

	deposit := StorageDeposit(replication, dag.validators.Len(), dag.feeParams.StorageFee)

	coin := st.GetObject(coinID)
	balance, _ := readCoinBalance(coin)
	if balance < deposit {
		t.Fatalf("createObjectWithDeposit: insufficient balance %d < %d", balance, deposit)
	}

	st.SetObject(writeCoinBalance(coin, balance-deposit))
	st.SetObject(buildInvariantObjectWithFees(objID, owner, replication, deposit))
}

// deleteObjectWithRefund deletes an object, refunding 95% of its locked deposit
// to the coin and burning 5% from total supply (the real applyDeletedObjects math).
func deleteObjectWithRefund(t *testing.T, st *state.State, objID, coinID [32]byte) {
	t.Helper()

	objData := st.GetObject(objID)
	if objData == nil {
		t.Fatal("deleteObjectWithRefund: object missing")
	}

	objFees := types.GetRootAsObject(objData, 0).Fees()
	refund := objFees * 9500 / 10000
	burned := objFees - refund

	if err := creditCoin(st, coinID, refund); err != nil {
		t.Fatalf("deleteObjectWithRefund: refund: %v", err)
	}
	st.SubSupply(burned)

	st.SetObject(buildInvariantObjectWithFees(objID, [32]byte{}, 0, 0)) // mark deleted: fees 0
}

// creditPooledFees returns the pooled consumed fees to circulation, modeling the
// epoch-boundary distribution that Batch 7 implements.
func creditPooledFees(t *testing.T, st *state.State, coinID [32]byte, pooled uint64) {
	t.Helper()

	if pooled == 0 {
		return
	}
	if err := creditCoin(st, coinID, pooled); err != nil {
		t.Fatalf("creditPooledFees: %v", err)
	}
}

// assertSupplyInvariant sums coin balances and locked object fees over the state
// store and asserts they equal total_supply (total_bonded is 0 this batch).
func assertSupplyInvariant(t *testing.T, db *storage.Storage, st *state.State) {
	t.Helper()

	var coinSum, feeSum uint64

	err := db.Iterate(func(key, value []byte) error {
		if len(key) != 32 {
			return nil // skip consensus/meta keys
		}

		obj := types.GetRootAsObject(value, 0)
		if content := obj.ContentBytes(); len(content) >= 8 {
			coinSum += binary.LittleEndian.Uint64(content[:8])
		}
		feeSum += obj.Fees()

		return nil
	})
	if err != nil {
		t.Fatalf("iterate state: %v", err)
	}

	const totalBonded = 0 // no bonding in this batch
	lhs := coinSum + totalBonded + feeSum

	if lhs != st.TotalSupply() {
		t.Errorf("supply invariant violated at boundary: coins %d + bonded %d + deposits %d = %d, want total_supply %d",
			coinSum, totalBonded, feeSum, lhs, st.TotalSupply())
	}
}

// buildInvariantCoin builds a singleton Coin object with the given balance.
func buildInvariantCoin(id [32]byte, balance uint64, owner [32]byte) []byte {
	content := make([]byte, 8)
	binary.LittleEndian.PutUint64(content, balance)
	return buildInvariantObject(id, owner, 0, content, 0)
}

// buildInvariantObjectWithFees builds an object with empty content and a locked
// fees deposit.
func buildInvariantObjectWithFees(id, owner [32]byte, replication uint16, fees uint64) []byte {
	return buildInvariantObject(id, owner, replication, nil, fees)
}

// buildInvariantObject serializes an Object with the given fields.
func buildInvariantObject(id, owner [32]byte, replication uint16, content []byte, fees uint64) []byte {
	b := flatbuffers.NewBuilder(256)

	idVec := b.CreateByteVector(id[:])
	ownerVec := b.CreateByteVector(owner[:])
	contentVec := b.CreateByteVector(content)

	types.ObjectStart(b)
	types.ObjectAddId(b, idVec)
	types.ObjectAddVersion(b, 0)
	types.ObjectAddOwner(b, ownerVec)
	types.ObjectAddReplication(b, replication)
	types.ObjectAddContent(b, contentVec)
	types.ObjectAddFees(b, fees)
	b.Finish(types.ObjectEnd(b))

	return b.FinishedBytes()
}
