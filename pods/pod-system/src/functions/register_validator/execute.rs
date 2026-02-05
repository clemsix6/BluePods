use alloc::vec::Vec;
use pod_sdk::{
    flatbuffers::FlatBufferBuilder, Context, CreatedObject, ExecuteResult,
    ValidatorBuilder, ERR_INVALID_ARGS, ERR_INVALID_INPUT,
};

use super::Args;

/// Registers a new validator on the network.
/// Creates a Validator object (FlatBuffers) that can be read by the Go node.
///
/// The validator's pubkey is taken from the transaction sender.
///
/// # Arguments (Borsh encoded)
/// - `http_address`: HTTP API endpoint
/// - `quic_address`: QUIC attestation endpoint
///
/// # Returns
/// Creates a new Validator singleton object (replication=0).
pub fn execute(ctx: &Context) -> ExecuteResult {
    let args: Args = match ctx.args() {
        Some(a) => a,
        None => return ExecuteResult::err(ERR_INVALID_ARGS),
    };

    // Get sender pubkey (will be the validator's pubkey)
    let sender = match ctx.sender() {
        Some(s) => s,
        None => return ExecuteResult::err(ERR_INVALID_INPUT),
    };

    let pubkey: [u8; 32] = match sender.try_into() {
        Ok(p) => p,
        Err(_) => return ExecuteResult::err(ERR_INVALID_INPUT),
    };

    // Build Validator FlatBuffer
    let content = build_validator_content(&pubkey, &args.http_address, &args.quic_address);

    // Create validator as singleton (replication=0)
    let created = CreatedObject {
        owner: pubkey,
        replication: 0, // Singleton - replicated on all validators
        content,
    };

    ExecuteResult::ok().with_created(created)
}

/// Builds Validator content as FlatBuffer bytes.
fn build_validator_content(pubkey: &[u8; 32], http_address: &[u8], quic_address: &[u8]) -> Vec<u8> {
    let mut builder = FlatBufferBuilder::with_capacity(256);

    let pubkey_vec = builder.create_vector(pubkey);
    let http_vec = builder.create_vector(http_address);
    let quic_vec = builder.create_vector(quic_address);

    let mut vb = ValidatorBuilder::new(&mut builder);
    vb.add_pubkey(pubkey_vec);
    vb.add_http_address(http_vec);
    vb.add_quic_address(quic_vec);
    let validator = vb.finish();

    builder.finish(validator, None);
    builder.finished_data().to_vec()
}
