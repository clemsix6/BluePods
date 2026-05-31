use pod_sdk::{
    borsh::BorshSerialize, Context, ExecuteResult, UpdatedObject, ObjectMeta,
    ERR_INVALID_ARGS, ERR_MISSING_OBJECT,
};

use super::Args;
use crate::objects::Object;

/// Overwrites the content of a replicated object.
/// Keeps the object metadata (owner, replication) unchanged and only replaces
/// the stored content bytes. The protocol auto-increments the version when the
/// updated object is applied.
///
/// # Objects
/// - `0`: object to modify (Object)
///
/// # Returns
/// - Updated object with the new content
pub fn execute(ctx: &Context) -> ExecuteResult {
    let args: Args = match ctx.args() {
        Some(a) => a,
        None => return ExecuteResult::err(ERR_INVALID_ARGS),
    };

    // Read the input object (index 0) to ensure it exists.
    let _object: Object = match ctx.object(0) {
        Some(n) => n,
        None => return ExecuteResult::err(ERR_MISSING_OBJECT),
    };

    // Keep the existing metadata (owner and replication unchanged).
    let meta: ObjectMeta = match ctx.object_meta(0) {
        Some(m) => m,
        None => return ExecuteResult::err(ERR_MISSING_OBJECT),
    };

    // Replace the content with the new bytes from the arguments.
    let object = Object {
        metadata: args.content,
    };

    let mut content = alloc::vec::Vec::new();
    object.serialize(&mut content).unwrap();

    ExecuteResult::ok().with_updated(UpdatedObject { meta, content })
}
