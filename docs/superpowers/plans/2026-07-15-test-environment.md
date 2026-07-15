# Test Environment Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Structured typed events in the node plus a scenario harness that orchestrates real node processes, per `docs/superpowers/specs/2026-07-15-test-environment-design.md`.

**Architecture:** One slog stream with two channels (typed events via `internal/events` constructors, free human logs), four prerequisite protocol conservation fixes, test-only QUIC hooks (state fingerprint, partition control) behind `--test-hooks`, a `test/harness` library driving `exec.Cmd` node processes through their JSON event streams, and a scenario corpus under `test/scenarios` with an invariant checker on every scenario.

**Tech Stack:** Go stdlib only (`log/slog`, `os/exec`, `encoding/json`). No new dependencies.

## Global Constraints

- All implementation happens in the worktree `.wt/test-harness` on branch `feat/test-harness`. Every command uses `-C`: `go -C /Users/clement/BluePods/.wt/test-harness test ./...`, `git -C /Users/clement/BluePods/.wt/test-harness commit ...`. Never touch the main checkout.
- 1 task = 1 commit. Commit format per the repo convention: plain title line, then `[+]/[&]/[!]/[-]` body lines, no footers, no Co-Authored-By.
- Each batch leaves the build green: `go build ./... && go vet ./... && go test ./internal/... ./pkg/... ./cmd/...` must pass at the end of every batch (`./test/harness/...` joins the gate from batch 5 on, once the package exists). Scenario runs (`./test/scenarios/`) are slow and run AFTER the batch's commits; a failure there becomes a follow-up fix commit (harness defect) or a `test/BUGS.md` entry (project bug, per spec).
- Go style rules apply (docstrings on everything exported and unexported, `fmt.Errorf("...:\n%w", err)`, functions 15-25 lines, files 200-300 lines, minimal public API).
- Events are emitted ONLY through `internal/events` constructors, at Info level, with the reserved attribute key `event`. Human logs never set that key.
- Found project bugs are recorded in `test/BUGS.md` and NOT fixed. The only protocol fixes are Tasks 3-6.
- `pods/` and `wasm-gas/` are untouched by this plan.

## Interfaces relied on across tasks (verified in code)

- `internal/logger`: `Init()`, `SetOutput(w)`, `Info/Debug/Warn/Error(msg, args...)`, custom `Handler` with broken `WithAttrs` (logger.go:76).
- `consensus.DAG`: `ExportConsistentCut(historyRounds uint64) ConsistentCut` (regime_sync.go:91), `LastCommittedRound()`, `TotalSupply()`, `ValidatorsInfo() []*ValidatorInfo`, `Epoch()`, `anchorProducerFor(round) (Hash, bool)` (anchor.go:26), `emitTransaction(tx, success, reason FailReason)` (commit.go:1425), fail reasons `FailNone/FailAuth/FailExpired/FailVersion/FailFee/FailOwner/FailRevert`.
- `consensus.CoinStore` interface (coins.go:15): implemented by `*state.State`; supply counter in `internal/state/supply.go`.
- `network.Node`: `Gossip(data, fanout)`, `GetPeer(pubkey)`, `OnMessage/OnRequest`, dedup check in `peer.go:handleUniStream` (line 177), message tags 0x04..0x16 in `messages.go` (`minClientTag = MsgTagSubmitTx`).
- `pkg/client.Wallet`: `Split/Transfer/CreateObject/TransferObject/SetObject/DeregisterValidator`, `buildCoinTx`, `buildSignedGasTx`; `pkg/client.Client`: `Faucet/Status/GetObject/GetTxStatus/Validators`.
- `pkg/daemon.Daemon`: `New(nodeAddrs)`, `SubmitTransaction(ctx, rawTx)`.
- Old cluster lineage: `test/integration/helpers.go` (`NewCluster`, `buildNodeArgs`, sequential/parallel startup, `WithGossipFanout` = cluster size for big clusters).

---

## Batch 1: event foundation

### Task 1: internal/events package

**Files:**
- Create: `internal/events/events.go` (emit core, encoding helpers)
- Create: `internal/events/catalog.go` (event name constants + `Names` slice)
- Create: `internal/events/node.go`, `internal/events/ingress.go`, `internal/events/consensus.go`, `internal/events/tx.go`, `internal/events/state.go`, `internal/events/fees.go`, `internal/events/stake.go`, `internal/events/supply.go`, `internal/events/epoch.go`, `internal/events/sync.go`, `internal/events/net.go` (one constructor file per domain)
- Test: `internal/events/events_test.go`

**Interfaces:**
- Consumes: `log/slog` only.
- Produces: `events.Key` (the reserved attribute name, `"event"`), `events.Names []string`, and one constructor per catalog entry, e.g. `events.TxCommitted(tx [32]byte, vertex [32]byte, round uint64, success bool, reason string)`, `events.VertexReceived(vertex [32]byte, producer [32]byte, round uint64)`, `events.RoundAdvanced(round uint64, designated [32]byte)`, `events.AnchorCommitted(round uint64, anchor, producer [32]byte, vertices int)`, `events.ObjectCreated(object, tx [32]byte, version uint64, replication uint16, owner [32]byte)`, `events.PartitionApplied(blocked []string)`, etc. Full list = the spec taxonomy table, one constructor per row, one Go parameter per stable attribute.

- [ ] **Step 1: Write the failing test**

```go
package events

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

// TestCatalogNamesUniqueAndDotted asserts every catalog name is unique and dotted.
func TestCatalogNamesUniqueAndDotted(t *testing.T) {
	seen := map[string]bool{}
	for _, n := range Names {
		if seen[n] {
			t.Fatalf("duplicate event name %q", n)
		}
		seen[n] = true
		if !strings.Contains(n, ".") {
			t.Fatalf("event name %q is not dotted", n)
		}
	}
	if len(Names) < 30 {
		t.Fatalf("catalog too small: %d", len(Names))
	}
}

// TestEmitCarriesReservedKey asserts a constructor emits one record with the
// reserved key, the name as message, and its typed attributes.
func TestEmitCarriesReservedKey(t *testing.T) {
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	defer slog.SetDefault(old)

	TxCommitted([32]byte{0xAA}, [32]byte{0xBB}, 7, false, "version_conflict")

	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatalf("not JSON: %v", err)
	}
	if rec[Key] != "tx.committed" || rec["msg"] != "tx.committed" {
		t.Fatalf("bad record: %v", rec)
	}
	if rec["round"] != float64(7) || rec["success"] != false || rec["reason"] != "version_conflict" {
		t.Fatalf("bad attrs: %v", rec)
	}
	if !strings.HasPrefix(rec["tx"].(string), "aa000000") {
		t.Fatalf("tx not lowercase hex: %v", rec["tx"])
	}
	_ = context.Background()
}
```

- [ ] **Step 2: Run it, verify it fails** — `go -C .wt/test-harness test ./internal/events/` → FAIL (package missing).

- [ ] **Step 3: Implement the core**

```go
// events.go
package events

import (
	"context"
	"encoding/hex"
	"log/slog"
)

// Key is the reserved attribute name that marks a record as a typed event.
const Key = "event"

// emit writes one event record at Info level. The record message is the event
// name and the reserved Key attribute carries it for machine filtering.
func emit(name string, attrs ...slog.Attr) {
	all := make([]slog.Attr, 0, len(attrs)+1)
	all = append(all, slog.String(Key, name))
	all = append(all, attrs...)
	slog.LogAttrs(context.Background(), slog.LevelInfo, name, all...)
}

// hexAttr encodes a 32-byte identifier as a lowercase hex attribute.
func hexAttr(key string, id [32]byte) slog.Attr {
	return slog.String(key, hex.EncodeToString(id[:]))
}
```

