# Integration Test Suite

## Overview

BDD-style tests organized as simulations. Each simulation starts a cluster of N nodes, runs ATP test cases as `t.Run` subtests, then tears down the cluster automatically via `t.Cleanup`.

The node is QUIC only. There is no HTTP anywhere. Tests talk to a node over the QUIC client surface through the Go SDK (`client/`) and the client daemon (`daemon/`). A transaction that touches only singletons is submitted raw and wrapped by the validator; a transaction that touches replicated objects is collected off-chain by the daemon (rendezvous hashing, attestation collection, ATX assembly) and submitted as an attested transaction.

The receiving validator adds the transaction to its pending set and forwards it to mesh peers over the one-way gossip stream, tagged so peers tell it apart from a vertex and add it to their own pending sets rather than misparsing it. Because several producers may then include the same transaction, the commit path keeps a per-transaction-hash committed-once guard: the first occurrence executes and every later one is skipped, so object creation, fees, and validator changes are never applied twice.

## Prerequisites

- Build the system pod: `cd pods/pod-system && make`
- Go 1.26

## Running tests

```bash
# All simulations
go test ./test/integration/ -v -count=1 -timeout 30m

# Single simulation
go test ./test/integration/ -v -run TestSimBootstrap -count=1 -timeout 12m

# Single ATP item
go test ./test/integration/ -v -run "TestSimBootstrap/tx-validation/ATP-1.1" -count=1
```

## Architecture

```
test/integration/
  TESTING.md              Documentation
  helpers.go              Cluster, Node, lifecycle management
  helpers_quic.go         QUIC queries, assertions, polling helpers
  helpers_tx.go           Malformed transaction builders and submission helpers
  sim_bootstrap_test.go   1-node simulation
  sim_consensus_test.go   5-node simulation
  sim_fees_test.go        5-node simulation
  sim_epochs_test.go      10-node simulation
  sim_objects_test.go     12-node simulation
  sim_stress_test.go      12-node simulation
  sim_progressive_test.go progressive and batch joining
  sim_aggregation_test.go off-chain aggregation scenarios
```

### helpers.go

Core infrastructure for cluster lifecycle.

- `Cluster` manages N nodes with lifecycle, cleanup, and polling helpers, and holds the system pod ID (the BLAKE3 hash of the pod WASM) so clients can be constructed without discovering it over the wire.
- `Node` wraps an `exec.Cmd` with thread-safe log capture. `Addr()` returns the node's QUIC address, which every client and operational query uses.
- `ClusterOption` configures port bases, epoch length, min validators, and the transition window.
- `NewCluster(t, size, opts...)` builds the binary, starts the bootstrap and validators (each with only a `--quic` address, no HTTP), and registers `t.Cleanup`.
- `Client(i)` returns a `client.Client` connected to node `i` over QUIC with the cluster's system pod ID.
- `WaitReady` / `WaitForRound` / `WaitForEpoch` / `WaitForValidators` poll the QUIC status message.

### helpers_quic.go

QUIC queries and assertions, backed by a cached SDK transport per node address.

- `QueryStatus` / `QueryStatusSafe` read the status message (round, last committed, validators, epoch, epoch holders, system pod).
- `QueryHealth` / `QueryValidators` / `QueryObject` / `QueryObjectLocal` / `QueryDomain` are the QUIC equivalents of the former HTTP endpoints. `QueryObjectLocal` sets the local-only flag so a non-holder answers not-found instead of routing.
- `FaucetAndWait`, `WaitForObject`, `WaitForOwner`, `WaitForHolders`, `CountHolders`, `ReadBalance` are unchanged in intent and now run over QUIC.

### helpers_tx.go

Malformed transaction builders for negative testing, plus the submission helpers.

- `SubmitRawBytes` submits arbitrary bytes over the QUIC submit-tx message and maps the result onto the former HTTP codes: the node accepting the transaction maps to 202, rejecting it at ingress maps to 400.
- `SubmitFaucet` requests a faucet mint over QUIC and maps amount-zero or invalid-pubkey to 400 and acceptance to 202.
- `BuildTxWith*` build deliberately malformed transactions for the validation tests.

## Simulations

### TestSimBootstrap (1 node)

Single bootstrap node. Tests transaction validation, the QUIC operational and query messages, the faucet, and pod VM operations.

### TestSimConsensus (5 nodes)

Tests DAG consensus, client operations (faucet, split, transfer, object create), version tracking, security (replay, hash tampering, signature forgery, non-owner transfer), and convergence.

### TestSimFees (5 nodes)

Tests fee deduction, gas-coin validation, and fee consistency across nodes. Fees follow the 70 epoch / 30 burn split with no aggregator share.

### TestSimEpochs (10 nodes, epoch length 50)

Tests epoch transitions, the frozen epoch-holder snapshot, validator addition and deregistration with churn limiting, and stability. Validator registration carries only a QUIC address.

### TestSimObjects (12 nodes)

Tests object sharding, rendezvous hashing determinism and diversity, singleton replication to all validators, replication exceeding the validator count, inter-node object routing over QUIC, and the local-only flag.

### TestSimStress (12 nodes, epoch length 50)

Tests concurrent modification, double-spend and replay under load, throughput, and transactions during epoch transitions.

### TestSimProgressiveJoining / TestSimBatchJoining

Start a cluster, then add nodes in one batch or in several, and verify all nodes converge on the same validator set and round.

### TestSimAggregation / TestSimAggregationEpochBoundary

These exercise the moved-to-client design directly. The first four run on a 5-node cluster (`TestSimAggregation`); the epoch-boundary scenario runs on a separate short-epoch cluster (`TestSimAggregationEpochBoundary`).

- **End-to-end client aggregation.** A transfer of a replicated object is collected by the daemon and submitted as an attested transaction; every holder converges on the new owner and an advanced version.
- **Singleton fast path.** A coin transfer is submitted raw and wrapped by the validator into a trivial attested transaction, with no attestation round, and executes on all validators.
- **Version race during collection.** Two concurrent transfers of one replicated object contend during attestation collection, forcing the daemon's bounded-backoff retry; the object stays readable and ends owned by exactly one contender, never corrupted.
- **Epoch boundary during attestation validity.** A chain of replicated transfers spans an epoch boundary. An attestation collected late in an epoch and committed shortly into the next is accepted within the grace window; one committed past the grace window is rejected and the daemon resyncs and recollects against the new epoch. Every transfer eventually commits.
- **Cold-holder scenario.** Back-to-back transfers attest a just-produced version with no delay: eager signing at execution plus the bounded sign-on-miss fallback means a holder always serves a signature for a held current version, leaving no exploitable cold window.

The genesis epoch keeps no frozen holder snapshot. It tracks the live validator set so attestation verification recomputes holders against the same converged set the client daemon syncs; the first frozen snapshot is taken at the first epoch boundary.

## Adding new tests

1. Pick the simulation by the cluster size needed.
2. Add a `t.Run("ATP-X.Y: description", ...)` subtest in the appropriate `run*Tests` function.
3. Use the helpers in `helpers_quic.go` and `helpers_tx.go`. Add a new helper there if needed.
4. Run the specific simulation to verify: `go test ./test/integration/ -v -run TestSimXxx -count=1`.
