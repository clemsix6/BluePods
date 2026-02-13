# Integration Test Suite

## Overview

BDD-style tests organized as simulations. Each simulation starts a cluster of N nodes, runs ATP test cases as `t.Run` subtests, then tears down the cluster automatically via `t.Cleanup`.

## Prerequisites

- Build the system pod: `cd pods/pod-system && make`
- Go 1.21+

## Running Tests

```bash
# All simulations
go test ./test/integration/ -v -count=1 -timeout 30m

# Single simulation
go test ./test/integration/ -v -run TestSimBootstrap -count=1 -timeout 5m

# Single ATP item
go test ./test/integration/ -v -run "TestSimBootstrap/tx-validation/ATP-1.1" -count=1
```

## Simulations

### TestSimBootstrap (1 node, ~25s)

Single bootstrap node. Tests transaction validation, API endpoints, and pod VM.

| Group | ATP Items | Description |
|-------|-----------|-------------|
| tx-validation | 1.1-1.7, 1.9-1.22 | Field sizes, hash/signature, refs, edge cases |
| api-endpoints | 1.23-1.29, 1.36 | Health, status, validators, object queries |
| faucet-endpoints | 1.32-1.34 | Faucet validation |
| bootstrap-dag | 2.12 | Bootstrap produces vertices alone |
| pod-vm | 16.1-16.3 | Mint success, zero, large amount |

### TestSimConsensus (5 nodes, ~180s)

Five-node cluster. Tests DAG consensus, client operations, version tracking, and security.

| Group | ATP Items | Description |
|-------|-----------|-------------|
| consensus-dag | 2.6, 2.8, 2.9, 2.20 | Quorum, round progression, commit order, gossip |
| client-operations | 15.1-15.5 | Faucet, split, transfer, NFT create/transfer |
| version-tracking | 3.4 | Sequential version increments |
| security | 24.1-24.3 | Replay, hash tampering, signature forgery |
| convergence | -- | Node agreement, no unexpected errors |

### TestSimFees (5 nodes, ~150s)

Five-node cluster. Tests fee deduction, gas coin validation, and fee consistency.

| Group | ATP Items | Description |
|-------|-----------|-------------|
| fee-deduction | 5.1, 5.3, 15.2+fees, 20.4 | Deduction, no gas_coin, split fees, exact balance |
| gas-coin | 6.4, 3.4+fees | Valid gas coin, version increment after fees |
| fee-consistency | -- | All nodes agree on balance after fees |

### TestSimEpochs (10 nodes, ~120s)

Ten-node cluster with epoch length 50. Tests epoch transitions, validator lifecycle, and stability.

| Group | ATP Items | Description |
|-------|-----------|-------------|
| epoch-transitions | 9.1, 9.4, 9.7 | Boundary detection, counter, epochHolders snapshot |
| validator-addition | 9.17, 9.12 | Mid-epoch register, churn applied at boundary |
| validator-removal | 9.18, 9.6, 29.1 | Deregister pending, applied at boundary, 0-fee epoch |
| stability | -- | Round progression, NFT retention, no errors |

### TestSimObjects (12 nodes, ~30s)

Twelve-node cluster. Tests object sharding, Rendezvous hashing, and routing.
Creates all test objects upfront, then runs checks to minimize runtime.

| ATP | Description |
|-----|-------------|
| 12.24 | Different objects get different holders |
| 21.3 | Standard object stored by holders only |
| 21.5 | Holders via Rendezvous deterministic |
| 21.10 | Singleton stored by all validators |
| 32.1 | Replication > validators uses all |
| 37.5 | Route to holders |
| 37.11 | local=true skips routing |

### TestSimStress (12 nodes, ~210s)

Twelve-node cluster with epoch length 50. Tests concurrency, throughput, and epoch behavior under load.

| Group | ATP Items | Description |
|-------|-----------|-------------|
| concurrent-modification | 22.2, 24.6+stress, 24.1+stress | Double modify, double spend, replay under load |
| throughput | -- | 12 concurrent faucets, sequential transfers |
| epoch-under-load | 29.7 | Transactions during epoch transition |
| final-check | -- | No panics or unexpected errors across all nodes |

## Port Allocation

Each simulation uses non-overlapping port ranges (QUIC = HTTP + 920):

| Simulation | Nodes | HTTP Base | QUIC Base |
|------------|-------|-----------|-----------|
| bootstrap | 1 | 18100 | 19020 |
| consensus | 5 | 18200 | 19120 |
| fees | 5 | 18210 | 19130 |
| epochs | 10 | 18300 | 19220 |
| objects | 12 | 18400 | 19320 |
| stress | 12 | 18500 | 19420 |

## Architecture

```
test/integration/
  TESTING.md              Documentation
  helpers.go              Cluster, Node, lifecycle management
  helpers_http.go         HTTP queries, assertions, polling helpers
  helpers_tx.go           Malformed transaction builders
  sim_bootstrap_test.go   1-node simulation (~35 ATP items)
  sim_consensus_test.go   5-node simulation (~12 ATP items)
  sim_fees_test.go        5-node simulation (~7 ATP items)
  sim_epochs_test.go      10-node simulation (~11 ATP items)
  sim_objects_test.go     12-node simulation (~7 ATP items)
  sim_stress_test.go      12-node simulation (~9 ATP items)
```

### helpers.go

Core infrastructure replacing the old duplicated startup patterns.

- `Cluster` -- manages N nodes with lifecycle, cleanup, and polling helpers
- `Node` -- wraps an `exec.Cmd` with thread-safe log capture
- `ClusterOption` -- functional options for port bases, epoch length, min validators, etc.
- `NewCluster(t, size, opts...)` -- builds binary, starts bootstrap + validators, registers `t.Cleanup`
- `WaitReady/WaitForRound/WaitForEpoch/WaitForValidators` -- polling helpers