`catalog.go` declares one exported `const` per name (`EvTxCommitted = "tx.committed"`, ...) and `var Names = []string{...}` listing all of them. Every constructor uses its const, never a literal.

Constructors follow one shape; write all of them (spec taxonomy, ~38). Representative examples the other files copy:

```go
// tx.go
// TxCommitted marks a transaction decided at commit, success or typed failure.
// reason is empty on success and one of the fixed set otherwise.
func TxCommitted(tx, vertex [32]byte, round uint64, success bool, reason string) {
	emit(EvTxCommitted, hexAttr("tx", tx), hexAttr("vertex", vertex),
		slog.Uint64("round", round), slog.Bool("success", success), slog.String("reason", reason))
}

// consensus.go
// RoundAdvanced marks the local round advancing, carrying the round's
// designated anchor producer so scenarios can target it.
func RoundAdvanced(round uint64, designated [32]byte) {
	emit(EvRoundAdvanced, slog.Uint64("round", round), hexAttr("designated", designated))
}
```

Where the spec attribute is a list (`epoch.transitioned` added/removed, `net.partition.applied` blocked), pass `[]string` of hex pubkeys and use `slog.Any`.

- [ ] **Step 4: Run tests, verify pass** — `go -C .wt/test-harness test ./internal/events/` → PASS.
- [ ] **Step 5: Commit** — title `Typed event constructors`, body `[+] internal/events: emit core, catalog, one constructor per taxonomy event`, `[+] catalog uniqueness test`.

### Task 2: log format flag, JSON handler, node attribute

**Files:**
- Modify: `internal/logger/logger.go` (fix `WithAttrs`/`WithGroup`, add `UseJSON`, `SetNode`)
- Modify: `cmd/node/config.go` (`LogFormat string`, flag `--log-format`, values `text|json`)
- Modify: `cmd/node/main.go` (apply format after `parseFlags`, set node attr after key load)
- Modify: `cmd/node/node.go:66` (`useTUI` also false when `LogFormat == "json"`)
- Test: `internal/logger/logger_test.go`

**Interfaces:**
- Produces: `logger.UseJSON(w io.Writer)` (JSON handler at `slog.LevelDebug`), `logger.SetNode(prefix string)` (installs a permanent `node` attribute on the default logger, works for both handlers), fixed text `Handler.WithAttrs` that actually renders logger-level attrs.

- [ ] **Step 1: Failing tests** — three cases: (a) text handler `.With("node","abcd1234")` renders `node=abcd1234` on every line; (b) `UseJSON` output lines unmarshal as JSON and include a Debug-level record; (c) `SetNode` after `UseJSON` stamps `"node"` on subsequent records.
- [ ] **Step 2: Run, verify fail.**
- [ ] **Step 3: Implement.** Text handler: store `attrs []slog.Attr` in `Handler`, `WithAttrs` returns a copy with appended attrs, `Handle` prints stored attrs before record attrs. `UseJSON(w)`: `slog.SetDefault(slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{Level: slog.LevelDebug})))` and remember the choice so `SetNode`/`SetOutput` rebuild consistently (keep package vars `format`, `writer`, `nodeAttr`; every setter rebuilds the default logger from those three). `main.go run()`: after `loadOrGenerateKey`, `if cfg.LogFormat == "json" { logger.UseJSON(os.Stdout) }` then `logger.SetNode(hex.EncodeToString(pubKey)[:8])`. Flag validation: any value other than `text`/`json` is an error.
- [ ] **Step 4: Run tests + `go build ./...`** → PASS.
- [ ] **Step 5: Commit** — `Structured log output`, `[+] --log-format=text|json (json at Debug level, disables TUI)`, `[!] text handler WithAttrs dropped logger-level attributes`, `[+] node identity attribute on every record`.

---

## Batch 2: prerequisite protocol fixes (the ONLY protocol fixes in the cycle)

### Task 3: fee conservation — partial coverage pooled, undistributable rewards carried over

**Files:**
- Modify: `internal/consensus/commit.go:637-640` (`deductFees` partial branch)
- Modify: `internal/consensus/epoch.go` (`distributeEpochRewards` returns leftover; `transitionEpoch` re-seeds `epochFees` with it after `clearEpochState`)
- Test: `internal/consensus/supply_conservation_test.go`

**Interfaces:**
- Consumes: `FeeSplit{Total, Burned, Epoch}`, `d.epochFees`, `creditValidatorReward`/`creditDelegators`/`creditRemainder` return values (epoch.go:116-256).
- Produces: `distributeEpochRewards(issuance uint64) (leftover uint64)`.

- [ ] **Step 1: Failing tests.** (a) A tx whose gas coin covers only part of the fee: after commit, `deducted == coin balance` and the returned split pools it (`Epoch == deducted`), so `sum(coins) + epochFees` is unchanged. (b) An epoch transition where the remainder recipient has no reward coin: `transitionEpoch` leaves the uncreditable amount in `d.epochFees` instead of zero. Build on the existing test style in `internal/consensus/supply_invariant_test.go` (stub CoinStore).
- [ ] **Step 2: Run, verify fail.**
- [ ] **Step 3: Implement.** In `deductFees`: `if !fullyCovered { return FeeSplit{Total: fee, Epoch: deducted}, false }` (the taken balance enters the pool; vertex fee-summary validation is header-derived in `validate.go` and unaffected — verify by running its tests). In epoch.go: `creditShare` already returns the credited amount but `creditRemainder` (epoch.go:242) returns nothing — change its signature to return the credited amount. Change `distributeEpochRewards` to return `pool - totalCredited` (totalCredited = sum of `creditShare` returns + the `creditRemainder` return); its two early returns (`feeParams == nil`, `pool == 0 || totalWeight == 0`) must return the WHOLE pool as leftover, not zero (carrying it forward is harmless and conserving). In `transitionEpoch` capture `leftover := d.distributeEpochRewards(issuance)` then after `d.clearEpochState()` set `d.epochFees = leftover` (persistence is safe: `epochStateKVs`/`accumulatorKVs` are serialized after `transitionEpoch` returns, commit.go:186).
- [ ] **Step 4: Run** `go -C .wt/test-harness test ./internal/consensus/` → PASS.
- [ ] **Step 5: Commit** — `Fee conservation at commit and epoch boundary`, `[!] partially covered fee is pooled instead of vanishing`, `[!] uncreditable reward shares carry over to the next epoch pool`.

### Task 4: reward coin designation — genesis founder + registration arg

**Files:**
- Modify: `internal/consensus/dag.go:619` (`SeedGenesis`: set founder reward coin)
- Modify: `internal/genesis/borsh.go` (add `EncodeRegisterValidatorArgs` variant carrying the optional reward coin, mirroring `DecodeRegisterValidatorRewardCoin`)
- Modify: `internal/genesis/transaction.go:94` (`BuildRegisterValidatorRawTx` gains a `rewardCoin [32]byte` parameter; a zero value encodes no designation, callers in `cmd/node/registration.go` pass zero)
- Test: `internal/consensus/reward_coin_test.go`, extend `internal/genesis` borsh tests

