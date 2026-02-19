# BluePods: A Sharded Object-Oriented Blockchain with DAG Consensus

## Abstract

BluePods is a Layer 1 blockchain designed around three core ideas: an object-oriented state model where data is split into independent, versioned objects rather than stored in global account trees; a leaderless DAG-based consensus that achieves finality in two rounds without requiring a designated block proposer; and horizontal execution sharding where each transaction is processed only by the validators that hold the objects it touches.

Together, these properties allow the network to scale throughput with the number of validators rather than being bottlenecked by a single leader or global state, while keeping the programming model simple: a transaction declares its inputs, calls a function, and produces outputs — much like a function call with explicit parameters.

This document describes the architecture, the reasoning behind key design decisions, and the tradeoffs involved.

---

## 1. Introduction

### The Scalability Trilemma in Practice

Most Layer 1 blockchains make a fundamental tradeoff between throughput, decentralization, and state management:

- **Ethereum** achieves strong decentralization but every validator must store and process the entire global state, capping throughput at roughly 15-30 TPS on L1.
- **Solana** achieves high throughput (thousands of TPS) through a leader-based pipeline, but the leader becomes a performance bottleneck and hardware requirements limit validator participation.
- **Sui** introduces an object-oriented model that enables parallel execution of non-conflicting transactions, but still relies on all validators processing all transactions.

The common thread is that scaling typically requires either trusting fewer validators (centralization) or offloading work to Layer 2 solutions (complexity).

### A Different Approach

BluePods takes a different path: instead of having every validator process every transaction, it splits the workload across the validator set. Each object in the network has a defined set of holders — validators responsible for storing it, attesting to its state, and executing transactions that modify it. A simple transfer between two users only involves the 10-50 holders of the objects involved, not the entire network.

This is made possible by three design choices working together:

1. **Object-oriented state**: the network state is composed of independent objects, each with its own version, owner, and replication factor. There is no global state tree to synchronize.
2. **DAG consensus for ordering**: a leaderless directed acyclic graph determines transaction ordering and finality, without requiring every validator to execute every transaction.
3. **Direct attestation**: instead of broadcasting votes through the entire network, an aggregator contacts object holders directly over persistent QUIC connections, collecting cryptographic proofs in a single round-trip.

The result is a system where adding more validators increases the network's total capacity rather than adding overhead. The tradeoff is increased complexity in managing distributed state — a tradeoff this document explores in detail.

---

## 2. Design Principles

Several guiding principles shaped the architecture of BluePods. Understanding them helps explain why certain decisions were made over seemingly simpler alternatives.

**Objects over accounts.** Traditional blockchains store state in account-based trees where each address maps to a balance and storage. This couples unrelated data: two users transacting have to reason about the same global state root. BluePods decouples state into independent objects, each versioned and owned separately. Two transactions touching different objects never conflict, enabling natural parallelism without requiring dependency analysis.

**Explicit inputs, deterministic outputs.** A transaction explicitly declares every object it reads or modifies. This makes conflict detection trivial (compare declared versions against current versions) and eliminates the need for speculative execution or optimistic rollbacks. Any validator can determine whether a transaction will conflict without executing it.

**Compute follows data.** Instead of routing data to a fixed set of executors, BluePods routes execution to the validators that already hold the data. If an object is replicated on 10 holders, those 10 holders execute transactions involving that object. This eliminates the need to transfer large state between validators before execution.

**Protocol-level simplicity, pod-level flexibility.** Core operations like fee deduction, version tracking, and ownership checks are handled at the protocol level, outside the smart contract runtime. Smart contracts (called pods) focus on business logic. This separation keeps the protocol predictable and auditable while allowing arbitrary logic in the execution layer.

**Honest majority per object, not globally.** Security in BluePods is scoped to individual objects. For an object with a replication factor of 50, an attacker needs to corrupt 34 holders of that specific object — not 34% of the entire network. This is a meaningful distinction in large networks.

---

## 3. Data Model

### Objects

The global state of the network is composed of discrete objects. Each object is an independent unit of data with the following fields:

- **ID** (32 bytes): a unique identifier computed via BLAKE3, immutable for the lifetime of the object.
- **Version** (uint64): an integer that increments on every mutation, used for conflict detection.
- **Owner** (32 bytes): the Ed25519 public key of the current owner.
- **Replication** (uint16): the number of validators that store this object, immutable after creation.
- **Content** (up to 4 KB): the application-specific payload, serialized by the pod.
- **Fees** (uint64): the storage deposit locked at creation, read-only for pods.

This model is conceptually similar to Sui's object model but diverges in how replication and execution are distributed. In Sui, all validators store all objects and execute all transactions. In BluePods, objects are stored only by their designated holders, and execution is scoped to those holders.