### helpers_http.go

HTTP queries and assertion helpers.

- `QueryStatus/QueryHealth/QueryValidators/QueryObject/QueryObjectLocal` -- typed GET requests
- `FaucetAndWait` -- mints a coin and polls until the object appears
- `WaitForObject/WaitForOwner/WaitForHolders` -- polling helpers
- `ReadBalance` -- extracts balance from object content bytes
- `CountHolders` -- queries `?local=true` on each node to count holders

### helpers_tx.go

Malformed transaction builders for negative testing.

- `SubmitRawBytes` -- sends arbitrary bytes to `POST /tx`
- `BuildTxWithFieldSize` -- builds a tx with a specific field having wrong size
- `BuildTxWithBadHash/BadSig/WrongSender` -- content-level corruption
- `BuildTxWithDuplicateRefs/TooManyRefs` -- reference validation tests
- `BuildTxWithDomainRef/DuplicateDomainRefs` -- domain reference tests

## Adding New Tests

1. Pick the right simulation by cluster size needed
2. Add a `t.Run("ATP-X.Y: description", ...)` subtest in the appropriate `run*Tests` function
3. Use helper functions from `helpers*.go`
4. If a new helper is needed, add it to the appropriate helpers file
5. Run the specific simulation to verify: `go test ./test/integration/ -v -run TestSimXxx -count=1`

## ATP Coverage Map

| ATP | Subtest | Simulation |
|-----|---------|------------|
| 1.1 | hash must be 32 bytes | Bootstrap |
| 1.2 | sender must be 32 bytes | Bootstrap |
| 1.3 | signature must be 64 bytes | Bootstrap |
| 1.4 | pod must be 32 bytes | Bootstrap |
| 1.5 | function name must be non-empty | Bootstrap |
| 1.6 | gas_coin must be 0 or 32 bytes | Bootstrap |
| 1.7 | hash mismatch rejected | Bootstrap |
| 1.9 | invalid signature rejected | Bootstrap |
| 1.10 | wrong sender key rejected | Bootstrap |
| 1.11 | too many refs rejected | Bootstrap |
| 1.12 | duplicate read_refs rejected | Bootstrap |
| 1.13 | duplicate mutable_refs rejected | Bootstrap |
| 1.14 | cross-list duplicate rejected | Bootstrap |
| 1.15 | duplicate domain refs rejected | Bootstrap |
| 1.16 | invalid ref ID size rejected | Bootstrap |
| 1.17 | domain ref with no ID accepted | Bootstrap |
| 1.18 | FlatBuffers panic recovery | Bootstrap |
| 1.19 | data too short rejected | Bootstrap |
| 1.20 | body over 1MB rejected | Bootstrap |
| 1.21 | empty body rejected | Bootstrap |
| 1.22 | valid tx returns 202 | Bootstrap |
| 1.23 | GET /health | Bootstrap |
| 1.24 | GET /status fields | Bootstrap |
| 1.25 | GET /validators | Bootstrap |
| 1.26 | GET /object valid hex | Bootstrap |
| 1.27 | GET /object invalid hex | Bootstrap |
| 1.28 | GET /object not found | Bootstrap |
| 1.29 | GET /object local=true | Bootstrap |
| 1.32 | POST /faucet valid | Bootstrap |
| 1.33 | POST /faucet amount=0 | Bootstrap |
| 1.34 | POST /faucet invalid pubkey | Bootstrap |
| 1.36 | systemPod in /status | Bootstrap |
| 2.6 | quorum advances round | Consensus |
| 2.8 | round progression | Consensus |
| 2.9 | commit order sequential | Consensus |
| 2.12 | bootstrap produces alone | Bootstrap |
| 2.20 | vertex gossip to all | Consensus |
| 3.4 | sequential version increments | Consensus |
| 3.4+fees | version increments after fee deduction | Fees |
| 5.1 | sufficient balance full deduction | Fees |
| 5.3 | no gas_coin skips fees | Fees |
| 6.4 | valid gas coin accepted | Fees |
| 9.1 | epoch boundary at epochLength rounds | Epochs |
| 9.4 | epoch counter increments | Epochs |
| 9.6 | pending removals applied at epoch | Epochs |
| 9.7 | epochHolders snapshot taken | Epochs |
| 9.12 | churn additions applied at epoch | Epochs |
| 9.17 | register validator tracks epochAdditions | Epochs |
| 9.18 | deregister added to pendingRemovals | Epochs |
| 12.24 | different objects get different holders | Objects |
| 15.1 | faucet request | Consensus |
| 15.2 | split operation | Consensus |
| 15.2+fees | split deducts fees | Fees |
| 15.3 | transfer operation | Consensus |
| 15.4 | create NFT | Consensus |
| 16.1 | mint success | Bootstrap |
| 16.2 | mint zero amount | Bootstrap |
| 16.3 | mint large amount | Bootstrap |
| 20.4 | balance equals fee exactly | Fees |
| 21.3 | standard object stored by holders only | Objects |
| 21.5 | holders via Rendezvous deterministic | Objects |
| 21.10 | singleton stored by all validators | Objects |
| 22.2 | two clients modify same object | Stress |
| 24.1 | replay attack rejected | Consensus |
| 24.1+stress | replay under load | Stress |
| 24.2 | hash tampering rejected | Consensus |
| 24.3 | signature forgery rejected | Consensus |
| 24.6+stress | double-spend under load | Stress |
| 29.1 | epoch with 0 fees collected | Epochs |
| 29.7 | transactions during epoch transition | Stress |
| 32.1 | replication > validators uses all | Objects |
| 37.5 | route to holders | Objects |
| 37.11 | local=true skips routing | Objects |
