package genesis

import (
	"encoding/binary"
	"testing"

	"BluePods/internal/types"
)

// TestBuildInitialState_CoinAndStake confirms the coin holds InitialMint minus
// the staked portion, the founder's self-stake is recorded, supply equals the
// full mint, and the coin is a singleton owned by the founder.
func TestBuildInitialState_CoinAndStake(t *testing.T) {
	var owner [32]byte
	owner[0] = 0x07

	cfg := Config{InitialMint: 1000, GenesisStake: 300, QUICAddress: "quic://x:1"}
	is := BuildInitialState(cfg, owner)

	if is.SelfStake != 300 {
		t.Errorf("SelfStake: got %d, want 300", is.SelfStake)
	}

	if is.Supply != 1000 {
		t.Errorf("Supply: got %d, want 1000", is.Supply)
	}

	if is.CoinID != GenesisCoinID(owner) {
		t.Error("CoinID does not match deterministic derivation")
	}

	obj := types.GetRootAsObject(is.Coin, 0)

	balance := binary.LittleEndian.Uint64(obj.ContentBytes()[:8])
	if balance != 700 {
		t.Errorf("coin balance: got %d, want 700", balance)
	}

	if obj.Replication() != 0 {
		t.Errorf("coin should be a singleton (replication 0), got %d", obj.Replication())
	}

	if string(obj.OwnerBytes()) != string(owner[:]) {
		t.Error("coin owner is not the founder")
	}
}

// TestBuildInitialState_ClampsStake confirms GenesisStake greater than
// InitialMint is clamped so the coin balance never underflows.
func TestBuildInitialState_ClampsStake(t *testing.T) {
	var owner [32]byte
	owner[0] = 0x09

	cfg := Config{InitialMint: 500, GenesisStake: 9000}
	is := BuildInitialState(cfg, owner)

	if is.SelfStake != 500 {
		t.Errorf("SelfStake should clamp to InitialMint: got %d, want 500", is.SelfStake)
	}

	obj := types.GetRootAsObject(is.Coin, 0)
	balance := binary.LittleEndian.Uint64(obj.ContentBytes()[:8])
	if balance != 0 {
		t.Errorf("clamped coin balance: got %d, want 0", balance)
	}
}