**Interfaces:**
- Produces: `genesis.BuildRegisterValidatorRawTx(privKey, systemPod, quicAddr, blsPubkey, rewardCoin)`; `SeedGenesis` leaves the founder with `RewardCoin == GenesisCoinID(founder)`.

- [ ] **Step 1: Failing tests.** (a) After `SeedGenesis`, `d.validators.Get(founder).RewardCoin == genesis.GenesisCoinID(founder)`. (b) Borsh round-trip: encode register args with a reward coin, `DecodeRegisterValidatorRewardCoin` returns it; encode without, returns `ok=false`. (c) A re-registration tx built with a reward coin flows through `handleRegisterValidator` and `setRewardCoinFromArgs` (commit.go:1029) sets it.
- [ ] **Step 2: Run, verify fail.**
- [ ] **Step 3: Implement** (SeedGenesis one line via `d.validators.SetRewardCoin`; borsh encode mirrors the existing decode layout; thread the parameter).
- [ ] **Step 4: Run consensus + genesis + cmd/node tests** → PASS.
- [ ] **Step 5: Commit** — `Reward coin designation`, `[!] genesis seeds the founder's reward coin`, `[+] register_validator args can carry a reward coin end to end`.

### Task 5: coins_total protocol counter

**Files:**
- Modify: `internal/state/supply.go` (add `coinsTotal` maintained counter, persisted under its own key next to the supply key, loaded at `New`)
- Modify: `internal/consensus/coins.go` (`CoinStore` gains `CoinsTotal() uint64`, `SetCoinsTotal(uint64)`, `AddCoins(uint64)`, `SubCoins(uint64)`; `deductCoinFee` calls `store.SubCoins(deducted)`; `creditCoin` calls `store.AddCoins(amount)`)
- Modify: `internal/consensus/commit.go:1312` (`strictDebit` calls `d.coinStore.SubCoins(amount)` on success)
- Modify: `internal/consensus/dag.go:619` (`SeedGenesis` also seeds `coinsTotal` — with the SEEDED COIN'S BALANCE, not `is.Supply`: `genesis.BuildInitialState` locks the founder's stake out of the coin, so `coinsTotal = is.Supply - founderStake`; read the stake field from `genesis.InitialState` or the coin's actual balance)
- Modify: `internal/state/state.go` deletion-refund site in `applyDeletedObjects` (the 95% refund credit adds `AddCoins(refund)`; the 5% burn already calls `SubSupply`)
- Modify: `internal/sync/snapshot.go` (snapshot carries `coinsTotal`; bump `snapshotVersion`; include in `computeChecksumWithInfo`; restore on apply)
- Test: `internal/consensus/coins_total_test.go` + snapshot round-trip extension

**Interfaces:**
- Produces: the four `CoinsTotal` methods on `*state.State` (and on every consensus test stub implementing `CoinStore` — update the stubs in existing `_test.go` files, which will otherwise fail to compile; that compile failure is the guard that no implementer is missed).

Rationale (from spec): pod-level split/merge/transfer conserve coin value internally and there is no mint, so every change to total coin value flows through exactly these protocol touchpoints: fee/deposit debit (`deductCoinFee`), credits (`creditCoin`: unbond, undelegate, rewards, remainder, refunds), stake debits (`strictDebit`: bond, delegate), genesis, issuance-into-rewards (credited via `creditCoin`), deletion refund.

- [ ] **Step 1: Failing tests.** (a) Persistence round-trip of `coinsTotal`. (b) `deductCoinFee` reduces `CoinsTotal` by the deducted amount (full and partial coverage). (c) `creditCoin` raises it. (d) `strictDebit` lowers it. (e) After a synthetic bond+unbond cycle, `CoinsTotal` is back to its start.
- [ ] **Step 2: Run, verify fail.**
- [ ] **Step 3: Implement**, mirroring `totalSupply`'s persistence pattern exactly (same file, same locking).
- [ ] **Step 4: Run** `./internal/state/ ./internal/consensus/ ./internal/sync/` tests → PASS.
- [ ] **Step 5: Commit** — `Protocol coins_total counter`, `[+] coins_total maintained at every protocol coin flow, persisted, in snapshots`, `[&] CoinStore interface extended; test stubs updated`.

### Task 6: genesis re-seed guard

**Files:**
- Modify: `cmd/node/init.go:175` (`seedGenesisState`)
- Test: `cmd/node/init_test.go`

- [ ] **Step 1: Failing test.** Calling `seedGenesisState` twice against the same storage leaves the reserve coin balance and `TotalSupply` unchanged after the second call (simulate restart: reopen state over the same Pebble dir).
- [ ] **Step 2: Run, verify fail** (second seed currently overwrites).
- [ ] **Step 3: Implement.** In `seedGenesisState`: derive `owner`; when `n.state.GetObject(genesis.GenesisCoinID(owner)) != nil`, skip ONLY the coin/supply/coinsTotal seeding. The founder's `validators.Add` + `SetSelfStake` + `recordCommittedMember` MUST still run on a restart (they are idempotent, and nothing else restores the live validator set on a bootstrap-mode restart — skipping them would leave the restarted founder with zero live self-stake, zero `TotalBonded`, and a broken supply invariant). Split the function so the guard covers exactly the ledger seeding.
- [ ] **Step 4: Run** `./cmd/node/` tests → PASS. **Batch 2 gate:** full build + vet + unit tests green.
- [ ] **Step 5: Commit** — `Idempotent genesis seeding`, `[!] bootstrap restart no longer re-seeds the reserve coin and total_supply`.

---

## Batch 3: test hooks and client operations

### Task 7: network partition filter

**Files:**
- Create: `internal/network/blocklist.go`
- Modify: `internal/network/peer.go` (`handleUniStream` drop BEFORE `dedup.Check`; `handleBidiStream` drop; `Send` silent drop; `Request` error on blocked)
- Test: `internal/network/blocklist_test.go`

**Interfaces:**
- Produces: `(n *Node) SetBlocklist(pubkeys []ed25519.PublicKey)`, `(n *Node) ClearBlocklist()`, `(n *Node) isBlocked(pk ed25519.PublicKey) bool` (hex-keyed map under its own RWMutex, empty by default).

- [ ] **Step 1: Failing tests** (two real in-process network nodes, pattern from `network_test.go`): (a) after A blocks B, a message B sends A is never delivered to A's handler; (b) A's `Send` to a blocked B delivers nothing yet returns nil; (c) A's `Request` to blocked B errors; (d) B's `Request` served by A gets no response (context deadline); (e) after `ClearBlocklist`, a RE-SENT message with the SAME bytes IS delivered — this is the dedup-ordering regression test; (f) concurrent Set/Clear race test with `-race`.
- [ ] **Step 2: Run, verify fail.**
- [ ] **Step 3: Implement.** In `handleUniStream`, insert before the dedup check:

```go
	if p.node.isBlocked(p.publicKey) {
		return
	}
```

Same guard at the top of `handleBidiStream` (return without responding), in `Peer.Send` (return nil), and in `Peer.Request` (return error `"peer blocked"`). Gossip/Broadcast need no change (they go through `Send`).
- [ ] **Step 4: Run** `go -C .wt/test-harness test -race ./internal/network/` → PASS.
- [ ] **Step 5: Commit** — `Peer blocklist in the network layer`, `[+] SetBlocklist/ClearBlocklist drop mesh traffic both ways, before dedup`.

### Task 8: state fingerprint

**Files:**
- Create: `internal/sync/fingerprint.go`
- Modify: `internal/consensus/dag.go` (add accessors `EpochFees() uint64`, `TotalBonded() uint64` — sum of `EffectiveStake` over `validators.All()` — both under the same mutex discipline as existing getters)
- Test: `internal/sync/fingerprint_test.go`

**Interfaces:**
- Consumes: a NEW `(d *DAG) ExportFingerprintCut() FingerprintCut` in `internal/consensus` that captures EVERYTHING under ONE `commitMu` hold (mirroring `ExportConsistentCut`, regime_sync.go:91): last committed round, tracker entries, full validator infos, sorted `pendingRemovals`, sorted `epochAdditions`, `currentEpoch`, `issuanceRateMicro`, `epochFees`, `TotalSupply`, `CoinsTotal`, and the `DBSnapshot`. The supply terms MUST come from this single cut — reading them through separate accessors at a later instant lets an epoch boundary land between reads and produces transient false supply failures, exactly the flakiness the spec forbids. `TotalBonded` is derived from the cut's validator infos as `SelfStake + DelegatedTotal` summed UNCONDITIONALLY (jailed included: jailed stake has not returned to coins; `EffectiveStake` is a consensus weight, not an accounting term).
- Produces:

```go
// Fingerprint is the convergence digest of globally replicated state plus the
// locally evaluable supply terms. It deliberately excludes holder-scoped data
// (standard object content, the attestation signature store), the current
// round, and the per-epoch liveness counters, so it is stable across idle
// rounds and identical on every converged node.
type Fingerprint struct {
	Round        uint64   // Round is the last committed round at the cut (informational, NOT hashed)
	Checksum     [32]byte // Checksum is BLAKE3 over the canonical global state below
	TotalSupply  uint64   // TotalSupply is the protocol supply counter
	CoinsTotal   uint64   // CoinsTotal is the protocol sum-of-coin-balances counter
	TotalBonded  uint64   // TotalBonded is the sum of effective stake over the active set
	Deposits     uint64   // Deposits is the sum of locked storage deposits from the tracker
	FeesInFlight uint64   // FeesInFlight is the epoch's accumulated, undistributed consumed fees
}

// ComputeFingerprint captures one consistent cut and digests it.
func ComputeFingerprint(dag *consensus.DAG, st *state.State) Fingerprint
```

Hashed content, in this canonical order, each section length-prefixed: (1) tracker entries sorted by object ID (id, version, replication, fees) — `Deposits` is the running sum of the fees field; (2) domain entries sorted by name; (3) validators sorted by pubkey (pubkey, selfStake, delegatedTotal, jailed); (4) sorted `pendingRemovals` and sorted `epochAdditions`; (5) `currentEpoch` and `issuanceRateMicro`; (6) singleton objects only (`replication == 0`), sorted by ID, full serialized bytes; (7) `TotalSupply`, `CoinsTotal`, `FeesInFlight` as u64 BE. NOT hashed: round, liveness counters, holder snapshots, objsig store.

- [ ] **Step 1: Failing tests.** (a) Two computes with no state change are byte-identical (do NOT try to force `updateRound` — it is unexported outside `internal/consensus`; two back-to-back computes over the same state suffice, plus one after committed-but-idle activity). (b) Mutating a singleton changes the checksum; mutating a standard (replication>0) object's content does NOT (only its tracker entry does). (c) The supply terms satisfy `CoinsTotal + TotalBonded + Deposits + FeesInFlight == TotalSupply` on a seeded genesis state (remember Task 5: `CoinsTotal` at genesis is the coin balance, `TotalBonded` is the founder's seeded stake, and they sum to `TotalSupply`).
- [ ] **Step 2: Run, verify fail.**
- [ ] **Step 3: Implement** (reuse blake3 + sorting utilities already in snapshot.go; keep the file under 300 lines by delegating section encoders).
- [ ] **Step 4: Run** `./internal/sync/` tests → PASS.
- [ ] **Step 5: Commit** — `Convergence fingerprint`, `[+] BLAKE3 digest of globally replicated state under one consistent cut`, `[+] supply terms exposed for the invariant checker`.

