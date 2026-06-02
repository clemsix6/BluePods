# Usage layer: node and client terminal UIs

**Goal:** make BluePods interactable. The protocol is advanced but abstract: there is no comfortable way to watch a node or to drive the chain by hand. This chantier reworks the two existing binaries into live terminal interfaces, a node dashboard and a client console, backed by a small set of clean data sources rather than by log scraping. It also tidies the repository layout and shapes the client library so that everything a human does by hand is trivially scriptable later.

**Scope.** It covers: a project-layout move (`client` and `daemon` into `pkg/`), a single in-node stats source feeding three consumers, three small additions to the QUIC client surface (richer status, a peer count, a transaction-status query), a bounded per-transaction outcome index, wallet persistence on the client SDK, and two Bubble Tea interfaces. It does not change consensus, the object model, the economic layer, or the off-chain attestation mechanism. It adds read-only observability and a usage front end on top of what exists.

**Guiding constraints.** Two non-negotiables shape every decision below.

1. The display layer is a pure renderer over a well-defined state source. Nothing shown on screen may bypass that state or be assembled by scraping logs. A node already emits everything needed; the interfaces consume it, they do not invent it.
2. Every manual action is a typed method on the client library, returning a structured result and an error, printing nothing. The interfaces and a future simulator are thin adapters over that one surface, with no action logic living in a binary.

---

## 1. Two binaries, and a deferred third

`cmd/node` and `cmd/cli` already exist and work; both are reworked, not rewritten.

- `cmd/node` runs a node. When attached to a real terminal it shows a live dashboard; otherwise it logs as today. A `--log` flag forces logs even on a terminal.
- `cmd/cli` (`bpctl`) is the client. Run bare on a terminal it opens an interactive console (a status header, an activity body, a command input line). Run with a subcommand it behaves as a one-shot tool, preserved for scripting and tests.
- A future `cmd/simulator` is the natural home for automated fake users. It is out of scope here, but the client library is shaped for it now (section 7).

## 2. Project layout

The repository keeps build and runtime artifacts (`bin/`, `data/`, the `node` and `bpctl` binaries, `.DS_Store`) out of git already, so the tracked tree is close to clean. The one wart is that `client` and `daemon` sit loose at the root next to `internal/`, with no symmetric bucket for the public, importable libraries.

The fix is a `pkg/` bucket, mirroring the Docker and Kubernetes convention: `internal/` holds what is private to the module, `pkg/` holds what external code may import.

- `client/` moves to `pkg/client/`, `daemon/` moves to `pkg/daemon/`.
- Import paths become `BluePods/pkg/client` and `BluePods/pkg/daemon`. About fifteen files reference these packages; the rewrite is mechanical.
- The `daemon` stays the submission engine encapsulated by `client` (an unexported field), not merged into it. The two are layers, not duplicates: `daemon` collects attestations, assembles an ATX, and submits; `client` is the SDK (wallet, keys, transaction building, high-level operations) built on that engine. Keeping them separate preserves the deliberate dependency hygiene that lets `daemon` become its own module when a non-Go consumer appears.

`types/` (the root `.fbs` schema directory) is left as is. It holds FlatBuffers schemas, not Go, and the generated Go already lives in `internal/types`.

## 3. The node: one stats source, three consumers

The node already exposes everything the dashboard needs through one clean stream. `DAG.Committed()` is a channel of `CommittedTx`, emitted for every committed transaction, and `cmd/node` already drains it in `processCommitted()` (today it only logs). That single consumer is the one place transaction-derived numbers are accumulated.

- A thread-safe `Stats` value accumulates only what the transaction stream carries: total finalized, total failed, and a rolling window from which a transactions-per-second figure is computed. It is fed exclusively from `processCommitted()`.
- Round, epoch, validator count, and connected-peer count are not stored in `Stats`. They are read live from their authoritative sources (the DAG and the network layer) at the moment a snapshot is assembled. This avoids duplicated counters: each number has exactly one owner.
- A `StatusSnapshot` is assembled on demand by reading `Stats` plus the live DAG and network values. It is the single shape consumed by the dashboard, by the log line, and by the QUIC status handler, so a remote client sees the same numbers the node renders locally.

Display mode is chosen by terminal detection: a real TTY runs the Bubble Tea dashboard, anything else (a pipe, a test harness, a service manager) logs. `--log` forces logs on a TTY. The dashboard lives in `cmd/node/tui/` and reads `StatusSnapshot`; it computes nothing.

## 4. QUIC substrate additions