The 4 KB content limit is a deliberate choice. A wallet with metadata fits in a few hundred bytes, an NFT with its URI and attributes in 1-2 KB, a DeFi position in under 1 KB. For larger payloads, an off-chain storage layer with erasure coding is planned, where only metadata and availability certificates would live on-chain.

### Standard Objects and Singletons

Objects come in two flavors, determined by their replication factor:

**Standard objects** have a replication factor between 10 and several hundred. They are stored by a subset of validators (holders) determined by Rendezvous Hashing. The minimum replication of 10 guarantees that a 67% quorum is reachable with 7 holders and tolerates up to 3 simultaneous failures. Higher replication increases availability at the cost of higher storage fees.

**Singletons** have a replication factor of 0, meaning they are stored by every validator in the network. Singletons are used for data that must be universally accessible: pod code (smart contracts), system parameters, the active validator list, and gas coins used for fee payment. Developers can also create their own singletons for use cases requiring maximum availability guarantees, though the creation and modification fees are significantly higher than for standard objects since the cost scales with the entire validator set.

Singletons benefit from a key optimization: since every validator already has them locally, they are excluded from the transaction body and do not require attestation. Only their ID and expected version appear in the transaction header. This saves bandwidth and eliminates an entire collection round for singleton-only transactions.

### Versioning and Conflict Detection

Every time an object appears in the MutableRefs list of a successful transaction, its version increments by 1 — regardless of whether the pod actually changed its content. This ensures that version tracking is deterministic and deducible from the transaction header alone, without executing the pod.

Conflict detection is straightforward: a transaction declares the expected version of each object it touches. If the current version (computed from the committed DAG history) does not match, the transaction is rejected. No locks, no two-phase commits — just an optimistic version check.

This design means two transactions modifying the same object will conflict if submitted concurrently: the first to commit wins, the second is rejected with a version mismatch. The client can retry with the updated version. This is the same behavior as Sui and represents an acceptable tradeoff for an MVP.

### Object References in Transactions

A transaction declares its object dependencies through two lists:

- **ReadRefs**: objects read but not modified. Their version is checked but not incremented.
- **MutableRefs**: objects that may be modified. Their version is incremented upon success.

A transaction may reference up to 40 objects total across both lists. References can use either a 32-byte object ID or a human-readable domain name (see Section 4). Domain references are resolved at execution time and are exempt from ownership validation on MutableRefs, enabling shared-access patterns.

---

## 4. Domain Name System

The network provides a protocol-level naming system that maps human-readable identifiers to object IDs. Instead of referencing `0x7a3f8b2c...`, developers and users can use names like `system.validators` or `myapp.config`.

### Architecture

The domain registry is stored in Pebble (a local key-value store) on each validator, maintained at the protocol level. It is not an object or a singleton — it is local infrastructure. This design avoids the prohibitive cost of reading and rewriting a large singleton on every registration, where gas costs would grow with the number of registered domains.

Consistency is guaranteed because all validators process the same committed transactions in the same DAG-determined order, updating their local registry deterministically. The registry is included in state snapshots for new validator synchronization.

### Namespaces and Registration

Domains follow a hierarchical convention using `.` as separator. The `system.*` namespace is reserved for protocol objects (validator list, network parameters). Other namespaces are first-come, first-served: once an entity registers a root namespace, only that entity can add sub-domains.

Registration happens through pod execution. A pod declares domains to register in its `PodExecuteOutput`, specifying for each entry a domain name and either an `object_index` (referencing a newly created object from the same transaction) or a direct `object_id` (for an existing object). After execution, the protocol resolves any index reference into the computed ObjectID, checks uniqueness in Pebble, and inserts the mapping. If a domain already exists, the transaction reverts — the same pattern as version conflicts. Domain resolution is a purely local operation: a direct lookup in the validator's Pebble store with no network communication, exposed to clients via the `GET /domain/{name}` endpoint.

A domain can be updated to point to a different object, or deleted entirely, by its owner through the same pod execution mechanism. To prevent squatting, domain registration carries a fee significantly higher than a simple transaction (currently 100x the base compute cost). Updates and deletions pay only the standard compute fee.

---

## 5. Storage Distribution

### Rendezvous Hashing

For standard objects, holders are determined by Rendezvous Hashing (also known as Highest Random Weight hashing). For each object, every validator's score is computed as `BLAKE3(objectID || validatorPubkey)`. Validators are sorted by score in descending order, and the top N become the holders, where N is the object's replication factor.

This approach has a critical property: **minimal disruption on validator changes**. When a validator joins or leaves the network, only a fraction of objects need to be reassigned. If a validator disappears, only the (N+1)th-ranked validator for each affected object becomes a new holder — the other N-1 holders remain unchanged. This minimizes reshuffling and synchronization traffic during epoch transitions.

Any participant can independently compute the holder list for any object using only the object ID, its replication factor, and the current validator set. The computation is purely deterministic and requires no network communication.

