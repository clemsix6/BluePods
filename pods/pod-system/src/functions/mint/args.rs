use borsh::{BorshDeserialize, BorshSerialize};

/// Arguments for the mint function.
#[derive(BorshSerialize, BorshDeserialize)]
pub struct Args {
    /// Amount of tokens to mint into a new coin object.
    pub amount: u64,
    /// Owner of the newly created coin (32 bytes public key).
    pub owner: [u8; 32],
}