Three small, additive changes to the client-facing QUIC surface. These are the only protocol-level additions; all are reads.

- `StatusResponse` gains `TotalTx`, `TPS`, and `ConnectedPeers`. The response is a fixed-layout binary encoding, so the struct and its encode and decode are extended together.
- `network.Node` gains `ConnectedPeers() int`, the length of its peer map taken under the existing lock. This is net new; no such accessor exists today.
- A new `GetTxStatus(hash)` message returns a transaction's state and, on failure, its reason (section 5).

## 5. Transaction status tracking

The consensus already determines a per-transaction outcome at commit. `executeTx` calls `emitTransaction(tx, success)` on every path, success and failure alike, and the result reaches `Committed()`. The minimum a user wants, finalized or failed, is therefore already emitted; the reason is known at the call site and is the one piece not yet carried.

- `CommittedTx` gains a `Reason` field, an enum over `none`, `version`, `fee`, `owner`, `auth`, `revert`, and `expired`, populated at the commit-path sites that already distinguish these cases.
- A bounded recent-transaction index maps a transaction hash to its state and reason. It is fed at two clean points: at submission ingress the hash is recorded as `PENDING`, and on the `Committed()` stream it flips to `FINALIZED` (success) or `FAILED` with a reason. Entries are evicted by round, which finally implements the prune the committed-hash guard already flags as a TODO.
- `GetTxStatus` reads this index and returns `PENDING`, `FINALIZED`, `FAILED` with reason, or `UNKNOWN` (never seen, or evicted). The honest limit: `PENDING` is known only for transactions submitted to this node, which is exactly the node a client polls after submitting through it.

This is a read-only observability index. It is not consulted by consensus and changes no commit behavior.

## 6. The client: wallet, console, one-shot commands

Because there is no owner-to-objects index in the protocol (a client cannot enumerate its objects), the wallet must know which coins it owns to show a balance. The chosen approach is local and protocol-free.

- `client.Wallet` gains `Load` and `Save`. The CLI persists a small wallet file next to the key, holding the key and the set of known coin IDs. Coins from the faucet and from creation are tracked automatically; a coin received from someone else is added with `coin import <id>`. Balance is the polled sum of `GetObject(id).balance` over known coins. This is faithful to the object model and changes no protocol surface. The accepted limitation is that a received coin must be imported by ID, since auto-discovery would require an owner index, which is out of scope.
- Wallet construction stays in memory: `NewWallet()` needs no file. File persistence is a CLI concern only, never required to build a wallet (this matters for section 7).
- The interactive console is a Bubble Tea program in `cmd/cli/tui/`. Its model holds the connection target and reachability, the latest `StatusSnapshot`, the wallet with live balances, the set of tracked transaction statuses, and the input line. A ticker polls status, balances, and the statuses of tracked transactions. A command typed in the footer is dispatched to `pkg/client` as a `tea.Cmd`, its result is appended to the activity body, and any returned hash is tracked automatically. The header shows the connection, chain status, and total balance; the body is the activity and live transaction-status log; the footer is the input.
- The one-shot subcommands are kept. The interactive console replaces the old `watch` command, which is removed.

## 7. Automation readiness

The simulation goal (many automated fake users driving the chain) is met not by building the simulator now but by shaping the action surface correctly.

- Every manual action is a typed method on `pkg/client` returning a structured result, the transaction hash for tracking, and an error, and printing nothing. This already holds for transfer and faucet. Action logic still living in `cmd/cli/object.go` and `cmd/cli/holders.go` is moved down into `pkg/client` so that no behavior is trapped in a binary.
- Three adapters consume that one surface with no duplicated logic: the one-shot CLI maps argv to a method to printed output, the console maps typed input to a method to a rendered result, and the future `cmd/simulator` maps a scripted or randomized policy to method calls. The console's text parser is a convenience over the typed methods; a simulator calls the methods directly rather than passing strings.
- Wallets are independent and cheap, constructed in memory, so a simulator can hold many of them and run several `Client` instances against one or more nodes.
- Outcome awaiting is programmatic through `GetTxStatus`, so a scenario can wait for `FINALIZED` or `FAILED` per action.
- The multi-wallet integration simulation (section 11) is the readiness proof: it is automation through the same surface a simulator would use.

## 8. TUI library