### Task 9: --test-hooks flag, fingerprint and partition QUIC messages

**Files:**
- Modify: `internal/network/messages.go` (tags `MsgTagStateFingerprint = 0x17`, `MsgTagStateFingerprintResp = 0x18`, `MsgTagTestControl = 0x19`, `MsgTagTestControlResp = 0x1A`; encode/decode functions in the file's existing hand-rolled binary style — length-prefixed fields, one worked pair to mirror: `EncodeGetObject`/`DecodeGetObject`)
- Modify: `cmd/node/config.go` (`TestHooks bool`, flag `--test-hooks`)
- Modify: `cmd/node/clienthandlers.go` (`handleClientMessage` cases; handlers `handleStateFingerprint`, `handleTestControl`)
- Modify: `pkg/client/client.go` (methods `Fingerprint()`, `SetPartition(blocked [][32]byte)`, `ClearPartition()`)
- Test: `internal/network/messages_test.go` (codec round-trips), `cmd/node` handler tests

**Interfaces:**
- Produces: `network.FingerprintResponse{Round, Checksum, TotalSupply, CoinsTotal, TotalBonded, Deposits, FeesInFlight uint64/[32]byte, Err string}`; `network.TestControlRequest{Op byte, Pubkeys [][32]byte}` with `Op` 1 = set partition (replace blocklist), 2 = clear; `network.TestControlResponse{Err string}`.
- Behavior: without `--test-hooks`, both handlers return the typed refusal `Err: "test hooks disabled"` and change nothing. With it, `handleTestControl` op 1 calls `n.network.SetBlocklist(...)` and emits `events.PartitionApplied(hexKeys)`; op 2 calls `ClearBlocklist()` and emits `events.PartitionCleared()`. `handleStateFingerprint` returns `sync.ComputeFingerprint(n.dag, n.state)`.

- [ ] **Step 1: Failing tests** — codec round-trips for both message pairs; handler refusal without the flag; handler success path with a stub.
- [ ] **Step 2: Run, verify fail.**
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** network + cmd/node tests → PASS.
- [ ] **Step 5: Commit** — `Test hooks QUIC surface`, `[+] --test-hooks flag`, `[+] state-fingerprint and partition-control messages with typed refusal when disabled`, `[+] client methods Fingerprint/SetPartition/ClearPartition`.

### Task 10: client Bond and RegisterValidator operations

**Files:**
- Modify: `pkg/client/transactions.go` (add `Bond`, `RegisterValidator`)
- Test: `pkg/client/transactions_stake_test.go`

**Interfaces:**
- Produces:

```go
// Bond stakes amount from a singleton coin the wallet owns behind this wallet's
// validator identity. The coin is both the staked coin (first mutable_ref) and
// the gas coin. Returns the tx hash.
func (w *Wallet) Bond(c *Client, coinID [32]byte, coinVersion uint64, amount uint64) ([32]byte, error)

// RegisterValidator (re-)registers this wallet's key as a validator, optionally
// designating a reward coin (zero value = no designation). Raw, gas-free tx.
func (w *Wallet) RegisterValidator(c *Client, quicAddr string, blsPubkey []byte, rewardCoin [32]byte) error
```

`Bond` builds args = Borsh u64 LE amount, function `"bond"`, via the existing `buildCoinTx` path (mutable ref = coin at its version, gas coin = same coin). `RegisterValidator` wraps `genesis.BuildRegisterValidatorRawTx` (Task 4 signature) and submits raw.

- [ ] **Step 1: Failing tests** — built tx decodes with function name `bond`, Borsh amount, coin as first mutable_ref and gas coin; register tx carries the reward coin arg (decode with `genesis.DecodeRegisterValidatorRewardCoin`).
- [ ] **Step 2: Run, verify fail.** — **Step 3: Implement.** — **Step 4: PASS + batch 3 gate** (full build + vet + unit tests).
- [ ] **Step 5: Commit** — `Client staking operations`, `[+] Wallet.Bond and Wallet.RegisterValidator (reward coin aware)`.

---

## Batch 4: event emission sweep

Each task is a sweep with an exact site table. The existing `logger.Debug/Warn` lines at those sites stay or go per the table (an event REPLACES the log line when they say the same thing). Every task's tests assert emission by capturing slog output in a buffer (pattern from Task 1's test).

