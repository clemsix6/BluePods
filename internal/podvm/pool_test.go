package podvm

import (
	"os"
	"path/filepath"
	"testing"

	"BluePods/internal/types"

	flatbuffers "github.com/google/flatbuffers/go"
)

// TestPool_LoadAndExecute tests loading a WASM module and executing it.
func TestPool_LoadAndExecute(t *testing.T) {
	wasmPath := findPodSystemWasm(t)

	wasmBytes, err := os.ReadFile(wasmPath)
	if err != nil {
		t.Fatalf("failed to read wasm: %v", err)
	}

	pool := New()
	defer pool.Close()

	id, err := pool.Load(wasmBytes, nil)
	if err != nil {
		t.Fatalf("failed to load module: %v", err)
	}

	input := buildTestInput()

	output, gasUsed, err := pool.Execute(id, input, 10_000_000)
	if err != nil {
		t.Fatalf("failed to execute: %v", err)
	}

	// With the instrumented pod this is the real per-instruction gas cost of a
	// create_object call; it must sit well under the 10M execution budget.
	t.Logf("gas consumed: %d", gasUsed)
	if gasUsed == 0 {
		t.Error("gasUsed is 0: pod is not instrumented (run: cd pods/pod-system && make release)")
	}

	if len(output) == 0 {
		t.Fatal("expected output bytes")
	}

	result := types.GetRootAsPodExecuteOutput(output, 0)
	if result.Error() != 0 {
		t.Errorf("expected error=0, got %d", result.Error())
	}

	t.Logf("execution successful: gasUsed=%d, outputLen=%d", gasUsed, len(output))
}

// TestPool_GasExhausted verifies that execution aborts when the gas budget is
// exceeded. A tiny budget is exhausted by the first metered block, which only
// happens once the pod is instrumented with wasm-gas (make release).
func TestPool_GasExhausted(t *testing.T) {
	wasmPath := findPodSystemWasm(t)

	wasmBytes, err := os.ReadFile(wasmPath)
	if err != nil {
		t.Fatalf("failed to read wasm: %v", err)
	}

	pool := New()
	defer pool.Close()

	id, err := pool.Load(wasmBytes, nil)
	if err != nil {
		t.Fatalf("failed to load module: %v", err)
	}

	_, _, err = pool.Execute(id, buildTestInput(), 10)
	if err != ErrGasExhausted {
		t.Fatalf("expected ErrGasExhausted with a tiny budget, got %v", err)
	}
}

// TestPool_ModuleNotFound tests that executing an unknown module returns an error.
func TestPool_ModuleNotFound(t *testing.T) {
	pool := New()
	defer pool.Close()

	var unknownID [32]byte

	_, _, err := pool.Execute(unknownID, nil, 1000)
	if err != ErrModuleNotFound {
		t.Errorf("expected ErrModuleNotFound, got %v", err)
	}
}

// findPodSystemWasm locates the pod-system WASM file.
func findPodSystemWasm(t *testing.T) string {
	t.Helper()

	// Prefer the instrumented build output (build/pod.wasm), which is what the
	// node actually loads. Gas metering is only exercised against this artifact.
	paths := []string{
		"../../pods/pod-system/build/pod.wasm",
		"pods/pod-system/build/pod.wasm",
	}

	for _, p := range paths {
		abs, err := filepath.Abs(p)
		if err != nil {
			continue
		}

		if _, err := os.Stat(abs); err == nil {
			return abs
		}
	}

	t.Skip("instrumented pod.wasm not found, run: cd pods/pod-system && make release")

	return ""
}

// buildTestInput creates a PodExecuteInput for the "create_object" function: a
// minimal load-and-execute smoke test that needs no input objects.
func buildTestInput() []byte {
	builder := flatbuffers.NewBuilder(512)

	txArgs := builder.CreateByteVector(encodeCreateObjectArgs())

	// Create transaction
	txHash := builder.CreateByteVector(make([]byte, 32))
	txSender := builder.CreateByteVector(make([]byte, 32))
	txPod := builder.CreateByteVector(make([]byte, 32))
	txFuncName := builder.CreateString("create_object")

	types.TransactionStart(builder)
	types.TransactionAddHash(builder, txHash)
	types.TransactionAddSender(builder, txSender)
	types.TransactionAddPod(builder, txPod)
	types.TransactionAddFunctionName(builder, txFuncName)
	types.TransactionAddArgs(builder, txArgs)
	tx := types.TransactionEnd(builder)

	// Create sender
	sender := builder.CreateByteVector(make([]byte, 32))

	// Create PodExecuteInput
	types.PodExecuteInputStart(builder)
	types.PodExecuteInputAddTransaction(builder, tx)
	types.PodExecuteInputAddSender(builder, sender)
	input := types.PodExecuteInputEnd(builder)

	types.FinishPodExecuteInputBuffer(builder, input)

	return builder.FinishedBytes()
}

// encodeCreateObjectArgs encodes create_object args in borsh format:
// owner [u8;32] + replication u16 LE + metadata Vec<u8> (u32 LE length + bytes).
func encodeCreateObjectArgs() []byte {
	buf := make([]byte, 32+2+4) // owner + replication + empty metadata length
	// owner all zeros, replication 0, metadata length 0
	return buf
}