### Replication Factor Tradeoffs

The replication factor creates a direct tradeoff between availability and cost:

| Replication | Fault Tolerance | Quorum Size (67%) | Storage Cost Multiplier |
|---|---|---|---|
| 10 (minimum) | 3 failures | 7 | 1x |
| 50 | 16 failures | 34 | 5x |
| 100 | 33 failures | 67 | 10x |
| 0 (singleton) | N/A — all validators | 67% of network | Full network |

Applications choose the appropriate factor based on their availability requirements. A rarely-accessed configuration object might use the minimum of 10, while a high-value DeFi vault might use 50 or more.

---

## 6. Consensus

### The DAG Structure

BluePods uses a leaderless DAG-based consensus inspired by Mysticeti. The fundamental unit is a **vertex** — a data structure produced by a single validator containing:

- Transactions with their attested objects and quorum proofs
- Parent links to vertices from the previous round
- A fee summary for the included transactions
- The producer's Ed25519 signature over the BLAKE3 hash of the vertex content

Unlike traditional blockchains where a single leader proposes a block, every validator produces vertices in parallel. This eliminates the leader bottleneck and allows the network to utilize the aggregate bandwidth of all validators simultaneously.

### Rounds and Quorum

The DAG progresses through sequential rounds. A vertex at round N must include parent links to vertices from round N-1 produced by at least **(2n/3 + 1)** distinct validators, where n is the total validator count. This BFT quorum requirement on parent links prevents the DAG from fragmenting: a validator cannot advance to the next round without acknowledging a supermajority of the previous round's output.

Each validator produces at most one vertex per round. A liveness timer (500ms) triggers vertex production when the network is idle, ensuring continuous progress even without incoming transactions.

### Commit Rule and Finality

A vertex V at round N is committed when it is referenced (directly or transitively) by vertices at round N+2 produced by a quorum of validators. This two-round commit rule provides fast finality: once a vertex is committed, all transactions it contains are final and irreversible.

The commit check runs every 50ms. Rounds are committed sequentially — the protocol stops at the first uncommitted round and does not skip ahead, ensuring deterministic ordering.

### Conflict Resolution Through Ordering

When two transactions declare the same object in their MutableRefs with the same expected version, the DAG determines which one succeeds. The committed ordering is deterministic: if the conflicting transactions are in different vertices, commit order decides priority. If they are in the same vertex, lexicographic ordering of transaction hashes provides a deterministic tiebreaker.

The key insight is that **conflict detection requires no execution**. Every validator can compute the current version of any object by scanning the committed DAG history: for each committed transaction, objects in its MutableRefs have their version incremented by 1. This lightweight tracking costs only 18 bytes per object in persistent storage (8 bytes version + 2 bytes replication + 8 bytes fees).

### Bootstrap and Convergence

When the network starts or new validators join, a grace period relaxes the quorum requirements:

- **Bootstrap mode**: the first validator produces vertices alone with a quorum of 1.
- **Transition grace**: after reaching the minimum validator count, 20 rounds of relaxed quorum allow the network to converge.
- **Transition buffer**: 10 additional rounds with relaxed quorum provide a safety margin.
- **Full BFT quorum**: once the first vertex achieves a full 2n/3+1 quorum after the buffer period, the network switches to strict BFT mode permanently.

---

## 7. Attestation and Aggregation

### The Problem with Broadcast Voting

In traditional BFT consensus, validators broadcast their votes to the entire network. With N validators, each vote generates N messages, and each round generates N² total messages. At 200 validators this is manageable (40,000 messages). At 2,000 validators it becomes untenable (4 million messages per round).

### Direct Collection

BluePods replaces broadcast voting with **direct collection**. When a validator receives a transaction from a user, it becomes the **aggregator** for that transaction. The aggregator contacts only the holders of the referenced objects — not the entire network — over persistent QUIC connections. With a typical replication factor of 50, this means 50 direct messages instead of thousands of broadcasts.

The aggregator's role is purely coordinative: it cannot forge attestations because it does not possess the holders' private keys. If the aggregator fails, the user resubmits to another validator.

### BLS Signatures and Aggregation

Each holder attests to an object's state by signing `H = BLAKE3(content || version_u64_BE)` with its BLS key. BLS keys are derived deterministically from the validator's Ed25519 seed: `BLAKE3("bluepods-bls-keygen" || ed25519_seed)`.

An asymmetric response pattern minimizes bandwidth:
- The **top-ranked holder** (by Rendezvous score) sends the full object content plus its BLS signature (~500 bytes + 96 bytes).
- All **other holders** send only the hash and their BLS signature (32 + 96 = 128 bytes each).

Once the quorum is reached (67% of holders signing the same hash), the aggregator combines the individual BLS signatures into a single **aggregated signature** of 96 bytes, accompanied by a bitmap indicating which holders signed. This aggregated proof is compact and verifiable by any validator.

