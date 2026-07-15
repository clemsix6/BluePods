# Test environment redesign: structured events and scenario harness

## Goal

Rebuild the BluePods test environment around two pieces delivered in one cycle:

1. A structured event system in the node. Every QUIC request that changes state, and every state change that follows from it, emits a typed, machine-parsable event. Reading a node's event stream tells you exactly what the node did and when.
2. A scenario harness. A Go library that controls a cluster of real node processes, reads their event streams, injects transactions through the real client path, kills and spawns nodes, partitions the network, and checks the cardinal invariants after every scenario.

The existing acceptance test plan (`docs/ATP.md`) and the integration suite (`test/integration/`) are deleted and replaced by this system. Unit tests are unaffected and remain the tool for isolated mechanisms.

## Non-goals

- Latency and packet-loss injection (a later iteration; the partition filter is the extension point).
- Deterministic simulation testing. Real processes, real QUIC and real timers stay timing-dependent; the harness reduces flakiness with event-driven waits, it does not eliminate nondeterminism.
- Log sampling, rotation, shipping, or dashboard tooling. The JSON stream is designed so those can be built later without touching the node.
- Performance benchmarking.
- Any change to the Rust side (`pods/`, `wasm-gas/`).
- **Fixing the bugs the scenarios uncover.** The consensus likely has real bugs, and the corpus is expected to find them. Fixing them is the next project, not this one; this cycle delivers the instrument. See "Found bugs are recorded, not fixed" below.

## Part 1: structured events

### Two channels, one stream

The node keeps a single log stream on stdout, carried by `slog`, with two kinds of records:

- **Events**: typed records marking a state change. Emitted only through constructors in a new `internal/events` package. Each carries the reserved attribute `event` with a stable dotted name (for example `tx.committed`). The record message equals the event name.
- **Human logs**: free-form `logger.Debug/Info/Warn/Error` calls. They never carry the `event` key. The harness ignores them; humans read them. Their wording can change at any time without breaking anything.

### The events package

`internal/events` defines one Go constructor per event, for example:

```go
events.TxCommitted(txHash, vertexHash, round, success, reason)
events.VertexReceived(vertexHash, producer, round)
events.EpochTransitioned(epoch, added, removed)
```

Constructors are the only way to emit an event. The schema lives in the function signatures, so it is enforced at compile time and cannot drift silently. The package is dependency-light (slog plus basic types) so every subsystem can import it.

Events are emitted at Info level. Attribute values use stable encodings: hashes and IDs as lowercase hex, rounds and versions as integers, reasons as short snake_case strings from a fixed set per event.

### Taxonomy

The governing rule: **every mutation of persisted state emits an event**, carrying the identifiers needed to reconstruct causality (transaction hash, vertex hash, round, object ID, versions). The list below is the initial catalog; the rule, not the list, is the contract.

