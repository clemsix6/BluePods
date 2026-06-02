use borsh::{BorshDeserialize, BorshSerialize};

/// Arguments for the bond function.
/// The validator's pubkey is taken from the transaction sender; the staked coin
/// is passed as a mutable_ref and debited Go-side.
#[derive(BorshSerialize, BorshDeserialize)]
pub struct Args {
    /// Amount of native token to bond as self-stake.
    pub amount: u64,
}
