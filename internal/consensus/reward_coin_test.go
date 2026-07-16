package consensus

import (
	"testing"

	"BluePods/internal/genesis"
)

// TestSeedGenesis_FounderRewardCoin verifies that SeedGenesis designates the
// founder's own genesis coin as its reward coin. Without this designation the
// founder's liquid epoch reward share has no coin to land in and silently
// vanishes at every boundary with a non-zero pool: Task 3 pools an uncreditable
// leftover forward, but it does not conjure a destination for it, so genesis
// itself must seed one.
func TestSeedGenesis_FounderRewardCoin(t *testing.T) {
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

	info := dag.validators.Get(is.Pubkey)
	if info == nil {
		t.Fatal("founder not present after SeedGenesis")
	}

	want := genesis.GenesisCoinID(founder.pubKey)
	if info.RewardCoin != want {
		t.Errorf("founder RewardCoin = %x, want %x (its own genesis coin)", info.RewardCoin, want)
	}
}
