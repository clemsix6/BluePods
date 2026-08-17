package consensus

import "BluePods/internal/attest"

// reasonBLSPoPInvalid is the tx.committed reason for a register_validator
// transaction that claims a BLS public key without proving possession of it.
const reasonBLSPoPInvalid = "bls_pop_invalid"

// verifiedRegistrationBLSKey returns the BLS public key a register_validator
// transaction may be credited with, and false when the registration must be
// refused outright.
//
// Object attestations are verified as one aggregated signature over one message,
// which is only sound while every aggregated public key is one its holder can
// prove it owns. A registrant free to pick any 48 bytes can register
// pk_rogue = pk_attacker - sum(pk_honest) and then produce, single-handedly, an
// aggregate that verifies against the quorum's aggregated key as if every honest
// holder had signed. The proof of possession closes that: it is a signature over
// the sender and the key under a domain tag of its own
// (attest.VerifyKeyPossession), which only the holder of the secret behind the
// key can produce, which no attestation signature can stand in for, and which a
// key derived from other members' keys cannot have at all.
//
// Args that carry no full-length key claim nothing and need no proof: the
// validator is admitted with a zero BLS key, carries no attestation weight, and
// can claim a key later by re-registering with a proof. This is the shape a
// re-registration that only designates a reward coin has.
//
// The check reads committed transaction bytes and the sender only, so every node
// reaches the same verdict on the same registration.
func verifiedRegistrationBLSKey(sender Hash, blsPubkey, pop []byte) ([48]byte, bool) {
	var key [48]byte

	if len(blsPubkey) != attest.BLSPublicKeySize {
		return key, true
	}

	if !attest.VerifyKeyPossession(pop, sender, blsPubkey) {
		return key, false
	}

	copy(key[:], blsPubkey)

	return key, true
}
