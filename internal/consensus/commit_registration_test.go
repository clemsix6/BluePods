package consensus

import (
	"testing"

	"BluePods/internal/types"
)

// TestHandleRegisterValidator_EpochAdditionsUniformAcrossSelfAdd reproduces
// test/BUGS.md entry 1: a node that optimistically self-adds its own
// registration to the LIVE validator set (mirroring cmd/node/registration.go
// selfAddToValidatorSet -> AddValidator, called before the registration it
// just submitted ever commits) must still end up with the same epochAdditions
// bookkeeping as every other node once the identical committed
// register_validator transaction replays through handleRegisterValidator.
// Before the fix, the self-adding node's local validators.Add call returns
// isNew=false for that same transaction (its key is already in the live set),
// while every other node sees isNew=true — isNew gates the epochAdditions
// append, so the two nodes disagree on epochAdditions contents, and the
// fingerprint hashes epochAdditions verbatim.
func TestHandleRegisterValidator_EpochAdditionsUniformAcrossSelfAdd(t *testing.T) {
	founder := newTestValidator()
	newVal := newTestValidator()

	dagSelf := New(newTestStorage(t), NewValidatorSet([]Hash{founder.pubKey}), nil, testSystemPod, 0, founder.privKey, nil,
		WithEpochLength(100))
	defer dagSelf.Close()

	dagOther := New(newTestStorage(t), NewValidatorSet([]Hash{founder.pubKey}), nil, testSystemPod, 0, founder.privKey, nil,
		WithEpochLength(100))
	defer dagOther.Close()

	// Optimistic self-add, dagSelf ONLY, exactly what selfAddToValidatorSet does
	// before the registration transaction it just submitted ever commits.
	dagSelf.AddValidator(newVal.pubKey, "quic://new:9090", [48]byte{0xAA})

	// The identical committed register_validator transaction reaches every
	// node, dagSelf included, through the normal commit path.
	atxBytes := buildRegisterATX(t, newVal.pubKey, testSystemPod, "quic://new:9090", [48]byte{0xAA})
	tx := types.GetRootAsAttestedTransaction(atxBytes, 0).Transaction(nil)

	dagSelf.handleRegisterValidator(tx, 5)
	dagOther.handleRegisterValidator(tx, 5)

	t.Logf("dagSelf  epochAdditions: %x", dagSelf.epochAdditions)
	t.Logf("dagOther epochAdditions: %x", dagOther.epochAdditions)

	if len(dagSelf.epochAdditions) != len(dagOther.epochAdditions) {
		t.Fatalf("epochAdditions asymmetric: dagSelf has %d entries, dagOther has %d (diff = the self-registering node's own key)",
			len(dagSelf.epochAdditions), len(dagOther.epochAdditions))
	}

	assertEpochAdditionsHasOnce(t, dagSelf.epochAdditions, newVal.pubKey, "dagSelf")
	assertEpochAdditionsHasOnce(t, dagOther.epochAdditions, newVal.pubKey, "dagOther")
}

// assertEpochAdditionsHasOnce fails the test unless pubkey appears in
// additions exactly once, reporting label to identify which DAG failed.
func assertEpochAdditionsHasOnce(t *testing.T, additions []Hash, pubkey Hash, label string) {
	t.Helper()

	count := 0
	for _, a := range additions {
		if a == pubkey {
			count++
		}
	}

	if count != 1 {
		t.Errorf("%s: expected joining key exactly once in epochAdditions, found %d times", label, count)
	}
}