### Quorum Validation and Fail-Fast

The quorum threshold is **(replication × 67 + 99) / 100**, implementing a 67% requirement with integer arithmetic.

The aggregator implements fail-fast: as soon as enough negative votes accumulate to make the quorum mathematically impossible, it abandons the collection immediately. For example, with 50 holders and a quorum of 34, receiving 17 negative votes means only 33 positive votes are possible — the transaction is rejected without waiting for remaining responses.

### Singleton Optimization

Transactions involving only singletons skip the entire attestation phase. Since every validator already stores every singleton, there is nothing to collect or attest. The transaction is included directly in a vertex and propagated via gossip. This optimization benefits system transactions (staking, parameter updates, validator registration) which primarily interact with singletons.

---

## 8. Transaction Lifecycle

A complete transaction follows these stages from submission to finality:

### Submission and Aggregation

The user sends a signed transaction to any validator, which becomes the aggregator. The transaction is serialized in FlatBuffers for compact binary encoding with zero-copy field access. Maximum transaction size is 1 MB. Each transaction is uniquely identified by its hash, computed as `BLAKE3` over the canonical unsigned content.

The transaction header declares: sender public key, pod ID, function name, Borsh-serialized arguments, ReadRefs, MutableRefs, created object replications, max gas budget, gas coin ID, and an Ed25519 signature.

### Object Collection

The aggregator identifies holders for each standard object via Rendezvous Hashing and contacts them in parallel over QUIC. Singletons are skipped — every validator has them locally. Upon reaching quorum for all objects, the aggregator assembles the attested transaction (ATX) with the collected objects, aggregated BLS signatures, and signer bitmaps.

### Vertex Production and Gossip

The ATX is included in the aggregator's next vertex along with other pending transactions. The vertex is gossipped with a production fanout of 40 peers. Relaying validators forward with a reduced fanout of 10 to prevent amplification. With these parameters, a vertex reaches the entire network in approximately 3 hops: first hop reaches 40 validators, second hop reaches ~400, third hop covers the rest.

### Consensus and Commit

The vertex enters the DAG. When referenced by vertices two rounds later from a quorum of validators, it is committed. All transactions in the vertex become final.

### Fee Deduction

After commit, the protocol computes fees from the transaction header and deducts them from the sender's gas coin. This deduction is implicit — it does not increment the gas coin's version, allowing multiple in-flight transactions from the same sender without version conflicts on the gas coin. Fees are always deducted, even if the transaction subsequently fails.

### Version Check and Ownership Validation

Each validator checks that the declared versions match the versions computed from the DAG history. If any version mismatches, the transaction is marked as failed without execution.

The protocol then verifies that the sender owns all objects in MutableRefs by checking the Owner field. Domain references (identified by name rather than ID) are exempt from this check, enabling shared-access patterns through the naming system.

### Execution

If versions and ownership are valid, each holder of at least one MutableRef object calls `execute()` on the target pod. Since all holders receive the same inputs (objects attested by quorum), they compute the same output deterministically. The pod returns updated objects, created objects, deleted objects, registered domains, and logs.

### Post-Execution

Object versions in MutableRefs are incremented. Created objects receive deterministic IDs computed as `BLAKE3(tx_hash || index_u32_LE)` and are stored by their respective holders (computed via Rendezvous Hashing). This eliminates the need for a separate object creation transaction — finality is achieved in 2 rounds instead of 4. Deleted objects refund 95% of their storage deposit to the sender's gas coin.

When a validator becomes a new holder of an existing object (due to an epoch change or another validator departing), it recovers the object from the remaining holders via the routing mechanism. The DAG contains the trace of the transaction that created the object, allowing verification of authenticity.

---

## 9. Execution Model: Pods

### WASM Runtime

Smart contracts in BluePods are called **pods**. Each pod is a WebAssembly module executed via the **wazero** runtime (a pure-Go, zero-dependency WASM implementation). Modules are compiled once and cached in a pool. Each execution creates an isolated instance with fresh memory, ensuring no state leaks between transactions.

Pods are singletons: their code is replicated on every validator, ensuring any validator can execute any transaction without fetching code from peers.

### Host Interface

The WASM sandbox exposes four host functions:

| Function | Signature | Purpose |
|---|---|---|
| `gas` | `(cost: u32)` | Declares gas consumption. Aborts execution if the budget is exceeded. |
| `input_len` | `() → u32` | Returns the size of the input buffer. |
| `read_input` | `(ptr: u32)` | Copies input data into WASM memory. |
| `write_output` | `(ptr: u32, len: u32)` | Copies output from WASM memory to the host. |

This minimal interface keeps the attack surface small. The pod has no access to the filesystem, network, or system clock. Its only interaction with the outside world is through the structured input/output buffers.

