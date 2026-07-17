# Test environment

BluePods has no integration-test framework separate from the language's own.
A scenario is an ordinary Go test in `test/scenarios/`, driving one or more
real node processes through `test/harness/`, a small Go library with no
mocks: real `exec.Cmd` processes, real QUIC, real timers. Scenarios read a
node's structured event stream instead of grepping logs or sleeping, and
every scenario ends with an automatic check of the three cardinal
invariants (convergence, zero rollback, supply conservation) before the
cluster tears down.

Unit tests are unaffected by any of this and remain the tool for isolated
mechanisms; this document covers the scenario corpus only.

## Prerequisites

- Go 1.26 or newer.
- The system pod built once: `cd pods/pod-system && make release && cd ../..`.
  `test/harness` locates `pods/pod-system/build/pod.wasm` by walking up from
  the working directory to `go.mod`; it fails fast with the build command if
  the artifact is missing.
- The node binary itself needs no manual build: the first `harness.NewCluster`
  call in a test binary compiles `cmd/node` and every later cluster in the
  same run reuses that binary.

## Running scenarios

`go test ./test/scenarios/ -run TestScenarioX` is the only runner. There is
no wrapper script and no meta-harness: filtering, timeouts, parallelism and
CI integration all come from the standard toolchain.

```bash
# One scenario, verbose, uncached, with a bounded timeout
go test ./test/scenarios/ -run TestScenarioBootstrap -v -count=1 -timeout 2m

# A heavier, multi-node adversarial scenario needs a longer bound
go test ./test/scenarios/ -run TestScenarioEpochCrash -v -count=1 -timeout 10m
```

Run scenarios **one at a time**, each with its own `-timeout`. They start
real processes and poll real QUIC endpoints, so they are slow relative to
unit tests, and a shared unbounded run makes a hang in one scenario
indistinguishable from a hang in another. `-count=1` disables Go's test
cache, which would otherwise report a stale PASS for a scenario whose
non-deterministic outcome (see "Bug triage protocol" below) can differ
between runs.

Every scenario checks `testing.Short()` first and skips itself, so
`go test -short ./...` (the fast path for the rest of the repository) never
runs the corpus. The current corpus:

| Family | Test | Nodes |
|---|---|---|
| functional | `TestScenarioBootstrap` | 1 |
| functional | `TestScenarioConsensusBasics` | 5 |
| functional | `TestScenarioFees` | 5 |
| functional | `TestScenarioEpochs` | 10 |
| functional | `TestScenarioObjects` | 12 |
| functional | `TestScenarioAggregation` | 5 |
| functional | `TestScenarioSponsored` | 5 |
| functional | `TestScenarioStake` | 5 |
| functional | `TestScenarioJoining` | 5+ |
| functional | `TestScenarioStress` | 12 |
| functional | `TestScenarioChurn` | 2+3 |
| adversarial | `TestScenarioCrash` | 5 |
| adversarial | `TestScenarioAnchorCrash` | 5 |
| adversarial | `TestScenarioJoinLoad` | 5+1 |
| adversarial | `TestScenarioPartition` | 5 |
| adversarial | `TestScenarioEpochCrash` | 10 |
| adversarial | `TestScenarioColdRestart` | 5 |

## Writing a scenario

A scenario is a normal `_test.go` file in `test/scenarios`, package
`scenarios`, importing `BluePods/test/harness` and `BluePods/pkg/client`.
Start it with the short-mode guard every existing scenario uses:

```go
func TestScenarioExample(t *testing.T) {
	if testing.Short() {
		t.Skip("scenario")
	}
	// ...
}
```

### Cluster and node lifecycle

`harness.NewCluster(t, size, opts...)` builds the node binary (once per test
binary), starts `size` real node processes with `--log-format=json
--test-hooks`, waits for the whole cluster to reach round 1, bonds an equal
stake behind every non-founder validator so consensus weight is distributed,
and registers automatic teardown. A minimal scenario:

```go
func TestScenarioExample(t *testing.T) {
	if testing.Short() {
		t.Skip("scenario")
	}

	c := harness.NewCluster(t, 1)
	node, cli := c.Node(0), c.Client(0)

	w := client.NewWallet()
	coinID, hash, err := cli.Faucet(w.Pubkey(), 1_000_000)
	if err != nil {
		t.Fatalf("faucet: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if _, err := node.WaitEvent(ctx, "tx.committed",
		harness.Attr("tx", hex.EncodeToString(hash[:])),
		harness.Attr("success", true),
	); err != nil {
		t.Fatalf("faucet did not commit: %v", err)
	}

	requireNoErr(t, w.RefreshCoin(cli, coinID))
}
```

