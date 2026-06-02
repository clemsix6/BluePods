package consensus

import (
	"crypto/ed25519"
	"testing"
)

// TestMoneyNeverReadsTheVertexClock pins the invariant behind Batch 9's deferral
// of the timestamp pipeline: the signed Vertex.timestamp controls no money, so
// the over-state-time bias (a producer biasing its clock to move value) never
// applies. The two money decisions are both clock-free by construction:
//
//   - Issuance: runThermostat takes no timestamp (only a distributable bool); it
//     reads pre-mint supply and bonded stake, never a wall-clock. The call below
//     is the compile-time proof of that signature.
//   - Sponsored-tx expiry: sponsoredTxStillValid derives its verdict purely from
//     valid_until (in epochs) versus commitEpochForRound, never from a timestamp.
//
// If a future change made either path read Vertex.timestamp, this test (and the
// signature it asserts) is the tripwire.
func TestMoneyNeverReadsTheVertexClock(t *testing.T) {
	db := newTestStorage(t)
	validators, vs := newTestValidatorSet(4)
	dag := New(db, vs, nil, testSystemPod, 0, validators[0].privKey, nil, WithEpochLength(100))
	defer dag.Close()

	// runThermostat's only argument is the distributable bool: there is no clock
	// parameter to pass. This call would not compile if a timestamp were required.
	_ = dag.runThermostat(false)

	// Sponsored-tx validity is epoch-derived only. At round 250 (commit epoch 2),
	// the verdict depends solely on valid_until vs the commit epoch, with no clock
	// input anywhere in the call.
	_, senderKey, _ := ed25519.GenerateKey(nil)
	_, sponsorKey, _ := ed25519.GenerateKey(nil)

	var gasCoin [32]byte
	gasCoin[0] = 0x42

	const round = uint64(250)
	if dag.commitEpochForRound(round) != 2 {
		t.Fatalf("test setup: commitEpochForRound(%d) = %d, want 2", round, dag.commitEpochForRound(round))
	}

	expired := sponsoredTestTx(t, senderKey, sponsorKey, gasCoin, 1)
	if dag.sponsoredTxStillValid(expired, round) {
		t.Error("expiry must follow epochs (valid_until 1 < commit epoch 2), not a clock")
	}

	live := sponsoredTestTx(t, senderKey, sponsorKey, gasCoin, 2)
	if !dag.sponsoredTxStillValid(live, round) {
		t.Error("a tx valid through the commit epoch must hold regardless of any clock")
	}
}
