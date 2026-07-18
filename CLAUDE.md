# BluePods Project

## Context Files (IMPORTANT)

Before working on this project, read the two documentation pillars:

- **`docs/VISION.md`** for why the project exists: the decentralized-cloud goal, the non-negotiable properties (zero rollback, global atomic composability), the accepted tradeoff, and the positioning against ICP, Sui, Solana, and Ethereum.
- **`docs/WHITEPAPER.md`** for how it works: object model, consensus, attestation, execution sharding, fees, validators, network, and security.

The test environment guide is `test/TESTING.md`.

## Documentation Conventions

When writing or editing the docs:

- One document of record per subject: VISION owns the why, WHITEPAPER owns the how. Never cover the same topic in both.
- Prose for reasoning, structure (tables, lists) for data. Sentence-case headings, straight quotes, no em dashes, bold only where it earns its place.
- VISION carries opinion and positioning; the whitepaper stays sober and factual.
- Edit in place. No files named v2, old, or draft. Dates come from git, so docs do not repeat a last-updated line.

## Project Layout

The node is written in Go; pods are written in Rust and compiled to WebAssembly. The pillars (coarse on purpose — the code is its own map):

- `cmd/node/` — node entrypoint and CLI; `internal/` — the node implementation, split by subsystem.
- `pkg/` — the Go surface consumed from outside the node: `client` (talking to a node) and `daemon` (off-chain attestation collection).
- `types/` — generated FlatBuffers types shared across the node.
- `pods/` — the Rust side: `pod-sdk` (dispatcher, Context, serialization) and `pod-system` (the system pod).
- `wasm-gas/` — Rust tool that instruments a pod's WASM with gas metering (at the root, not under `pods/`).
- `test/` — the scenario harness (`harness/`), the scenario corpus (`scenarios/`), and the environment's document of record, `TESTING.md`.
- `docs/superpowers/specs/` and `docs/superpowers/plans/` — the specs and plans of the development workflow below.

@~/Skills/general.md
@~/Skills/go-style.md
@~/Skills/commit-convention.md
@~/Skills/git-workflow.md
@~/Skills/feature-pipeline.md

## BluePods Pipeline Deltas

- When a batch touches `pods/` or `wasm-gas/`, their builds and tests must pass
  too before the batch is considered green.
- Heavy batches (consensus, multi-file, hours of work) are implemented
  task-by-task by successive fresh subagents — a single agent must never carry
  hours of dense work in one context. Implementers commit as soon as unit tests
  pass; slow integration sims run AFTER the commit (a failure becomes a
  follow-up fix commit).
- Every new state mutation gets an `internal/events` constructor, every new
  feature extends or adds a scenario in `test/scenarios/`, and renaming or
  removing an event or attribute is a breaking change to call out in the
  commit that does it (see `test/TESTING.md`).
