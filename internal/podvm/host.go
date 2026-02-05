package podvm

import (
	"context"

	"github.com/tetratelabs/wazero/api"
)

// execContext holds the execution state for a single WASM invocation.
type execContext struct {
	input        []byte     // input is the FlatBuffers-encoded PodExecuteInput
	output       []byte     // output is the FlatBuffers-encoded PodExecuteOutput
	memory       api.Memory // memory is the WASM linear memory
	gasLimit     uint64     // gasLimit is the maximum gas allowed
	gasUsed      uint64     // gasUsed tracks consumed gas
	gasExhausted bool       // gasExhausted is true if gas limit was exceeded
}

// buildHostModule creates the "env" module with host functions.
func (p *Pool) buildHostModule(ctx context.Context, execCtx *execContext) (api.Module, error) {
	return p.runtime.NewHostModuleBuilder("env").
		NewFunctionBuilder().
		WithFunc(func(ctx context.Context, cost uint32) {
			hostGas(ctx, execCtx, cost)
		}).
		Export("gas").
		NewFunctionBuilder().
		WithFunc(func(ctx context.Context) uint32 {
			return hostInputLen(execCtx)
		}).
		Export("input_len").
		NewFunctionBuilder().
		WithFunc(func(ctx context.Context, ptr uint32) {
			hostReadInput(execCtx, ptr)
		}).
		Export("read_input").
		NewFunctionBuilder().
		WithFunc(func(ctx context.Context, ptr, len uint32) {
			hostWriteOutput(execCtx, ptr, len)
		}).
		Export("write_output").
		Instantiate(ctx)
}

// hostGas handles gas metering.
// Panics if gas limit is exceeded to abort execution.
func hostGas(ctx context.Context, execCtx *execContext, cost uint32) {
	execCtx.gasUsed += uint64(cost)

	if execCtx.gasUsed > execCtx.gasLimit {
		execCtx.gasExhausted = true
		panic("gas exhausted")
	}
}

// hostInputLen returns the length of the input buffer.
func hostInputLen(execCtx *execContext) uint32 {
	return uint32(len(execCtx.input))
}

// hostReadInput copies the input buffer into WASM memory at the given pointer.
func hostReadInput(execCtx *execContext, ptr uint32) {
	if execCtx.memory == nil || len(execCtx.input) == 0 {
		return
	}

	execCtx.memory.Write(ptr, execCtx.input)
}

// hostWriteOutput reads the output from WASM memory and stores it.
func hostWriteOutput(execCtx *execContext, ptr, length uint32) {
	if execCtx.memory == nil || length == 0 {
		return
	}

	data, ok := execCtx.memory.Read(ptr, length)
	if !ok {
		return
	}

	execCtx.output = make([]byte, length)
	copy(execCtx.output, data)
}
