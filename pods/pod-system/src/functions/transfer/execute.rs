use pod_sdk::{
    borsh::BorshSerialize, Context, ExecuteResult, UpdatedObject, ObjectMeta,
    ERR_INVALID_ARGS, ERR_MISSING_OBJECT,
};

use super::Args;
use crate::objects::Coin;

/// Transfers ownership of a coin object to a new owner.
/// Does not modify the coin's balance, only changes the owner.
///
/// # Objects
/// - `0`: Coin to transfer (Coin)
///
/// # Returns
/// - Updated coin with new owner
pub fn execute(ctx: &Context) -> ExecuteResult {
    let args: Args = match ctx.args() {
        Some(a) => a,
        None => return ExecuteResult::err(ERR_INVALID_ARGS),
    };

    let coin: Coin = match ctx.object(0) {
        Some(c) => c,
        None => return ExecuteResult::err(ERR_MISSING_OBJECT),
    };

    let mut meta: ObjectMeta = match ctx.object_meta(0) {
        Some(m) => m,
        None => return ExecuteResult::err(ERR_MISSING_OBJECT),
    };

    // Change owner
    meta.owner = args.new_owner;

    // Serialize coin (unchanged)
    let mut content = alloc::vec::Vec::new();
    coin.serialize(&mut content).unwrap();

    ExecuteResult::ok().with_updated(UpdatedObject { meta, content })
}
