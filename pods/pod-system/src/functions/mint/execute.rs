use pod_sdk::{
    borsh::BorshSerialize, Context, ExecuteResult, CreatedObject,
    ERR_INVALID_ARGS,
};

use super::Args;
use crate::objects::Coin;

/// Mints tokens by creating a new coin object.
///
/// # Objects
/// - None required
///
/// # Returns
/// - New coin object with the minted amount
pub fn execute(ctx: &Context) -> ExecuteResult {
    let args: Args = match ctx.args() {
        Some(a) => a,
        None => return ExecuteResult::err(ERR_INVALID_ARGS),
    };

    // Create new coin with minted amount
    let new_coin = Coin {
        balance: args.amount,
    };

    // Serialize new coin
    let mut content = alloc::vec::Vec::new();
    new_coin.serialize(&mut content).unwrap();

    ExecuteResult::ok().with_created(CreatedObject {
        owner: args.owner,
        replication: 0,
        content,
    })
}