| Event | Stable attributes |
|---|---|
| `node.started` | pubkey, quic_addr |
| `node.ready` | round (listener up, genesis or sync complete) |
| `node.stopping` | reason |
| `ingress.tx.received` | tx, kind (raw or attested) |
| `ingress.tx.rejected` | reason, detail |
| `consensus.vertex.produced` | vertex, round, txs |
| `consensus.vertex.received` | vertex, round, producer |
| `consensus.vertex.rejected` | vertex, reason |
| `consensus.round.advanced` | round, designated (the round's designated anchor producer) |
| `consensus.anchor.committed` | round, anchor, producer, vertices |
| `consensus.round.skipped` | round |
| `tx.committed` | tx, vertex, round, success, reason |
| `tx.executed` | tx, success, error_code |
| `state.object.created` | object, tx, version, replication, owner |
| `state.object.updated` | object, tx, version |
| `state.object.deleted` | object, tx, refund |
| `state.domain.registered` | name, object, tx |
| `state.domain.updated` | name, object, tx |
| `state.domain.deleted` | name, tx |
| `fees.deducted` | tx, coin, amount, covered |
| `fees.deposit.locked` | object, amount |
| `fees.deposit.refunded` | object, coin, amount |
| `stake.bonded` | validator, coin, amount |
| `stake.unbonded` | validator, coin, amount |
| `stake.delegated` | validator, position, amount |
| `stake.undelegated` | validator, position, amount |
| `supply.issued` | epoch, amount, rate |
| `supply.burned` | amount, reason (deletion) |
| `epoch.transitioned` | epoch, added, removed |
| `epoch.validator.registered` | validator, quic_addr |
| `epoch.validator.deregistered` | validator |
| `epoch.rewards.distributed` | epoch, pool, validators |
| `sync.snapshot.created` | round, checksum |
| `sync.snapshot.applied` | round, checksum, objects |
| `sync.completed` | round |
| `net.peer.connected` | peer |
| `net.peer.disconnected` | peer |
| `net.partition.applied` | blocked |
| `net.partition.cleared` | (none) |

`tx.committed` failure reasons form a fixed set: `version_conflict`, `fee_rejected`, `ownership`, `proof_failed`, `authenticity_failed`, `duplicate`, `expired_sponsorship`, `execution_error`. Today the commit path does not distinguish all of these (proof and authenticity failures share one code path, the duplicate path emits nothing), so part of the migration is new plumbing in the commit path, not just converting existing logs.

### Schema stability

Event names and their existing attributes are stable: production tooling and the harness both depend on them. New attributes and new events may be added freely. Renaming or removing a name or an attribute is a breaking change and must be called out in the commit that does it. The catalog table in `test/TESTING.md` is kept current as events are added.

### Output format and node identity

A `--log-format=text|json` flag on the node selects the handler:

- `text` (default): the current human-readable handler, for a person watching a node.
- `json`: the standard `slog.JSONHandler` configured at Debug level, one JSON object per line, so human Debug logs are preserved in the per-node post-mortem files. This is what the harness and any production collector consume.

`--log-format=json` implies plain line output: the TUI dashboard is disabled, as with the existing `--log` flag.

Both handlers write to stdout, a single stream. Every record carries a `node` attribute (short prefix of the validator pubkey) so aggregated multi-node logs stay attributable. Two fixes make this work: the text handler's `WithAttrs` currently drops logger-level attributes and must be implemented properly, and the attribute is installed right after the key is loaded (records before that point lack it, which is accepted).

### Migration

`internal/logger` remains the seam and keeps its API for human logs; its custom handler becomes the `text` format. The work is a subsystem-by-subsystem sweep (`internal/consensus` first with the majority of call sites, then `cmd/node`, `internal/state`, `internal/sync`, `internal/network`) placing an `events.X(...)` call at every state mutation. Many existing Debug logs on the commit path mark exactly these mutations and become typed events; the rest stay human logs; a few events need new plumbing where the code does not yet distinguish the cases the taxonomy names.

## Prerequisite protocol fixes

The invariant checker (below) verifies conservation laws the current code breaks. These four fixes are in scope, and they are the only protocol fixes that are: they are not bugs the scenarios will discover but already-known defects that would blind the instrument itself (a supply check that is always red measures nothing). Everything the scenarios discover later is out of scope:

- **Partial fee coverage leaks supply.** When a gas coin cannot cover the fee, the balance is taken from the coin but never pooled: it leaves coins and enters no accounted term. Fix: pool the partially deducted amount into the epoch pool like any consumed fee.
- **Reward crediting needs a destination.** The liquid share of epoch rewards is credited to a validator's designated reward coin, but the founder never registers one (genesis does not seed it) and the registration path never passes one, so at every boundary with a non-zero pool the liquid share silently vanishes while the pool resets. Fix: seed a reward coin for the founder at genesis and have `register_validator` (client and harness paths) designate one.
- **Coins are not identifiable.** Objects carry no type tag, so no node can compute "sum of coin balances" reliably (delegation positions and arbitrary 8-byte payloads would be miscounted). Fix: maintain a `coins_total` protocol counter in the commit path next to `total_supply`, updated wherever coin balances change (faucet, fees, refunds, rewards, bond/unbond, genesis).
- **Genesis re-seeding corrupts restarted bootstraps.** The bootstrap node re-runs genesis seeding unconditionally at startup, which would overwrite the reserve coin and `total_supply` on restart. Fix: guard seeding on pre-existing state, making the bootstrap restartable like any node.

## Part 2: the scenario harness

### Layout

- `test/harness/`: the orchestration library.
- `test/scenarios/`: the scenarios, ordinary `_test.go` files run by `go test`.
- `test/TESTING.md`: the document of record for the test environment.

`go test ./test/scenarios/ -run TestScenarioX` is the only runner. Filtering, timeouts, parallelism, cleanup and CI integration come from the standard toolchain.

### Cluster and node lifecycle

`harness.NewCluster(t, n, opts...)` builds the node binary once, starts n real processes (`exec.Cmd`) each with `--log-format=json` and `--test-hooks`, waits for readiness via events, and registers teardown in `t.Cleanup`. Options cover epoch length, min validators, the transition window, gossip fanout, sync buffer window and initial mint, as the current helpers do. Ports are allocated per scenario from disjoint ranges so scenarios can run in parallel without collisions.

By default the cluster setup faucets and bonds an equal stake on every validator once the network is ready, so consensus weight is distributed and partition arithmetic is meaningful; a scenario can opt out to test the founder-heavy genesis regime. This requires a `Bond` operation in `pkg/client` (missing today), which the system pod already supports.

Node verbs:

- `Stop()`: graceful shutdown.
- `Kill()`: SIGKILL, a real crash.
- `Restart()`: start the same process again with the same key, data directory and port. The restarted node syncs from a configurable alive node (not necessarily its original bootstrap address). Restart is a re-sync over existing data, not a resume; that is the node's real recovery path and is what the harness tests.
- `cluster.Spawn()`: start a brand-new node that must register and sync.

### Event journal

The harness consumes each node's stdout line by line. Lines carrying the `event` key are parsed into a per-node in-memory journal; every raw line is also appended to a per-node file under the test temp directory for post-mortem reading. In JSON mode, an unparsable stdout line fails the scenario: it is the schema-drift detector. One exception: the final, possibly truncated line of a killed node is exempt, since SIGKILL can cut a write mid-line.

A journal spans process runs: each restart opens a new segment, and the journal records the boundary. Scenario-facing API (representative, not exhaustive):

```go
n.WaitEvent(ctx, "tx.committed", harness.Attr("tx", hash))     // block until matched
n.Events("consensus.anchor.committed")                          // journal read
cluster.WaitAll(ctx, "epoch.transitioned", harness.Attr("epoch", 2))
```

`WaitEvent` replaces sleeps and blind polling; scenarios contain no `time.Sleep`. Matching is by event name plus attribute predicates (equality, numeric comparison).

### State queries and transaction injection

The harness talks to nodes exactly as a mainnet client would: `pkg/client` for status, objects, validators and raw submissions; `pkg/daemon` for attestation collection and attested submissions. Wallet-level helpers (faucet, split, transfer, create object, bond) wrap these. Events say what happened; queries say what is; scenarios confront the two.

### State fingerprint

Behind `--test-hooks`, the node answers a `state-fingerprint` QUIC message with the last committed round, a convergence fingerprint, and the supply terms.

The fingerprint is a BLAKE3 hash over canonically sorted **globally replicated state only**: tracker entries (every node tracks every object), the domain registry, the validator set with stakes and pending changes, epoch state, singleton objects, `total_supply` and `coins_total`. It deliberately excludes holder-scoped data (standard object content, the attestation signature store), which differs between correct nodes by design, and it excludes the current round, which advances even when idle. It reuses the snapshot system's canonical-sort-and-hash pattern and is computed under the same consistent-cut hold the snapshot exporter uses, so the terms form one atomic cut. Two nodes at the same committed round with the same fingerprint hold the same replicated state.

The supply terms are `total_supply`, `coins_total`, `total_bonded`, the sum of locked deposits from the tracker, and the epoch's accumulated fees in flight, all globally replicated and locally evaluable.

### Partitions

The node's network layer gains a blocklist of peer pubkeys, empty by default. While a peer is blocked, mesh traffic to and from it is silently dropped on both the gossip stream and the request-response seam (vertex fetch, snapshot serving, inter-node object routing), so a partitioned node neither hears from nor serves its blocked peers. The inbound filter runs before deduplication marks a message as seen, so gossip redelivered after healing is not lost to the dedup cache; post-heal catch-up otherwise rides the existing stall-fetch recovery. The QUIC connection object survives; data stops flowing, which is what an asymmetric or total partition looks like from the application.

The blocklist is set and cleared by a test-only QUIC control message. `cluster.Partition(groupA, groupB)` installs crossed blocklists on every member; `cluster.Heal()` clears them. Client traffic from the harness is never blocked: the observer keeps its control plane. This is a known artifact of local testing (a real partition would also cut clients) and is accepted.

Without `--test-hooks`, the fingerprint and partition messages get a static refusal. Production binaries never expose them.

### Failure diagnostics

When a wait times out or an assertion fails, the harness automatically dumps: the last events of every node, each node's QUIC status (round, epoch, last commit), and the paths of the full per-node log files. A failing scenario must be diagnosable without rerunning it.

## Invariant checker

Every scenario ends with an automatic invariant pass, run in teardown before the nodes stop, over all alive nodes:

- **Convergence**: all alive nodes reach the same committed round and identical state fingerprints. The check waits (bounded) for laggards to catch up and for injection to quiesce before comparing.
- **Zero rollback**: within each process run of each node, `consensus.anchor.committed` rounds are strictly increasing; across runs and across nodes, any round committed twice must carry the same anchor hash. A crash can legitimately re-decide the last round after restart; it must decide it identically. A contradicted commit anywhere is a failure.
- **Supply**: `coins_total + total_bonded + sum(locked deposits) + fees_in_flight == total_supply` on every node, from the fingerprint terms.

These are the two cardinal properties of VISION.md plus the whitepaper's supply invariant, verified on every scenario for free. A scenario may opt out explicitly (for example one that deliberately ends inside an open partition), and the opt-out is visible in the scenario code.

## Scenario corpus

Two families, one directory. Functional scenarios carry forward the value of the old suite, rewritten on events; adversarial scenarios are the new capability and the reason the harness exists. Partition splits assume the default equal-stake setup; the exact quorum test is `3 x capped_sum >= 2 x total`, so with 5 equal stakes a 4-node side has quorum (12 >= 10) and a 3-node side does not (9 < 10).

| Family | Scenario | Nodes | Focus |
|---|---|---|---|
| functional | bootstrap | 1 | single-node startup, basic ops, faucet, validation rejects |
| functional | consensus basics | 5 | client ops (split, transfer, objects), tracking, security rejects |
| functional | fees | 5 | deduction, gas coin rules, underfunded fees, cross-node consistency |
| functional | epochs | 10 | transitions, churn, register/deregister, holder snapshot, rewards |
| functional | objects and sharding | 12 | rendezvous, singleton replication, routing, local-only |
| functional | aggregation | 5 | attested path, version races, epoch-boundary grace, cold holder |
| functional | joining | 5+ | progressive and batch joining, convergence |
| functional | stress | 12 | concurrent modification, double spend under load, tx during epoch transition |
| adversarial | crash and recover | 5 | kill a node under traffic, restart, it catches up |
| adversarial | anchor crash | 5 | kill the designated anchor producer of an upcoming round (identified via the `designated` attribute) |
| adversarial | join under load | 5+1 | spawn a node under traffic, it syncs and converges |
| adversarial | minority partition | 5 | 4|1 split: majority side commits, isolated node halts |
| adversarial | symmetric split | 5 | 3|2 split: no side has quorum, the whole network halts, nobody forks |
| adversarial | heal | 5 | partition heals, stalled nodes catch up, never contradict a commit |
| adversarial | epoch crash | 10 | kill -9 during an epoch transition, restart, converge |

## Found bugs are recorded, not fixed

A scenario that fails against correct harness behavior has found a project bug. The bug is registered in `test/BUGS.md`: one entry per bug with the scenario that reproduces it, the suspected subsystem, and the event-journal evidence. The scenario stays red as the living reproduction; it is not skipped, weakened or worked around, and the bug is not fixed in this cycle.

The burden of proof sits with the harness: before registering a bug, the failure must be shown to be the project's (cross-node event evidence, reproducible on rerun), not a race or a timeout in the test. A flaky scenario is a harness defect and is always in scope to fix.

## Deletions and documentation

- Delete `docs/ATP.md` and every reference to it.
- Delete `test/integration/` entirely once the corpus above covers it. Coverage, not greenness, is the bar: a new scenario that is red because it found a real bug still replaces its ancestor.
- Write `test/TESTING.md`: how to run scenarios, how to write one, the harness API, the event catalog, the maintenance rule.
- Update `CLAUDE.md`: point to `test/TESTING.md`, drop the ATP mention, and add the maintenance rule as a principle: every new state mutation gets an `internal/events` constructor, every new feature extends or adds a scenario, renaming an event is a breaking change to call out.

## Error handling

- A node that fails to start fails the scenario immediately with its captured stderr.
- An unparsable stdout line in JSON mode fails the scenario (schema drift detector), except the truncated final line of a killed node.
- Every wait takes a context or timeout; defaults are per-scenario configurable and bounded. No unbounded waits.
- Fingerprint or partition messages against a node without `--test-hooks` return a typed refusal, and the harness reports it as a setup error.

## Testing the system itself

The harness engine gets unit tests where logic lives: journal parsing and attribute matching, timeout behavior, restart segment boundaries, teardown ordering. The partition filter gets unit tests in `internal/network`. The `internal/events` package gets a test asserting name uniqueness and catalog consistency. The prerequisite protocol fixes get unit tests next to the code they fix. The scenarios themselves are the integration proof of the harness; there is no meta-harness.

## Success criteria

- Every persisted-state mutation in the node emits a typed event through `internal/events`; both output formats work; the event catalog is documented.
- The harness can start, stop, kill, restart, spawn and partition nodes, journal their events, query their state and inject transactions through the real client path.
- The full scenario corpus runs with the invariant checker active on each scenario. Green is not required: a red scenario must always mean a bug registered in `test/BUGS.md`, never harness flakiness. Reruns of the corpus produce the same verdicts.
- No `time.Sleep` in any scenario.
- `docs/ATP.md` and `test/integration/` are gone; `test/TESTING.md` and `CLAUDE.md` are current.
