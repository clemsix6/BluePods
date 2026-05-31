use borsh::{BorshDeserialize, BorshSerialize};

/// Arguments for the set_object function.
#[derive(BorshSerialize, BorshDeserialize)]
pub struct Args {
    /// Object ID to modify (32 bytes).
    pub object_id: [u8; 32],
    /// New content bytes to store in the object.
    pub content: alloc::vec::Vec<u8>,
}
