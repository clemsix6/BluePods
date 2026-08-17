use alloc::vec::Vec;
use borsh::{BorshDeserialize, BorshSerialize};

/// Arguments for the register_validator function.
/// Note: The validator's ed25519 pubkey is taken from the transaction sender.
#[derive(BorshSerialize, BorshDeserialize)]
pub struct Args {
    /// The QUIC attestation endpoint address (e.g., "192.168.1.1:9000").
    pub quic_address: Vec<u8>,

    /// The BLS public key for attestation signing (48 bytes).
    pub bls_pubkey: Vec<u8>,

    /// The proof of possession of `bls_pubkey`: a BLS signature over the sender's
    /// Ed25519 key followed by `bls_pubkey`, under the proof-of-possession domain
    /// tag. The node verifies it at commit and refuses a registration claiming a
    /// key without it; the pod carries the field so the args deserialize.
    pub bls_pop: Vec<u8>,
}