### Input and Output

The input is a `PodExecuteInput` serialized in FlatBuffers, containing the sender's public key, function name, Borsh-serialized arguments, and all referenced objects (both mutable and read-only, resolved by the protocol before execution).

The output is a `PodExecuteOutput` containing:

1. **Updated objects**: modified versions of MutableRef objects.
2. **Created objects**: new objects, each with a replication factor declared in the transaction header via the `created_objects_replication` vector.
3. **Deleted objects**: objects to remove from the state. The protocol verifies ownership before applying.
4. **Registered domains**: name-to-ObjectID mappings to insert in the domain registry.
5. **Logs**: debug messages emitted by the pod.
6. **Error code**: 0 for success, non-zero causes a revert (fees still deducted).

### SDK and Developer Experience

A Rust SDK (`pod-sdk`) provides abstractions over the raw host interface:

- `dispatcher!` macro for routing to handlers based on function name.
- `Context` type giving access to sender, deserialized arguments, and local objects.
- `ExecuteResult` with chainable builders: `ok()`, `err(code)`, `with_updated()`, `with_created()`, `log()`.

The development model is deliberately similar to Ethereum's Solidity or Sui's Move: a function receives inputs, performs logic, and returns state changes. The key difference is that objects are explicit parameters rather than implicit global state.

### Gas Metering

Gas metering is implemented through WASM instrumentation: a separate tool (`wasm-gas`) injects calls to the `gas()` host function into the pod's bytecode before deployment. This ensures metering is transparent to the developer and cannot be bypassed.

The default gas budget is 10,000,000 units per transaction. If execution exceeds this budget, it is immediately aborted and the transaction reverts. Fees are deducted regardless — preventing attacks that submit intentionally-failing transactions to consume validator resources for free.

### System Pod

The system pod is the foundational smart contract of the network. It exposes eight functions covering basic financial operations and validator management:

| Function | Description |
|---|---|
| `mint` | Creates a new Coin with an initial balance |
| `split` | Divides a Coin into two (original balance reduced, new Coin created) |
| `merge` | Combines two Coins into one |
| `transfer` | Changes the owner of a Coin |
| `create_nft` | Creates an object with arbitrary metadata |
| `transfer_nft` | Changes the owner of an object |
| `register_validator` | Registers a new validator on the network |
| `deregister_validator` | Schedules a validator for removal |

Coins follow a minimal structure: a single `balance` field of type uint64, serialized in Borsh (8 bytes). This simplicity is intentional — complex financial logic belongs in application-level pods, not the system pod.

---

## 10. Fee System

### Design Rationale

Two non-obvious decisions shape the fee system:

**Fees are at the protocol level, not inside pods.** If fees were deducted by the system pod, the gas coin (a singleton) would need to be executed by every validator for every transaction. This would destroy execution sharding — the system pod would become a bottleneck on 100% of transactions. By moving fee deduction to the protocol layer, it becomes a simple arithmetic operation computed from the transaction header, with no pod execution required.

**Fees are based on declared max_gas, not actual gas_used.** The gas coin is a singleton. All validators store it and must agree on the amount deducted. But only holders of the mutable objects execute the transaction — non-holders do not know the actual gas consumed. If fees depended on gas_used, either all validators would have to execute (killing sharding) or non-holders could not update the gas coin correctly. Using the declared max_gas makes the fee deterministic from the header alone. Pods that consume less gas simply declare a lower max_gas and pay less.

### Fee Components

Each transaction pays four types of fees:

```
total = max_gas × gas_price × replication_ratio
      + standard_objects_in_ATX × transit_fee
      + Σ(effective_rep(replication_i) / total_validators) × storage_fee
      + max_create_domains × domain_fee
```

Where:

- **Compute**: proportional to the declared gas budget and the fraction of validators that execute.
- **Transit**: a flat fee per standard object included in the transaction body (singletons excluded).
- **Storage**: a flat fee per created object, weighted by its replication ratio.
- **Domain**: a flat fee per domain registration.

`effective_rep(r)` equals `total_validators` for singletons (r=0) and `r` for standard objects. `replication_ratio` is the fraction of validators that execute the transaction, computed as the union of holders across all mutable objects.

### Current Constants

| Constant | Value | Description |
|---|---|---|
| `gas_price` | 1 | Price per gas unit |
| `min_gas` | 100 | Minimum gas per transaction (anti-spam) |
| `transit_fee` | 10 | Per standard object in the transaction body |
| `storage_fee` | 1,000 | Per created object (flat 4 KB rate) |
| `domain_fee` | 10,000 | Per domain registration |

These values are provisional and will be adjusted based on mainnet observations.

### Distribution

Collected fees are split into three components:

| Destination | Share | Timing |
|---|---|---|
| Aggregator | 20% | Immediate credit to the aggregator's coin |
| Burn | 30% | Tokens permanently destroyed |
| Epoch rewards | 50% | Accumulated and distributed at epoch boundary |

