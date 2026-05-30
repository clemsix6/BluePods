# BluePods status

This is the gap between the design in WHITEPAPER.md and the code: what works, what is partial, what is missing, and the known spec-code mismatches.

## Done

These subsystems are implemented and exercised by the integration tests.

- DAG consensus: leaderless vertex production, quorum on parent links, the two-round commit rule, epoch transitions, and version tracking.
- Direct attestation: aggregator collection over QUIC, BLS signatures aggregated into quorum proofs, fail-fast on negative votes.
- Execution sharding: transactions executed by the holders of the objects they touch, holders computed by rendezvous hashing.
- Fee deduction: protocol-level deduction from the gas coin, the three-way split, and the per-vertex fee summary.
- Storage sharding: objects stored only by their holders, with the global 18-byte version tracker on every node.
- Domain naming: local Pebble registry, registration through pod output, resolution over the API.
- Validator management: registration and deregistration, churn limiting, epoch holder snapshots.
- Networking: full QUIC mesh, gossip with deduplication, snapshot creation and sync.
- Execution runtime: the wazero-based WASM runtime with module pooling and a fresh instance per transaction.
- System pod: mint, split, merge, transfer, create_nft, transfer_nft, register_validator, deregister_validator. (The old CLAUDE.md called this "in development"; the code implements all eight, so the code wins here.)

## In progress

Implemented but partial. These work in the common case but do not yet meet the design.

- Gas metering. The WASM instrumentation injects gas accounting, but it only counts the entry block of each function, so loops and nested blocks run uncounted (see wasm-gas). On top of that, the budget is hardcoded and ignores the transaction's declared max_gas (defaultGasLimit is always 10M). Until both are fixed, gas does not bound execution as the design intends. See WHITEPAPER.md, Execution Model.
- Epoch rewards. The reward share per validator is computed at each epoch boundary, but it is never credited to anyone (the aggregator credit path is a no-op). See WHITEPAPER.md, Validator Management.

## To do

Not built yet. These are the open problems carried over from the design.

- Fraud proofs for incorrect execution. Holders that execute the same transaction should agree; there is no mechanism yet to detect and penalize a holder that produces a divergent state. See WHITEPAPER.md, Security Analysis.
- Storage challenge system. No protocol yet to challenge a holder to prove it still stores its objects, nor a proof format or anti-spam rule.
- Aggregator failover. If an aggregator fails mid-collection, the transaction is lost and the user resubmits. No automatic handoff.
- Inactivity detection, progressive penalties, and slashing. An idle validator only misses rewards today; there is no detection, no graduated penalty, no forced removal.
- Cross-shard composability at scale. Transactions touching many objects with different holder sets face latency in the attestation phase. Efficient patterns are not defined, and this may bound which applications fit.
- Runtime hardening. No memory limit on WASM instances, and gas metering must cover all blocks (the In progress item above).

## Known spec-code gaps

Specific mismatches between the design and the code, each pointing at the section it contradicts. The first group is recorded in the acceptance test plan (ATP sections 24 and 25); the rest were found by code review.

- Transaction size limit is 1 MB in code, against a 48 KB figure in older notes. Pick one and make spec and code agree. (ATP 25.1)
- The 4 KB per-object limit is not enforced anywhere. (ATP 25.2; WHITEPAPER.md, Data Model)
- The minimum replication of 10 is not enforced; any uint16 is accepted. (ATP 25.3; WHITEPAPER.md, Storage Distribution)
- Vertex size is unbounded, and the pending-vertex buffer has no cap, which is a memory-pressure DoS surface. (ATP 25.4, 24.9)
- Aggregator fee credits are never distributed (the credit path is a no-op). (ATP 25.6; WHITEPAPER.md, Fee System)
- tracker.checkRefList has no FlatBuffer panic recovery, unlike the API validation path. (ATP 25.7)
- creditGasCoin can overflow and then silently returns without crediting, instead of failing. (ATP 24.13; WHITEPAPER.md, Fee System)
- The system pod's merge does not check for balance overflow when summing coins, so it can wrap in a release build. (WHITEPAPER.md, Execution Model)
- BLS keys have no proof of possession, which leaves a rogue-key risk in aggregation. (WHITEPAPER.md, Attestation and Aggregation)
- Several storage writes ignore the Pebble error return, which can diverge state silently on a write failure. (WHITEPAPER.md, Data Model)
