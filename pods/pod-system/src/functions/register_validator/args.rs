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
}
