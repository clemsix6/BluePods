.PHONY: build release clean test

# Default target
all: release

# Debug build
build:
	cargo build --target wasm32-unknown-unknown

# Release build (optimized)
release:
	cargo build --target wasm32-unknown-unknown --release
	mkdir -p build
	cp target/wasm32-unknown-unknown/release/pod_system.wasm build/pod.wasm

# Clean build artifacts
clean:
	cargo clean
	rm -rf build

# Run Go tests (requires release build)
test: release
	cd ../.. && go test ./internal/podvm/... -v -run "Test(Mint|Transfer|RegisterValidator|Unknown)"