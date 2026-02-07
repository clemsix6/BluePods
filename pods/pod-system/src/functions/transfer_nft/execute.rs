use pod_sdk::{
    borsh::BorshSerialize, Context, ExecuteResult, UpdatedObject, ObjectMeta,
    ERR_INVALID_ARGS, ERR_MISSING_OBJECT,
};

use super::Args;
use crate::objects::Nft;

/// Transfers ownership of an NFT to a new owner.
/// Does not modify the NFT metadata, only changes the owner.
///
/// # Objects
/// - `0`: NFT to transfer (Nft)
///
/// # Returns
/// - Updated NFT with new owner
pub fn execute(ctx: &Context) -> ExecuteResult {
    let args: Args = match ctx.args() {
        Some(a) => a,
        None => return ExecuteResult::err(ERR_INVALID_ARGS),
    };

    let nft: Nft = match ctx.object(0) {
        Some(n) => n,
        None => return ExecuteResult::err(ERR_MISSING_OBJECT),
    };

    let mut meta: ObjectMeta = match ctx.object_meta(0) {
        Some(m) => m,
        None => return ExecuteResult::err(ERR_MISSING_OBJECT),
    };

    // Change owner
    meta.owner = args.new_owner;

    // Serialize NFT (unchanged)
    let mut content = alloc::vec::Vec::new();
    nft.serialize(&mut content).unwrap();

    ExecuteResult::ok().with_updated(UpdatedObject { meta, content })
}
