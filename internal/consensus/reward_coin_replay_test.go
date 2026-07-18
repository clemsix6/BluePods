package consensus

import (
	"testing"

	flatbuffers "github.com/google/flatbuffers/go"

	"BluePods/internal/types"
)

// buildRegisterTxWithGasCoin builds a register_validator transaction that declares a
// gas coin but carries no explicit reward-coin arg. It is the shape whose reward-coin
// designation must be fixed by committed transaction data, never by a live coin-store
// read at the moment the registration is applied.
func buildRegisterTxWithGasCoin(t *testing.T, sender Hash, gasCoin Hash) *types.Transaction {
	t.Helper()

	builder := flatbuffers.NewBuilder(512)

	args := encodeRegisterValidatorArgsBorsh([]byte("quic://join:1"), make([]byte, 48))

	hashVec := builder.CreateByteVector(make([]byte, 32))
	senderVec := builder.CreateByteVector(sender[:])
	podVec := builder.CreateByteVector(testSystemPod[:])
	funcNameOff := builder.CreateString("register_validator")
	argsVec := builder.CreateByteVector(args)
	gasCoinVec := builder.CreateByteVector(gasCoin[:])

	types.TransactionStart(builder)
	types.TransactionAddHash(builder, hashVec)
	types.TransactionAddSender(builder, senderVec)
	types.TransactionAddPod(builder, podVec)
	types.TransactionAddFunctionName(builder, funcNameOff)
	types.TransactionAddArgs(builder, argsVec)
	types.TransactionAddGasCoin(builder, gasCoinVec)
	txOff := types.TransactionEnd(builder)

	builder.Finish(txOff)

	return types.GetRootAsTransaction(builder.FinishedBytes(), 0)
}

// registerReplayDAG builds a DAG wired to store, with an existing committee, ready to
// apply a register_validator transaction through the registration handler.
func registerReplayDAG(t *testing.T, store CoinStore) *DAG {
	t.Helper()

	db := newTestStorage(t)
	validators, vs := newTestValidatorSet(2)

	dag := New(db, vs, nil, testSystemPod, 0, validators[0].privKey, nil)

	params := DefaultFeeParams()
	dag.SetFeeSystem(store, &params, nil)

	return dag
}

// applyRegistrationRewardCoin applies a register_validator tx on a DAG wired to store
// and returns the newly registered validator's designated reward coin.
func applyRegistrationRewardCoin(t *testing.T, sender Hash, gasCoin Hash, store CoinStore) Hash {
	t.Helper()

	dag := registerReplayDAG(t, store)
	defer dag.Close()

	tx := buildRegisterTxWithGasCoin(t, sender, gasCoin)
	dag.handleRegisterValidator(tx, 5)

	info := dag.validators.Get(sender)
	if info == nil {
		t.Fatal("validator should be registered")
	}

	return info.RewardCoin
}

// TestRegisterValidator_RewardCoinDeterministicAcrossReplay proves the reward-coin
// designation is fixed by committed transaction data alone, never by the live
// coin-store contents at the instant the registration is applied. Two nodes commit
// the IDENTICAL registration (a declared gas coin, no explicit reward-coin arg) but
// see different coin stores at apply time: the live node already holds the sender's
// gas coin, the replay node has not yet materialized it. A live-store read designates
// different reward coins on the two nodes; a committed-only designation must agree, so
// the epoch reward lands on the same coin everywhere and the fingerprints stay equal.
func TestRegisterValidator_RewardCoinDeterministicAcrossReplay(t *testing.T) {
	newVal := newTestValidator()
	gasCoin := Hash{0x77}

	// Live node: the sender already funded and owns gasCoin when the registration
	// commits.
	liveStore := newMockCoinStore()
	liveStore.SetObject(buildTestCoinObject(gasCoin, 1000, newVal.pubKey, 0))
	liveCoin := applyRegistrationRewardCoin(t, newVal.pubKey, gasCoin, liveStore)

	// Replay node: same committed registration, but at the moment it replays the gas
	// coin is not yet present in its store (a different local apply-time timing).
	replayStore := newMockCoinStore()
	replayCoin := applyRegistrationRewardCoin(t, newVal.pubKey, gasCoin, replayStore)

	if liveCoin != replayCoin {
		t.Fatalf("reward coin diverged between live and replay: live=%x replay=%x", liveCoin, replayCoin)
	}
}
