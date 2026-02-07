use pod_sdk::{
    borsh::BorshSerialize, Context, ExecuteResult, CreatedObject,
    ERR_INVALID_ARGS,
};

use super::Args;
use crate::objects::Nft;

/// Creates a new NFT object with configurable replication.
///
/// Unlike coins (singleton, replication=0), NFTs are replicated on a subset
/// of validators. They are included in the ATX body since not all nodes have them.
///
/// # Objects
/// - None required
///
/// # Returns
/// - New NFT object with the given metadata and replication factor
pub fn execute(ctx: &Context) -> ExecuteResult {
    let args: Args = match ctx.args() {
        Some(a) => a,
        None => return ExecuteResult::err(ERR_INVALID_ARGS),
    };

    let nft = Nft {
        metadata: args.metadata,
    };

    let mut content = alloc::vec::Vec::new();
    nft.serialize(&mut content).unwrap();

    ExecuteResult::ok().with_created(CreatedObject {
        owner: args.owner,
        replication: args.replication,
        content,
    })
}