`Option` values tune the cluster at construction: `WithEpochLength`,
`WithMinValidators`, `WithGossipFanout`, `WithSyncBuffer`, `WithInitialMint`,
`WithTransitionGrace`, `WithTransitionBuffer`, `WithMaxChurn`, `WithStake`,
`WithoutStakeSetup` (leaves only the founder's genesis self-stake, for
testing the founder-heavy regime), and `WithoutInvariants` (below).

Node verbs, used on `c.Node(i)` or through the cluster:

- `cluster.Kill(i)`: SIGKILL, a real crash.
- `cluster.Restart(i)`: starts the same node again with the same key, data
  directory and port, syncing from another alive node. This is a re-sync
  over existing data, not a resume: it is the node's real recovery path, and
  what the harness tests.
- `cluster.Spawn()`: starts a brand-new node that must register and sync,
  returning once it reports `sync.completed`.

### The event journal

The harness parses every JSON stdout line carrying an `event` key into a
per-node, thread-safe journal; every raw line also lands in a per-node
`stdout.log` under the test's temp directory for post-mortem reading. A
journal spans process runs: each `Restart` opens a new segment.

```go
n.WaitEvent(ctx, "tx.committed", harness.Attr("tx", hash))   // block until matched
n.Events("consensus.anchor.committed")                        // read what's there, no blocking
cluster.WaitAll(ctx, "epoch.transitioned", harness.AttrGE("epoch", 2))
```

`harness.Attr(key, want)` matches an attribute by equality (numeric `want`
values compare against the JSON-decoded `float64` regardless of the Go
integer type passed in); `harness.AttrGE(key, min)` matches a numeric
attribute greater than or equal to `min`. Both are `harness.Pred` values, and
`WaitEvent`/`Events`/`WaitAll` accept any number of them, all of which must
match.

**No `time.Sleep` anywhere in a scenario.** `WaitEvent` (and the cluster-wide
`WaitAll`) replace sleeping and blind polling: every wait takes a context or
timeout, so it is bounded, and it returns the instant the matching event
lands rather than after a fixed guess. A scenario that finds itself reaching
for a sleep is missing an event to wait on instead.

### Partitions

`cluster.Partition(groupA, groupB)` installs crossed blocklists so every
member of `groupA` drops mesh traffic to and from every member of `groupB`
(and vice versa), waiting for each affected node to confirm its own
`net.partition.applied`. `cluster.Heal()` clears every blocklist and waits
for `net.partition.cleared` on every alive node. Client traffic from the
harness itself is never blocked, a known artifact of local testing. Both
calls talk to the node's test-only QUIC surface, which is refused on a node
started without `--test-hooks` (the harness always passes it).

### Invariants

