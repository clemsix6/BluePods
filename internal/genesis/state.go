package genesis

import (
	"encoding/binary"

	flatbuffers "github.com/google/flatbuffers/go"

	"BluePods/internal/types"
)

// InitialState is the genesis ledger state, seeded directly (no transactions).
type InitialState struct {
	Coin      []byte   // Coin is the serialized initial Coin object (a singleton).
	CoinID    [32]byte // CoinID is its deterministic object ID.
	Pubkey    [32]byte // Pubkey is the founding validator's Ed25519 key.
	QUIC      string   // QUIC is the founding validator's QUIC address.
	BLS       []byte   // BLS is the founding validator's 48-byte BLS key.
	SelfStake uint64   // SelfStake is the founder's bonded stake, locked from the mint.
	Supply    uint64   // Supply is the initial total supply (== InitialMint).
}

// BuildInitialState constructs the genesis state for the bootstrap owner. The
// staked portion is locked out of the coin so the supply invariant holds.
func BuildInitialState(cfg Config, owner [32]byte) InitialState {
	coinID := GenesisCoinID(owner)

	stake := cfg.GenesisStake
	if stake > cfg.InitialMint {
		stake = cfg.InitialMint // clamp: never underflow the coin balance
	}

	coin := buildGenesisCoin(coinID, owner, cfg.InitialMint-stake)

	return InitialState{
		Coin:      coin,
		CoinID:    coinID,
		Pubkey:    owner,
		QUIC:      cfg.QUICAddress,
		BLS:       cfg.BLSPubkey,
		SelfStake: stake,
		Supply:    cfg.InitialMint,
	}
}

// buildGenesisCoin serializes a singleton Coin object owned by owner with the
// given little-endian balance and no locked storage deposit.
func buildGenesisCoin(coinID, owner [32]byte, balance uint64) []byte {
	content := make([]byte, 8)
	binary.LittleEndian.PutUint64(content, balance)

	b := flatbuffers.NewBuilder(256)
	idVec := b.CreateByteVector(coinID[:])
	ownerVec := b.CreateByteVector(owner[:])
	contentVec := b.CreateByteVector(content)

	types.ObjectStart(b)
	types.ObjectAddId(b, idVec)
	types.ObjectAddVersion(b, 0)
	types.ObjectAddOwner(b, ownerVec)
	types.ObjectAddReplication(b, 0)
	types.ObjectAddContent(b, contentVec)
	types.ObjectAddFees(b, 0)
	b.Finish(types.ObjectEnd(b))

	return b.FinishedBytes()
}
