use borsh::{BorshDeserialize, BorshSerialize};

/// Arguments for the delegate function.
/// The delegator is the transaction sender; the delegated coin is passed as a
/// mutable_ref and debited Go-side. The validator is the delegation target.
#[derive(BorshSerialize, BorshDeserialize)]
pub struct Args {
    /// Target validator's Ed25519 pubkey receiving the delegation.
    pub validator: [u8; 32],
    /// Amount of native token to delegate.
    pub amount: u64,
}
