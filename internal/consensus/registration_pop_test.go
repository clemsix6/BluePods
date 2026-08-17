package consensus

import (
	"testing"

	"BluePods/internal/attest"
	"BluePods/internal/events"
	"BluePods/internal/types"
)

// registrationDAG builds a two-validator DAG ready to commit a register_validator
// transaction through the full executeTx path.
func registrationDAG(t *testing.T) *DAG {
	t.Helper()

	db := newTestStorage(t)
	validators, vs := newTestValidatorSet(2)

	dag := New(db, vs, &mockBroadcaster{}, testSystemPod, 0, validators[0].privKey, nil)
	t.Cleanup(dag.Close)
	disableTxAuth(dag)

	return dag
}

// commitRegistration commits a register_validator ATX and reports whether the
// sender ended up in the validator set with the BLS key the transaction claimed.
func commitRegistration(t *testing.T, dag *DAG, sender Hash, atxBytes []byte) *ValidatorInfo {
	t.Helper()

	dag.executeTx(types.GetRootAsAttestedTransaction(atxBytes, 0), 5, Hash{}, nil, Hash{})

	return dag.validators.Get(sender)
}

// TestRegisterValidator_RogueBLSKeyRejected is the regression test for the
// rogue-key attack on aggregated attestation. Attestations are verified as one
// aggregated signature over a single message, so a registrant free to choose any
// 48 bytes as its BLS public key can pick pk_rogue = pk_attacker - sum(pk_honest)
// and sign an aggregate alone that verifies as if a quorum of honest holders had
// signed it. Such a key has no known secret, so it cannot come with a proof of
// possession, and the registration that claims it must not be admitted.
func TestRegisterValidator_RogueBLSKeyRejected(t *testing.T) {
	dag := registrationDAG(t)
	rogue := newTestValidator()

	// Any 48 bytes an attacker likes, with no proof of possession behind them.
	rogueKey := make([]byte, attest.BLSPublicKeySize)
	rogueKey[0] = 0xAB

	atxBytes := buildRegisterATX(t, rogue.pubKey, testSystemPod, "quic://rogue:9090", rogueKey, nil)

	if info := commitRegistration(t, dag, rogue.pubKey, atxBytes); info != nil {
		t.Fatalf("a registration claiming an unproven BLS key must be refused, got key %x", info.BLSPubkey)
	}
}

// TestRegisterValidator_ValidProofAccepted checks the honest path: a BLS key with
// a proof of possession bound to the sender is admitted and stored.
func TestRegisterValidator_ValidProofAccepted(t *testing.T) {
	dag := registrationDAG(t)
	newVal := newTestValidator()
	key := testRegistrationBLSKey(t, newVal.pubKey)

	atxBytes := buildRegisterATX(t, newVal.pubKey, testSystemPod, "quic://new:9090",
		key.PublicKeyBytes(), key.ProveKeyPossession(newVal.pubKey))

	info := commitRegistration(t, dag, newVal.pubKey, atxBytes)
	if info == nil {
		t.Fatal("a registration with a valid proof of possession must be admitted")
	}

	var want [48]byte
	copy(want[:], key.PublicKeyBytes())

	if info.BLSPubkey != want {
		t.Fatalf("stored BLS key = %x, want %x", info.BLSPubkey, want)
	}
}

// TestRegisterValidator_ProofOverOtherKeyRejected covers the proof that verifies,
// but not for the key being claimed: the attacker proves possession of a key it
// really holds while registering a different one.
func TestRegisterValidator_ProofOverOtherKeyRejected(t *testing.T) {
	dag := registrationDAG(t)
	rogue := newTestValidator()

	held := testRegistrationBLSKey(t, rogue.pubKey)

	claimed, err := attest.GenerateBLSKeyFromSeed(make([]byte, 32))
	if err != nil {
		t.Fatalf("generate claimed key: %v", err)
	}

	atxBytes := buildRegisterATX(t, rogue.pubKey, testSystemPod, "quic://rogue:9090",
		claimed.PublicKeyBytes(), held.ProveKeyPossession(rogue.pubKey))

	if info := commitRegistration(t, dag, rogue.pubKey, atxBytes); info != nil {
		t.Fatal("a proof over a different key must not register the claimed key")
	}
}

