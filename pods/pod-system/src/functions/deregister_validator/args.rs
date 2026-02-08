use borsh::{BorshDeserialize, BorshSerialize};

/// Arguments for deregister_validator.
/// Empty — the sender pubkey identifies the validator to remove.
#[derive(BorshSerialize, BorshDeserialize)]
pub struct Args {}
