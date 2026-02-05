//! BluePods SDK for building WASM pods.
//!
//! # Example
//!
//! ```ignore
//! mod functions;
//! mod objects;
//!
//! pod_sdk::dispatcher! {
//!     "mint" => functions::mint::execute,
//!     "transfer" => functions::transfer::execute,
//! }
//! ```

#![no_std]

extern crate alloc;

use alloc::string::String;
use alloc::vec;
use alloc::vec::Vec;
use borsh::BorshDeserialize;
use flatbuffers::FlatBufferBuilder;

// Re-export FlatBuffers types
mod object_generated;
mod transaction_generated;
mod podio_generated;
mod validator_generated;

pub use object_generated::types::*;
pub use transaction_generated::types::*;
pub use podio_generated::types::*;
pub use validator_generated::types::{
    Validator, ValidatorArgs, ValidatorBuilder, finish_validator_buffer,
};

// Re-export dependencies
pub use borsh::{self, BorshSerialize};
pub use flatbuffers;

// ============================================================================
// Error Codes
// ============================================================================

pub const ERR_INVALID_INPUT: u32 = 1;
pub const ERR_UNKNOWN_FUNCTION: u32 = 2;
pub const ERR_INVALID_ARGS: u32 = 3;
pub const ERR_MISSING_OBJECT: u32 = 4;

// ============================================================================
// Runtime Interface
// ============================================================================

extern "C" {
    fn gas(cost: u32);
    fn input_len() -> u32;
    fn read_input(ptr: *mut u8);
    fn write_output(ptr: *const u8, len: u32);
}

// ============================================================================
// Global Allocator & Panic Handler
// ============================================================================

#[global_allocator]
static ALLOC: wee_alloc::WeeAlloc = wee_alloc::WeeAlloc::INIT;

#[panic_handler]
fn panic(_info: &core::panic::PanicInfo) -> ! {
    loop {}
}

// ============================================================================
// Context
// ============================================================================

/// Execution context passed to function handlers.
pub struct Context<'a> {
    input: &'a PodExecuteInput<'a>,
}

impl<'a> Context<'a> {
    /// Creates a new context from PodExecuteInput.
    pub fn new(input: &'a PodExecuteInput<'a>) -> Self {
        Self { input }
    }

    /// Returns the function name being called.
    pub fn function_name(&self) -> Option<&str> {
        self.input.transaction()?.function_name()
    }

    /// Parses the function arguments.
    pub fn args<T: BorshDeserialize>(&self) -> Option<T> {
        let tx = self.input.transaction()?;
        let args_bytes = tx.args()?;
        let slice: Vec<u8> = (0..args_bytes.len()).map(|i| args_bytes.get(i)).collect();
        T::try_from_slice(&slice).ok()
    }

    /// Returns the number of local objects.
    pub fn object_count(&self) -> usize {
        self.input.local_objects().map(|o| o.len()).unwrap_or(0)
    }

    /// Parses an object's content at the given index.
    /// Returns None if the index is out of bounds or if deserialization fails.
    pub fn object<T: BorshDeserialize>(&self, index: usize) -> Option<T> {
        let objects = self.input.local_objects()?;

        if index >= objects.len() {
            return None;
        }

        let obj = objects.get(index);
        let content = obj.content()?;
        let slice: Vec<u8> = (0..content.len()).map(|i| content.get(i)).collect();
        T::try_from_slice(&slice).ok()
    }

    /// Returns raw object metadata at the given index.
    pub fn object_meta(&self, index: usize) -> Option<ObjectMeta> {
        let objects = self.input.local_objects()?;

        if index >= objects.len() {
            return None;
        }

        let obj = objects.get(index);
        let id = obj.id()?;
        let owner = obj.owner()?;

        Some(ObjectMeta {
            id: id.bytes().try_into().ok()?,
            version: obj.version(),
            owner: owner.bytes().try_into().ok()?,
            replication: obj.replication(),
        })
    }

    /// Returns the sender's public key (32 bytes).
    pub fn sender(&self) -> Option<&[u8]> {
        let sender = self.input.sender()?;
        Some(sender.bytes())
    }

    /// Returns the raw PodExecuteInput for advanced use cases.
    pub fn raw_input(&self) -> &PodExecuteInput<'a> {
        self.input
    }
}

/// Metadata about an object (without content).
#[derive(Clone)]
pub struct ObjectMeta {
    /// Object ID (32 bytes).
    pub id: [u8; 32],
    /// Object version.
    pub version: u64,
    /// Owner public key (32 bytes).
    pub owner: [u8; 32],
    /// Replication factor (0 = singleton).
    pub replication: u16,
}

// ============================================================================
// Updated Object
// ============================================================================

/// Represents an updated object to be returned.
pub struct UpdatedObject {
    /// Object metadata.
    pub meta: ObjectMeta,
    /// New content (Borsh or FlatBuffers serialized).
    pub content: Vec<u8>,
}

/// Represents a newly created object.
pub struct CreatedObject {
    /// Owner public key (32 bytes).
    pub owner: [u8; 32],
    /// Replication factor (0 = singleton).
    pub replication: u16,
    /// Content (Borsh or FlatBuffers serialized).
    pub content: Vec<u8>,
}

// ============================================================================
// Execute Result
// ============================================================================

/// Result of pod execution.
pub struct ExecuteResult {
    /// Error code (0 = success).
    pub error: u32,
    /// Objects that were updated.
    pub updated_objects: Vec<UpdatedObject>,
    /// Objects that were created.
    pub created_objects: Vec<CreatedObject>,
    /// Debug logs.
    pub logs: Vec<String>,
}

