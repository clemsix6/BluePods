# BluePods Project

## Context Files (IMPORTANT)

Before working on this project:

- **ALWAYS read `bluepods_v2.md`** at the project root to understand the blockchain architecture, consensus mechanism, and data model
- **ALWAYS read `pods/ARCHITECTURE.md`** before working in the `pods/` directory to understand the WASM runtime, gas metering, and instance pooling

## Project Structure

### BluePods-Node/

The blockchain node implementation. Contains:

- `cmd/` - Command-line interface
- `consensus/` - DAG-based consensus (Mysticeti v2)
- `crypto/` - Cryptographic primitives
- `podvm/` - WASM runtime for pod execution (wazero-based)
- `storage/` - State storage
- `types/` - Node-specific types

### BluePods-Shared/

Shared types and functions exchanged between the node and pods. This package defines the interface contract for verify/execute functions and transaction structures.

### pods/BluePods-Interface/

The SDK/library used by all pods. Provides:

- Serialization helpers
- Cryptographic functions accessible from WASM
- I/O utilities for reading/writing transaction data
- Type definitions for the pod interface

### pods/BluePods-System/

The main system pod of the network (in development). Will handle:

- Native token management
- Staking operations
- Validator management
- Core protocol operations

### pods/BluePods-Stress/

A stress test pod used by tests and benchmarks in `BluePods-Node/podvm/`. Designed to measure runtime performance under load.

### pods/wasm-gas/

Rust tool for instrumenting WASM modules with gas metering.

---

## Go Coding Standards

### KISS Principles (CRUCIAL)

- It is CRUCIAL to avoid over-engineering at all costs
- Keep everything simple: Keep It Simple, Stupid (KISS)
- Do not create unnecessary or premature abstractions
- Prefer explicit and straightforward code over "clever" code
- Do not add features "just in case" - only implement what is actually needed
- Avoid complex design patterns when a simple solution suffices
- The simplest code that works is often the best code

### Minimal Public API (CRUCIAL)

- Keep package APIs as small as possible - fewer public functions/methods/structs is better
- Start with the absolute minimum: if you only need 2 functions, expose only 2 functions
- Add new public API elements only when there is an actual, concrete need for them
- Do not expose functions "just in case" someone might need them later
- Prefer unexported (private) by default - only export what is strictly necessary
- A package with 3 well-designed public functions is better than one with 15 "convenient" ones
- Question every exported symbol: "Is this really needed by external code?"

### Documentation Requirements

- Every function, type, struct, and interface must have a docstring
- Every struct field must be documented with inline comments
- All code and documentation must be written in English
- Use Go's standard documentation format (start with the name being documented)
- Do NOT add docstrings before package declarations - Package declarations should not have documentation comments above them

### Code Style

- Maximize code readability with generous spacing
- Use descriptive variable and function names
- Follow Go naming conventions (PascalCase for exported, camelCase for unexported)

### Function Size (CRUCIAL)

- Functions MUST be short and focused - aim for 15-25 lines maximum
- A function that does multiple tasks MUST be split into smaller sub-functions
- If a function exceeds 30 lines, it is almost certainly doing too much
- Each function should have ONE clear responsibility
- Long functions (50+ lines) are NEVER acceptable - split them into well-named helper functions
- Prefer 3 small focused functions over 1 large function doing everything
- Good code reads like a story: the main function orchestrates, sub-functions execute specific tasks

### File Size (CRUCIAL)

- Files MUST stay focused and manageable - aim for 200-300 lines maximum
- A file that exceeds 400 lines is almost certainly doing too much and MUST be split
- Each file should have ONE clear responsibility or theme
- Split by logical domain: initialization, networking, sync, routing, etc.
- Prefer 4 small focused files over 1 large file doing everything
- File names should clearly describe their content (e.g., `sync.go`, `handlers.go`, `routing.go`)
- **Exception**: a file can exceed 400 lines if it represents a single cohesive type with tightly coupled methods (e.g., a core struct and all its methods). Only accept this when splitting would scatter related logic across files and hurt readability

### TODO Comments

- Use `// TODO:` comments to mark incomplete implementations or future improvements
- This helps avoid doing everything at once and prevents forgetting items for later
- Format: `// TODO: description of what needs to be done`
- Example: `// TODO: add validation for negative amounts`

### Error Handling

- Always wrap errors with `fmt.Errorf` to add context about where/why the error occurred
- Use `%w` verb to preserve the original error for error unwrapping
- Include `\n` before `%w` for better readability in deep error stacks
- Never return raw errors without context - makes debugging much harder
- Format: `fmt.Errorf("operation description:\n%w", err)`
- Example: `return nil, fmt.Errorf("failed to read config file:\n%w", err)`

### Example Documentation Style

```go
package user

import (
	"context"
	"time"
)

// User represents a user entity in the system.
// It contains authentication and profile information.
type User struct {
	ID        int64     `json:"id"`         // ID is the unique identifier for the user
	Email     string    `json:"email"`      // Email is the user's email address used for authentication
	CreatedAt time.Time `json:"created_at"` // CreatedAt stores when the user account was created
}

// CreateUser creates a new user in the database.
// It validates the input and returns the created user with generated ID.
func CreateUser(ctx context.Context, input CreateUserInput) (*User, error) {
	// ...
}
```

## Git Commit Convention

All commits across TrueWallet repositories follow this concise format:

- **[+]** Feature addition
- **[-]** Feature removal
- **[&]** Changes, refactors, updates
- **[!]** Bug fixes

**Format**:
- **ONE modification per line** (never concatenate multiple changes on the same line)
- Major change on its own line with prefix `[]`
- Minor related changes each on their own line with `\t-` (tab + dash)
- Keep descriptions minimal and efficient

**Examples**:

Simple commit:
```
[!] Fix SMS service initialization
```

Commit with minor changes:
```
[+] Add user authentication
	- JWT token generation
	- Password hashing with bcrypt
	- Session middleware
```

Multiple major changes:
```
[&] Refactor database layer
	- Extract connection pooling
	- Add retry logic
[+] Redis caching support
	- Cache user sessions
	- TTL configuration
```

**IMPORTANT**: NO footers, NO "🤖 Generated with...", NO "Co-Authored-By: Claude". Keep commits clean and minimal.
