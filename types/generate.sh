#!/bin/bash
# Generates Go and Rust files from FlatBuffers schemas

set -e

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PROJECT_ROOT="$(dirname "$SCRIPT_DIR")"

# Generate Go types
flatc --go -o "$PROJECT_ROOT/internal" \
    "$SCRIPT_DIR/object.fbs" \
    "$SCRIPT_DIR/transaction.fbs" \
    "$SCRIPT_DIR/podio.fbs" \
    "$SCRIPT_DIR/validator.fbs" \
    "$SCRIPT_DIR/domain.fbs" \
    "$SCRIPT_DIR/vertex.fbs" \
    "$SCRIPT_DIR/snapshot.fbs"

echo "Generated Go files in internal/types/"

# Generate Rust types for pod-sdk
RUST_OUT="$PROJECT_ROOT/pods/pod-sdk/src"
mkdir -p "$RUST_OUT"

flatc --rust -o "$RUST_OUT" \
    "$SCRIPT_DIR/object.fbs" \
    "$SCRIPT_DIR/transaction.fbs" \
    "$SCRIPT_DIR/podio.fbs" \
    "$SCRIPT_DIR/validator.fbs" \
    "$SCRIPT_DIR/domain.fbs" \
    "$SCRIPT_DIR/vertex.fbs"

# Add re-exports to generated Rust files
# This fixes cross-module imports (e.g., podio using Object from object)
for file in "$RUST_OUT"/*_generated.rs; do
    if ! grep -q "pub use types::\*;" "$file"; then
        echo "" >> "$file"
        echo "pub use types::*;" >> "$file"
    fi
done

echo "Generated Rust files in pods/pod-sdk/src/"
