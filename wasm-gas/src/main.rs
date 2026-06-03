//! CLI to instrument a WASM module with gas metering.
//! Usage: wasm-gas <input.wasm> <output.wasm>
//!
//! The instrumented module calls `env.gas(cost)` at the start of every instruction
//! sequence (function body, blocks, loops, if/else arms), so loops are metered per
//! iteration. Each instruction costs 1 gas. See the library crate for the pass.

use anyhow::{Context, Result};
use std::{env, fs, process};
use walrus::{Module, ModuleConfig};

fn main() {
    if let Err(e) = run() {
        eprintln!("Error: {:#}", e);
        process::exit(1);
    }
}

fn run() -> Result<()> {
    let args: Vec<String> = env::args().collect();
    if args.len() < 3 {
        eprintln!("Usage: wasm-gas <input.wasm> <output.wasm>");
        eprintln!("Instruments a WASM module with gas metering calls.");
        process::exit(1);
    }

    let input_path = &args[1];
    let output_path = &args[2];

    let config = ModuleConfig::new();
    let mut module = Module::from_file_with_config(input_path, &config)
        .with_context(|| format!("Failed to parse WASM file: {}", input_path))?;

    wasm_gas::instrument(&mut module)?;

    module
        .emit_wasm_file(output_path)
        .with_context(|| format!("Failed to write output file: {}", output_path))?;

    let size = fs::metadata(output_path).map(|m| m.len()).unwrap_or(0);
    eprintln!(
        "Successfully instrumented {} -> {} ({} bytes)",
        input_path, output_path, size
    );

    Ok(())
}