Both interfaces use Bubble Tea v2 with Lip Gloss and the Bubbles components (`textinput`, `viewport`). The Elm architecture (model, message, update, pure view) structurally enforces the renderer-over-state constraint: a display that does not derive from the model does not fit the pattern, and all change (clock ticks, QUIC responses) arrives as a message handled in update. The core stays small because only the needed components are pulled in. tview was the alternative and would be less code for this exact layout, but its mutable-widget state is the kind of thing that drifts toward the unclean over time. Raw tcell was rejected as hand-rolled layout, the opposite of the clean mandate.

## 9. Package layout and boundaries

- `pkg/client` is the single high-level library and the whole action surface. `pkg/daemon` is its internal engine.
- The node dashboard lives in `cmd/node/tui/` and the node's `Stats` and snapshot assembly in `cmd/node` (for example `cmd/node/stats.go`). The console lives in `cmd/cli/tui/`.
- No shared TUI package is introduced up front. A small shared Lip Gloss style helper is extracted only if the header and dashboard genuinely duplicate styling, not before.

## 10. Error handling

- Node: no TTY or `--log` falls back to logs. Terminal resize is handled by Bubble Tea's window-size message. Before the DAG is ready (bootstrap), the snapshot reads as zeros or a starting state rather than failing.
- Client console: an unreachable node shows a disconnected header and an error in the activity log, and the next poll retries. A status that is `UNKNOWN` after eviction is shown as such. A missing wallet file is created on first save; a corrupt one surfaces a clear error. A coin that has disappeared shows its balance line as unavailable.
- One-shot commands return a non-zero exit on error, so scripts can rely on them.

## 11. Testing

- Bubble Tea models are pure, so update and view are unit-tested by feeding messages and asserting the resulting model and rendered output.
- `Stats` is tested for its counters and its TPS window over a sequence of `CommittedTx`. The transaction index is tested for the `PENDING` to `FINALIZED` and `FAILED` transitions and for eviction.
- The QUIC additions are tested at the encode and decode level for the new status fields and `GetTxStatus`, and at the handler level.
- Wallet `Load`, `Save`, and `import` are tested as a round-trip.
- One integration simulation drives `pkg/client` with multiple wallets, submits a transaction, polls `GetTxStatus` until `FINALIZED`, and asserts that `Status` reports a transaction count and a TPS figure. The interfaces themselves are not driven end to end through a terminal; their data and model layers are what is tested.

## 12. Implementation decomposition

One design of record, one plan, ordered batches so each builds on a tested base.

- **Batch 0, restructure.** Move `client` to `pkg/client` and `daemon` to `pkg/daemon`, rewrite imports, confirm a green build and test run. Pure mechanical change, no behavior.
- **Batch A, substrate.** `CommittedTx.Reason`, the bounded transaction index, the extended `StatusResponse`, `ConnectedPeers()`, and `GetTxStatus` (handler and client method). Fully tested, no interface yet.
- **Batch B, node.** `Stats` and snapshot assembly, terminal detection and `--log`, the Bubble Tea dashboard.
- **Batch C, client.** Wallet `Load` and `Save`, the move of remaining action logic into `pkg/client`, the Bubble Tea console with command dispatch and automatic transaction tracking, and removal of `watch`.

## 13. Rejected alternatives

| Decision | Rejected | Why |
|---|---|---|
| Node display | always-on full-screen TUI | hostile to tests, background, and service managers; TTY detection with a `--log` override is cleaner |
| Transaction status | inferring state by polling the touched object | a heuristic with no failure reason, the kind of guess the clean mandate forbids; the `Committed()` stream already carries the outcome |
| Coin discovery | an owner-to-coins index served over QUIC | reintroduces an account-style global view; out of scope, the local wallet file suffices to start |
| client and daemon | merging daemon into client | they are layers (engine and SDK), not duplicates; merging breaks the dependency hygiene that keeps daemon extractable |
| Layout | leaving client and daemon at the root | no symmetric bucket for public libraries; `pkg/` mirrors `internal/` |
| TUI library | tview, raw tcell | mutable-widget state drifts unclean; raw tcell is hand-rolled layout |
| Simulator | building `cmd/simulator` now | deferred; shaping the action surface for it is what matters now |

## 14. Out of scope and doc impact

Out of scope: the `cmd/simulator` binary, an owner-to-coins index and any account-style enumeration, a shared TUI package, metrics or Prometheus surfaces, and any change to consensus, the economic layer, or the off-chain attestation mechanism.

Doc impact, to apply once implemented: the whitepaper's QUIC client surface table (section 11) gains `GetTxStatus` and the richer status fields; the README build and run section reflects the `pkg/` paths and the new interface behavior. These are edited in place when the work lands.