### Task 11: consensus commit-path events

**Files:**
- Modify: `internal/consensus/commit.go`, `internal/consensus/epoch.go`, `internal/consensus/thermostat.go`
- Test: `internal/consensus/events_emit_test.go`

Thread the vertex hash into `executeTx`: `applyBatch` already has `h` (the vertex hash); pass it through `processTransactions(v, h, commitRound, verdicts)` into `executeTx(atx, commitRound, producer, verdicts, vertexHash)`.

| Site (function, file:line area) | Emit |
|---|---|
| `executeTx` proof failure (commit.go:451) | `events.TxCommitted(hash, vertex, round, false, "proof_failed")` |
| `executeTx` authenticity failure (:466) | `... false, "authenticity_failed"` |
| `executeTx` sponsorship expiry (:476) | `... false, "expired_sponsorship"` |
| `executeTx` duplicate skip (:489) — currently emits nothing | `... false, "duplicate"` (event only; the `d.committed` channel behavior is unchanged) |
| `executeTx` version conflict (:498) | `... false, "version_conflict"` |
| `executeTx` fee rejection (:508) | `... false, "fee_rejected"` |
| `executeTx` ownership rejection (:515) | `... false, "ownership"` |
| `executeTx` executor error / success (:551) | `... success, "" or "execution_error"` |
| `executeTx` non-holder skip (:532) | `... true, ""` (committed successfully without local execution) |
| `executeTx` executor call (:542) | `events.TxExecuted(hash, success, errorCode)` right after `d.executor.Execute` returns (in addition to the `tx.committed` below) |
| `deductFees` success and partial branch (commit.go:631-643) | `events.FeesDeducted(tx, gasCoin, deducted, fullyCovered)` |
| `handleBond` success (:1115) | `events.StakeBonded(sender, coinID, amount)` |
| `handleUnbond` success (:1149) | `events.StakeUnbonded(sender, coinID, amount)` |
| `handleDelegate` success (:1176) | `events.StakeDelegated(validator, posID, amount)` |
| `handleUndelegate` success (:1203) | `events.StakeUndelegated(validator, posID, amount)` |
| `handleRegisterValidator` after `Add` (:996) | `events.ValidatorRegistered(pubkey, quicAddr)` |
| `handleDeregisterValidator` (:1080) | `events.ValidatorDeregistered(pubkey)` |
| `transitionEpoch` end (epoch.go:88) | `events.EpochTransitioned(newEpoch, addedHex, removedHex)` (capture added/removed inside the transition) |
| `distributeEpochRewards` after loop (epoch.go:136) | `events.RewardsDistributed(epoch, pool, len(vals))` |
| `runThermostat` mint (thermostat.go:78) | `events.SupplyIssued(epoch, issuance, rateMicro)` |

- [ ] Steps: failing emission tests (drive `executeTx` directly per existing unit-test style; one test per reason) → implement → `go test ./internal/consensus/` PASS → commit `Consensus commit-path events` with `[+]` lines per group.

### Task 12: DAG lifecycle, node and ingress events

**Files:**
- Modify: `internal/consensus/dag.go` (`updateRound`, `tryProduceVertex`, `AddVertex`), `internal/consensus/validate.go` (rejection reasons), `internal/consensus/commit.go` (`commitAnchorBatch`, skip branch of `commitNextRound`/`advanceCommitCursor`)
- Modify: `cmd/node/node.go` (`runBootstrap`/`runValidator`/`runListener` ready points, `waitForShutdown`), `cmd/node/main.go` (`printStartupInfo` → also `events.NodeStarted`), `cmd/node/clienthandlers.go` (`handleSubmitTx`, `handleFaucet`)
- Test: `internal/consensus/events_dag_test.go`, `cmd/node` handler tests

| Site | Emit |
|---|---|
| `tryProduceVertex` after signing/storing its vertex | `events.VertexProduced(hash, round, txCount)` |
| `AddVertex` accepted | `events.VertexReceived(hash, producer, round)` |
| `validateVertex` TERMINAL rejection returns only | `events.VertexRejected(hash, reason)` — reasons: `bad_signature`, `wrong_epoch`, `parent_round`, `parent_quorum`, `fee_summary`. A vertex BUFFERED on `unknown_producer`/`missing_parents` emits NOTHING (it is often accepted later — a rejected-then-received pair would corrupt journal semantics), and the `store.has` duplicate early-return in `AddVertex` emits nothing either |
| `checkCommits` (commit.go:43), under `commitMu` | `events.RoundAdvanced(r, designated)` — RACE WARNING: do NOT emit from `updateRound`; it is a lock-free CAS on network goroutines, while `anchorProducerFor` reads `currentEpoch`/holder snapshots/eligibility maps that are guarded by `commitMu` (concurrent map read/write crash). Instead, in `checkCommits` (already under `commitMu`) keep a `lastAnnouncedRound` field; each tick, for every round `r` in `(lastAnnounced, d.Round()]` emit the event with `designated, _ := d.anchorProducerFor(r)` and advance the marker. Latency ≤ one 50ms tick, and the production round always leads the commit cursor, so the harness still learns the designation before round `r` is decided |
| `commitAnchorBatch` (commit.go:223) | `events.AnchorCommitted(round, anchor, producerOf(anchor), len(batch))` |
| skip decision (`status.kind == anchorSkip` path reaching `advanceCommitCursor`) | `events.RoundSkipped(round)` |
| `run()` after key load (main.go) | `events.NodeStarted(pubkey, quicAddr)` |
| bootstrap: end of `runBootstrap` setup before `serve`; validator: end of sync in `runValidator` | `events.NodeReady(round)` |
| `waitForShutdown` on signal, `Close()` | `events.NodeStopping(reason)` |
| `handleSubmitTx` accepted (:87) | `events.IngressTxReceived(hash, kind)` kind `raw` or `attested` (from the `ingestSubmission` branch taken) |
| `handleSubmitTx`/`ingest` rejection | `events.IngressTxRejected(reason, detail)` |
| `handleFaucet` accepted (:389) | `events.IngressTxReceived(hash, "faucet")` |

- [ ] Steps: failing tests → implement → PASS → commit `DAG and ingress events`.

### Task 13: state, sync and network events

**Files:**
- Modify: `internal/state/state.go` (`applyCreatedObjects`, `applyUpdatedObjects`, `applyDeletedObjects`, `applyRegisteredDomains`), `internal/state/domain.go` (update/delete sites)
- Modify: `internal/sync/manager.go` (creation site, `createSnapshot` line 133), `cmd/node/sync.go` (`performSync` apply + completion)
- Modify: `internal/network/node.go` (`setupPeer` → `events.PeerConnected(pubkeyHex)`, `handlePeerDisconnect` → `events.PeerDisconnected(pubkeyHex)`)
- Test: `internal/state/events_test.go` + extensions in sync/network tests

