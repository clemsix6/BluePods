use borsh::{BorshDeserialize, BorshSerialize};

/// Coin represents a quantity of native tokens.
/// In the object-oriented model (like SUI), coins are objects that hold value.
#[derive(BorshSerialize, BorshDeserialize)]
pub struct Coin {
    /// The amount of tokens held by this coin object.
    pub balance: u64,
}
