use borsh::{BorshDeserialize, BorshSerialize};

/// Arguments for the undelegate function.
/// The delegator is the transaction sender; the coin to credit back is passed as
/// a mutable_ref and updated Go-side. The validator identifies the position.
#[derive(BorshSerialize, BorshDeserialize)]
pub struct Args {
    /// Target validator's Ed25519 pubkey whose delegation is withdrawn.
    pub validator: [u8; 32],
}
