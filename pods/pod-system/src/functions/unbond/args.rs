use borsh::{BorshDeserialize, BorshSerialize};

/// Arguments for the unbond function.
/// The validator's pubkey is taken from the transaction sender; the coin to
/// credit back is passed as a mutable_ref and updated Go-side.
#[derive(BorshSerialize, BorshDeserialize)]
pub struct Args {
    /// Amount of self-stake to unbond and credit back to the coin.
    pub amount: u64,
}
