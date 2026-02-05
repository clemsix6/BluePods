use borsh::{BorshDeserialize, BorshSerialize};

/// Arguments for the transfer function.
#[derive(BorshSerialize, BorshDeserialize)]
pub struct Args {
    /// New owner of the coin (32 bytes public key).
    pub new_owner: [u8; 32],
}
