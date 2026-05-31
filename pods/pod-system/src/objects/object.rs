use borsh::{BorshDeserialize, BorshSerialize};

/// Object represents a replicated, owned object holding arbitrary content.
/// Unlike coins, objects use replication > 0 and are stored on a subset of validators.
#[derive(BorshSerialize, BorshDeserialize)]
pub struct Object {
    /// Arbitrary metadata bytes (application-defined content).
    pub metadata: alloc::vec::Vec<u8>,
}
