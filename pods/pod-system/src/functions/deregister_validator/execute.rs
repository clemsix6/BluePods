use pod_sdk::{Context, ExecuteResult, ERR_INVALID_INPUT};

/// Handles voluntary validator departure.
/// The sender pubkey identifies the validator to deregister.
/// No arguments or object modifications are needed — the Go node handles
/// the actual removal at the next epoch boundary via pendingRemovals.
pub fn execute(ctx: &Context) -> ExecuteResult {
    match ctx.sender() {
        Some(s) if s.len() == 32 => {}
        _ => return ExecuteResult::err(ERR_INVALID_INPUT),
    }

    ExecuteResult::ok().log("deregister_validator")
}
