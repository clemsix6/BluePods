package podvm

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/zeebo/blake3"
)

var (
	// ErrModuleNotFound is returned when a module ID is not found in the pool.
	ErrModuleNotFound = errors.New("module not found")

	// ErrGasExhausted is returned when execution runs out of gas.
	ErrGasExhausted = errors.New("gas exhausted")
)

// Pool manages a pool of compiled WASM modules.
// Modules are compiled once and kept hot-loaded for fast instantiation.
type Pool struct {
	runtime wazero.Runtime            // runtime is the wazero runtime instance
	modules map[[32]byte]wazero.CompiledModule // modules maps blake3 hash to compiled module
	mu      sync.RWMutex              // mu protects modules map
}

// New creates a new Pool with an initialized wazero runtime.
func New() *Pool {
	ctx := context.Background()
	runtime := wazero.NewRuntime(ctx)

	return &Pool{
		runtime: runtime,
		modules: make(map[[32]byte]wazero.CompiledModule),
	}
}

// Load compiles and stores a WASM module.
// If customID is nil, uses the blake3 hash of wasmBytes as the module ID.
// If customID is provided, uses it as the module ID.
// Returns the module ID used.
func (p *Pool) Load(wasmBytes []byte, customID *[32]byte) ([32]byte, error) {
	var id [32]byte
	if customID != nil {
		id = *customID
	} else {
		id = blake3.Sum256(wasmBytes)
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	if _, exists := p.modules[id]; exists {
		return id, nil
	}

	compiled, err := p.runtime.CompileModule(context.Background(), wasmBytes)
	if err != nil {
		return [32]byte{}, fmt.Errorf("compile module: %w", err)
	}

	p.modules[id] = compiled

	return id, nil
}

// Execute runs a module with the given input and gas limit.
// Returns the output bytes and the amount of gas consumed.
func (p *Pool) Execute(id [32]byte, input []byte, gasLimit uint64) ([]byte, uint64, error) {
	p.mu.RLock()
	compiled, exists := p.modules[id]
	p.mu.RUnlock()

	if !exists {
		return nil, 0, ErrModuleNotFound
	}

	return p.executeModule(compiled, input, gasLimit)
}

// executeModule instantiates and runs a compiled module.
func (p *Pool) executeModule(compiled wazero.CompiledModule, input []byte, gasLimit uint64) ([]byte, uint64, error) {
	ctx := context.Background()

	execCtx := &execContext{
		input:    input,
		gasLimit: gasLimit,
		gasUsed:  0,
	}

	hostModule, err := p.buildHostModule(ctx, execCtx)
	if err != nil {
		return nil, 0, fmt.Errorf("build host module: %w", err)
	}
	defer hostModule.Close(ctx)

	instance, err := p.runtime.InstantiateModule(ctx, compiled, wazero.NewModuleConfig())
	if err != nil {
		return nil, execCtx.gasUsed, fmt.Errorf("instantiate module: %w", err)
	}
	defer instance.Close(ctx)

	execCtx.memory = instance.Memory()

	return p.callExecute(ctx, instance, execCtx)
}

// callExecute calls the execute function on the WASM instance.
func (p *Pool) callExecute(ctx context.Context, instance api.Module, execCtx *execContext) ([]byte, uint64, error) {
	executeFn := instance.ExportedFunction("execute")
	if executeFn == nil {
		return nil, execCtx.gasUsed, fmt.Errorf("execute function not exported")
	}

	_, err := executeFn.Call(ctx)
	if err != nil {
		if execCtx.gasExhausted {
			return nil, execCtx.gasUsed, ErrGasExhausted
		}

		return nil, execCtx.gasUsed, fmt.Errorf("execute: %w", err)
	}

	return execCtx.output, execCtx.gasUsed, nil
}

// Unload removes a module from the pool.
func (p *Pool) Unload(id [32]byte) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if compiled, exists := p.modules[id]; exists {
		compiled.Close(context.Background())
		delete(p.modules, id)
	}
}

// Close releases all resources held by the pool.
func (p *Pool) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()

	for id, compiled := range p.modules {
		compiled.Close(context.Background())
		delete(p.modules, id)
	}

	return p.runtime.Close(context.Background())
}