The 30% burn creates deflationary pressure and prevents validators from manipulating fees for profit. The aggregator's 20% share incentivizes validators to accept and process user transactions promptly. Integer rounding remainders are added to the epoch pool.

### Fee Summary Verification

Each vertex contains a pre-computed `FeeSummary` with fields `total_fees`, `total_aggregator`, `total_burned`, and `total_epoch`. Every receiving validator recalculates this summary from the transaction headers and rejects the vertex if it does not match. This summary serves as a cache for epoch reward distribution — instead of re-scanning all transactions at epoch boundary, validators sum the `total_epoch` fields across committed vertices.

### Storage Deposits and Refunds

Every created object locks a storage deposit in its `fees` field, computed as `storage_fee × effective_rep(replication) / total_validators`. This deposit is fixed at creation time, independent of future changes to fee constants or validator count.

On deletion, 95% of the deposit is refunded to the owner's gas coin and 5% is burned. The burn prevents spam through rapid creation/deletion cycles.

### Gas Coin Mechanics

The gas coin is a Coin singleton (replication=0) owned by the sender. It is referenced in a dedicated `gas_coin` field in the transaction header, separate from the business-logic inputs. This separation ensures that fee deduction does not interfere with the transaction's object references.

Gas coin modifications are **implicit protocol operations**: they change the balance but do not increment the version. This is critical — it allows a sender to have multiple transactions in flight simultaneously without version conflicts on their gas coin. Fee deductions are applied sequentially in DAG-committed order.

---

## 11. Validator Management

### Epochs

The network operates in epochs of configurable length (measured in consensus rounds). At each epoch boundary, the active validator set is frozen into a snapshot called `epochHolders`, which determines storage distribution and Rendezvous Hashing for the entire epoch. Changes to the validator set (additions, removals) only take effect at the next epoch boundary.

### Epoch Transitions

At each epoch boundary, the protocol executes the following steps in order:

1. **Reward distribution**: accumulated epoch fees (plus future issuance) are distributed to validators proportionally to their stake and participation (rounds produced / total rounds in epoch).
2. **Removal application**: pending validator removals are applied, subject to churn limiting.
3. **Validator set snapshot**: the current set is frozen as `epochHolders` for the new epoch.
4. **Counter reset**: epoch fees, round production counters, and pending additions are cleared.
5. **Epoch increment**: the epoch counter advances.

### Registration and Deregistration

Validators join by submitting a `register_validator` transaction containing their Ed25519 public key, HTTP and QUIC addresses, and BLS public key (48 bytes). They are added to the active set and tracked in the epoch's additions for churn limiting.

Deregistration is a two-phase process: a `deregister_validator` transaction places the validator in a pending removal list, but the validator remains active until the next epoch boundary. This ensures no disruption mid-epoch.

### Churn Limiting

To maintain network stability, the number of validators added or removed per epoch is capped. When pending removals exceed the limit, they are sorted by public key (for deterministic ordering across all validators) and only the first N are processed. Excess removals are deferred to the following epoch. The same logic applies to additions.

### Reward Distribution

At each epoch boundary, the accumulated epoch fees are distributed to validators based on a weighted formula:

```
weight_i = stake_i × (rounds_produced_i / total_rounds_in_epoch)
share_i  = (weight_i / Σ weights) × reward_total
```

This formula rewards both stake commitment and active participation. A validator that produces no vertices during an epoch receives no rewards, creating a natural soft penalty for inactivity without requiring explicit slashing.

For the current implementation, stake is equal across all validators (1 per validator). Stake-weighted participation will be introduced with the staking system.

---

## 12. Network Architecture

### QUIC Mesh

Every validator maintains a persistent QUIC connection to every other validator. With 5,000 validators, this represents roughly 5,000 connections per node. QUIC provides stream multiplexing (parallel requests without head-of-line blocking), persistent connections (no repeated handshakes), and integrated TLS 1.3 encryption.

On machines with 64 GB of RAM, the memory overhead of 5,000 connections (approximately 250 MB) is negligible. The architecture targets an elite validator profile with powerful hardware and excellent network connectivity.

### Gossip Protocol

Vertices are propagated through gossip rather than direct broadcast to avoid the O(n²) message complexity of full broadcast. The gossip parameters are:

- **Production fanout**: 40 peers (when a validator produces a new vertex).
- **Relay fanout**: 10 peers (when forwarding a received vertex).

With a production fanout of 40, a vertex reaches the entire network in approximately 3 hops: 40 → 400 → 4,000+. A deduplication cache with TTL prevents message amplification loops.

### REST API

Users interact with the network through a standard HTTP REST API:

