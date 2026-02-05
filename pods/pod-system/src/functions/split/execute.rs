use pod_sdk::{
    borsh::BorshSerialize, Context, ExecuteResult, UpdatedObject, CreatedObject,
    ERR_INVALID_ARGS, ERR_MISSING_OBJECT,
};

use super::Args;
use crate::objects::Coin;

/// Error code for insufficient balance.
const ERR_INSUFFICIENT_BALANCE: u32 = 100;

/// Splits a coin into two coins.
/// Creates a new coin with the specified amount and reduces the source coin's balance.
///
/// # Objects
/// - `0`: Source coin (Coin) - will have its balance reduced
///
/// # Returns
/// - Updated source coin with reduced balance
/// - New coin object with the split amount
pub fn execute(ctx: &Context) -> ExecuteResult {
    let args: Args = match ctx.args() {
        Some(a) => a,
        None => return ExecuteResult::err(ERR_INVALID_ARGS),
    };

    let source: Coin = match ctx.object(0) {
        Some(c) => c,
        None => return ExecuteResult::err(ERR_MISSING_OBJECT),
    };

    let source_meta = match ctx.object_meta(0) {
        Some(m) => m,
        None => return ExecuteResult::err(ERR_MISSING_OBJECT),
    };

    // Check balance
    if source.balance < args.amount {
        return ExecuteResult::err(ERR_INSUFFICIENT_BALANCE);
    }

    // Update source coin with reduced balance
    let updated_source = Coin {
        balance: source.balance - args.amount,
    };

    // Create new coin with split amount
    let new_coin = Coin {
        balance: args.amount,
    };

    // Serialize coins
    let mut source_content = alloc::vec::Vec::new();
    updated_source.serialize(&mut source_content).unwrap();

    let mut new_content = alloc::vec::Vec::new();
    new_coin.serialize(&mut new_content).unwrap();

    ExecuteResult::ok()
        .with_updated(UpdatedObject {
            meta: source_meta,
            content: source_content,
        })
        .with_created(CreatedObject {
            owner: args.new_owner,
            replication: 0,
            content: new_content,
        })
}
