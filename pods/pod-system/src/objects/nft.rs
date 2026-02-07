use borsh::{BorshDeserialize, BorshSerialize};

/// Nft represents a non-fungible token with arbitrary metadata.
/// Unlike coins, NFTs use replication > 0 and are stored on a subset of validators.
#[derive(BorshSerialize, BorshDeserialize)]
pub struct Nft {
    /// Arbitrary metadata bytes (application-defined content).
    pub metadata: alloc::vec::Vec<u8>,
}
