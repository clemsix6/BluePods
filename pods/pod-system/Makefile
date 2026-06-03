.PHONY: build release clean test instrumenter

WASM_GAS := ../../wasm-gas/target/release/wasm-gas
RAW_WASM := target/wasm32-unknown-unknown/release/pod_system.wasm

# Default target
all: release

# Debug build (not instrumented)
build:
	cargo build --target wasm32-unknown-unknown

# Build the gas-instrumentation tool (host binary).
instrumenter:
	cargo build --release --manifest-path ../../wasm-gas/Cargo.toml

# Release build: compile the pod, then instrument it with gas metering so the
# deployed build/pod.wasm enforces the gas budget (loops included).
release: instrumenter
	cargo build --target wasm32-unknown-unknown --release
	mkdir -p build
	$(WASM_GAS) $(RAW_WASM) build/pod.wasm

# Clean build artifacts
clean:
	cargo clean
	rm -rf build

# Run Go tests (requires release build)
test: release
	cd ../.. && go test ./internal/podvm/... -v -run "Test(Mint|Transfer|RegisterValidator|Unknown|Pool)"
