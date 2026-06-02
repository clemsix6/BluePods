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
    "bond" => functions::bond::execute,
    "create_object" => functions::create_object::execute,
    "deregister_validator" => functions::deregister_validator::execute,
    "merge" => functions::merge::execute,
    "register_validator" => functions::register_validator::execute,
    "set_object" => functions::set_object::execute,
    "split" => functions::split::execute,
    "transfer" => functions::transfer::execute,
    "transfer_object" => functions::transfer_object::execute,
    "unbond" => functions::unbond::execute,
}
