//! BluePods System Pod
//!
//! Handles native token operations using the object-oriented coin model.
//! Coins are objects that hold a balance of native tokens.
//! Also manages validator registration.

#![no_std]

extern crate alloc;

mod functions;
mod objects;

pod_sdk::dispatcher! {
    "merge" => functions::merge::execute,
    "mint" => functions::mint::execute,
    "register_validator" => functions::register_validator::execute,
    "split" => functions::split::execute,
    "transfer" => functions::transfer::execute,
}
