use borsh::{BorshDeserialize, BorshSerialize};

/// Arguments for the transfer_object function.
#[derive(BorshSerialize, BorshDeserialize)]
pub struct Args {
    /// New owner of the object (32 bytes public key).
    pub new_owner: [u8; 32],
}