// TestRegisterValidator_ProofUnderAttestationDSTRejected covers the domain
// separation at the registration seam: a signature over the same bytes under the
// attestation tag is not a proof of possession, so an attestation signature
// harvested off the network cannot stand in for one.
func TestRegisterValidator_ProofUnderAttestationDSTRejected(t *testing.T) {
	dag := registrationDAG(t)
	newVal := newTestValidator()
	key := testRegistrationBLSKey(t, newVal.pubKey)

	underAttestationDST := key.Sign(append(newVal.pubKey[:], key.PublicKeyBytes()...))

	atxBytes := buildRegisterATX(t, newVal.pubKey, testSystemPod, "quic://new:9090",
		key.PublicKeyBytes(), underAttestationDST)

	if info := commitRegistration(t, dag, newVal.pubKey, atxBytes); info != nil {
		t.Fatal("a signature under the attestation domain tag must not pass as a proof of possession")
	}
}

// TestRegisterValidator_ProofOfOtherIdentityRejected covers the identity binding:
// a proof lifted verbatim from an honest validator's own registration must not let
// a second sender claim the same BLS key, which would let it echo that validator's
// attestation signature as a second signer.
func TestRegisterValidator_ProofOfOtherIdentityRejected(t *testing.T) {
	dag := registrationDAG(t)
	honest := newTestValidator()
	thief := newTestValidator()

	key := testRegistrationBLSKey(t, honest.pubKey)

	atxBytes := buildRegisterATX(t, thief.pubKey, testSystemPod, "quic://thief:9090",
		key.PublicKeyBytes(), key.ProveKeyPossession(honest.pubKey))

	if info := commitRegistration(t, dag, thief.pubKey, atxBytes); info != nil {
		t.Fatal("a proof bound to another identity must not register that key for this sender")
	}
}

// TestRegisterValidator_AbsentProofRejected covers a well-formed BLS key with no
// proof at all, the shape every pre-proof registration had.
func TestRegisterValidator_AbsentProofRejected(t *testing.T) {
	dag := registrationDAG(t)
	newVal := newTestValidator()
	key := testRegistrationBLSKey(t, newVal.pubKey)

	atxBytes := buildRegisterATX(t, newVal.pubKey, testSystemPod, "quic://new:9090",
		key.PublicKeyBytes(), nil)

	if info := commitRegistration(t, dag, newVal.pubKey, atxBytes); info != nil {
		t.Fatal("a BLS key with no proof of possession must be refused")
	}
}

// TestRegisterValidator_NoBLSClaimNeedsNoProof covers the re-registration that
// claims no BLS key at all (the shape that only designates a reward coin): it
// carries no attestation weight, so there is nothing to prove and it is admitted.
func TestRegisterValidator_NoBLSClaimNeedsNoProof(t *testing.T) {
	dag := registrationDAG(t)
	newVal := newTestValidator()

	atxBytes := buildRegisterATX(t, newVal.pubKey, testSystemPod, "quic://new:9090", nil, nil)

	info := commitRegistration(t, dag, newVal.pubKey, atxBytes)
	if info == nil {
		t.Fatal("a registration claiming no BLS key must still be admitted")
	}

	if info.BLSPubkey != [48]byte{} {
		t.Fatalf("no key was claimed, stored BLS key = %x", info.BLSPubkey)
	}
}

// TestRegisterValidator_RejectionFailsTheTransaction checks how the refusal is
// surfaced: the registration commits as a failed transaction naming the reason,
// the same way every other commit-path rejection is reported.
func TestRegisterValidator_RejectionFailsTheTransaction(t *testing.T) {
	buf := captureEvents(t)
	dag := registrationDAG(t)
	rogue := newTestValidator()

	rogueKey := make([]byte, attest.BLSPublicKeySize)
	rogueKey[0] = 0xAB

	atxBytes := buildRegisterATX(t, rogue.pubKey, testSystemPod, "quic://rogue:9090", rogueKey, nil)
	dag.executeTx(types.GetRootAsAttestedTransaction(atxBytes, 0), 5, Hash{}, nil, Hash{})

	records := eventsNamed(t, buf, events.EvTxCommitted)
	if len(records) != 1 {
		t.Fatalf("expected one tx.committed record, got %d", len(records))
	}

	if got := records[0]["success"]; got != false {
		t.Fatalf("success = %v, want false", got)
	}

	if got := records[0]["reason"]; got != reasonBLSPoPInvalid {
		t.Fatalf("reason = %v, want %s", got, reasonBLSPoPInvalid)
	}
}
