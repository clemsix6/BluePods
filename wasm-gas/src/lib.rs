//! Instruments a WASM module with gas metering.
//!
//! A `gas(n)` call is injected at the start of every instruction sequence: the
//! function body and every nested block, loop, and if/else arm, where `n` is the
//! number of instructions directly in that sequence. Because a loop's body
//! sequence is re-entered at the top on each iteration, this charges gas per
//! iteration, so loops and branches are metered, not only the function entry.
//! Each instruction costs 1 gas.

use anyhow::Result;
use walrus::ir::{Call, Const, Instr, InstrLocId, InstrSeqId, Value};
use walrus::{FunctionId, FunctionKind, ImportKind, LocalFunction, Module, ValType};

/// instrument injects gas-metering calls into every local function of the module.
pub fn instrument(module: &mut Module) -> Result<()> {
    let gas_fn = add_gas_import(module);

    let func_ids: Vec<FunctionId> = module
        .funcs
        .iter()
        .filter_map(|f| match f.kind {
            FunctionKind::Local(_) => Some(f.id()),
            _ => None,
        })
        .collect();

    for func_id in func_ids {
        instrument_function(module, func_id, gas_fn);
    }

    Ok(())
}

/// add_gas_import returns the existing or newly added `env.gas(i32)` import.
fn add_gas_import(module: &mut Module) -> FunctionId {
    for import in module.imports.iter() {
        if import.module == "env" && import.name == "gas" {
            if let ImportKind::Function(fid) = import.kind {
                return fid;
            }
        }
    }

    let ty = module.types.add(&[ValType::I32], &[]);
    let (func_id, _) = module.add_import_func("env", "gas", ty);

    func_id
}

/// instrument_function prepends a gas charge to every instruction sequence in the
/// function. It collects sequence ids and their original instruction counts first
/// (injection changes counts but never sequence ids), then injects into each.
fn instrument_function(module: &mut Module, func_id: FunctionId, gas_fn: FunctionId) {
    let mut seqs = Vec::new();

    {
        let local_func = match &module.funcs.get(func_id).kind {
            FunctionKind::Local(l) => l,
            _ => return,
        };
        collect_seqs(local_func, local_func.entry_block(), &mut seqs);
    }

    let local_func = match &mut module.funcs.get_mut(func_id).kind {
        FunctionKind::Local(l) => l,
        _ => return,
    };

    for (seq_id, count) in seqs {
        if count == 0 {
            continue;
        }
        prepend_gas(local_func, seq_id, count, gas_fn);
    }
}

/// collect_seqs records a sequence id with its instruction count, then recurses
/// into every nested block, loop, and if/else arm.
fn collect_seqs(func: &LocalFunction, seq_id: InstrSeqId, out: &mut Vec<(InstrSeqId, u32)>) {
    let seq = func.block(seq_id);
    out.push((seq_id, seq.instrs.len() as u32));

    for (instr, _) in seq.instrs.iter() {
        match instr {
            Instr::Block(b) => collect_seqs(func, b.seq, out),
            Instr::Loop(l) => collect_seqs(func, l.seq, out),
            Instr::IfElse(ie) => {
                collect_seqs(func, ie.consequent, out);
                collect_seqs(func, ie.alternative, out);
            }
            _ => {}
        }
    }
}

/// prepend_gas inserts `i32.const count; call gas` at the start of a sequence.
fn prepend_gas(func: &mut LocalFunction, seq_id: InstrSeqId, count: u32, gas_fn: FunctionId) {
    let block = func.block_mut(seq_id);

    let mut new_instrs: Vec<(Instr, InstrLocId)> = Vec::with_capacity(block.instrs.len() + 2);
    new_instrs.push((
        Instr::Const(Const {
            value: Value::I32(count as i32),
        }),
        InstrLocId::default(),
    ));
    new_instrs.push((Instr::Call(Call { func: gas_fn }), InstrLocId::default()));
    new_instrs.extend(block.instrs.drain(..));
    block.instrs = new_instrs;
}

#[cfg(test)]
mod tests {
    use super::*;