| Site | Emit |
|---|---|
| `applyCreatedObjects` per stored-or-tracked object | `events.ObjectCreated(id, tx, version, replication, owner)` + `events.DepositLocked(id, fees)` when the stamped deposit is > 0 |
| `applyUpdatedObjects` per object | `events.ObjectUpdated(id, tx, newVersion)` |
| `applyDeletedObjects` per object | `events.ObjectDeleted(id, tx, refund)` |
| deletion-deposit settlement (`settleDeletionDeposit`, state.go:641) | `events.DepositRefunded(id, gasCoin, refund)` + `events.SupplyBurned(burnAmount, "deletion")` |
| `applyRegisteredDomains` per domain | `events.DomainRegistered(name, objectID, tx)` |
| domain overwrite / delete | `events.DomainUpdated(name, objectID, tx)` / `events.DomainDeleted(name, tx)` |
| snapshot creation (`SnapshotManager.createSnapshot`, internal/sync/manager.go:133) | `events.SnapshotCreated(round, checksum)` |
| snapshot apply (`performSync`) | `events.SnapshotApplied(round, checksum, objectCount)` |
| end of `performSync` | `events.SyncCompleted(round)` |

- [ ] Steps: failing tests → implement → PASS → **batch 4 gate** (full build + vet + all unit tests; grep-audit `rg 'logger\.(Info|Debug|Warn)' internal/ cmd/` against the spec rule "every persisted-state mutation has an event" and add any missed site) → commit `State, sync and network events`.

---

## Batch 5: harness core (`test/harness`)

### Task 14: event model and journal

**Files:**
- Create: `test/harness/event.go`, `test/harness/journal.go`
- Test: `test/harness/journal_test.go`

**Interfaces (produced, relied on by Tasks 15-24):**

```go
// Event is one parsed typed event from a node's JSON log stream.
type Event struct {
	Name  string         // Name is the dotted event name
	Time  time.Time      // Time is the record timestamp
	Node  string         // Node is the emitting node's identity attribute
	Seg   int            // Seg is the process-run segment (increments on restart)
	Attrs map[string]any // Attrs holds the remaining attributes (JSON-decoded)
}

// Pred filters events during matching.
type Pred func(Event) bool

// Attr matches an attribute by equality (numbers compared as float64).
func Attr(key string, want any) Pred
// AttrGE matches a numeric attribute >= min.
func AttrGE(key string, min uint64) Pred

// Journal is a thread-safe, appendable, waitable event log for one node.
type Journal struct{ ... }
func NewJournal(node string) *Journal
// Append parses one COMPLETE stdout line. A line without the reserved "event"
// key (a human log) is ignored. A line that is not valid JSON returns an error
// (schema-drift detection); the caller decides whether it is exempt.
func (j *Journal) Append(line []byte) error
// NewSegment records a process restart boundary.
func (j *Journal) NewSegment()
// Events returns matching events across all segments, in order.
func (j *Journal) Events(name string, preds ...Pred) []Event
// Wait blocks until an event matches (including already-recorded ones) or ctx ends.
func (j *Journal) Wait(ctx context.Context, name string, preds ...Pred) (Event, error)
```

Implementation notes: slice + `sync.Mutex` + a broadcast channel replaced on every append (`chan struct{}` closed on append) so `Wait` loops without polling. Parse with `encoding/json` into `map[string]any`; extract `event`, `time`, `node`; everything else goes to `Attrs`.

- [ ] **Step 1: Failing tests** — append human line (ignored), event line (stored), garbage line (error), `Wait` released by a later append, `Wait` satisfied by an earlier event, `Attr`/`AttrGE` matching including float64 coercion, segment increments, `-race` on concurrent append/wait.
- [ ] **Step 2-4: red, implement, green** (`go -C .wt/test-harness test -race ./test/harness/`).
- [ ] **Step 5: Commit** — `Harness event journal`.

### Task 15: node process management

**Files:**
- Create: `test/harness/node.go`, `test/harness/ports.go`, `test/harness/build.go`
- Test: `test/harness/node_test.go`

**Interfaces:**

```go
// Node is one managed node process.
type Node struct {
	Index    int    // Index is the node's position in the cluster
	Dir      string // Dir is the node's data directory
	KeyPath  string // KeyPath is the Ed25519 key file
	QUICAddr string // QUICAddr is the node's listen address
	...
}
func (n *Node) Start(args NodeArgs) error   // spawn the process, wire the stdout pump
func (n *Node) Stop() error                 // SIGTERM, wait
func (n *Node) Kill()                       // SIGKILL, wait
// Restart starts the same node again: same key, data dir and port,
// journal.NewSegment(). Flags depend on the role: the bootstrap node restarts
// with --bootstrap and NO --bootstrap-addr (a bootstrap restarted with a
// bootstrap-addr would skip initConsensus and nil-deref; Task 6 makes the
// re-seed safe); every other node restarts with --bootstrap-addr=syncFrom
// (an alive node, not necessarily its original bootstrap) and no --bootstrap.
func (n *Node) Restart(syncFrom string) error
func (n *Node) Alive() bool
func (n *Node) Journal() *Journal
func (n *Node) WaitEvent(ctx context.Context, name string, preds ...Pred) (Event, error)
func (n *Node) ParseError() error           // first schema-drift error, if any
```

