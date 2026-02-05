use borsh::{BorshDeserialize, BorshSerialize};

/// Arguments for the split function.
#[derive(BorshSerialize, BorshDeserialize)]
pub struct Args {
    /// Amount to split from the source coin into a new coin.
    pub amount: u64,
    /// Owner of the newly created coin (32 bytes public key).
    pub new_owner: [u8; 32],
}
