use pod_sdk::{
    borsh::BorshSerialize, Context, ExecuteResult, UpdatedObject,
    ERR_INVALID_ARGS, ERR_MISSING_OBJECT,
};

use super::Args;
use crate::objects::Coin;

/// Error code for insufficient coins to merge.
const ERR_INSUFFICIENT_COINS: u32 = 101;

/// Merges multiple coins into a single coin.
/// The first coin (index 0) receives the total balance, other coins are consumed.
///
/// # Objects
/// - `0`: Destination coin (Coin) - will receive the total balance
/// - `1..N`: Source coins (Coin) - will be consumed (balance transferred to destination)
///
/// # Returns
/// - Updated destination coin with total balance
/// - Source coins are not returned (consumed by the operation)
pub fn execute(ctx: &Context) -> ExecuteResult {
    let _args: Args = match ctx.args() {
        Some(a) => a,
        None => return ExecuteResult::err(ERR_INVALID_ARGS),
    };

    let obj_count = ctx.object_count();

    // Need at least 2 coins to merge
    if obj_count < 2 {
        return ExecuteResult::err(ERR_INSUFFICIENT_COINS);
    }

    // Get destination coin (first object)
    let mut total_balance: u64 = 0;

    // Accumulate balances from all coins
    for i in 0..obj_count {
        let coin: Coin = match ctx.object(i) {
            Some(c) => c,
            None => return ExecuteResult::err(ERR_MISSING_OBJECT),
        };
        total_balance += coin.balance;
    }

    // Get destination coin metadata
    let dest_meta = match ctx.object_meta(0) {
        Some(m) => m,
        None => return ExecuteResult::err(ERR_MISSING_OBJECT),
    };

    // Create updated destination coin with total balance
    let merged_coin = Coin {
        balance: total_balance,
    };

    let mut content = alloc::vec::Vec::new();
    merged_coin.serialize(&mut content).unwrap();

    // Return only the destination coin - source coins are consumed
    ExecuteResult::ok().with_updated(UpdatedObject {
        meta: dest_meta,
        content,
    })
}
