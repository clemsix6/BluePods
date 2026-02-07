use borsh::{BorshDeserialize, BorshSerialize};

/// Arguments for the create_nft function.
#[derive(BorshSerialize, BorshDeserialize)]
pub struct Args {
    /// Owner of the newly created NFT (32 bytes public key).
    pub owner: [u8; 32],
    /// Replication factor (number of validators storing this object).
    pub replication: u16,
    /// Arbitrary metadata bytes.
    pub metadata: alloc::vec::Vec<u8>,
}