Stdout pump: `bufio.Reader.ReadString('\n')`; ONLY newline-terminated lines are fed to `Journal.Append` — a final unterminated chunk after process exit is dropped (the spec's SIGKILL truncation exemption falls out structurally). Every raw line (terminated or not) is also appended to `<Dir>/stdout.log`. Stderr is captured to `<Dir>/stderr.log`; a failed start surfaces its content in the returned error, and `Dump` prints both paths. A `Journal.Append` error is stored once in the node (`ParseError`), not fatal at pump time. `ports.go`: package-level `atomic.Int32` starting at 21000; each cluster reserves a stride of 4 ports per node (probe-bind to skip collisions). `build.go`: `sync.Once` building `cmd/node` to `os.TempDir()/bluepods-harness/node-bin` with `go build`.

- [ ] **Step 1: Failing tests** — start a real node binary (bootstrap, `--log-format=json --test-hooks`), `WaitEvent` for `node.started` and `node.ready`; `Kill` then `Alive()==false` and no `ParseError` even with a hard kill mid-traffic; `Restart` opens segment 1 and reaches `node.ready` again.
- [ ] **Steps 2-4: red, implement, green.**
- [ ] **Step 5: Commit** — `Harness node process management`.

### Task 16: cluster orchestration

**Files:**
- Create: `test/harness/cluster.go`, `test/harness/options.go`, `test/harness/setup.go` (stake setup), `test/harness/partition.go`
- Test: `test/harness/cluster_test.go`

**Interfaces:**

```go
func NewCluster(t *testing.T, size int, opts ...Option) *Cluster
// Options: WithEpochLength(uint64), WithMinValidators(int), WithGossipFanout(int),
// WithSyncBuffer(int), WithInitialMint(uint64), WithTransitionGrace(int),
// WithTransitionBuffer(int), WithMaxChurn(int), WithoutStakeSetup(), WithoutInvariants(),
// WithStake(amount uint64)  // default equal stake per validator, e.g. 1_000_000
func (c *Cluster) Node(i int) *Node
func (c *Cluster) Nodes() []*Node
func (c *Cluster) Alive() []*Node
func (c *Cluster) Client(i int) *client.Client
func (c *Cluster) Daemon(i int) *daemon.Daemon
func (c *Cluster) Kill(i int)
func (c *Cluster) Restart(i int)              // syncFrom = first alive node's addr
func (c *Cluster) Spawn() *Node               // new registered validator, waits for sync.completed
func (c *Cluster) Partition(a, b []int)       // crossed blocklists via SetPartition on every member
func (c *Cluster) Heal()                      // ClearPartition on every alive node
func (c *Cluster) WaitAll(ctx context.Context, name string, preds ...Pred) error
func (c *Cluster) SystemPod() [32]byte
```

Startup transplants the proven sequencing from `test/integration/helpers.go` (`startValidatorsSequential` under 10 nodes, batched parallel above, `WithGossipFanout(size)` default for big clusters), but readiness is event-driven: bootstrap `node.ready`, each validator `sync.completed` or `node.ready`, then `WaitAll` on `consensus.round.advanced` with `AttrGE("round", 1)`.

Default stake setup (`setup.go`), skipped by `WithoutStakeSetup`: the founder already holds `InitialMint/10` genesis self-stake (cmd/node/init.go), so EQUAL weights mean bonding exactly that amount on every NON-founder node and bonding nothing extra on the founder. For each non-founder node `i`: load its key file into a `client.Wallet`; `Faucet` a stake coin of `InitialMint/10` plus a fee margin; wait `tx.committed` for the faucet tx; `RefreshCoin` to learn the coin's committed version; `RegisterValidator` (re-register) with `rewardCoin =` that coin; `Bond(coin, version, InitialMint/10)`; wait for the bond's `tx.committed` with `success=true`. The founder still gets a reward coin via Task 4's genesis designation. Reserve math: the genesis reserve holds `0.9 x InitialMint`, so equal stakes fit for clusters up to 9 non-founders; `NewCluster` therefore passes `--initial-mint` scaled so that `(size-1) x InitialMint/10 + fee margin <= 0.9 x InitialMint` holds by construction for sizes <= 10, and for larger clusters (11+) the default bond drops to `reserve / (2 x (size-1))` with UNEQUAL founder weight — acceptable because no large-cluster scenario asserts quorum arithmetic (Task 23 uses 5-node clusters). After setup: log the resulting stake distribution.

`Partition(a, b)`: for each node in `a`, `SetPartition(pubkeys of b)`; for each in `b`, `SetPartition(pubkeys of a)`. Wait for `net.partition.applied` on every member.

- [ ] **Step 1: Failing test** — a 3-node cluster comes up, stake setup completes (assert `stake.bonded` on all three via journals), a coin `Split` through `Client(0)` commits on all nodes (`WaitAll` on `tx.committed` with the hash), `Partition([0],[1,2])` then `Heal` round-trips (events observed).
- [ ] **Steps 2-4: red, implement, green.**
- [ ] **Step 5: Commit** — `Harness cluster orchestration`.

### Task 17: invariant checker and failure diagnostics

**Files:**
- Create: `test/harness/invariants.go`, `test/harness/diagnostics.go`
- Test: `test/harness/invariants_test.go`

**Interfaces:**

```go
// CheckInvariants runs the three cardinal checks over all alive nodes. It is
// registered by NewCluster in t.Cleanup (runs BEFORE node teardown) unless
// WithoutInvariants was given.
func (c *Cluster) CheckInvariants(t *testing.T)
```

- **Convergence:** poll `Client(i).Fingerprint()` on every alive node every 200ms until ONE sweep observes all checksums identical AND all committed rounds equal (spec: same committed round and same fingerprint — checksum-only would pass a node wedged at an idle tail with identical state, which is exactly the shape of a broken heal), timeout 30s. Rounds advance between polls; the retry loop absorbs the skew. On timeout: fail with the per-node (round, checksum) table.
- **Zero rollback:** per node, per segment: `consensus.anchor.committed` rounds strictly increasing. Across all nodes and segments: build `map[round]anchorHex`; any round seen with two different anchors fails.
- **Supply:** per node, from the final fingerprint: `CoinsTotal + TotalBonded + Deposits + FeesInFlight == TotalSupply`.
- **Schema drift:** any node with `ParseError() != nil` fails the scenario here at the latest.

`diagnostics.go`: `func (c *Cluster) Dump(t *testing.T)` prints, per node: last 15 events, `Status()` fields (round, epoch, lastCommitted, validators), the stdout.log path. Called by `CheckInvariants` and by `WaitEvent`-timeout helpers on failure.

- [ ] **Step 1: Failing tests** — unit-level: rollback detector flags a fabricated journal with a contradicted anchor; supply checker flags a fabricated fingerprint off by one; integration: a 3-node cluster passes all checks after traffic.
- [ ] **Steps 2-4: red, implement, green. Batch 5 gate:** full build + vet + `./test/harness/` green.
- [ ] **Step 5: Commit** — `Invariant checker and diagnostics`.

---

## Batch 6: functional scenarios (`test/scenarios`)

Every scenario: `//go:build scenario` is NOT used — they are plain `_test.go` in `test/scenarios`, long-running, guarded by `testing.Short()` skip. No `time.Sleep` anywhere: every wait is `WaitEvent`/`WaitAll`/fingerprint polling with bounded contexts. Each task's step 4 runs its own scenarios: `go -C .wt/test-harness test ./test/scenarios/ -run 'TestScenarioBootstrap|TestScenarioConsensus' -v -count=1 -timeout 12m` (adjust the -run per task). A red scenario is triaged per the spec: harness defect → fix now; project bug → entry in `test/BUGS.md` (created in the first task that needs it) and the scenario stays red.

### Task 18: bootstrap and consensus basics

**Files:** Create `test/scenarios/scenario_bootstrap_test.go`, `test/scenarios/scenario_consensus_test.go`, `test/scenarios/helpers_test.go` (tiny shared helpers: funded wallet, committed-tx assertion), `test/BUGS.md` (header + entry format).

Exemplar shape (bootstrap; the others follow it):

```go
// TestScenarioBootstrap: single node, basic operations, ingress rejects.
func TestScenarioBootstrap(t *testing.T) {
	if testing.Short() { t.Skip("scenario") }
	c := harness.NewCluster(t, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	w := client.NewWallet()
	coin, hash, err := c.Client(0).Faucet(w.Pubkey(), 1_000_000) // NOTE: returns (coinID, txHash, err)
	requireNoErr(t, err)
	_, err = c.Node(0).WaitEvent(ctx, "tx.committed",
		harness.Attr("tx", hex.EncodeToString(hash[:])), harness.Attr("success", true))
	requireNoErr(t, err)
	requireNoErr(t, w.RefreshCoin(c.Client(0), coin)) // Split errors on untracked coins

	// split, transfer, create_object, get_object round-trips ...
	// ingress rejects: malformed hash/signature/oversize via raw submissions,
	// each asserted through ingress.tx.rejected with its typed reason.
}
```

Consensus basics (5 nodes): split/transfer/object create-transfer through client and daemon, `WaitAll` commit on every node, security rejects (replay → `duplicate`, tampered hash → `authenticity_failed`, wrong owner → `ownership`) asserted via `tx.committed` events with those reasons.

- [ ] Steps: write both scenarios → run them → triage failures (fix harness / record bugs) → commit `Scenarios: bootstrap and consensus basics`.

### Task 19: fees and aggregation scenarios

**Files:** Create `test/scenarios/scenario_fees_test.go`, `test/scenarios/scenario_aggregation_test.go`.

Fees (5 nodes): fee deduction visible via `fees.deducted` events and balance reads; underfunded gas coin → `tx.committed` reason `fee_rejected` AND the partial amount pooled (assert supply invariant still passes at teardown — this exercises Task 3); gas-coin ownership violation → `fee_rejected`. Aggregation (5 nodes): replicated-object transfer through the daemon (ATX path), version race with two contending daemons (one wins, one retries; object never corrupted), epoch-boundary grace (short-epoch cluster), cold-holder immediacy — carried over from the old sims but asserted on events (`tx.committed`, `state.object.updated`) instead of log-grepping.

- [ ] Steps: write → run → triage → commit `Scenarios: fees and aggregation`.

### Task 20: epochs and object sharding scenarios

**Files:** Create `test/scenarios/scenario_epochs_test.go`, `test/scenarios/scenario_objects_test.go`.

Epochs (10 nodes, `WithEpochLength(50)`, `WithMaxChurn`...): transitions observed via `epoch.transitioned` on all nodes with identical `epoch` sequence; validator add/deregister with churn deferral; `epoch.rewards.distributed` observed and supply invariant green across boundaries (exercises Tasks 3-5). Objects (12 nodes): rendezvous holder counts via `QueryObjectLocal`-equivalent (`GetObjectLocal`), singleton on all nodes, replication > validator count, routing from non-holder, local-only flag.

- [ ] Steps: write → run → triage → commit `Scenarios: epochs and object sharding`.

### Task 21: joining and stress scenarios

**Files:** Create `test/scenarios/scenario_joining_test.go`, `test/scenarios/scenario_stress_test.go`.

Joining: 5-node cluster, `Spawn()` two more (one at a time, then a batch), all converge (`sync.completed`, then identical fingerprints via teardown invariants). Stress (12 nodes, `WithEpochLength(50)`): N concurrent wallets hammering split/transfer while epochs roll; double-spend storms (same coin, expect exactly one `success=true` per version); commits continue across boundaries. Throughput is observed, not asserted (no perf criteria per spec).

- [ ] Steps: write → run → triage → **batch 6 gate**: `go -C .wt/test-harness test ./test/scenarios/ -run 'TestScenario(Bootstrap|Consensus|Fees|Aggregation|Epochs|Objects|Joining|Stress)' -count=1 -timeout 45m` verdicts recorded → commit `Scenarios: joining and stress`.

---

## Batch 7: adversarial scenarios

### Task 22: crash, anchor crash, join under load

**Files:** Create `test/scenarios/scenario_crash_test.go`, `test/scenarios/scenario_anchor_crash_test.go`, `test/scenarios/scenario_join_load_test.go`.

- Crash and recover (5 nodes): background traffic generator (goroutine driving faucet+splits through node 0); `Kill(3)`; traffic continues committing on the 4 alive; `Restart(3)`; node 3 reaches `sync.completed` and the teardown invariants prove it converged. Segment handling and the truncated-line exemption are exercised here for real.
- Anchor crash (5 nodes): read a fresh `consensus.round.advanced` event, take its `designated` producer, kill that node before its round decides; the network skips or indirectly decides the round (`consensus.round.skipped` or a later `anchor.committed` covering it) and keeps committing; restart, converge.
- Join under load (5+1): traffic running, `Spawn()`; the newcomer syncs (snapshot events), registers, converges.

- [ ] Steps: write → run → triage (crash-window bugs around the commit cursor and re-sync are LIKELY findings here; record them in `test/BUGS.md` with journal evidence, do not fix) → commit `Scenarios: crash and join adversaries`.

### Task 23: partitions and epoch crash

**Files:** Create `test/scenarios/scenario_partition_test.go`, `test/scenarios/scenario_epoch_crash_test.go`.

Quorum arithmetic: the default setup gives 5 nodes exactly equal weight (`InitialMint/10` each — the founder's genesis stake matched by four bonds), so the exact test `3*capped_sum >= 2*total` gives 4|1 → 12 >= 10 (majority commits) and 3|2 → 9 < 10 on one side, 6 < 10 on the other (everyone halts), regardless of which side holds the founder. State these weights in the scenario comments:
- Minority partition (5 nodes, 4|1): the 4-side keeps emitting `anchor.committed` (12 >= 10); the isolated node's committed round plateaus (assert: no new `anchor.committed` for 10s of wall context — poll its journal length, not sleep-assert). `WithoutInvariants` NOT set: `Heal()` before teardown, then invariants prove no contradiction.
- Symmetric split (5 nodes, 3|2): NEITHER side commits new anchors (9 < 10 and 6 < 10); both plateau; heal; commits resume; invariants green. This is the CP-promise scenario.
- Heal-focused variant: partition 4|1 with sustained traffic on the majority, heal, isolated node catches up past the traffic burst, zero-rollback check does the proving.
- Epoch crash (10 nodes, epoch 50): `Kill(-9)` two NON-founder nodes in the commit window right after `epoch.transitioned` fires on node 0 (killing non-founders keeps the survivor stake share above two thirds for ANY bond amount, so liveness is guaranteed by arithmetic, not by tuning); restart both; all nodes converge on epoch counter and fingerprint.

- [ ] Steps: write → run → triage → **batch 7 gate**: full adversarial run, verdicts recorded in `test/BUGS.md` → commit `Scenarios: partitions and epoch crash`.

---

## Batch 8: replacement and documentation

### Task 24: delete the old suite, write the docs

**Files:**
- Delete: `docs/ATP.md`, `test/integration/` (entire directory)
- Create: `test/TESTING.md` (how to run, how to write a scenario, harness API reference, the full event catalog table copied from `internal/events/catalog.go`, the maintenance rule, `test/BUGS.md` triage protocol)
- Modify: `CLAUDE.md` (+ mirror `.claude/CLAUDE.md` if still present): point to `test/TESTING.md`, drop ATP references, add the maintenance principle (every new state mutation gets an `internal/events` constructor; every new feature extends or adds a scenario; renaming an event is a breaking change to call out)
- Modify: `README.md` if it references `test/integration` or ATP
- Verify: `rg -i 'ATP|test/integration' --type md --type go` returns nothing outside `docs/superpowers/`

- [ ] Steps: sweep references → write docs → full gate: `go build ./... && go vet ./... && go test ./... -short` then the complete scenario corpus once, final verdicts in `test/BUGS.md` → commit `Replace ATP and the integration suite`, `[-] docs/ATP.md and test/integration`, `[+] test/TESTING.md (runner guide, harness API, event catalog, maintenance rule)`, `[&] CLAUDE.md points at the new environment`.

---

## Self-review notes (already applied)

- Spec coverage: events (T1-2, T11-13), prerequisite fixes (T3-6), hooks (T7-9), client ops (T4, T10), harness (T14-17), corpus functional (T18-21) and adversarial (T22-23), deletions/docs (T24), BUGS.md protocol (T18, T22-24). Non-goals honored: no latency injection, no bug fixing beyond T3-6, no Rust changes.
- Type consistency: `Fingerprint` fields (T8) match the client method and invariant checker usage (T9, T17); `Journal`/`Pred` (T14) match `Node.WaitEvent` (T15) and `Cluster.WaitAll` (T16); `Bond` signature (T10) matches the stake setup (T16); `events.*` constructors (T1) match every sweep table entry (T11-13).
- Known risk, accepted: exact line numbers drift as batches land; implementers anchor on function names, which are stable across this plan.
