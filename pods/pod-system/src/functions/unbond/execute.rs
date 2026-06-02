use pod_sdk::{Context, ExecuteResult, ERR_INVALID_ARGS, ERR_INVALID_INPUT};

use super::Args;

/// Unbonds native token from the sender's validator self-stake.
///
/// The stake reduction and coin credit are applied Go-side at commit (mirroring
/// register_validator and bond): this entry only makes the function dispatch and
/// validates the sender and args. The coin to credit is referenced as a
/// mutable_ref so the Go node's ownership validation covers it.
pub fn execute(ctx: &Context) -> ExecuteResult {
    let args: Args = match ctx.args() {
        Some(a) => a,
        None => return ExecuteResult::err(ERR_INVALID_ARGS),
    };

    if args.amount == 0 {
        return ExecuteResult::err(ERR_INVALID_ARGS);
    }

    match ctx.sender() {
        Some(s) if s.len() == 32 => {}
        _ => return ExecuteResult::err(ERR_INVALID_INPUT),
    }

    ExecuteResult::ok().log("unbond")
}
