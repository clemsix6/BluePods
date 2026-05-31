use pod_sdk::{
    borsh::BorshSerialize, Context, ExecuteResult, UpdatedObject, ObjectMeta,
    ERR_INVALID_ARGS, ERR_MISSING_OBJECT,
};

use super::Args;
use crate::objects::Object;

/// Transfers ownership of an object to a new owner.
/// Does not modify the object metadata, only changes the owner.
///
/// # Objects
/// - `0`: object to transfer (Object)
///
/// # Returns
/// - Updated object with new owner
pub fn execute(ctx: &Context) -> ExecuteResult {
    let args: Args = match ctx.args() {
        Some(a) => a,
        None => return ExecuteResult::err(ERR_INVALID_ARGS),
    };

    let object: Object = match ctx.object(0) {
        Some(n) => n,
        None => return ExecuteResult::err(ERR_MISSING_OBJECT),
    };

    let mut meta: ObjectMeta = match ctx.object_meta(0) {
        Some(m) => m,
        None => return ExecuteResult::err(ERR_MISSING_OBJECT),
    };

    // Change owner
    meta.owner = args.new_owner;

    // Serialize object (unchanged)
    let mut content = alloc::vec::Vec::new();
    object.serialize(&mut content).unwrap();

    ExecuteResult::ok().with_updated(UpdatedObject { meta, content })
}
