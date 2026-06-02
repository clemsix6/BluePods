use pod_sdk::{Context, ExecuteResult, ERR_INVALID_ARGS, ERR_INVALID_INPUT};

use super::Args;

/// Withdraws the sender's delegation from a validator.
///
/// The position deletion, coin credit, and delegated-total mutation are applied
/// Go-side at commit (mirroring unbond): this entry only makes the function
/// dispatch and validates the sender and args. The coin to credit is referenced
/// as a mutable_ref so the Go node's ownership validation covers it.
pub fn execute(ctx: &Context) -> ExecuteResult {
    let _args: Args = match ctx.args() {
        Some(a) => a,
        None => return ExecuteResult::err(ERR_INVALID_ARGS),
    };

    match ctx.sender() {
        Some(s) if s.len() == 32 => {}
        _ => return ExecuteResult::err(ERR_INVALID_INPUT),
    }

    ExecuteResult::ok().log("undelegate")
}
