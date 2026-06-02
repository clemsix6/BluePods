use pod_sdk::{Context, ExecuteResult, ERR_INVALID_ARGS, ERR_INVALID_INPUT};

use super::Args;

/// Delegates native token from the sender to a validator.
///
/// The coin debit, position creation, and delegated-total mutation are applied
/// Go-side at commit (mirroring bond, whose stake mutation is also Go-side):
/// this entry only makes the function dispatch and validates the sender and
/// args. The delegated coin is referenced as a mutable_ref so the Go node's
/// ownership validation covers it.
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

    ExecuteResult::ok().log("delegate")
}
