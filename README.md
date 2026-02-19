# BluePods

BluePods is a Layer 1 blockchain built from scratch in Go. The core idea is simple: not every validator needs to process every transaction.

Most blockchains today work the same way — every node stores the entire state and executes every transaction. This works, but it means that adding more validators to the network doesn't increase its capacity. Whether you have 100 or 10,000 validators, each one does the same amount of work. Throughput is capped by what a single machine can handle.

BluePods takes a different approach. The network state is composed of independent objects, each replicated on a specific subset of validators called holders. When a transaction comes in, only the holders of the objects it touches need to execute it. A transfer between two users with objects replicated on 10 holders means 10 validators do the work — not the entire network. Adding more validators means the workload is spread thinner, and the network's aggregate capacity actually grows.

This isn't a new idea — it's essentially horizontal sharding, the same principle that makes distributed databases scale. The challenge is making it work in a Byzantine environment where you can't trust anyone. That's what most of this codebase is about.

## How it works

### Objects, not accounts

There's no global state tree. The state is a collection of discrete objects, each with an ID, a version, an owner, and a replication factor. An object with replication 10 is stored by 10 holders. An object with replication 0 (a singleton) is stored by everyone.

Two transactions touching different objects can never conflict. When they do touch the same object, conflict detection is trivial — just compare the declared version against the current one. No locks, no speculative execution, no rollbacks.

### DAG consensus

Consensus is a leaderless directed acyclic graph inspired by Mysticeti. Every validator produces vertices in parallel — there's no designated block proposer, so there's no leader bottleneck. Vertices reference each other across rounds, and a vertex is committed when it's been acknowledged (directly or transitively) by a supermajority of the network 2 rounds later. Finality is fast and deterministic.

The important property of the DAG is that it orders transactions without executing them. Every validator can track object versions just by scanning the committed history. If a transaction declares an object in its mutable list, that object's version gets incremented — regardless of what the smart contract actually does with it. This makes version tracking deterministic and deducible from the DAG alone.

### Direct attestation

In traditional BFT consensus, validators broadcast votes to everyone. With N validators, that's N² messages per round. At 200 nodes it's fine. At 2,000 it's a problem.

BluePods replaces this with direct collection. When a validator receives a transaction from a user, it becomes the aggregator. It contacts only the holders of the referenced objects over persistent QUIC connections, collects their BLS signatures on the object hashes, and aggregates them into a single compact proof. The transaction, its objects, and the aggregated BLS proof are then packaged into a vertex and gossipped to the network.

The result is that attestation cost scales with the object's replication factor, not with the network size. A transaction involving a 50-holder object generates 50 messages, whether the network has 100 or 5,000 validators.

### Pods

Smart contracts are called pods. They're Rust compiled to WebAssembly, executed in a sandboxed wazero runtime with gas metering. The interface is minimal — four host functions (gas, input_len, read_input, write_output) and nothing else. No filesystem, no network, no clock. The pod receives its inputs as a FlatBuffers message, does its logic, and returns the state changes.

The system pod handles the basics: minting coins, transfers, splits, merges, NFTs, and validator registration/deregistration. Application developers build their own pods using the Rust SDK.

### Fees

Fees are computed at the protocol level, outside of pod execution. This is deliberate — if fees were deducted inside a smart contract, the gas coin (a singleton) would force every validator to execute every transaction, destroying the whole point of sharding.

Instead, fee computation is pure arithmetic on the transaction header. Four components: compute (gas budget × gas price × fraction of validators that execute), transit (per standard object in the transaction body), storage (per created object, weighted by replication), and domain registration. Fees are always deducted, even if the transaction fails. 20% goes to the aggregator, 30% is burned, 50% accumulates for epoch rewards.

## Build & run

Prerequisites: **Go 1.26+** and **Rust** with the WASM target (`rustup target add wasm32-unknown-unknown`).

```bash
# Build the system pod (Rust → WASM) and the node binary (Go)
cd pods/pod-system && make release && cd ../..
go build -o node ./cmd/node
```

Start a local cluster in separate terminals:

```bash
./node -bootstrap -http :8080 -quic :9000 -data ./data1
./node -bootstrap-addr localhost:9000 -http :8081 -quic :9001 -data ./data2
./node -bootstrap-addr localhost:9000 -http :8082 -quic :9002 -data ./data3
```

Hit the API:

```bash
curl localhost:8080/health          # {"status":"ok"}
curl localhost:8080/status          # round, epoch, validator count
curl localhost:8080/validators      # active validator list with addresses
```

The node supports bootstrap mode (genesis), validator mode (joins an existing network), and listener mode (observe without participating in consensus). Run `./node -help` for all flags — epoch length, churn limits, gossip fanout, sync buffer, etc.

## Testing

This is a consensus system. If the tests don't prove it works, nothing does.

The test suite is split into two layers: unit tests that verify individual components in isolation, and integration tests that spin up real multi-node clusters and run end-to-end scenarios against them.

### Unit tests

Standard Go tests spread across all packages. They cover the DAG validation logic, fee calculation and overflow safety, BLS signature aggregation and verification, version tracking, snapshot encoding/decoding, Rendezvous hashing, the WASM runtime, and the HTTP API validation layer.

```bash
go test ./internal/... ./client/...
```

### Integration tests

