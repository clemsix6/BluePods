use pod_sdk::{
    borsh::BorshSerialize, Context, ExecuteResult, UpdatedObject,
    ERR_INVALID_ARGS, ERR_MISSING_OBJECT,
};

use super::Args;
use crate::objects::Coin;

/// Error code for insufficient coins to merge.
const ERR_INSUFFICIENT_COINS: u32 = 101;

/// Error code for a balance sum that overflows u64.
const ERR_BALANCE_OVERFLOW: u32 = 102;

/// Merges multiple coins into a single coin.
/// The first coin (index 0) receives the total balance; every source coin
/// (index 1..N) is emptied to zero so total balance is conserved (no
/// duplication). A checked add guards against u64 wraparound.
///
/// # Objects
/// - `0`: Destination coin (Coin) - receives the total balance
/// - `1..N`: Source coins (Coin) - emptied to zero
///
/// # Returns
/// - Updated destination coin with the total balance
/// - Each source coin updated to a zero balance
pub fn execute(ctx: &Context) -> ExecuteResult {
    let _args: Args = match ctx.args() {
        Some(a) => a,
        None => return ExecuteResult::err(ERR_INVALID_ARGS),
    };

    let obj_count = ctx.object_count();

    if obj_count < 2 {
        return ExecuteResult::err(ERR_INSUFFICIENT_COINS);
    }

    let total_balance = match sum_balances(ctx, obj_count) {
        Ok(total) => total,
        Err(code) => return ExecuteResult::err(code),
    };

    build_result(ctx, obj_count, total_balance)
}

/// sum_balances accumulates the balances of all input coins with a checked add.
fn sum_balances(ctx: &Context, obj_count: usize) -> Result<u64, u32> {
    let mut total: u64 = 0;

    for i in 0..obj_count {
        let coin: Coin = match ctx.object(i) {
            Some(c) => c,
            None => return Err(ERR_MISSING_OBJECT),
        };

        total = match total.checked_add(coin.balance) {
            Some(sum) => sum,
            None => return Err(ERR_BALANCE_OVERFLOW),
        };
    }

    Ok(total)
}

/// build_result emits the destination coin with the total and zeroes every source.
fn build_result(ctx: &Context, obj_count: usize, total_balance: u64) -> ExecuteResult {
    let mut result = match updated_coin(ctx, 0, total_balance) {
        Some(obj) => ExecuteResult::ok().with_updated(obj),
        None => return ExecuteResult::err(ERR_MISSING_OBJECT),
    };

    for i in 1..obj_count {
        match updated_coin(ctx, i, 0) {
            Some(obj) => result = result.with_updated(obj),
            None => return ExecuteResult::err(ERR_MISSING_OBJECT),
        }
    }

    result
}

/// updated_coin builds an UpdatedObject for the coin at `index` with `balance`.
fn updated_coin(ctx: &Context, index: usize, balance: u64) -> Option<UpdatedObject> {
    let meta = ctx.object_meta(index)?;

    let coin = Coin { balance };
    let mut content = alloc::vec::Vec::new();
    coin.serialize(&mut content).unwrap();

    Some(UpdatedObject { meta, content })
}
