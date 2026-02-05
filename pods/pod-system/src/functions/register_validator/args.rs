use alloc::vec::Vec;
use borsh::{BorshDeserialize, BorshSerialize};

/// Arguments for the register_validator function.
/// Note: The validator's pubkey is taken from the transaction sender.
#[derive(BorshSerialize, BorshDeserialize)]
pub struct Args {
    /// The HTTP API endpoint address (e.g., "192.168.1.1:8080").
    pub http_address: Vec<u8>,

    /// The QUIC attestation endpoint address (e.g., "192.168.1.1:9000").
    pub quic_address: Vec<u8>,
}
