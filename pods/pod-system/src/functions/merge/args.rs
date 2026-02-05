use borsh::{BorshDeserialize, BorshSerialize};

/// Arguments for the merge function.
#[derive(BorshSerialize, BorshDeserialize)]
pub struct Args {
    // No arguments needed - will merge all input coins
}