The integration tests are where it gets interesting. Each test is a simulation that starts a real cluster of N nodes as separate processes, waits for them to reach consensus, then runs a sequence of scenarios against the live network. When the test ends, the cluster is torn down automatically.

There are 7 simulations, each targeting a different aspect of the system:

**TestSimBootstrap** starts a single node in bootstrap mode and hammers the API with ~35 test cases. It submits transactions with wrong field sizes, bad hashes, invalid signatures, duplicate object references, oversized bodies, malformed FlatBuffers — everything that should be rejected at the validation layer. It also verifies that valid transactions go through, that the faucet works, and that the pod VM correctly executes mints.

**TestSimConsensus** spins up 5 nodes and tests the actual consensus mechanism. It verifies that rounds progress, that vertices are gossipped to all nodes, that the commit rule works. Then it runs client operations (faucet, split, transfer, NFT creation) and checks that all 5 nodes converge to the same state. It also tests security: replay attacks, hash tampering, and signature forgery are all rejected.

**TestSimFees** runs a 5-node cluster with the fee system enabled. It mints coins, performs operations, and verifies that fees are correctly deducted from the gas coin. Tests the exact-balance boundary case (balance equals fee exactly), verifies that transactions without a gas coin skip fee deduction, and checks that all nodes agree on the final balances.

**TestSimEpochs** starts 10 nodes with a short epoch length (50 rounds). It waits for an epoch boundary, verifies the epoch counter increments and the epochHolders snapshot is taken. Then it registers a new validator mid-epoch and checks that it's included at the next boundary. It deregisters a validator and verifies it's removed. It also tests the edge case of an epoch with zero fees collected.

**TestSimObjects** runs 12 nodes and focuses on the object sharding layer. It creates objects with different replication factors and verifies they're stored only by the correct holders (determined by Rendezvous hashing). It tests that singletons are stored by all validators, that routing correctly forwards object queries to holders, and that `?local=true` prevents cascading lookups.

**TestSimStress** pushes a 12-node cluster under load. It fires concurrent modifications at the same objects from multiple clients to trigger version conflicts. It tests double-spend resistance under concurrency, submits transactions during epoch transitions, and verifies that no node panics or produces unexpected errors throughout the entire run.

**TestSimProgressiveJoining** tests the network's ability to grow. It starts with 5 validators, then adds 5 more one by one, verifying that each new validator syncs, starts producing vertices, and reaches consensus with the rest. A variant (TestSimBatchJoining) adds validators in batches of 5 up to 20 total.

```bash
# Run everything (~12 minutes)
go test ./test/integration/ -v -count=1 -timeout 30m

# Run a single simulation
go test ./test/integration/ -v -run TestSimConsensus -count=1 -timeout 5m

# Run a specific test case
go test ./test/integration/ -v -run "TestSimBootstrap/tx-validation/ATP-1.1" -count=1
```

The full [acceptance test plan](ATP.md) documents ~417 test cases across 38 categories, covering every protocol feature, edge case, and attack vector. The integration test [architecture and coverage map](test/integration/TESTING.md) shows which ATP items are covered by which simulation.

## Project layout

The node binary is in `cmd/node/`. All core logic lives in `internal/`, split by domain: `consensus/` handles the DAG, commit rules, epoch transitions, fee calculation, and version tracking. `aggregation/` handles BLS attestation collection and Rendezvous hashing for holder computation. `state/` manages objects, applies execution results, and maintains the domain registry. `podvm/` is the WASM runtime — module compilation, instance pooling, gas metering, and the four host functions. `network/` manages the QUIC mesh and gossip protocol. `api/` is the HTTP REST layer. `sync/` handles snapshot creation and the synchronization protocol for new validators joining the network. `storage/` wraps Pebble for persistence.

The `pods/` directory contains the Rust side: `pod-sdk/` is the development SDK (dispatcher macro, context, result builders), `pod-system/` is the system pod (mint, transfer, split, merge, NFTs, validator registration), and `wasm-gas/` is the tool that instruments WASM modules with gas metering calls.

`types/` holds the FlatBuffers schemas (objects, transactions, vertices, snapshots, pod I/O) and the generated code for both Go and Rust. `client/` is a Go library for building and signing transactions.

The node is written in Go, pods are written in Rust and compiled to WebAssembly (executed via [wazero](https://github.com/tetratelabs/wazero)). Protocol serialization uses [FlatBuffers](https://github.com/google/flatbuffers) for zero-copy access, pod arguments use [Borsh](https://borsh.io). Cryptography: Ed25519 for transaction signatures, [BLS12-381](https://github.com/supranational/blst) for attestation aggregation, [BLAKE3](https://github.com/zeebo/blake3) for all hashing. P2P networking over [QUIC](https://github.com/quic-go/quic-go), storage on [Pebble](https://github.com/cockroachdb/pebble), snapshots compressed with zstd.

## Documentation

The **[design document](WHITEPAPER.md)** covers the full architecture in detail: data model, consensus mechanism, attestation protocol, fee system, validator management, security analysis, performance projections, and a comparison with Sui, Solana, and Ethereum.

The **[acceptance test plan](ATP.md)** is an exhaustive list of ~417 test cases across 38 categories, covering every protocol feature, security mechanism, edge case, and attack vector.