| Endpoint | Method | Description |
|---|---|---|
| `/tx` | POST | Submit a transaction (returns hash, 202 Accepted) |
| `/health` | GET | Node health check |
| `/status` | GET | Consensus state (round, last commit, validators, epoch) |
| `/faucet` | POST | Mint test tokens (returns hash and predicted coin ID) |
| `/validators` | GET | Active validator list with addresses |
| `/object/{id}` | GET | Retrieve an object by ID (with automatic holder routing) |
| `/domain/{name}` | GET | Resolve a domain name to an object ID |

Object retrieval includes transparent routing: if the queried validator is not a holder of the requested object, it forwards the request to the computed holders via Rendezvous Hashing. The `?local=true` parameter disables routing to prevent cascading queries.

### Snapshot and Synchronization

New validators synchronize through state snapshots:

1. The new validator buffers incoming vertices for a configurable period (default 12 seconds).
2. It requests a snapshot from the bootstrap node, containing: all locally stored objects, the current validator set with addresses and BLS keys, the version tracker (18 bytes per tracked object), registered domains, and the last 100 rounds of committed vertices.
3. The snapshot is compressed with zstd and verified via a BLAKE3 checksum over canonically sorted data.
4. The new validator applies the snapshot atomically, replays buffered vertices, and enters normal operation.

Snapshots are created every 10 seconds. A 2-second delay after genesis prevents premature snapshot creation before the network has bootstrapped.

---

## 13. Security Analysis

### Object Attestation Security

A minority of malicious holders cannot corrupt attested objects. Each holder computes the object hash independently from its local copy. A holder sending a falsified object or hash produces an attestation that differs from honest holders, isolating it from the quorum.

With a replication factor of 50, an attacker would need to control 34 holders of a specific object to forge a quorum — a targeted attack that becomes exponentially harder as the replication factor increases. The attacker cannot choose which objects they hold: holder assignment is determined by Rendezvous Hashing using the validator's permanent public key.

### Double-Spend Prevention

Double-spending is prevented by version tracking. If a user submits two transactions spending the same coin, both declare the same version. The first transaction to be committed increments the version; the second encounters a version mismatch and is rejected. This holds even if the transactions are processed by different aggregators and included in different vertices — the DAG's deterministic ordering ensures consistent conflict resolution.

### Aggregator Trust Model

