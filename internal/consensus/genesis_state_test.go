package consensus

import (
	"testing"

	"BluePods/internal/genesis"
	"BluePods/internal/types"
)

// TestSeedGenesis_SeedsCoinAndValidator confirms SeedGenesis seeds the genesis
// coin into the coin store and the founding validator (with its self-stake) into
// the validator set, without injecting any pending transactions.
func TestSeedGenesis_SeedsCoinAndValidator(t *testing.T) {
	db := newTestStorage(t)
	founder := newTestValidator()
	vs := NewValidatorSet([]Hash{founder.pubKey})
	mock := &mockBroadcaster{}

	dag := New(db, vs, mock, testSystemPod, 0, founder.privKey, nil)
	defer dag.Close()

	store := newMockCoinStore()
	params := DefaultFeeParams()
	dag.SetFeeSystem(store, &params, nil)

	cfg := genesis.Config{InitialMint: 1000, GenesisStake: 300, QUICAddress: "quic://x:1"}
	is := genesis.BuildInitialState(cfg, founder.pubKey)

	dag.SeedGenesis(is)

	coin := store.GetObject(is.CoinID)
	if coin == nil {
		t.Fatal("genesis coin not seeded into the coin store")
	}

	balance, err := readCoinBalance(coin)
	if err != nil {
		t.Fatalf("read seeded coin balance: %v", err)
	}

	if want := is.Supply - is.SelfStake; balance != want {
		t.Errorf("seeded coin balance: got %d, want %d", balance, want)
	}

	info := dag.validators.Get(is.Pubkey)
	if info == nil {
		t.Fatal("founding validator not present after SeedGenesis")
	}

	if info.SelfStake != is.SelfStake {
		t.Errorf("founder SelfStake: got %d, want %d", info.SelfStake, is.SelfStake)
	}

	if len(dag.pendingTxs) != 0 {
		t.Errorf("SeedGenesis injected %d pending txs, want 0", len(dag.pendingTxs))
	}

	// The coin must be a singleton owned by the founder.
	obj := types.GetRootAsObject(coin, 0)
	if obj.Replication() != 0 {
		t.Errorf("seeded coin replication: got %d, want 0", obj.Replication())
	}

	// Total supply is seeded to the initial mint.
	if got := store.TotalSupply(); got != is.Supply {
		t.Errorf("seeded total supply: got %d, want %d", got, is.Supply)
	}

	// coins_total is seeded to the coin's own balance, NOT the total supply: the
	// founder's self-stake is locked out of the coin, so it is bonded, not in a
	// coin balance.
	if got := store.CoinsTotal(); got != balance {
		t.Errorf("seeded coins_total: got %d, want %d (the coin's balance)", got, balance)
	}

	// At genesis the invariant holds: coin + self-stake + deposits(0) == supply.
	if balance+is.SelfStake != store.TotalSupply() {
		t.Errorf("genesis invariant: coin %d + stake %d != supply %d",
			balance, is.SelfStake, store.TotalSupply())
	}
}