    // A function whose work is entirely inside a loop body. The old entry-only
    // instrumentation left this loop body unmetered.
    const LOOP_WAT: &str = r#"
        (module
          (func (export "run") (param $n i32) (result i32)
            (local $i i32)
            (loop $l
              (local.set $i (i32.add (local.get $i) (i32.const 1)))
              (br_if $l (i32.lt_s (local.get $i) (local.get $n))))
            (local.get $i)))
    "#;

    // gas_import_id returns the env.gas function id, if present.
    fn gas_import_id(module: &Module) -> Option<FunctionId> {
        module.imports.iter().find_map(|i| {
            if i.module == "env" && i.name == "gas" {
                if let ImportKind::Function(fid) = i.kind {
                    return Some(fid);
                }
            }
            None
        })
    }

    // loop_body_has_gas walks the function and reports whether every loop body
    // sequence starts with a gas charge (const + call gas), and that at least one
    // loop was checked.
    fn loop_body_has_gas(
        func: &LocalFunction,
        seq_id: InstrSeqId,
        gas_fn: FunctionId,
        loops: &mut usize,
    ) -> bool {
        let seq = func.block(seq_id);
        let mut ok = true;

        for (instr, _) in seq.instrs.iter() {
            match instr {
                Instr::Loop(l) => {
                    *loops += 1;
                    if !starts_with_gas(func, l.seq, gas_fn) {
                        ok = false;
                    }
                    ok &= loop_body_has_gas(func, l.seq, gas_fn, loops);
                }
                Instr::Block(b) => ok &= loop_body_has_gas(func, b.seq, gas_fn, loops),
                Instr::IfElse(ie) => {
                    ok &= loop_body_has_gas(func, ie.consequent, gas_fn, loops);
                    ok &= loop_body_has_gas(func, ie.alternative, gas_fn, loops);
                }
                _ => {}
            }
        }

        ok
    }

    // starts_with_gas reports whether a sequence begins with `i32.const _; call gas`.
    fn starts_with_gas(func: &LocalFunction, seq_id: InstrSeqId, gas_fn: FunctionId) -> bool {
        let seq = func.block(seq_id);
        if seq.instrs.len() < 2 {
            return false;
        }
        let is_const = matches!(seq.instrs[0].0, Instr::Const(_));
        let is_gas_call = matches!(seq.instrs[1].0, Instr::Call(Call { func }) if func == gas_fn);

        is_const && is_gas_call
    }

    fn local_func(module: &Module) -> &LocalFunction {
        module
            .funcs
            .iter()
            .find_map(|f| match &f.kind {
                FunctionKind::Local(l) => Some(l),
                _ => None,
            })
            .expect("a local function")
    }

    #[test]
    fn meters_loop_body_per_iteration() {
        let wasm = wat::parse_str(LOOP_WAT).expect("parse wat");
        let mut module = Module::from_buffer(&wasm).expect("parse wasm");

        instrument(&mut module).expect("instrument");

        let gas_fn = gas_import_id(&module).expect("gas import present");
        let func = local_func(&module);

        // The function body itself must be metered.
        assert!(
            starts_with_gas(func, func.entry_block(), gas_fn),
            "entry sequence not metered"
        );

        // Every loop body must be metered (this is what the entry-only pass missed),
        // and the module must actually contain a loop to make the check meaningful.
        let mut loops = 0usize;
        let ok = loop_body_has_gas(func, func.entry_block(), gas_fn, &mut loops);
        assert!(loops >= 1, "test module has no loop");
        assert!(ok, "a loop body was left unmetered");
    }

    #[test]
    fn idempotent_gas_import() {
        // Instrumenting twice must not add a second env.gas import.
        let wasm = wat::parse_str(LOOP_WAT).expect("parse wat");
        let mut module = Module::from_buffer(&wasm).expect("parse wasm");

        instrument(&mut module).expect("instrument once");
        instrument(&mut module).expect("instrument twice");

        let gas_imports = module
            .imports
            .iter()
            .filter(|i| i.module == "env" && i.name == "gas")
            .count();
        assert_eq!(gas_imports, 1, "duplicate gas import");
    }
}
