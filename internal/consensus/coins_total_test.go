package consensus

import "testing"

// TestDeductCoinFee_LowersCoinsTotal_FullyCovered verifies that a fully covered
// fee deduction lowers CoinsTotal by exactly the fee, mirroring the balance
// leaving the coin.
func TestDeductCoinFee_LowersCoinsTotal_FullyCovered(t *testing.T) {
	store := newMockCoinStore()
	coinID := Hash{0x01}
	store.SetObject(buildTestCoinObject(coinID, 1000, Hash{0xAA}, 0))
	store.SetCoinsTotal(1000)

	deducted, fullyCovered, err := deductCoinFee(store, coinID, 300)
	if err != nil {
		t.Fatalf("deductCoinFee: %v", err)
	}
	if !fullyCovered || deducted != 300 {
		t.Fatalf("deducted=%d fullyCovered=%v, want 300/true", deducted, fullyCovered)
	}

	if got := store.CoinsTotal(); got != 700 {
		t.Errorf("CoinsTotal after fully-covered deduction = %d, want 700", got)
	}
}

// TestDeductCoinFee_LowersCoinsTotal_Partial verifies that a partially covered
// fee deduction lowers CoinsTotal by only the amount actually taken (the
// drained balance), not the requested fee.
func TestDeductCoinFee_LowersCoinsTotal_Partial(t *testing.T) {
	store := newMockCoinStore()
	coinID := Hash{0x01}
	store.SetObject(buildTestCoinObject(coinID, 10, Hash{0xAA}, 0))
	store.SetCoinsTotal(10)

	deducted, fullyCovered, err := deductCoinFee(store, coinID, 300)
	if err != nil {
		t.Fatalf("deductCoinFee: %v", err)
	}
	if fullyCovered || deducted != 10 {
		t.Fatalf("deducted=%d fullyCovered=%v, want 10/false", deducted, fullyCovered)
	}

	if got := store.CoinsTotal(); got != 0 {
		t.Errorf("CoinsTotal after partial deduction = %d, want 0 (only the drained balance left)", got)
	}
}

// TestCreditCoin_RaisesCoinsTotal verifies that crediting a coin raises
// CoinsTotal by exactly the credited amount.
func TestCreditCoin_RaisesCoinsTotal(t *testing.T) {
	store := newMockCoinStore()
	coinID := Hash{0x01}
	store.SetObject(buildTestCoinObject(coinID, 100, Hash{0xAA}, 0))
	store.SetCoinsTotal(100)

	if err := creditCoin(store, coinID, 50); err != nil {
		t.Fatalf("creditCoin: %v", err)
	}

	if got := store.CoinsTotal(); got != 150 {
		t.Errorf("CoinsTotal after credit = %d, want 150", got)
	}
}

// TestStrictDebit_LowersCoinsTotal verifies that a bond's strict debit lowers
// CoinsTotal by exactly the bonded amount: the value left coins to become
// bonded stake, so it must leave the coin-accounting term too.
func TestStrictDebit_LowersCoinsTotal(t *testing.T) {
	dag, store, sender, coinID := bondTestDAG(t, 1000)
	defer dag.Close()
	store.SetCoinsTotal(1000)

	tx := buildStakeTx(t, sender, coinID, "bond", 300)
	if !dag.handleBond(tx) {
		t.Fatal("bond should be applied")
	}

	if got := store.CoinsTotal(); got != 700 {
		t.Errorf("CoinsTotal after bond = %d, want 700", got)
	}
}

// TestBondUnbondCycle_CoinsTotalReturnsToStart verifies that a bond followed by
// an unbond of the same amount leaves CoinsTotal exactly where it started: the
// value left coins into stake and came straight back, with nothing gained or
// lost along the way.
func TestBondUnbondCycle_CoinsTotalReturnsToStart(t *testing.T) {
	dag, store, sender, coinID := bondTestDAG(t, 1000)
	defer dag.Close()
	store.SetCoinsTotal(1000)

	bondTx := buildStakeTx(t, sender, coinID, "bond", 300)
	if !dag.handleBond(bondTx) {
		t.Fatal("bond should be applied")
	}
	if got := store.CoinsTotal(); got != 700 {
		t.Fatalf("CoinsTotal after bond = %d, want 700", got)
	}

	unbondTx := buildStakeTx(t, sender, coinID, "unbond", 300)
	if !dag.handleUnbond(unbondTx) {
		t.Fatal("unbond should be applied")
	}

	if got := store.CoinsTotal(); got != 1000 {
		t.Errorf("CoinsTotal after bond+unbond cycle = %d, want 1000 (back to start)", got)
	}
}