`NewCluster` registers `Cluster.CheckInvariants` in `t.Cleanup`, ordered so
it runs before the nodes stop, over the still-alive set: schema drift (an
unparsable JSON line), convergence (all alive nodes reach the same committed
round and the same state fingerprint, polled with a bounded wait for
laggards), zero rollback (anchor rounds strictly increase within a process
segment; a round committed twice never carries two different anchors), and
supply conservation (`coins_total + total_bonded + deposits + fees_in_flight
== total_supply` on every node's fingerprint).

Pass `harness.WithoutInvariants()` only for a scenario that deliberately ends
in a state the checker would reject on purpose (for example, one that ends
inside an open partition). The opt-out must be visible in the scenario code,
never used to silence a real finding — see "Bug triage protocol" below for
what to do when the checker is right to fail.

## Harness API reference

| Symbol | What it does |
|---|---|
| `harness.NewCluster(t, size, opts...) *Cluster` | Builds and starts a cluster, registers teardown |
| `(*Cluster) Node(i) *Node` / `Nodes() []*Node` / `Alive() []*Node` | Access started nodes |
| `(*Cluster) Client(i) *client.Client` / `Daemon(i) *daemon.Daemon` | Client/daemon connected to node `i` |
| `(*Cluster) SystemPod() [32]byte` | System pod ID every client and daemon in the cluster is configured with |
| `(*Cluster) Kill(i)` / `Restart(i)` / `Spawn() *Node` | Node lifecycle |
| `(*Cluster) Partition(a, b []int)` / `Heal()` | Network partitioning |
| `(*Cluster) WaitAll(ctx, name, preds...) error` | Wait for an event on every alive node |
| `(*Cluster) CheckInvariants(t)` | The teardown invariant pass (usually automatic) |
| `(*Cluster) Dump(t)` | Print per-node diagnostics (last events, live status, log paths) |
| `(*Node) WaitEvent(ctx, name, preds...) (Event, error)` | Block until a matching event lands |
| `(*Node) Events(name, preds...) []Event` | Read the journal without blocking |
| `(*Node) Stop() error` / `Kill()` / `Restart(syncFrom string) error` | Process control |
| `(*Node) Alive() bool` / `Journal() *Journal` / `ParseError() error` | Node introspection |
| `harness.Attr(key, want) Pred` / `AttrGE(key, min) Pred` | Event attribute predicates |
| `harness.WithEpochLength/WithMinValidators/WithGossipFanout/WithSyncBuffer/WithInitialMint/WithTransitionGrace/WithTransitionBuffer/WithMaxChurn/WithStake` | Cluster tuning options |
| `harness.WithoutStakeSetup()` / `WithoutInvariants()` | Opt out of a default |

Client and daemon operations (`pkg/client`, `pkg/daemon`) are the same code
a real client uses: `Wallet.Split/Transfer/CreateObject/TransferObject/
SetObject/Bond/RegisterValidator/DeregisterValidator`, `Client.Faucet/
Status/GetObject/GetTxStatus/Validators/Fingerprint/SetPartition/
ClearPartition`. Events say what happened; these queries say what is;
scenarios confront the two.

## Event catalog

Every mutation of persisted state emits a typed event through a constructor
in `internal/events` (`internal/events/catalog.go` for the full name list,
one file per domain for the constructors). The table below is copied from
that package and is kept current as events are added: **event names and
their existing attributes are stable**, depended on by the harness and by
any production log consumer. New attributes and new events may be added
freely. Renaming or removing a name or an attribute is a breaking change and
must be called out in the commit that does it.

| Event | Stable attributes |
|---|---|
| `node.started` | pubkey, quic_addr |
| `node.ready` | round |
| `node.stopping` | reason |
| `ingress.tx.received` | tx, kind |
| `ingress.tx.rejected` | reason, detail |
| `consensus.vertex.produced` | vertex, round, txs |
| `consensus.vertex.received` | vertex, round, producer |
| `consensus.vertex.rejected` | vertex, reason |
| `consensus.round.advanced` | round, designated |
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
| `stake.released` | validator, coin, amount |
| `supply.issued` | epoch, amount, rate |
| `supply.burned` | amount, reason |
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

`tx.committed`'s `reason` attribute is always present: an empty string on
success, and one of a fixed set when `success` is false: `version_conflict`,
`fee_rejected`, `ownership`, `proof_failed`, `authenticity_failed`,
`duplicate`, `expired_sponsorship`, `execution_error`.

`consensus.vertex.rejected`'s `reason` attribute is one of a fixed set:
`bad_signature`, `wrong_epoch`, `parent_round`, `parent_quorum`,
`fee_summary`, or `unknown` (a defensive fallback for a validation failure
path that does not map to any of the above; should not occur in practice).

Attribute values use stable encodings: hashes and object/validator IDs as
lowercase hex, rounds and versions as integers, reasons as short snake_case
strings from a fixed set per event. Every record also carries `node` (a
short prefix of the emitting validator's pubkey), stripped into `Event.Node`
by the journal rather than left in `Event.Attrs`.

## Maintenance rule

- Every new mutation of persisted state gets a constructor in
  `internal/events`, called at the point of mutation. If the taxonomy above
  has no matching row yet, add one, in both this table and the package.
- Every new feature gets a new scenario, or extends an existing one, in
  `test/scenarios`. The corpus is the record of what the harness proves
  about the system; a feature with no scenario is unverified by
  construction.
- Renaming or removing an event name or an existing attribute is a breaking
  change: both the harness and any production log consumer depend on the
  current names. Call it out explicitly in the commit that does it.

## Bug triage protocol

A scenario failing against otherwise-correct harness behavior has found a
project bug, not a test bug.

Before treating a red as the project's, the burden of proof is on the
harness:

1. Rule out a race or a timeout in the test itself. Rerun the scenario;
   check whether a longer bound or a harness fix removes the failure.
2. A flaky scenario is a harness defect: always in scope to fix directly.
3. Once a failure is confirmed as the project's, it is fixed on a
   dedicated branch — root cause first, a failing test that pins the
   mechanism, then the fix — or, when it cannot be fixed now, filed as a
   GitHub issue carrying the suspected subsystem, the reproducing
   scenario(s), and the event-journal evidence.
4. Until its fix lands, the scenario stays red as the living
   reproduction. It is never skipped, weakened, or worked around.
5. A later scenario reproducing a known open issue is added to that
   issue, not filed as a new one.