The aggregator has limited trust requirements. It cannot forge holder signatures (it lacks their private keys), cannot exclude valid attestations (any validator can verify the quorum independently), and cannot alter transaction content (the user's Ed25519 signature covers the transaction hash).

The aggregator can only cause liveness failures (by not including a transaction in a vertex), which the user resolves by resubmitting to another validator. This is a Byzantine fault tolerance property: the system remains safe even with a malicious aggregator.

### Ownership Enforcement

The protocol validates that the transaction sender owns all objects in MutableRefs before execution. This check is performed at the protocol level, not inside pods, making it impossible for a buggy or malicious pod to bypass ownership rules. Domain references are exempt from this check, enabling controlled shared-access patterns.

### Malicious Vote Detection

If a holder signs a hash H but provides data D where `BLAKE3(D) ≠ H`, this constitutes provable on-chain misbehavior. The signed hash and the incompatible data serve as a fraud proof, leading to stake slashing.

If the inconsistency is due to network corruption rather than malice, the aggregator simply requests the data from another holder that signed the same hash. No fraud proof is generated, and no penalty is applied.

### Fee Deduction Safety

Fees are always deducted, even on failed transactions. This prevents griefing attacks where an attacker submits transactions designed to fail after consuming validator resources. The `min_gas` requirement (currently 100 units) prevents dust transactions that would cost nearly nothing to submit but still consume processing resources.

Arithmetic overflow in fee computation is handled through `safeMul` and `safeAdd` functions that cap at `MaxUint64` instead of wrapping.

---

## 14. Scaling Characteristics

This section describes the theoretical scaling properties of the architecture. These are not benchmarked numbers — they are back-of-the-envelope analyses based on the protocol design. Real-world performance will depend on network conditions, hardware, geographic distribution, and workload patterns. Proper benchmarking on a distributed testnet remains future work.

### Latency Factors

Finality latency is determined by the critical path: one round-trip to collect attestations from holders, gossip propagation across ~3 hops, and 2 DAG rounds for the commit rule. Each of these steps depends on inter-validator latency, which varies with network topology and geographic distribution. BLS aggregation itself is negligible (sub-millisecond). The architecture is designed to minimize the number of sequential network round-trips, but actual finality time is an open question until measured on a real distributed deployment.

### Bandwidth Scaling

Network load scales linearly with throughput because each transaction is a fixed-size message propagated through gossip. With an average transaction size of ~1.5 KB for common operations (transfers, splits), the bandwidth cost per validator is roughly proportional to the total TPS. The gossip mechanism (fanout 40 for production, 10 for relay) adds a constant multiplier. The exact bandwidth requirements at scale depend on vertex batching behavior and the proportion of singleton-only transactions (which skip the attestation phase entirely).

### Storage Costs

Storage costs are deterministic from the protocol parameters:

Version tracking requires 18 bytes per object in Pebble (8 bytes version + 2 bytes replication + 8 bytes fees). With the key prefix, this is approximately 50 bytes per tracked object. At 1 million objects, the tracker consumes ~50 MB. At 100 million objects, ~5 GB. Every validator tracks every object regardless of whether it holds it — this is the cost of global version tracking for conflict detection.

Object storage depends on holder assignments. A validator's share of stored objects is roughly `replication / total_validators` for each object it holds. Singletons (replication=0) are stored by every validator. At 100 bytes per Coin singleton, 10 million coins consume roughly 1 GB per validator — the main storage cost at scale.

---

## 15. Comparison with Existing Systems

### vs. Sui

BluePods shares Sui's object-oriented data model and optimistic versioning for conflict detection. The key divergence is in **execution distribution**: Sui requires all validators to execute all transactions and store all objects, while BluePods shards both storage and execution across holders.

Sui compensates through parallel execution of non-conflicting transactions on each validator. BluePods achieves parallelism by distributing different transactions to different subsets of validators entirely. The tradeoff is complexity: Sui's model is simpler to reason about, while BluePods' model scales better with validator count but requires the attestation mechanism to ensure holders have correct state.

### vs. Solana

Solana achieves high throughput through a leader-based pipeline with Proof of History for clock synchronization. BluePods eliminates the leader entirely through a DAG-based consensus where all validators produce blocks in parallel. This removes the single-point bottleneck but introduces the complexity of DAG-based ordering and commit rules.

Solana's hardware requirements limit the validator set in practice. BluePods explicitly targets large validator sets by ensuring each validator only processes a fraction of the total workload.

### vs. Ethereum

Ethereum's account-based model with a global state trie requires every validator to process every transaction against the complete state. BluePods' object model decouples unrelated state, enabling horizontal scaling. However, BluePods does not currently support the rich cross-contract composability that Ethereum's global state enables — complex DeFi compositions that touch many objects may face practical limits with the 40-reference cap per transaction.

### What BluePods Borrows

- **From Sui**: the object-oriented state model with version-based conflict detection.
- **From Solana**: the philosophy of dedicated gas payment separate from business inputs, and simplicity in the fee model.
- **From Ethereum EIP-1559**: fee burning for anti-manipulation and deflationary pressure.
- **Original to BluePods**: the `replication_ratio`-based fee model that creates a natural equilibrium between network size and per-validator revenue, the direct attestation mechanism replacing broadcast voting, and the execution sharding where compute follows data.

---

## 16. Open Problems

Several areas remain as future work beyond the current implementation:

**Fraud proofs for incorrect execution.** Multiple holders execute the same transaction and should produce identical results for shared objects. A mechanism to detect and penalize incorrect execution — where a holder produces a different state than its peers — is not yet defined. Potential approaches include periodic cross-holder verification or challenge-based fraud proofs.

**Storage challenge system.** Holders can be challenged to prove they still store the objects they are responsible for. The exact proof format, challenge frequency, and spam prevention for challenges remain to be specified.

**Aggregator failover.** If an aggregator fails mid-collection, the transaction is lost and must be resubmitted. An automatic failover mechanism where another validator takes over as aggregator could improve user experience.

**Inactivity detection and progressive penalties.** Automatic detection of validators that stop producing vertices or responding to attestation requests, with graduated penalties (reduced rewards, then stake reduction, then removal), is planned but not yet implemented. The current system relies on voluntary deregistration.

**Cross-shard composability.** Complex transactions touching many objects with different holder sets may face latency challenges in the attestation phase. Patterns for efficient cross-shard composition remain to be explored.

---

## 17. Conclusion

BluePods demonstrates that horizontal scaling of a Layer 1 blockchain is achievable by making three fundamental shifts: from global state to independent objects, from leader-based block production to leaderless DAG consensus, and from broadcast voting to direct attestation.

The object-oriented model enables natural parallelism and makes conflict detection a simple version comparison. The DAG consensus achieves fast finality without a leader bottleneck. Direct attestation with BLS aggregation keeps network overhead proportional to object replication rather than total validator count. Together, these properties mean that adding validators to the network spreads the workload rather than duplicating it.

The system involves real tradeoffs. Distributed state management is more complex than global state. The 40-reference limit constrains transaction complexity. Fee computation is based on declared gas rather than actual consumption. These are deliberate engineering choices, not oversights, and each section of this document explains the reasoning behind them.

The current implementation covers the core protocol: consensus, attestation, execution, fee deduction, storage sharding, domain naming, and validator management. The open problems outlined in Section 16 represent the natural next steps toward a production-ready network.
