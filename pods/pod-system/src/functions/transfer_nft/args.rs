use borsh::{BorshDeserialize, BorshSerialize};

/// Arguments for the transfer_nft function.
#[derive(BorshSerialize, BorshDeserialize)]
pub struct Args {
    /// New owner of the NFT (32 bytes public key).
    pub new_owner: [u8; 32],
}