impl ExecuteResult {
    /// Creates a successful result with no object changes.
    pub fn ok() -> Self {
        Self {
            error: 0,
            updated_objects: vec![],
            created_objects: vec![],
            logs: vec![],
        }
    }

    /// Creates an error result.
    pub fn err(code: u32) -> Self {
        Self {
            error: code,
            updated_objects: vec![],
            created_objects: vec![],
            logs: vec![],
        }
    }

    /// Adds a log message.
    pub fn log(mut self, msg: &str) -> Self {
        self.logs.push(String::from(msg));
        self
    }

    /// Adds an updated object to the result.
    pub fn with_updated(mut self, obj: UpdatedObject) -> Self {
        self.updated_objects.push(obj);
        self
    }

    /// Adds a created object to the result.
    pub fn with_created(mut self, obj: CreatedObject) -> Self {
        self.created_objects.push(obj);
        self
    }
}

// ============================================================================
// Dispatcher Macro
// ============================================================================

/// Generates the WASM entry point with function dispatch.
#[macro_export]
macro_rules! dispatcher {
    ( $( $name:literal => $handler:path ),* $(,)? ) => {
        #[no_mangle]
        pub extern "C" fn execute() {
            $crate::__run_dispatcher(|ctx| {
                match ctx.function_name() {
                    $(
                        Some($name) => $handler(&ctx),
                    )*
                    Some(_) => $crate::ExecuteResult::err($crate::ERR_UNKNOWN_FUNCTION),
                    None => $crate::ExecuteResult::err($crate::ERR_INVALID_INPUT),
                }
            });
        }
    };
}

/// Internal function called by the dispatcher macro.
#[doc(hidden)]
pub fn __run_dispatcher<F>(handler: F)
where
    F: FnOnce(Context) -> ExecuteResult,
{
    let input_bytes = read_input_bytes();
    let output_bytes = run_with_context(&input_bytes, handler);
    write_output_bytes(&output_bytes);
}

/// Runs the handler with a Context.
fn run_with_context<F>(input_bytes: &[u8], handler: F) -> Vec<u8>
where
    F: FnOnce(Context) -> ExecuteResult,
{
    let input = match flatbuffers::root::<PodExecuteInput>(input_bytes) {
        Ok(i) => i,
        Err(_) => return build_error_output(ERR_INVALID_INPUT),
    };

    let ctx = Context::new(&input);
    let result = handler(ctx);

    build_output(result)
}

// ============================================================================
// Internal Functions
// ============================================================================

fn read_input_bytes() -> Vec<u8> {
    let len = unsafe { input_len() } as usize;
    if len == 0 {
        return vec![];
    }
    let mut buffer = vec![0u8; len];
    unsafe { read_input(buffer.as_mut_ptr()) };
    buffer
}

/// Builds error output (no objects).
fn build_error_output(error: u32) -> Vec<u8> {
    let mut builder = FlatBufferBuilder::with_capacity(64);
    let mut output_builder = PodExecuteOutputBuilder::new(&mut builder);
    output_builder.add_error(error);
    let output = output_builder.finish();
    builder.finish(output, None);
    builder.finished_data().to_vec()
}

/// Builds output with objects and logs.
fn build_output(result: ExecuteResult) -> Vec<u8> {
    let mut builder = FlatBufferBuilder::with_capacity(1024);

    // Build updated objects
    let updated_offsets: Vec<_> = result
        .updated_objects
        .iter()
        .map(|obj| build_object(&mut builder, &obj.meta, &obj.content))
        .collect();

    let updated_vec = if !updated_offsets.is_empty() {
        Some(builder.create_vector(&updated_offsets))
    } else {
        None
    };

    // Build created objects (with zero ID - will be assigned by runtime)
    let created_offsets: Vec<_> = result
        .created_objects
        .iter()
        .map(|obj| {
            let meta = ObjectMeta {
                id: [0u8; 32], // Zero ID for created objects
                version: 0,
                owner: obj.owner,
                replication: obj.replication,
            };
            build_object(&mut builder, &meta, &obj.content)
        })
        .collect();

    let created_vec = if !created_offsets.is_empty() {
        Some(builder.create_vector(&created_offsets))
    } else {
        None
    };

    // Build logs vector
    let log_offsets: Vec<_> = result
        .logs
        .iter()
        .map(|s| builder.create_string(s))
        .collect();

    let logs_vec = if !log_offsets.is_empty() {
        Some(builder.create_vector(&log_offsets))
    } else {
        None
    };

    // Build output
    let mut output_builder = PodExecuteOutputBuilder::new(&mut builder);
    output_builder.add_error(result.error);

    if let Some(v) = updated_vec {
        output_builder.add_updated_objects(v);
    }
    if let Some(v) = created_vec {
        output_builder.add_created_objects(v);
    }
    if let Some(v) = logs_vec {
        output_builder.add_logs(v);
    }

    let output = output_builder.finish();
    builder.finish(output, None);
    builder.finished_data().to_vec()
}

/// Builds a single Object in FlatBuffers format.
fn build_object<'a>(
    builder: &mut FlatBufferBuilder<'a>,
    meta: &ObjectMeta,
    content: &[u8],
) -> flatbuffers::WIPOffset<Object<'a>> {
    let id = builder.create_vector(&meta.id);
    let owner = builder.create_vector(&meta.owner);
    let content_vec = builder.create_vector(content);

    let mut obj_builder = ObjectBuilder::new(builder);
    obj_builder.add_id(id);
    obj_builder.add_version(meta.version);
    obj_builder.add_owner(owner);
    obj_builder.add_replication(meta.replication);
    obj_builder.add_content(content_vec);
    obj_builder.finish()
}

fn write_output_bytes(output: &[u8]) {
    if !output.is_empty() {
        unsafe { write_output(output.as_ptr(), output.len() as u32) };
    }
}
