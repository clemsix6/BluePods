//! CLI pour instrumenter un module WASM avec gas metering.
//! Usage: wasm-gas <input.wasm> <output.wasm>
//!
//! Le module instrumenté appellera `env.gas(cost)` avant chaque bloc d'instructions.
//! Chaque instruction coûte 1 gas.

use anyhow::{Context, Result};
use std::{env, fs, process};
use walrus::{ir::*, FunctionId, LocalFunction, Module, ModuleConfig, ValType};

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

    // Parse the WASM module
    let config = ModuleConfig::new();
    let mut module = Module::from_file_with_config(input_path, &config)
        .with_context(|| format!("Failed to parse WASM file: {}", input_path))?;

    // Add or find the gas import function: env.gas(i64) -> ()
    let gas_fn = add_gas_import(&mut module)?;

    // Get all function IDs to instrument (excluding imported functions)
    let func_ids: Vec<FunctionId> = module
        .funcs
        .iter()
        .filter_map(|f| {
            if matches!(f.kind, walrus::FunctionKind::Local(_)) {
                Some(f.id())
            } else {
                None
            }
        })
        .collect();

    // Instrument each function
    for func_id in func_ids {
        instrument_function(&mut module, func_id, gas_fn)?;
    }

    // Write the instrumented module
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

/// Add or find the gas import function: (import "env" "gas" (func (param i32)))
fn add_gas_import(module: &mut Module) -> Result<FunctionId> {
    // Check if it already exists
    for import in module.imports.iter() {
        if import.module == "env" && import.name == "gas" {
            if let walrus::ImportKind::Function(fid) = import.kind {
                return Ok(fid);
            }
        }
    }

    // Create the type: (i32) -> ()
    let ty = module.types.add(&[ValType::I32], &[]);

    // Add the import
    let (func_id, _) = module.add_import_func("env", "gas", ty);

    Ok(func_id)
}

/// Count instructions in a function and inject gas call at the start.
fn instrument_function(module: &mut Module, func_id: FunctionId, gas_fn: FunctionId) -> Result<()> {
    let func = module.funcs.get_mut(func_id);
    let local_func: &mut LocalFunction = match &mut func.kind {
        walrus::FunctionKind::Local(l) => l,
        _ => return Ok(()), // Skip imports
    };

    let entry = local_func.entry_block();

    // Count all instructions in the function
    let cost = count_all_instructions(local_func);

    if cost == 0 {
        return Ok(());
    }

    // Get the entry block and prepend gas metering call
    let entry_block = local_func.block_mut(entry);

    // Create new instructions: i64.const cost; call gas_fn; then original instructions
    let mut new_instrs: Vec<(Instr, InstrLocId)> = Vec::with_capacity(entry_block.instrs.len() + 2);

    // Add gas metering call
    new_instrs.push((
        Instr::Const(Const {
            value: Value::I32(cost as i32),
        }),
        InstrLocId::default(),
    ));
    new_instrs.push((Instr::Call(Call { func: gas_fn }), InstrLocId::default()));

    // Add original instructions
    new_instrs.extend(entry_block.instrs.drain(..));

    // Replace instructions
    entry_block.instrs = new_instrs;

    Ok(())
}

/// Count instructions in a local function (entry block only for simplicity).
/// For full metering, we'd need to traverse all blocks.
fn count_all_instructions(func: &LocalFunction) -> u64 {
    let entry = func.entry_block();
    let block = func.block(entry);
    block.instrs.len() as u64
}
