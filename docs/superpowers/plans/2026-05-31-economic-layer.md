# Economic Layer Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development to implement this plan **one batch at a time** (one implementation subagent per batch). Within a batch, the subagent executes its tasks in order; **each task ends in one commit**. After a batch's tasks are all committed, **push** before starting the next batch. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Close the BluePods economic loop end to end so the spec is 100% implemented: transaction authenticity at commit, genesis-as-state with a bonded founder, real supply accounting with locked storage deposits, no scarcity burn, staking with delegation, stake-weighted capped consensus, the adaptive issuance thermostat, stake-and-liveness reward payout, sponsored transactions, and the future-proofed signed timestamp field — then bring the docs and schemas in line.

**Architecture:** Build the primitives bottom-up so each batch ships working, testable software and later batches build on earlier ones. Money never depends on a clock (issuance is per-epoch-event); no reward is a farmable multiplier (`effective_stake x liveness`). Transaction authenticity (sender + sponsor signatures, tx hash) is verified deterministically in the commit path on every node, not only at client ingress. `total_supply` is owned by `state.State` (it already owns the coin store and the deletion-burn path); the storage component of a fee is locked in the object (never pooled) so the supply invariant holds. Stake lives on `validators.ValidatorInfo` (rides the snapshot's flat validator byte vector and is carried through the epoch holder snapshot); consensus quorum reads capped effective stake from the epoch holder snapshot selected by `commitEpochForRound`.

**Tech Stack:** Go 1.26, FlatBuffers (`types/*.fbs` → `internal/types` via `bash types/generate.sh`, requires `flatc`), BLAKE3 (`github.com/zeebo/blake3`), Pebble-backed storage, Rust/WASM system pod (`pods/pod-system`), the existing `CoinStore`/`State`/`ValidatorSet`/`DAG` APIs.

**Spec:** `docs/superpowers/specs/2026-05-31-economic-layer-design.md` (one design of record; all 11 sections + Parameters + Doc impact are covered here). Hardened through two adversarial plan-review iterations; the resolutions below are baked in.

**Branch:** `economic-layer` (already checked out).

---

## Execution model (batches)

This is one plan. Work is grouped into **batches**; each batch is a coherent, independently-pushable unit dispatched to a single implementation subagent.

- A batch has 1–10 tasks. **One task = one commit.** **Push after each batch.**
- Batches are ordered: a later batch may use symbols defined by an earlier one. Do not reorder.
- New integer math reuses the codebase's `safeMul`/`safeAdd` (`internal/consensus/fees.go`) — never raw `*`/`+` on attacker-influenced or compounding values.
- Test discipline: run unit tests for the touched package with a bounded timeout. Run integration sims (`TestSim*`) **individually** with `-timeout` (per project memory: never the whole suite unbounded).
- After a schema change (`types/*.fbs`), regenerate with `bash types/generate.sh` and rebuild before testing.

| Batch | Subsystem | Spec § | Tasks |
|---|---|---|---|
| 1 | Tx authenticity at commit; genesis as state; close the fee-less hole | 9, 11, 5 | 9 |
| 2 | `total_supply` accounting; lock storage deposits; remove the scarcity burn | 7, 5.2, 4 | 8 |
| 3 | Stake on `ValidatorInfo`; carry it in snapshots; `total_bonded`; jailing | 2, 1 | 8 |
| 4 | Delegation (positions, commission, epoch-boundary split) | 2 | 7 |
| 5 | Stake-weighted capped quorum; dual security model | 1 | 6 |
| 6 | The thermostat (per-epoch adaptive issuance) | 3 | 6 |
| 7 | Reward distribution (`effective_stake x liveness`, payout) | 6, 5.2 | 5 |
| 8 | Sponsored transactions | 9 | 7 |
| 9 | `Vertex.timestamp` field (pipeline deferred) | 8 | 4 |
| 10 | Docs and schema comments | Doc impact | 4 |

---

## File map (created / modified across the plan)

- `internal/consensus/txauth.go` (new) — verify sender/sponsor signature + tx hash at commit (Batch 1, extended in 8).
- `internal/genesis/ids.go` (new) — deterministic genesis object IDs.
- `internal/genesis/state.go` (new) — `BuildInitialState` / `InitialState` (incl. founder self-stake).
- `internal/genesis/genesis.go` — drop `BuildTransactions` (Batch 1); add `BuildSplitTx` (Batch 1).
- `internal/genesis/transaction.go` — `fee_payer`/`valid_until` in the canonical body (Batch 8).
- `internal/validation/validate.go` — keep `rebuildUnsignedTx` in lockstep with the body change (Batch 8).
- `internal/consensus/dag.go` — `SeedGenesis` method; new fields/Options (stake, commission, voting cap, thermostat).
- `internal/consensus/commit.go` — commit-time authenticity; `handleBond`/`handleUnbond`/`handleDelegate`/`handleUndelegate`; gas-coin owner==fee_payer; close fee-less hole; lock storage deposit.
- `internal/consensus/coins.go` — extend `CoinStore` with supply methods + the `DelegationEnumerator` accessor (Batch 2/4); the supply IMPL lives in `internal/state` (no separate consensus supply file).
- `internal/state/state.go` — own `total_supply` (with a retained `db` handle), decrement on deletion burn, lock storage deposits, track the live validator count so its storage-deposit formula matches consensus.
- `internal/consensus/fees.go` — `BurnBPS=0`; split consumed vs storage; thermostat math in `thermostat.go`.
- `internal/consensus/stake.go` (new) — effective stake, capped voting weight, `total_bonded` (Batch 3/5).
- `internal/validators/validators.go` — stake/jail/reward-coin fields + mutators + stake-aware `Add` (Batch 3/7).
- `internal/sync/snapshot.go` + `cmd/node/sync.go` + the `SnapshotManager` provider — encode/decode stake + supply + issuance rate; bump `snapshotVersion`; restore into `state.State`.
- `internal/consensus/delegation.go` (new) — delegation positions + split (Batch 4); enumeration via a narrow `state.State` method.
- `internal/consensus/epoch.go` — carry stake in `snapshotEpochHolders`/`snapshotOf`; reward payout; thermostat call; carry-forward of undistributed pool.
- `internal/consensus/thermostat.go` (new) — issuance control loop (Batch 6).
- `pods/pod-system/src/lib.rs` (+ a function directory each) — `bond`/`unbond`/`delegate`/`undelegate` entries; remove the user `mint` entry (Batch 1/3/4).
- `types/transaction.fbs` — `fee_payer`, `sponsor_signature`, `valid_until` (Batch 8).
- `types/vertex.fbs` — `timestamp` (Batch 9).
- `types/snapshot.fbs` — `total_supply`, `issuance_rate_micro` (Batch 2/6).
- `cmd/node/init.go`, `cmd/node/clienthandlers.go`, `cmd/node/handlers.go` — genesis seeding after `SetFeeSystem`; faucet via split.
- `docs/WHITEPAPER.md`, `docs/VISION.md` (Batch 10).

---

# Batch 1 — Tx authenticity at commit; genesis as state; close the fee-less hole

**Spec:** §9 (commit-time authenticity), §11 (genesis and fee integrity), §5 (fees).

**Context (verified against code):** Transactions reach a node two ways. Direct client submission (`cmd/node/clienthandlers.go:90 handleSubmitTx` → `validation.ValidateTx`) is authenticated. But gossiped transactions (`cmd/node/handlers.go:46 ingestGossipedTx` → `dag.SubmitTx`) are NOT validated, and `executeTx` (`commit.go:274`) never re-verifies the inner tx's ed25519 signature or recomputes its hash — it trusts the (producer-signed) vertex wrapper. A malicious/relaying node can therefore inject forged transactions that commit. Sponsored tx, bond, and delegate all mutate state from tx headers, so this gap is load-bearing and must be closed first. Separately, `genesis.BuildTransactions` injects fee-less mint+register txs via `WithGenesisTxs`; and `deductFees` (`commit.go:399-403`) treats "no gas coin" as "proceed free". `state.State` is the `CoinStore` (`SetFeeSystem(n.state, ...)`, `aggregation.go:99`), wired AFTER `consensus.New` runs its Options — so genesis seeding must happen via an explicit method, not an Option.

### Task 1.1: Verify transaction authenticity at commit

**Files:** Create `internal/consensus/txauth.go`; modify `internal/consensus/commit.go` (call it in `executeTx`, after the `proofVerdict` block and before the commit-once guard — see Implement); Test `internal/consensus/txauth_test.go`.

- [ ] **Test:** a tx whose `signature` does not verify against `sender` over the recomputed body hash is rejected (`executeTx` returns `FeeSplit{}` and emits `success=false`); a tx whose `hash` field does not equal the recomputed body hash is rejected; a correctly-signed tx passes. Build the txs with the existing `genesis` builders so the canonical body matches.
- [ ] **Run, expect FAIL.**
- [ ] **Implement** `verifyTxAuthenticity(tx *types.Transaction) error`: recompute the unsigned body by calling the SAME shared primitive client ingress uses — `genesis.BuildUnsignedTxBytesWithRefs` (the exact function `internal/validation`'s `rebuildUnsignedTx` delegates to) — NOT a hand-rolled reconstruction, so the three sites (builder, ingress, commit) cannot drift; `hash := blake3.Sum256(body)`, require `bytes.Equal(hash[:], tx.HashBytes())`, then `ed25519.Verify(sender, hash[:], tx.SignatureBytes())`. **Placement (critical):** call it in `executeTx` AFTER the `proofVerdict` block (so the index-aligned batch-proof cursor in `collectRoundProofs`/`proofVerdicts.next` is consumed in lockstep and never desyncs) and BEFORE the commit-once guard (so a forged tx cannot poison `d.tracker` with a chosen hash to censor a future legitimate tx). On error: log, `d.emitTransaction(tx, false); return FeeSplit{}`. This runs deterministically on every node for every committed tx, gossiped or not. `collectRoundProofs` is left untouched — auth runs after its verdict is consumed, so it never changes which ATXs the pre-pass batches.
- [ ] **Run, expect PASS;** then `go test ./internal/consensus/ -count=1 -timeout 120s` and fix any test tx that was unsigned/forged.
- [ ] **Commit:** `git add internal/consensus/txauth.go internal/consensus/commit.go internal/consensus/txauth_test.go && git commit -m "[!] Verify tx sender signature and hash at commit (close gossip-injection gap)"`

### Task 1.2: Deterministic genesis object IDs

**Files:** Create `internal/genesis/ids.go`; Test `internal/genesis/ids_test.go`.

- [ ] **Test:** `GenesisCoinID(owner)` is deterministic and distinct per owner.
- [ ] **Run, expect FAIL** → undefined `GenesisCoinID`.
- [ ] **Implement:**

```go
package genesis

import "github.com/zeebo/blake3"

// GenesisCoinID derives the deterministic ID of the initial coin seeded for an
// owner at genesis. There is no creating transaction, so the ID is fixed by a
// domain-separated hash of a constant tag and the owner pubkey.
func GenesisCoinID(owner [32]byte) [32]byte {
	h := blake3.New()
	_, _ = h.Write([]byte("bluepods/genesis/coin/v1"))
	_, _ = h.Write(owner[:])

	var id [32]byte
	copy(id[:], h.Sum(nil))

	return id
}
```

- [ ] **Run, expect PASS.**
- [ ] **Commit:** `git add internal/genesis/ids.go internal/genesis/ids_test.go && git commit -m "[+] Deterministic genesis coin ID derivation"`

### Task 1.3: Build the initial state as data (coin + bonded founder)

**Files:** Modify `internal/genesis/genesis.go` (`Config` gains `GenesisStake uint64`); Create `internal/genesis/state.go`; Test `internal/genesis/state_test.go`.

The founder's self-stake is genesis state (§11). Total supply is `InitialMint`; the founder's coin holds `InitialMint - GenesisStake` (the staked portion is locked), and `SelfStake = GenesisStake`. Invariant at genesis: `coin(InitialMint-stake) + total_bonded(stake) + deposits(0) == InitialMint`.

- [ ] **Test:** with `InitialMint=1000, GenesisStake=300`, `BuildInitialState` yields a coin of balance 700, `SelfStake==300`, `Supply==1000`, singleton, owner == founder.
- [ ] **Run, expect FAIL** → undefined `BuildInitialState` / `InitialState`.
- [ ] **Implement** `internal/genesis/state.go`:

```go
package genesis

import (
	"encoding/binary"

	flatbuffers "github.com/google/flatbuffers/go"

	"BluePods/internal/types"
)

// InitialState is the genesis ledger state, seeded directly (no transactions).
type InitialState struct {
	Coin      []byte   // Coin is the serialized initial Coin object (a singleton).
	CoinID    [32]byte // CoinID is its deterministic object ID.
	Pubkey    [32]byte // Pubkey is the founding validator's Ed25519 key.
	QUIC      string   // QUIC is the founding validator's QUIC address.
	BLS       []byte   // BLS is the founding validator's 48-byte BLS key.
	SelfStake uint64   // SelfStake is the founder's bonded stake, locked from the mint.
	Supply    uint64   // Supply is the initial total supply (== InitialMint).
}

// BuildInitialState constructs the genesis state for the bootstrap owner. The
// staked portion is locked out of the coin so the supply invariant holds.
func BuildInitialState(cfg Config, owner [32]byte) InitialState {
	coinID := GenesisCoinID(owner)

	stake := cfg.GenesisStake
	if stake > cfg.InitialMint {
		stake = cfg.InitialMint // clamp: never underflow the coin balance
	}
	coinBalance := cfg.InitialMint - stake
	content := make([]byte, 8)
	binary.LittleEndian.PutUint64(content, coinBalance)

	b := flatbuffers.NewBuilder(256)
	idVec := b.CreateByteVector(coinID[:])
	ownerVec := b.CreateByteVector(owner[:])
	contentVec := b.CreateByteVector(content)
	types.ObjectStart(b)
	types.ObjectAddId(b, idVec)
	types.ObjectAddVersion(b, 0)
	types.ObjectAddOwner(b, ownerVec)
	types.ObjectAddReplication(b, 0)
	types.ObjectAddContent(b, contentVec)
	types.ObjectAddFees(b, 0)
	b.Finish(types.ObjectEnd(b))

	return InitialState{
		Coin:      b.FinishedBytes(),
		CoinID:    coinID,
		Pubkey:    owner,
		QUIC:      cfg.QUICAddress,
		BLS:       cfg.BLSPubkey,
		SelfStake: stake,
		Supply:    cfg.InitialMint,
	}
}
```

Add `GenesisStake uint64` to `Config` with a doc comment; default it (in `cmd/node/init.go`) to a sane fraction of `InitialMint` (e.g. `InitialMint/10`, and at least `minStake`) so the bootstrap validator has reachable quorum weight in Batch 5. **Guard against underflow:** `BuildInitialState` must clamp `GenesisStake` to `InitialMint` (or the caller validates `GenesisStake <= InitialMint`) — `InitialMint - GenesisStake` underflowing to a near-MaxUint64 coin balance is a silent supply-minting bug. Add a test for `GenesisStake > InitialMint` asserting the clamp.

- [ ] **Run, expect PASS.**
- [ ] **Commit:** `git add internal/genesis/ && git commit -m "[+] BuildInitialState: coin + bonded founder as seeded data"`

### Task 1.4: SeedGenesis method (not an Option)

**Files:** Modify `internal/consensus/dag.go` (add `SeedGenesis`); Test `internal/consensus/genesis_state_test.go`.

`coinStore` is nil while Options run (`SetFeeSystem` comes later), so genesis is seeded by an explicit method called after the fee system is wired.

- [ ] **Test:** after `SetFeeSystem(stub, ...)` then `d.SeedGenesis(is)`: `d.coinStore.GetObject(is.CoinID)` has balance `is.Supply - is.SelfStake`; `d.validators.Get(is.Pubkey)` is present; no entries in `pendingTxs`. (Supply assertion lands in Batch 2.)
- [ ] **Run, expect FAIL** → undefined `SeedGenesis`.
- [ ] **Implement** (Batch 2 extends it to seed `total_supply`; here it seeds coin + validator + self-stake):

```go
// SeedGenesis seeds the initial ledger state directly: the genesis coin object
// into the coin store and the founding validator (with its bonded self-stake)
// into the validator set. Genesis is state, not transactions. Must be called
// AFTER SetFeeSystem (so coinStore is wired) and before the node produces.
func (d *DAG) SeedGenesis(is genesis.InitialState) {
	d.coinStore.SetObject(is.Coin)

	var bls [48]byte
	copy(bls[:], is.BLS)
	d.validators.Add(is.Pubkey, is.QUIC, bls) // founder is already in the set; Add back-fills addresses
	d.validators.SetSelfStake(is.Pubkey, is.SelfStake) // SetSelfStake lands in Batch 3; stub the call here behind a build tag or add the no-op setter first
}
```

Note: `SetSelfStake` is introduced in Batch 3.2. To keep Batch 1 shippable, add a minimal `SetSelfStake` setter to `ValidatorSet` in this task (a 3-line method) and let Batch 3 build the rest of the stake API on top; this avoids a forward dependency.

- [ ] **Run, expect PASS;** `go build ./...`.
- [ ] **Commit:** `git add internal/consensus/dag.go internal/validators/ internal/consensus/genesis_state_test.go && git commit -m "[+] SeedGenesis seeds coin + bonded founder (explicit, post-SetFeeSystem)"`

### Task 1.5: Rewire bootstrap; remove genesis transactions

**Files:** Modify `cmd/node/init.go` (call `SeedGenesis` after `SetFeeSystem`); Modify `internal/genesis/genesis.go` (delete `BuildTransactions`); Modify `internal/consensus/dag.go` (delete `WithGenesisTxs`).

- [ ] **Rewire:** in `buildConsensusOpts`, drop `genesis.BuildTransactions` + `WithGenesisTxs`. In the init sequence, after `initAggregation` wires `SetFeeSystem`, derive `owner` from `n.cfg.PrivateKey.Public()`, build `is := genesis.BuildInitialState(genesisCfg, owner)` (only on the bootstrap node), and call `n.dag.SeedGenesis(is)` before the consensus loops would commit user txs.
- [ ] **Build:** `go build ./...` → `BuildTransactions`/`WithGenesisTxs` unused.
- [ ] **Delete dead code:** remove `genesis.BuildTransactions` and `consensus.WithGenesisTxs`. Keep `BuildMintTx` only if still referenced (Task 1.8 removes the faucet's use; then it can go).
- [ ] **Run the bootstrap sim:** `go test ./test/integration/ -run TestSimBootstrap -count=1 -timeout 5m`. Expect PASS. Update any sub-case that asserted a genesis *transaction* committed.
- [ ] **Commit:** `git add -A && git commit -m "[&] Bootstrap from seeded genesis state; remove genesis transactions"`

### Task 1.6: Remove the user-callable mint

**Files:** Modify `pods/pod-system/src/lib.rs` (remove the `mint` dispatcher entry and its function dir); rebuild the system pod; Test via the system-pod tests + a consensus test that a `mint` tx no longer executes.

The user `mint` creates balance from nothing and is not recorded in `total_supply` — a money printer that survives the closed fee-less hole (Task 1.7) because it can carry a funded gas coin. Genesis seeding and protocol issuance are the only token creation.

- [ ] **Test:** a tx calling `function_name == "mint"` on the system pod is rejected/no-ops at execution (the dispatcher has no such entry), creating no coin.
- [ ] **Run, expect FAIL** (mint still works).
- [ ] **Implement:** remove the `mint` entry from the `dispatcher!`/match in `pods/pod-system/src/lib.rs` and delete its function directory. Rebuild the system-pod WASM (the repo's pod build + gas-instrumentation step). Migrate any remaining `mint` callers: the genesis path is gone (Task 1.5) and the faucet is rewired (Task 1.8), but the integration sims call `mint` directly — delete/rewrite the ATP-16.1/16.2/16.3 "mint success/zero/large" cases and the direct `SubmitRawBytes(..., BuildValidTx(systemPod, "mint", ...))` in `test/integration/sim_bootstrap_test.go` (re-fund those accounts via the genesis reserve / `SubmitFaucet` instead). This sim cleanup is part of this task's commit.
- [ ] **Run** the system-pod build + `go test ./internal/consensus/ -run TestMint -count=1` (expect the mint path gone). Confirm the surviving balance-moving system functions still conserve: `split`/`merge`/`transfer` neither create nor destroy balance (add a `merge` checked-add to avoid wraparound and a split+merge conservation assertion).
- [ ] **Commit:** `git add pods/pod-system/ && git commit -m "[-] Remove the user-callable mint (only genesis + issuance create supply)"`

### Task 1.7: Close the fee-less hole in deductFees

**Files:** Modify `internal/consensus/commit.go` (`deductFees`, ~399-403); Test `internal/consensus/commit_test.go`.

- [ ] **Test** `TestDeductFees_RejectsMissingGasCoin`: a tx whose `GasCoinBytes()` length != 32 returns `proceed == false`.
- [ ] **Run, expect FAIL** (currently `true`).
- [ ] **Implement:**

```go
// No gas coin: reject. Genesis is seeded state and protocol actions (issuance,
// reward crediting, slashing) are not transactions, so every user transaction
// must reference a funded gas coin.
gasCoinBytes := tx.GasCoinBytes()
if len(gasCoinBytes) != 32 {
	return FeeSplit{}, false
}
```

- [ ] **Run** `go test ./internal/consensus/ -count=1 -timeout 120s`; fix any test relying on the fee-less path.
- [ ] **Commit:** `git add internal/consensus/commit.go internal/consensus/commit_test.go && git commit -m "[!] Reject transactions without a funded gas coin"`

### Task 1.8: Faucet splits from the genesis reserve

**Files:** Modify `internal/genesis/transaction.go` (add `BuildSplitTx`); Modify `cmd/node/clienthandlers.go` (`buildFaucetTx`); Test `internal/genesis/split_test.go`.

The faucet must move balance from the genesis reserve coin to the requester. In the system pod `transfer` only reassigns ownership; moving an amount is a `split` (reduce the reserve coin, create a new coin for the requester). The faucet tx pays its own gas from the reserve coin.

- [ ] **Test** `TestBuildSplitTx`: `BuildSplitTx(privKey, systemPod, reserveCoinID, toOwner, amount)` builds a `split` system-pod ATX referencing `reserveCoinID` as a mutable ref and gas coin, with `GasCoinBytes()` length 32 and `FunctionName()=="split"`.
- [ ] **Run, expect FAIL.**
- [ ] **Implement** `BuildSplitTx` (encode the system pod's `split` args for `(toOwner, amount)`, reserve coin as a mutable ref and as the `gasCoin`). Rewire `buildFaucetTx` to call it with `genesis.GenesisCoinID(bootstrapOwner)`; faucet requests fail naturally once the reserve is exhausted (insufficient balance). Note (dev-only): every faucet tx mutates the single reserve coin, so concurrent faucet requests serialize on that one object's version (all but one lose the version race and retry) — acceptable for a testnet faucet; production has no reserve.
- [ ] **Verify:** `go test ./internal/genesis/ -count=1` and `go test ./test/integration/ -run TestSimConsensus -count=1 -timeout 5m`.
- [ ] **Commit:** `git add -A && git commit -m "[&] Faucet splits from the genesis reserve (no fee-less mint)"`

### Task 1.9: Verify Batch 1; push

- [ ] **Unit gate:** `go test ./internal/... ./client/... -count=1 -timeout 180s`.
- [ ] **Sims:** `go test ./test/integration/ -run TestSimBootstrap -count=1 -timeout 5m`; `go test ./test/integration/ -run TestSimConsensus -count=1 -timeout 5m`.
- [ ] **Push:** `git push -u origin economic-layer`

---

# Batch 2 — `total_supply`; lock storage deposits; remove the scarcity burn

**Spec:** §7 (supply tracking + invariant), §5.2 (storage deposit locked, never pooled), §4 (no scarcity burn). `state.State` owns the coin store and the deletion path (`applyDeletedObjects`, `state.go:533-586`); the storage deposit it stamps (`computeStorageDeposit`, `state.go:589`) must be funded by locking the storage portion of the fee out of the pool.

### Task 2.1: total_supply counter in state.State

**Files:** Modify `internal/state/state.go` (add a `db *storage.Storage` field if absent — `State.New` receives it but does not retain it; retain it); Test `internal/state/supply_test.go`.

- [ ] **Test:** fresh `State` has `TotalSupply()==0`; `AddSupply(100)`→100; `SubSupply(30)`→70; `SubSupply(1000)` floors at 0; `SetTotalSupply(500)`→500; persists across reopen on the same storage.
- [ ] **Run, expect FAIL.**
- [ ] **Implement:** retain `db`; add `totalSupply uint64` (under the existing mutex), key `var prefixSupply = []byte("m:supply")` (8-byte BE), load in `New`, and `TotalSupply`/`SetTotalSupply`/`AddSupply` (saturating)/`SubSupply` (floor-0), each persisting via one `persistSupplyLocked()`. (`m:` is already excluded from object iteration by `isConsensusKey`, so it never leaks into the object snapshot.)
- [ ] **Run, expect PASS.**
- [ ] **Commit:** `git add internal/state/state.go internal/state/supply_test.go && git commit -m "[+] total_supply counter persisted in state.State"`

### Task 2.2: Lock the storage deposit (never pooled)

**Files:** Modify `internal/consensus/fees.go` (split consumed vs storage); Modify `internal/consensus/commit.go` (`deductFees`, `calculateTxFee`); Test `internal/consensus/fees_test.go`, `internal/consensus/commit_test.go`.

Today the full fee (compute+transit+storage+domain) is deducted and the whole amount is pooled (`SplitFee`→epoch), while `state` separately stamps `object.fees = computeStorageDeposit(...)` from nothing and the deletion refund is minted. Fix: deduct `consumed + storage` from the coin, pool ONLY `consumed`; the `storage` part stays as the object's locked `fees`. The two formulas (`fees.StorageDeposit` and `state.computeStorageDeposit`) are both `effRep*storageFee/totalValidators` — but they only agree if both use the SAME `totalValidators`. **They do not today:** consensus reads live `d.validators.Len()` while `state.totalValidators` is frozen at init by `SetStorageFees`. The fix MUST keep the two counts equal (below), or the debited storage will not equal the stamped deposit once the validator set grows, silently creating/destroying supply. With equal counts, the deletion refund (Batch 2.4) returns exactly the locked money, and `total_supply` is unchanged at create.

- [ ] **Test:** for a create tx, `deductFees` debits `consumed+storage` from the coin but `epochFees` grows only by `consumed`; the created object's `fees` equals `storage`; after the commit, `sum(coins)+sum(object.fees)` is unchanged vs before (no supply created/destroyed at create). **Critical count-match case:** register additional validators so `d.validators.Len()` exceeds the init-time count, create an object, and assert `debited storage == stamped object.fees` (this fails if state's count is stale). Also a producer→validator round-trip asserting `validateFeeSummary` accepts a vertex whose `buildFeeSummary` used consumed-only.
- [ ] **Run, expect FAIL.**
- [ ] **Implement:**
  1. **Keep the two storage counts equal.** Wire `state`'s validator count to the live set: update `s.totalValidators` whenever the validator set changes (the same committed `register_validator`/removal events consensus applies), via the existing validator callback, so `state.computeStorageDeposit` and `d.validators.Len()` always read the same deterministic value at the same commit point. (Determinism holds because every node applies the same committed validator-set changes in the same order.)
  2. Add `func (d *DAG) calculateTxFeeSplit(tx, atx) (consumed, storage uint64)` — `storage` is the sum of `StorageDeposit(rep, d.validators.Len(), storageFee)` over `createdReps`; `consumed` is the rest (`CalculateFee` minus the storage term: compute + transit + domain).
  3. In `deductFees`: deduct `consumed+storage` from the coin in one `deductCoinFee`; if covered, `SplitFee(consumed)` is what feeds the pool (the returned `FeeSplit` carries only the consumed split, so `d.epochFees += fees.Epoch` pools only consumed); the `storage` amount is neither burned nor pooled — it is the locked deposit.
  4. Update `validateFeeSummary` (runs at vertex INGRESS, `validateVertex` step 6) AND `buildFeeSummary` to compute the summary over `consumed` only, in lockstep, so producers and validators agree (a mismatch rejects honest vertices → halt). Document that storage is locked, not summarized.
- [ ] **Run** `go test ./internal/consensus/ -count=1 -timeout 120s`.
- [ ] **Commit:** `git add internal/consensus/fees.go internal/consensus/commit.go internal/consensus/*_test.go && git commit -m "[&] Lock the storage deposit in the object (never pooled)"`

### Task 2.3: Expose supply on CoinStore; seed it at genesis

**Files:** Modify `internal/consensus/coins.go` (extend `CoinStore`); Modify `internal/consensus/dag.go` (`SeedGenesis` calls `SetTotalSupply`); update consensus test stubs.

- [ ] **Test:** after `SeedGenesis(is)`, `d.coinStore.TotalSupply() == is.Supply`; `d.totalBonded()` (Batch 3) will equal `is.SelfStake` and the invariant `coin + bonded + deposits(0) == supply` holds at genesis.
- [ ] **Run, expect FAIL.**
- [ ] **Implement:** add to `CoinStore`: `TotalSupply() uint64`, `SetTotalSupply(uint64)`, `AddSupply(uint64)`, `SubSupply(uint64)`. In `SeedGenesis`, add `d.coinStore.SetTotalSupply(is.Supply)`. Add the four methods to every consensus test stub `CoinStore`.
- [ ] **Run, expect PASS;** `go build ./...`.
- [ ] **Commit:** `git add -A && git commit -m "[&] Expose total_supply via CoinStore; seed it at genesis"`

### Task 2.4: Decrement supply on the deletion burn

**Files:** Modify `internal/state/state.go` (`applyDeletedObjects`); Test `internal/state/supply_test.go`.

- [ ] **Test:** seed supply 10000; delete an object with locked `fees=1000`, `storageRefundBPS=9500`: owner refunded 950, `TotalSupply()`→9950 (the 50 burned leaves supply; the 950 refund was locked supply moving back to a coin).
- [ ] **Run, expect FAIL.**
- [ ] **Implement:** in `applyDeletedObjects`, after `refund := objFees * s.storageRefundBPS / 10000`, compute `burned := objFees - refund` and call `s.SubSupply(burned)`. (The full `objFees` was locked supply; refund returns 95% to a coin, 5% is destroyed.)
- [ ] **Run, expect PASS.**
- [ ] **Commit:** `git add internal/state/state.go internal/state/supply_test.go && git commit -m "[&] Decrement total_supply by the 5% deletion burn"`

### Task 2.5: Remove the scarcity fee burn (100/0)

**Files:** Modify `internal/consensus/fees.go` (`DefaultFeeParams` + comments); Test `internal/consensus/fees_test.go`.

- [ ] **Test:** `SplitFee(1000, DefaultFeeParams())` returns `Burned==0`, `Epoch==1000`.
- [ ] **Run, expect FAIL.**
- [ ] **Implement:** `BurnBPS: 0`, `EpochBPS: 10000`. Update field/`FeeSplit.Burned` comments (scarcity burn removed; 100% of consumed fees pool). `validateFeeSummary`/`buildFeeSummary` use `SplitFee`, so `total_burned` is consistently 0.
- [ ] **Run** `go test ./internal/consensus/ -count=1 -timeout 120s`.
- [ ] **Commit:** `git add internal/consensus/fees.go internal/consensus/fees_test.go && git commit -m "[&] Remove the scarcity fee burn: 100% of consumed fees to validators"`

### Task 2.6: Persist total_supply in the snapshot (checksum + restore)

**Files:** Modify `types/snapshot.fbs` (`total_supply:uint64`); regenerate; Modify `internal/sync/snapshot.go` (`buildSnapshot`, `computeChecksumWithInfo`, AND `verifyChecksum`, `CreateSnapshot` signature); Modify the `SnapshotManager` provider so it can supply `state.TotalSupply()`; Modify `cmd/node/sync.go` (restore via `n.state.SetTotalSupply`, since `ApplySnapshot(db)` has no `*state.State`). Test `internal/sync/snapshot_test.go`.

- [ ] **Schema:** add `total_supply:uint64;` to `Snapshot`. Bump `snapshotVersion` to 7. `bash types/generate.sh && go build ./...`.
- [ ] **Test:** round-trip a snapshot with `total_supply=12345`; assert it survives and is checksum-covered (tampering the field fails `verifyChecksum`); the restore path sets `state.SetTotalSupply`.
- [ ] **Run, expect FAIL.**
- [ ] **Implement:** thread `totalSupply` into `CreateSnapshot`→`buildSnapshot` (the provider passes `state.TotalSupply()`), write its 8 BE bytes in `computeChecksumWithInfo` (used by BOTH `buildSnapshot` and `verifyChecksum` — update both call sites), `types.SnapshotAddTotalSupply`, read it in `verifyChecksum`, and restore it in `cmd/node/sync.go:requestAndApplySnapshot` (the function that already applies validators/vertices/tracker/domains after `ApplySnapshot`) via `n.state.SetTotalSupply(snapshot.TotalSupply())` — `ApplySnapshot(db)` has no `*state.State`, so the restore call must live here, not inside `ApplySnapshot`.
- [ ] **Run** `go test ./internal/sync/ -count=1 -timeout 120s`.
- [ ] **Commit:** `git add -A && git commit -m "[&] Persist total_supply in snapshots, checksum-covered (version 7)"`

### Task 2.7: Supply invariant property test (epoch boundary)

**Files:** Test `internal/consensus/supply_invariant_test.go`.

- [ ] **Test** `TestSupplyInvariant`: drive genesis seed → a fee-paying transfer → a create → a delete through a test DAG/state, advancing to an epoch boundary, and assert at the boundary (where in-flight `epochFees` is 0 after distribution) that `sum(coin balances) + total_bonded + sum(object.fees locked deposits) == total_supply` exactly. (Mid-epoch the LHS also needs `+ epochFees`; the test asserts the exact equality only at the boundary, per the spec.) Iterate coin balances + object fees via the state object store.
- [ ] **Run, expect PASS.**
- [ ] **Commit:** `git add internal/consensus/supply_invariant_test.go && git commit -m "[+] Property test: supply invariant at epoch boundaries"`

### Task 2.8: Verify Batch 2; push

- [ ] **Verify:** `go test ./internal/... -count=1 -timeout 180s`; `go test ./test/integration/ -run TestSimConsensus -count=1 -timeout 5m`.
- [ ] **Push:** `git push`

---

# Batch 3 — Stake on ValidatorInfo; carry it in snapshots; total_bonded; jailing

**Spec:** §2 (bonding, jailing), §1 (effective stake). Stake rides the snapshot's flat validator byte vector AND must be carried through the epoch holder snapshot (`snapshotEpochHolders`/`snapshotOf`), which Batch 5 reads for the stake-weighted quorum.

### Task 3.1: Stake/jail fields on ValidatorInfo

**Files:** Modify `internal/validators/validators.go`; Test `internal/validators/validators_test.go`.

- [ ] **Test:** zero value has `SelfStake==0`, `DelegatedTotal==0`, `Jailed==false`; `Get`/`All` copies include them.
- [ ] **Run, expect FAIL.**
- [ ] **Implement:** add `SelfStake uint64`, `DelegatedTotal uint64`, `Jailed bool` (documented); carry all three in the `Get`/`All` copy constructors. (`SetSelfStake` already exists from Task 1.4.)
- [ ] **Run, expect PASS.**
- [ ] **Commit:** `git add internal/validators/ && git commit -m "[+] Stake and jail fields on ValidatorInfo"`

### Task 3.2: Stake/jail mutators + stake-aware Add

**Files:** Modify `internal/validators/validators.go`; Test `internal/validators/validators_test.go`.

- [ ] **Test:** `AddDelegated`/`SubDelegated` (floor 0), `Jail`/`Unjail` toggle, each false on unknown pubkey; a new `AddWithStake(pubkey, quic, bls, selfStake, delegated, jailed)` adds carrying stake (used by snapshot rebuilds in 3.4).
- [ ] **Run, expect FAIL.**
- [ ] **Implement:** `AddDelegated`/`SubDelegated`/`Jail`/`Unjail` under the write lock; and `AddWithStake(...)` (or an exported setter trio callable after `Add`) so the epoch-holder snapshot can reconstruct stake. Keep the public surface minimal.
- [ ] **Run, expect PASS.**
- [ ] **Commit:** `git add internal/validators/ && git commit -m "[+] Stake/jail mutators and stake-aware Add on ValidatorSet"`

### Task 3.3: Persist stake in the snapshot encoder

**Files:** Modify `internal/sync/snapshot.go` (`encodeValidators`/`decodeValidators`); bump `snapshotVersion` to 8; Test `internal/sync/snapshot_test.go`.

- [ ] **Test:** encode/decode **two** validators with distinct `SelfStake`/`DelegatedTotal`/`Jailed` and assert the count is 2 with both sets of values preserved (a single-validator test would not catch a loop-guard misalignment); round-trip + checksum cover them.
- [ ] **Run, expect FAIL.**
- [ ] **Implement:** append `SelfStake`(8 BE), `DelegatedTotal`(8 BE), `Jailed`(1 byte) after the existing `pubkey|u16 quic_len|quic|bls(48)` record. **Extend the decode loop guard:** the loop is `for len(data) >= 32`; after the BLS read, add `if len(data) < 17 { break }` BEFORE reading the 17 stake bytes, else the trailing 17 bytes of validator N would be misread as the start of a phantom validator N+1's pubkey. Bump `snapshotVersion` to 8. `computeChecksumWithInfo` hashes `encodeValidators(...)`, so the new fields are covered automatically (no separate checksum change).
- [ ] **Run** `go test ./internal/sync/ -count=1 -timeout 120s`.
- [ ] **Commit:** `git add internal/sync/snapshot.go internal/sync/snapshot_test.go && git commit -m "[&] Persist validator stake and jail flag in snapshots (version 8)"`

### Task 3.4: Carry stake through the epoch holder snapshot

**Files:** Modify `internal/consensus/epoch.go` (`snapshotEpochHolders` AND `snapshotOf`); Test `internal/consensus/epoch_test.go`.

This is the linchpin for Batch 5: the stake-weighted quorum reads `HoldersForEpoch`, which is built by these two functions. Today they rebuild via `Add(pubkey, quic, bls)` and DROP stake.

- [ ] **Test:** after a boundary, `epochHolders.Get(pk)` carries the validator's `SelfStake`/`DelegatedTotal`/`Jailed`; the grace-window `prevEpochHolders` built by `snapshotOf` also carries them; a jailed validator is copied with `Jailed=true` (so `EffectiveStake` reads 0).
- [ ] **Run, expect FAIL.**
- [ ] **Implement:** in `snapshotEpochHolders` and `snapshotOf`, replace `Add(v.Pubkey, v.QUICAddr, v.BLSPubkey)` with `AddWithStake(v.Pubkey, v.QUICAddr, v.BLSPubkey, v.SelfStake, v.DelegatedTotal, v.Jailed)`.
- [ ] **Run, expect PASS.**
- [ ] **Commit:** `git add internal/consensus/epoch.go internal/consensus/epoch_test.go && git commit -m "[!] Carry stake/jail through the epoch holder snapshot"`

### Task 3.5: EffectiveStake and total_bonded

**Files:** Create `internal/consensus/stake.go`; Test `internal/consensus/stake_test.go`.

- [ ] **Test:** `EffectiveStake(&ValidatorInfo{SelfStake:100, DelegatedTotal:50})==150`; jailed → 0; `d.totalBonded()` sums effective stake over the active set.
- [ ] **Run, expect FAIL.**
- [ ] **Implement:**

```go
// EffectiveStake returns a validator's consensus/reward weight: self plus
// delegated stake. A jailed (or nil) validator contributes zero.
func EffectiveStake(v *ValidatorInfo) uint64 {
	if v == nil || v.Jailed {
		return 0
	}
	return safeAdd(v.SelfStake, v.DelegatedTotal)
}

// totalBonded sums effective stake over the active validator set (O(validators)).
func (d *DAG) totalBonded() uint64 {
	var total uint64
	for _, v := range d.validators.All() {
		total = safeAdd(total, EffectiveStake(v))
	}
	return total
}
```

- [ ] **Run, expect PASS.**
- [ ] **Commit:** `git add internal/consensus/stake.go internal/consensus/stake_test.go && git commit -m "[+] EffectiveStake and total_bonded derivation"`

### Task 3.6: bond / unbond system-pod entries (Rust)

**Files:** Modify `pods/pod-system/src/lib.rs` + a function directory each; build.

- [ ] **Read** `register_validator`'s real shape (its `functions/<name>/{mod,args,execute}.rs` + the `dispatcher!`/`mod.rs` lines) and mirror it — this is a few files per function, not a single edit.
- [ ] **Implement** minimal `bond(amount)` / `unbond(amount)` entries (the stake mutation is applied Go-side in Task 3.7; the Rust side just makes the function dispatch and validate args).
- [ ] **Build** the system-pod WASM (cargo wasm target + gas instrumentation).
- [ ] **Commit:** `git add pods/pod-system/ && git commit -m "[+] System pod: bond / unbond entries"`

### Task 3.7: Go-side bond/unbond; min stake; strict debit; jailing weight

**Files:** Modify `internal/consensus/commit.go` (`handleBond`/`handleUnbond` next to `handleRegisterValidator`); add `minStake` + `WithMinStake` in `dag.go`; Test `internal/consensus/bond_test.go`.

- [ ] **Test:** a `bond` from a registered validator, with the staked coin as a mutable_ref it owns, debits the coin and raises `SelfStake`; an under-funded `bond` is rejected WITHOUT zeroing the coin (strict debit); `bond` keeping `SelfStake` under `minStake` is rejected; `unbond` lowers `SelfStake`; a jailed validator carries zero weight via `EffectiveStake`.
- [ ] **Run, expect FAIL.**
- [ ] **Implement:** `handleBond` mirrors `handleRegisterValidator` (guard on system pod + `"bond"`); the staked coin MUST be a `mutable_ref` (so existing ownership validation covers it); read the amount from args; **strict debit** — `readCoinBalance(coin) >= amount` check first (do NOT use `deductCoinFee`, which zeroes on shortfall), then write `balance-amount`; `d.validators.SetSelfStake(sender, existing+amount)`; enforce `minStake` at register/bond. `handleUnbond` reduces `SelfStake` and credits the coin back (leave `// TODO: enforce unbonding delay` per §10/slashing branch). Add `WithMinStake(uint64) Option`. **Jailing scope:** this batch ships the jail MECHANISM (zeroed weight via `EffectiveStake`, carried in the snapshot by Task 3.4); it does NOT ship an automatic fault TRIGGER (that is the deferred dispute/fault-proof system, spec §10). At launch the live weight-removal path is deregistration; `Jail`/`Unjail` exist for that future trigger and for manual/governance use. Do not present jailing as an active automatic defense.
- [ ] **Run** `go test ./internal/consensus/ -count=1 -timeout 120s`.
- [ ] **Commit:** `git add -A && git commit -m "[+] Go-side bond/unbond (strict debit), minimum stake, jailing weight"`

### Task 3.8: Verify Batch 3; push

- [ ] **Verify:** `go test ./internal/... -count=1 -timeout 180s`; `go test ./test/integration/ -run TestSimBootstrap -count=1 -timeout 5m`.
- [ ] **Push:** `git push`

---

# Batch 4 — Delegation

**Spec:** §2. Each delegation is a stake-position object owned by the delegator `(validator, amount)`; the validator's `DelegatedTotal` is maintained. Fixed commission. Epoch-boundary proportional split. Mutations atomic and authenticated (Task 1.1 verifies the sender).

### Task 4.1: Delegation-position object codec

**Files:** Create `internal/consensus/delegation.go`; Test `internal/consensus/delegation_test.go`.

- [ ] **Test:** `DelegationID(delegator, validator)` deterministic & distinct per pair; `encode/decodeDelegationContent(validator, amount)` round-trips.
- [ ] **Run, expect FAIL.**
- [ ] **Implement:** position = an `Object`, `owner`=delegator, `content`=`validator(32)||amount(8 LE)`; `DelegationID` = BLAKE3(`"bluepods/delegation/v1"||delegator||validator`).
- [ ] **Run, expect PASS.**
- [ ] **Commit:** `git add internal/consensus/delegation.go internal/consensus/delegation_test.go && git commit -m "[+] Delegation-position object codec"`

### Task 4.2: delegate / undelegate system-pod entries (Rust)

**Files:** Modify `pods/pod-system/src/lib.rs` + dirs; build.

- [ ] **Implement** `delegate(validator, amount)` / `undelegate(validator)` mirroring `bond`/`unbond` (a few files each; Go applies the effect in 4.3).
- [ ] **Build.**
- [ ] **Commit:** `git add pods/pod-system/ && git commit -m "[+] System pod: delegate / undelegate entries"`

### Task 4.3: Go-side delegate/undelegate (atomic, strict)

**Files:** Modify `internal/consensus/commit.go` (`handleDelegate`/`handleUndelegate`); Test `internal/consensus/delegation_test.go`.

- [ ] **Test:** `delegate` to a known, non-jailed validator strictly debits the delegator's coin (rejects if under-funded without zeroing), creates the position (owner=delegator), and raises `DelegatedTotal` — all-or-nothing; `delegate` to an unknown or jailed validator is rejected; `undelegate` removes the position and lowers `DelegatedTotal`.
- [ ] **Run, expect FAIL.**
- [ ] **Implement** mirroring `handleBond`: validate target known and not jailed; strict-debit the coin by `amount`; on success create/delete the position object and `AddDelegated`/`SubDelegated` together (atomic — never raise `DelegatedTotal` without a funded position). Sender authenticity is already enforced by Task 1.1.
- [ ] **Run, expect PASS.**
- [ ] **Commit:** `git add internal/consensus/commit.go internal/consensus/delegation_test.go && git commit -m "[+] Go-side delegate/undelegate (atomic, strict debit)"`

### Task 4.4: Fixed commission parameter

**Files:** Modify `internal/consensus/dag.go` (`commissionBPS` + `WithCommissionBPS`, default 1000).

- [ ] **Implement** the field + Option (governed fixed commission, not per-validator).
- [ ] **Build + commit:** `git add internal/consensus/dag.go && git commit -m "[+] Fixed delegation commission parameter"`

### Task 4.5: Epoch-boundary proportional reward split

**Files:** Modify `internal/consensus/delegation.go`; Test `internal/consensus/delegation_test.go`.

- [ ] **Test** `TestSplitValidatorReward`: reward 1000, commission 1000 BPS, `SelfStake=600`, dels {100, 300} → validator keeps self-share + commission, delegators get pro-rata of the post-commission remainder; the sum of all returned amounts equals 1000 exactly (remainder to the validator). **Self-delegation case:** a validator that also appears as one of its own `dels` is paid its self-stake share AND that self-owned delegator share, on distinct capital (self-stake coin vs delegation-position coin) — assert no double-credit of the same tokens and that conservation still holds.
- [ ] **Run, expect FAIL.**
- [ ] **Implement:**

```go
// delegatorShare is one delegator's slice of an epoch reward.
type delegatorShare struct {
	Delegator [32]byte // Delegator is the position owner credited.
	Amount    uint64   // Amount is the delegation amount (input) / credited tokens (output).
}

// splitValidatorReward divides a validator's epoch reward between the validator
// (its self-stake share plus a fixed commission on the delegated portion) and its
// delegators (pro-rata to amount). Uses safeMul; the rounding remainder goes to
// the validator so the split conserves exactly.
func splitValidatorReward(reward, selfStake, commissionBPS uint64, dels []delegatorShare) (validatorAmount uint64, delegatorAmounts []delegatorShare) { /* ... */ }
```

- [ ] **Run, expect PASS.**
- [ ] **Commit:** `git add internal/consensus/delegation.go internal/consensus/delegation_test.go && git commit -m "[+] Epoch-boundary proportional reward split (fixed commission)"`

### Task 4.6: Enumerate a validator's delegations (narrow state method)

**Files:** Modify `internal/state/state.go` (the enumerator) + `internal/consensus/coins.go` (a narrow `DelegationEnumerator` interface) + `internal/consensus/dag.go` (a typed field set in `SetFeeSystem`) + `internal/consensus/delegation.go` (call it); update the consensus test stubs. Test `internal/consensus/delegation_test.go`.

Do NOT widen `CoinStore` with general iteration. Note `d.coinStore` is the `CoinStore` INTERFACE (tests inject a mock, not `*state.State`), so a type-assertion to `*state.State` would panic in unit tests — a dedicated interface is required.

- [ ] **Test:** after two `delegate` txs to V, the enumerator returns both `(delegator, amount)` for V; the consensus mock implements the enumerator too (so consensus tests compile).
- [ ] **Run, expect FAIL.**
- [ ] **Implement:** add `state.State.DelegationsFor(validator [32]byte) []DelegationEntry` — `db.Iterate`, filter `len(key)==32` and non-consensus keys (reuse the existing 32-byte object-key filter, factored into a small shared helper rather than duplicating `internal/sync`'s `isConsensusKey`), decode delegation-position objects whose `validator==V`. Declare `type DelegationEnumerator interface { DelegationsFor(validator [32]byte) []DelegationEntry }` in consensus; `SetFeeSystem` (which already receives the `*state.State` as `CoinStore`) also stores it as `d.delegations DelegationEnumerator`. Add the method to the consensus test stub. `// TODO: index per validator when delegation count grows.`
- [ ] **Run, expect PASS.**
- [ ] **Commit:** `git add -A && git commit -m "[+] Enumerate delegation positions per validator (narrow state method)"`

### Task 4.7: Verify Batch 4; push

- [ ] **Verify:** `go test ./internal/... -count=1 -timeout 180s`.
- [ ] **Push:** `git push`

---

# Batch 5 — Stake-weighted capped quorum; dual security model

**Spec:** §1. Voting weight is capped effective stake; quorum is exact integer `3*cappedSum >= 2*total`, read from the epoch holder snapshot selected by `commitEpochForRound(round)` at BOTH production and commit so they agree. Per-object attestation (`internal/aggregation`) stays equal-weight — untouched.

### Task 5.1: Capped voting weight (pure integer, safeMul)

**Files:** Modify `internal/consensus/stake.go`; add `votingCapMille` + `WithVotingCapMille` (default 100) in `dag.go`; Test `internal/consensus/stake_test.go`.

- [ ] **Test** `TestCappedWeight`: a validator above the cap is clamped; below is unchanged; with a small set the equal-share floor keeps a 2/3 quorum reachable; include a `total % setSize != 0` truncation case.
- [ ] **Run, expect FAIL.**
- [ ] **Implement:**

```go
// cappedWeight returns a validator's voting weight: effective stake capped at the
// per-validator ceiling. The ceiling is the larger of the configured fraction of
// total stake (per-mille) and an equal share, so a small set keeps a reachable
// 2/3 quorum. Uses safeMul to avoid overflow on total*capMille.
func cappedWeight(effective, total, capMille uint64, setSize int) uint64 {
	if setSize <= 0 || total == 0 {
		return effective
	}
	ceiling := safeMul(total, capMille) / 1000
	if equal := total / uint64(setSize); ceiling < equal {
		ceiling = equal
	}
	if effective > ceiling {
		return ceiling
	}
	return effective
}
```

- [ ] **Run, expect PASS.**
- [ ] **Commit:** `git add internal/consensus/stake.go internal/consensus/stake_test.go internal/consensus/dag.go && git commit -m "[+] Capped voting weight (safeMul, equal-share floor)"`

### Task 5.2: Capped quorum sum over a holder snapshot

**Files:** Modify `internal/consensus/stake.go`; Test `internal/consensus/stake_test.go`.

- [ ] **Test** `TestQuorumReached`: `quorumReached(cappedSum, total)` is `3*cappedSum >= 2*total` and returns FALSE when `total==0` (degenerate-safety guard); `cappedStakeOf(set, producers)` sums `cappedWeight` over present producers and returns the uncapped `total`.
- [ ] **Run, expect FAIL.**
- [ ] **Implement:**

```go
// quorumReached reports whether a capped-stake sum meets the 2/3 BFT threshold,
// using exact integer arithmetic. A zero total (no stake yet) is NOT quorum.
func quorumReached(cappedSum, total uint64) bool {
	if total == 0 {
		return false
	}
	return safeMul(3, cappedSum) >= safeMul(2, total)
}

// cappedStakeOf sums the capped voting weight of the given producers within a
// holder set and returns the set's uncapped total stake.
func cappedStakeOf(set *ValidatorSet, producers map[Hash]bool) (cappedSum, total uint64) { /* uses EffectiveStake + cappedWeight */ }
```

- [ ] **Run, expect PASS.**
- [ ] **Commit:** `git add internal/consensus/stake.go internal/consensus/stake_test.go && git commit -m "[+] Capped-stake quorum sum (guards total==0)"`

### Task 5.3: Stake-weight the commit quorum

**Files:** Modify `internal/consensus/commit.go` (`isRoundCommitted`); Test `internal/consensus/commit_test.go`.

- [ ] **Test:** with two validators at 90%/10% stake, a round with only the 10% producer does NOT commit; with the 90% producer it does — using `HoldersForEpoch(d.commitEpochForRound(round))`.
- [ ] **Run, expect FAIL.**
- [ ] **Implement:** replace the distinct-producer count check with `set := d.HoldersForEpoch(d.commitEpochForRound(round)); cappedSum, total := cappedStakeOf(set, producers); committed := quorumReached(cappedSum, total)`. Keep the relaxed bootstrap/transition paths count-based (single producer), since genesis seeds stake but early convergence still relaxes.
- [ ] **Run** `go test ./internal/consensus/ -count=1 -timeout 120s`.
- [ ] **Commit:** `git add internal/consensus/commit.go internal/consensus/commit_test.go && git commit -m "[&] Stake-weight the round commit quorum (epoch-pinned)"`

### Task 5.4: Stake-weight the production quorum (same epoch selection)

**Files:** Modify `internal/consensus/dag.go` (`hasQuorumFromRound`, `canProduceVertex`); Test `internal/consensus/dag_test.go`.

- [ ] **Test:** `hasQuorumFromRound(round)` returns true once the round's producers carry a 2/3 capped-stake majority of the holder snapshot for `commitEpochForRound(round)` — the SAME snapshot the committer uses (not `currentEpoch`), so production and commit cannot diverge across an epoch boundary.
- [ ] **Run, expect FAIL.**
- [ ] **Implement:** in `hasQuorumFromRound`, build producers from the round and evaluate `cappedStakeOf`/`quorumReached` against `d.HoldersForEpoch(d.commitEpochForRound(round))`, keeping transition/bootstrap relaxations. Stop using `ValidatorSet.QuorumSize()` as the commit/production authority (keep it only for logging/relaxed counts).
- [ ] **Run, expect PASS.**
- [ ] **Commit:** `git add internal/consensus/dag.go internal/consensus/dag_test.go && git commit -m "[&] Stake-weight the production quorum (same epoch snapshot as commit)"`

### Task 5.5: Clarify validateParentsQuorum

**Files:** Modify `internal/consensus/validate.go`; Test `internal/consensus/validate_test.go`.

- [ ] **Test:** still accepts a vertex with ≥1 known-validator parent; rejects zero known parents.
- [ ] **Implement:** keep the presence check; update the docstring to state the authoritative stake-weighted quorum is enforced in `hasQuorumFromRound`/`isRoundCommitted`, and receiving nodes cannot recompute another node's stake-quorum during convergence.
- [ ] **Commit:** `git add internal/consensus/validate.go internal/consensus/validate_test.go && git commit -m "[&] Clarify validateParentsQuorum under stake-weighting"`

### Task 5.6: Verify Batch 5; push

- [ ] **Verify:** `go test ./internal/consensus/ -count=1 -timeout 180s`; `go test ./test/integration/ -run TestSimConsensus -count=1 -timeout 5m`; `go test ./test/integration/ -run TestSimBootstrap -count=1 -timeout 5m`. The founder's self-stake is seeded at genesis (Batch 1.3), so quorum is reachable — do NOT grant free "default stake at registration" (that would be Sybil-free weight). If a sim registers extra validators expected to vote, ensure they bond before relying on their weight.
- [ ] **Push:** `git push`

---

# Batch 6 — The thermostat (per-epoch adaptive issuance)

**Spec:** §3. Adaptive control loop at each epoch boundary, denominated in epoch events (no clock). Band ~25–35% of `total_supply` (dead-band), bounded `[floor, ceiling]`, capped step, ratio on PRE-mint supply, mint into the pool, auto-restake a fraction. Runs in `transitionEpoch` before reward distribution.

### Task 6.1: Thermostat parameters + persisted rate

**Files:** Create `internal/consensus/thermostat.go`; modify `dag.go` (params + Option + `issuanceRateMicro`); modify `types/snapshot.fbs` (`issuance_rate_micro:uint64`, bump `snapshotVersion` to 9), regenerate. Test `internal/consensus/thermostat_test.go`.

- [ ] **Implement** `thermostatParams` (integer units): `targetLowMille=250`, `targetHighMille=350`, `floorRateMicro`/`ceilingRateMicro` (per-epoch ≈1%/20% annual), `genesisRateMicro` (≈8–10% annual), `stepCapMicro`, `autoRestakeMille`. Add `WithThermostat(...)`, store `d.issuanceRateMicro` (seeded to `genesisRateMicro`). Add the snapshot scalar (rate is stateful — the loop steps from the previous value, so it cannot be re-derived). Regenerate + build.
- [ ] **Commit:** `git add -A && git commit -m "[+] Thermostat parameters and persisted per-epoch rate (snapshot version 9)"`

### Task 6.2: Ratio and rate-adjustment math (pure, safeMul)

**Files:** Modify `internal/consensus/thermostat.go`; Test `internal/consensus/thermostat_test.go`.

- [ ] **Test** `TestAdjustRate`: below band raises by ≤ `stepCapMicro` (clamped to ceiling); above band lowers (clamped to floor); inside the dead-band holds. `stakingRatioMille(bonded, supply)` = `bonded*1000/supply` (0 when supply==0), using safeMul.
- [ ] **Run, expect FAIL.**
- [ ] **Implement** `stakingRatioMille` (safeMul, guard supply==0) and `adjustRate(rate, ratioMille, p) uint64` (dead-band hold; step-capped; clamp to [floor, ceiling]).
- [ ] **Run, expect PASS.**
- [ ] **Commit:** `git add internal/consensus/thermostat.go internal/consensus/thermostat_test.go && git commit -m "[+] Thermostat ratio and rate-adjustment math"`

### Task 6.3: Issuance from pre-mint supply

**Files:** Modify `internal/consensus/thermostat.go`; Test `internal/consensus/thermostat_test.go`.

- [ ] **Test** `TestComputeIssuance`: `issuanceFor(rateMicro, supply)` = `safeMul(rateMicro, supply)/1_000_000`; 0 when rate 0.
- [ ] **Run, expect FAIL.**
- [ ] **Implement** `issuanceFor` (safeMul); document supply is the PRE-mint value.
- [ ] **Run, expect PASS.**
- [ ] **Commit:** `git add internal/consensus/thermostat.go internal/consensus/thermostat_test.go && git commit -m "[+] Issuance from pre-mint supply"`

### Task 6.4: Run the thermostat at the epoch boundary (mint only when distributable)

**Files:** Modify `internal/consensus/epoch.go` (`transitionEpoch`, `distributeEpochRewards`); Test `internal/consensus/epoch_test.go`.

No `carryoverPool` field (it would be unpersisted consensus state → a synced node would distribute a different pool → consensus split). Instead: only mint issuance when it can be fully distributed this epoch, and distribute the whole pool (the integer-division remainder goes to a deterministic validator, Task 7.3), so supply only ever grows by exactly what lands in coins/stake. No orphaned issuance, nothing to persist.

- [ ] **Test:** at a boundary with bonded below band and at least one producer, `issuanceRateMicro` rises (capped) and `total_supply` rises by the minted issuance; a zero-FEE epoch with production still mints (bootstrap incentive); a zero-WEIGHT epoch (no rounds produced / `totalWeight==0`) mints NOTHING (`total_supply` unchanged) — there is no one to pay, and the rate still adjusts for next epoch.
- [ ] **Run, expect FAIL.**
- [ ] **Implement** `runThermostat(distributable bool) uint64`: `preMint := d.coinStore.TotalSupply(); bonded := d.totalBonded(); ratio := stakingRatioMille(bonded, preMint); d.issuanceRateMicro = adjustRate(d.issuanceRateMicro, ratio, d.thermostat)` (the rate adjusts every epoch); then `if !distributable { return 0 }`, else `issuance := issuanceFor(d.issuanceRateMicro, preMint); d.coinStore.AddSupply(issuance); return issuance`. In `transitionEpoch`, compute `distributable := d.totalRewardWeight() > 0` and call `issuance := d.runThermostat(distributable)`, then `distributeEpochRewards(issuance)`. Define the helper here (Task 7.2 reuses it): `func (d *DAG) totalRewardWeight() uint64` sums `safeMul(EffectiveStake(v), epochRoundsProduced[v.Pubkey])` over the active set — this is the EXACT denominator distribution uses, so minting only when it is positive guarantees the pool is fully distributable (covers the edge where producers have zero stake while stakers produced zero rounds). Remove the `epochFees==0` early-return; the pool is `epochFees + issuance`, distributed in full by Task 7.3 (remainder to a deterministic validator), so supply == sum of credits.
- [ ] **Run** `go test ./internal/consensus/ -count=1 -timeout 120s`.
- [ ] **Commit:** `git add internal/consensus/epoch.go internal/consensus/epoch_test.go && git commit -m "[+] Run the thermostat at each boundary; mint only what is fully distributed"`

### Task 6.5: Persist/restore the issuance rate

**Files:** Modify `internal/sync/snapshot.go` + sync wiring; Test `internal/sync/snapshot_test.go`.

- [ ] **Test:** `issuance_rate_micro` round-trips, is checksum-covered (tamper test, like `total_supply`), and is restored into the DAG on apply.
- [ ] **Run, expect FAIL.**
- [ ] **Implement:** thread `issuance_rate_micro` through `CreateSnapshot`/`computeChecksumWithInfo`/`verifyChecksum`; restore into the DAG via a `SetIssuanceRate` setter called from `cmd/node/sync.go:requestAndApplySnapshot` (same site as `SetTotalSupply` in Task 2.6) — otherwise a synced node boots with rate 0 and its thermostat mints nothing while the rest of the network issues, a consensus-affecting divergence.
- [ ] **Run** `go test ./internal/sync/ -count=1 -timeout 120s`.
- [ ] **Commit:** `git add -A && git commit -m "[&] Persist and restore the thermostat issuance rate (checksum-covered)"`

### Task 6.6: Verify Batch 6; push

- [ ] **Verify:** `go test ./internal/... -count=1 -timeout 180s`.
- [ ] **Push:** `git push`

---

# Batch 7 — Reward distribution (`effective_stake x liveness`)

**Spec:** §6, §5.2. Finishes the `TODO: credit to validator's reward_coin`. Pool = `epochFees + issuance`, distributed in full (remainder to a deterministic top-weight validator). Weight = `effective_stake x liveness` (`epochRoundsProduced`), NOT attestation count. Credit via `creditCoin` to a `reward_coin` the validator designates; split with delegators; auto-restake a fraction; reconcile the remainder.

### Task 7.1: reward_coin designation

**Files:** Modify `internal/validators/validators.go` (`RewardCoin [32]byte`, carried in copies + encoder, bump `snapshotVersion` to 10); Modify `internal/consensus/commit.go` (`handleRegisterValidator` reads it from args). Test accordingly.

- [ ] **Test:** registering with a reward-coin arg sets `Get(pk).RewardCoin`; snapshot preserves it; the default for a validator that designates none is a coin it actually owns (its gas coin / the founder's genesis coin) — NOT a derived `GenesisCoinID(pubkey)` that doesn't exist for late registrants (`creditCoin` would fail "not found").
- [ ] **Run, expect FAIL.**
- [ ] **Implement** the field + encoder extension (append 32 bytes, bump version 10) + registration read (default to a coin the validator owns; if none can be determined, leave zero and skip crediting that validator's liquid portion with a logged warning rather than failing the epoch).
- [ ] **Run, expect PASS.**
- [ ] **Commit:** `git add -A && git commit -m "[+] reward_coin designation on validators (snapshot version 10)"`

### Task 7.2: Reward weight = effective_stake x liveness

**Files:** Modify `internal/consensus/epoch.go` (`distributeEpochRewards`); Test `internal/consensus/epoch_test.go`.

- [ ] **Test** `TestRewardWeight`: equal stake, different `epochRoundsProduced` → reward ∝ rounds; equal rounds, different stake → reward ∝ stake; zero rounds → nothing; attestation count is not an input. A producer cannot be credited more than once per round (one vertex per round; equivocation does not double liveness) — add an assertion/guard that `epochRoundsProduced[p]` counts distinct rounds.
- [ ] **Run, expect FAIL.**
- [ ] **Implement:** `weight = safeMul(EffectiveStake(v), epochRoundsProduced[pubkey])` (the common `epochTotalRounds` denominator cancels); `totalWeight = d.totalRewardWeight()` (the helper defined in Task 6.4); `share = safeMul(pool, weight) / totalWeight` (divide last to limit overflow). `distributeEpochRewards` is only called when `totalRewardWeight() > 0` (Task 6.4 gates minting+distribution on it), so the division has no zero denominator. Pool from Task 6.4 (`epochFees + issuance`).
- [ ] **Run, expect PASS.**
- [ ] **Commit:** `git add internal/consensus/epoch.go internal/consensus/epoch_test.go && git commit -m "[&] Reward weight = effective_stake x liveness (safeMul)"`

### Task 7.3: Credit rewards; split; auto-restake; distribute the full pool

**Files:** Modify `internal/consensus/epoch.go` (`distributeEpochRewards(issuance uint64)`); Test `internal/consensus/epoch_test.go`.

- [ ] **Test** `TestRewardCrediting`: each validator's `share` is split via `splitValidatorReward`; the validator's portion minus the auto-restake part is `creditCoin`'d to its `RewardCoin`; the auto-restake part raises `SelfStake`; each delegator's portion is `creditCoin`'d to its coin; `sum(all credited + all restaked) == pool` EXACTLY — the integer-division remainder (floored shares + any `share==0` validators) is credited to the deterministic top-weight validator (largest `weight`, ties broken by lower pubkey bytes), so nothing is orphaned and no carry state is needed.
- [ ] **Run, expect FAIL.**
- [ ] **Implement:** `pool = epochFees + issuance`. Pick `topValidator` deterministically (max weight, tie-break by pubkey). For each validator: enumerate delegations (Task 4.6), `splitValidatorReward(share, v.SelfStake, d.commissionBPS, dels)`; credit delegator amounts to their coins; split the validator amount into `restake = amount*autoRestakeMille/1000` (→ `SetSelfStake(self+restake)`) and `liquid` (→ `creditCoin(v.RewardCoin, liquid)` when `RewardCoin` is set, else log+skip per Task 7.1). Track `distributed`; credit `pool - distributed` (the remainder) to `topValidator`'s `RewardCoin`. This conserves exactly (`sum credited == pool`), and since supply was raised only by `issuance` (Task 6.4) and the `epochFees` portion was in-flight supply, the §7 invariant holds at the boundary. Replace the `TODO: credit to validator's reward_coin` block. Self-delegation: `splitValidatorReward`'s `selfStake` (the validator's bonded coin) and the `dels` slice (delegation-position objects) are disjoint by construction, so a validator that self-delegates is paid its self-stake share AND its self-owned delegator share on distinct capital — no double-credit (asserted in Task 4.5).
- [ ] **Run** `go test ./internal/consensus/ -count=1 -timeout 120s`.
- [ ] **Commit:** `git add internal/consensus/epoch.go internal/consensus/epoch_test.go && git commit -m "[+] Credit rewards: split, auto-restake, full-pool with remainder to top validator"`

### Task 7.4: Reward conservation property test

**Files:** Test `internal/consensus/epoch_test.go`.

- [ ] **Test** `TestRewardConservation`: over an epoch, `sum(credited + restaked) == pool` exactly (remainder to the top validator, Task 7.3); and the supply invariant (Task 2.7) holds at the boundary — issuance was added to supply in 6.4 only because it is fully distributed here; crediting/restaking MOVES the pool into coins/stake (no new supply); the `epochFees` portion was in-flight supply now landing in validator coins.
- [ ] **Run, expect PASS.**
- [ ] **Commit:** `git add internal/consensus/epoch_test.go && git commit -m "[+] Property test: epoch reward conservation"`

### Task 7.5: Verify Batch 7; push

- [ ] **Verify:** `go test ./internal/... -count=1 -timeout 180s`; `go test ./test/integration/ -run TestSimConsensus -count=1 -timeout 5m`.
- [ ] **Push:** `git push`

---

# Batch 8 — Sponsored transactions

**Spec:** §9. `transaction.fbs` gains `fee_payer`, `sponsor_signature`, `valid_until`, absent-when-empty so a non-sponsored tx serializes byte-identically. Both parties sign the SAME canonical body hash. Sponsor signature verified AT COMMIT (extending Task 1.1's site). Gas-coin owner check becomes `owner == fee_payer`. Replay via the commit-once guard (now keyed on a commit-verified hash, Task 1.1); `valid_until` (epochs) checked against the commit epoch.

### Task 8.1: Schema fields (absent-when-empty)

**Files:** Modify `types/transaction.fbs`; regenerate; build. Test `internal/genesis/transaction_test.go`.

- [ ] **Implement:** add `fee_payer:[ubyte];`, `sponsor_signature:[ubyte];`, `valid_until:uint64;`. `bash types/generate.sh && go build ./...`.
- [ ] **Test** `TestNonSponsoredTxByteIdentical`: a tx with no fee_payer serializes identically to before (fields absent, not zero-filled); its hash/signature verify unchanged; existing genesis tx tests still pass.
- [ ] **Run, expect PASS.**
- [ ] **Commit:** `git add types/transaction.fbs internal/types/ && git commit -m "[+] Schema: fee_payer, sponsor_signature, valid_until (absent-when-empty)"`

### Task 8.2: Canonical body hash covers the new fields (client + consensus in lockstep)

**Files:** Modify `internal/genesis/transaction.go` (`BuildUnsignedTxBytesWithRefs`); Modify `internal/validation/validate.go` (`rebuildUnsignedTx`); Modify `internal/consensus/txauth.go` (recompute must include them). Test `internal/genesis/transaction_test.go`, `internal/validation/*_test.go`.

The body hash is recomputed at THREE places that MUST agree byte-for-byte: the builder, client ingress (`internal/validation`), and commit (`txauth.go`, Task 1.1). All include `fee_payer`/`valid_until` (when present), exclude both signature fields.

- [ ] **Test:** the unsigned body includes `fee_payer`/`valid_until` when set, never the signatures; two bodies differing only in `fee_payer` hash differently; an absent `fee_payer` yields legacy bytes; the recomputed hash matches at ingress AND at commit.
- [ ] **Run, expect FAIL.**
- [ ] **Implement:** add `fee_payer`/`valid_until` to the unsigned-body construction (conditionally, absent-when-empty, mirroring the `gasCoin` pattern) in `BuildUnsignedTxBytesWithRefs`; update `rebuildUnsignedTx` and the `txauth.go` recompute identically.
- [ ] **Run, expect PASS.**
- [ ] **Commit:** `git add -A && git commit -m "[+] Canonical body hash binds fee_payer and valid_until (builder, ingress, commit)"`

### Task 8.3: Build a doubly-signed sponsored tx

**Files:** Modify `internal/genesis/transaction.go`; Test `internal/genesis/transaction_test.go`.

- [ ] **Test:** `BuildSponsoredTx(senderKey, sponsorKey, ...)` → both `signature` (sender) and `sponsor_signature` (sponsor) verify against the same body hash; `fee_payer==sponsor pubkey`; `valid_until` set.
- [ ] **Run, expect FAIL.**
- [ ] **Implement** the builder (one body hash, two signatures).
- [ ] **Run, expect PASS.**
- [ ] **Commit:** `git add internal/genesis/transaction.go internal/genesis/transaction_test.go && git commit -m "[+] Build doubly-signed sponsored transactions"`

### Task 8.4: Verify the sponsor signature at commit; gate gas coin on fee_payer

**Files:** Modify `internal/consensus/txauth.go` (extend `verifyTxAuthenticity` for the sponsor) + `internal/consensus/commit.go` (`validateGasCoin`); Test `internal/consensus/commit_test.go`, `internal/consensus/txauth_test.go`.

- [ ] **Test:** when `len(FeePayerBytes())==32`, an invalid `sponsor_signature` is rejected AT COMMIT (so a gossiped forged sponsored tx cannot commit); a sponsored tx whose gas coin is owned by `fee_payer` passes `validateGasCoin`; a non-sponsored tx still requires `owner==sender`.
- [ ] **Run, expect FAIL.**
- [ ] **Implement:** define ONE predicate `isSponsored(tx) bool { return len(tx.FeePayerBytes()) == 32 }` and use it at all three sites (sponsor-sig verify, gas-coin owner, `valid_until` in Task 8.5) so a malformed 1-byte `fee_payer` cannot be "present" at one site and "absent" at another. In `verifyTxAuthenticity` (Task 1.1), when `isSponsored(tx)`, additionally `ed25519.Verify(fee_payer, hash[:], sponsor_signature)` (a malformed fee_payer fails the verify, closing safe). In `validateGasCoin`, require `owner == fee_payer` when `isSponsored`, else `owner == sender`.
- [ ] **Run** `go test ./internal/consensus/ -count=1 -timeout 120s`.
- [ ] **Commit:** `git add -A && git commit -m "[+] Verify sponsor signature at commit; gas coin owned by fee_payer"`

### Task 8.5: Enforce valid_until against the commit epoch

**Files:** Modify `internal/consensus/commit.go` (`executeTx`); Test `internal/consensus/commit_test.go`.

- [ ] **Test:** a sponsored tx with `valid_until < d.commitEpochForRound(commitRound)` is rejected; `>=` proceeds; a sponsored tx (`fee_payer` present) with `valid_until == 0` is rejected (a sponsored tx MUST carry a bound); the boundary round (which maps to epoch `k-1`) is tested explicitly.
- [ ] **Run, expect FAIL.**
- [ ] **Implement:** in `executeTx`, when `isSponsored(tx)` (the shared predicate from Task 8.4): reject if `valid_until == 0`, else reject if `valid_until < d.commitEpochForRound(commitRound)`. A non-sponsored tx never reads `valid_until` (an absent field defaulting to 0 must not reject it). Document the boundary-round mapping (round `k*epochLength` maps to epoch `k-1`).
- [ ] **Run, expect PASS.**
- [ ] **Commit:** `git add internal/consensus/commit.go internal/consensus/commit_test.go && git commit -m "[+] Enforce sponsored-tx valid_until (reject 0; precise epoch boundary)"`

### Task 8.6: Client/SDK support for sponsored submission

**Files:** Modify `client/`; optionally `cmd/cli` (`bpctl`). Test in `client/`.

- [ ] **Test:** the client assembles a sponsored tx (sender body + sponsor signature) and submits it; it commits and charges the sponsor's coin.
- [ ] **Run, expect FAIL.**
- [ ] **Implement** the minimal client helper(s); add a `bpctl` subcommand only if small (else `// TODO`).
- [ ] **Run** client tests + `go test ./test/integration/ -run TestSimConsensus -count=1 -timeout 5m`.
- [ ] **Commit:** `git add -A && git commit -m "[+] Client support for sponsored transactions"`

### Task 8.7: Verify Batch 8; push

- [ ] **Verify:** `go test ./internal/... ./client/... -count=1 -timeout 180s`; `go test ./test/integration/ -run TestSimConsensus -count=1 -timeout 5m`.
- [ ] **Push:** `git push`

---

# Batch 9 — `Vertex.timestamp` field (pipeline deferred)

**Spec:** §8. Land ONLY the field now. Median derivation / monotonic coercion / pod exposure deferred. Producers stamp from their local clock; it is in the signed/hashed body.

### Task 9.1: Add the timestamp field

**Files:** Modify `types/vertex.fbs`; regenerate; build.

- [ ] **Implement:** add `timestamp:uint64;` to `Vertex` (doc: producer local wall-clock at production, signed; pipeline deferred). `bash types/generate.sh && go build ./...`.
- [ ] **Commit:** `git add types/vertex.fbs internal/types/ && git commit -m "[+] Schema: signed Vertex.timestamp field"`

### Task 9.2: Producers stamp and sign the timestamp

**Files:** Modify `internal/consensus/build.go` (`buildUnsignedVertex` AND `buildSignedVertex`); Test `internal/consensus/build_test.go`.

- [ ] **Test:** a produced vertex has non-zero `Timestamp()`, it is covered by the hash (two vertices differing only in timestamp hash differently), and the signature verifies.
- [ ] **Run, expect FAIL.**
- [ ] **Implement:** set `types.VertexAddTimestamp(builder, uint64(time.Now().UnixNano()))` in BOTH `buildUnsignedVertex` (so it feeds the hash) and `buildSignedVertex` (with the identical value), so the signed field matches what was hashed/signed. Accept any value (the interpreting pipeline is deferred); document the deferral.
- [ ] **Run** `go test ./internal/consensus/ -count=1 -timeout 120s`.
- [ ] **Commit:** `git add internal/consensus/build.go internal/consensus/build_test.go && git commit -m "[+] Producers stamp and sign Vertex.timestamp"`

### Task 9.3: Assert money never reads the clock

**Files:** Test/comment in `internal/consensus/`.

- [ ] **Assert:** a short test/doc confirming `runThermostat` takes no timestamp and `valid_until` is epoch-based — money never reads `Vertex.timestamp`, so the over-state-time bias never applies.
- [ ] **Commit:** `git add -A && git commit -m "[&] Assert money never reads the vertex clock"`

### Task 9.4: Verify Batch 9; push

- [ ] **Verify:** `go test ./internal/... -count=1 -timeout 180s`; `go test ./test/integration/ -run TestSimBootstrap -count=1 -timeout 5m` (timestamp is consensus-breaking — confirm convergence).
- [ ] **Push:** `git push`

---

# Batch 10 — Docs and schema comments

**Spec:** Doc impact. VISION owns the why; WHITEPAPER owns the how. Edit in place; straight quotes; no em dashes.

### Task 10.1: WHITEPAPER fees, reward, supply

**Files:** `docs/WHITEPAPER.md`.

- [ ] **Edit** §9 (Fees): 70/30 → 100/0; drop burn-as-anti-gaming; storage component is a LOCKED deposit (not pooled); reward is `effective_stake x liveness` with serving enforcement deferred. §10: formula `stake x (rounds/total)` → `effective_stake x liveness`; add delegation (positions, fixed commission, epoch-boundary split, unbonding, jailing), minimum-stake / voting-cap note. New `total_supply` accounting subsection (genesis-set, issuance-only mint, locked deposits, invariant). Note the user `mint` is removed.
- [ ] **Commit:** `git add docs/WHITEPAPER.md && git commit -m "[&] Whitepaper: 100/0 fees, locked deposits, effective_stake x liveness, supply"`

### Task 10.2: WHITEPAPER consensus, security, sponsored, authenticity

**Files:** `docs/WHITEPAPER.md`.

- [ ] **Edit** §§10/5: stake-weighted capped quorum + dual security replace equal-weight and "stake is equal (1)"; genesis-as-state + bonded founder. §1 headline: "honest majority per object for attestation, honest two-thirds-of-stake for ordering". §§7/9/12: sponsored transactions (`fee_payer`, sponsor signature, gas coin owned by fee payer) AND that transaction authenticity (sender + sponsor signature, hash) is verified in the commit path on every node.
- [ ] **Commit:** `git add docs/WHITEPAPER.md && git commit -m "[&] Whitepaper: stake-weighted quorum, dual security, sponsored tx, commit-time authenticity"`

### Task 10.3: VISION positioning

**Files:** `docs/VISION.md`.

- [ ] **Edit:** add the utility-first, mildly-inflationary, no-burn stability stance (the why), without duplicating whitepaper mechanics.
- [ ] **Commit:** `git add docs/VISION.md && git commit -m "[&] Vision: utility-first, mildly-inflationary, no-burn stance"`

### Task 10.4: Schema comments; final verify; push

**Files:** `types/vertex.fbs` (comment).

- [ ] **Edit** the `FeeSummary.total_burned` comment: vestigial (always 0 since the scarcity burn was removed), soft-deprecated. Regenerate only if needed.
- [ ] **Final verify:** `go test ./internal/... ./client/... -count=1 -timeout 180s`; `go test ./test/integration/ -run TestSimBootstrap -count=1 -timeout 5m`; `go test ./test/integration/ -run TestSimConsensus -count=1 -timeout 5m`.
- [ ] **Commit + push:** `git add -A && git commit -m "[&] Soft-deprecate total_burned; final economic-layer verification"` then `git push`.

---

## Self-review

**Spec coverage.** §1 stake-weighted capped quorum + dual security → Batch 5 (+ headline 10.2). §2 bonding/delegation/jailing → Batches 3–4. §3 thermostat → Batch 6. §4 no scarcity burn → 2.5. §5 fees: 100/0 + structure kept + **storage locked, never pooled** → 2.2/2.5. §6 reward `effective_stake x liveness` → Batch 7. §7 supply + invariant → Batch 2 (counter, locked deposits, snapshot) + 3.5 (bonded) + 2.7/7.4 (property tests). §8 timestamp field only → Batch 9. §9 sponsored tx + **commit-time authenticity** → Batch 8 (built on 1.1). §10 deferred enforcement → respected (no slashing/storage-challenge; unbonding-delay TODO). §11 genesis-and-fee integrity + **bonded founder + mint removal** → Batch 1. Parameters → Options across Batches 3–6. Doc impact → Batch 10.

**Review-fix coverage (iteration 1 → 2).** Tx authenticity at commit (C1) → Task 1.1, extended 8.2/8.4. Unfunded storage deposit / impossible invariant (C2) → 2.2 (lock, never pool) + corrected §7 invariant (epoch-boundary, fees-in-flight). User mint printer (C3) → 1.6. `WithGenesisState` nil-panic ordering (C4) → 1.4/1.5 (`SeedGenesis` after `SetFeeSystem`). Faucet transfer→split (C5) → 1.8. Founder self-stake at genesis (H1) → 1.3. Snapshot drops stake (H2) → 3.4 + `quorumReached` guards total==0 (5.2). Reward under-credit vs pre-minted supply (H3) → carry-forward in 6.4/7.3. `deductCoinFee` zeroes coin on bond (H4) → strict debit in 3.7/4.3. Invariant "after every commit" (H5) → epoch-boundary assertion (2.7). Snapshot task missing verifyChecksum/provider/restore (M1) → 2.6 names all three. Production vs commit epoch mismatch (M2) → both use `commitEpochForRound` (5.3/5.4). valid_until off-by-one / ==0 (M3) → 8.5. Client hash check (M4) → 8.2 includes `internal/validation`. State has no db handle (M5) → 2.1. IterateObjects on CoinStore (M6) → narrow state method (4.6). RewardCoin default fails for registrants (M7) → 7.1. Overflow discipline → safeMul in all new math. Equivocation/liveness double-credit → 7.2 guard. Rust = full directory → noted (3.6/4.2). buildUnsignedVertex for timestamp → 9.2.

**Review-fix coverage (iteration 2).** Storage-deposit `totalValidators` divergence (consensus live vs state init-frozen) → 2.2 keeps the two counts equal + a count-match test. `carryoverPool` un-persisted / consensus-split → eliminated: no carryover field; mint only when `totalRewardWeight()>0`, distribute the full pool with the remainder to a deterministic top-weight validator (6.4/7.3). Commit-time auth vs the proof-verdict cursor → 1.1 placed AFTER `proofVerdict`, BEFORE commit-once (cursor untouched). Three-site body hash drift → 1.1/8.2 mandate reusing the single `genesis.BuildUnsignedTxBytesWithRefs` primitive. `DelegationsFor` on a `*state.State` the DAG does not hold → 4.6 defines a `DelegationEnumerator` interface implemented by state and the mock. `mint` removal under-budgeted the ATP-16.x sims → 1.6. `isSponsored` predicate unified across the three sponsored-tx sites → 8.4. `GenesisStake>InitialMint` underflow → 1.3 clamp. Jailing trigger honesty (mechanism ships, auto-trigger deferred) → spec §2/§10 + 3.7 note. Refund follows the gas coin → spec §9. Restore site pinned to `requestAndApplySnapshot` → 2.6/6.5. `decodeValidators` loop guard → 3.3 explicit `<17` guard + two-validator test. `state_supply.go` dropped (methods in `coins.go`) → file map.

**Deferred-by-design (not gaps).** F1 accumulator, liquid staking, per-validator commission, dynamic fee, clock pipeline, slashing, storage/serving challenges, unbonding-delay enforcement — all explicitly out of scope per the spec.

**Forward references.** `SetSelfStake` (1.4) before the stake API (3.2); `CoinStore` supply methods (2.3) before the thermostat (6); `AddWithStake` (3.2) before the snapshot carry (3.4); `EffectiveStake` (3.5) before quorum (5) and reward (7); `cappedWeight`/`quorumReached` (5.1/5.2) before the quorum sites (5.3/5.4); `splitValidatorReward` + delegation enumeration (4.5/4.6) before crediting (7.3); the body-hash change (8.2) before sponsor verification (8.4). No symbol is referenced before its defining task.

**Residual risks.** (1) Repeated snapshot version bumps (6→7→8→9→10) — each must update encode+decode+checksum on BOTH the build and verify sides. (2) Rust system-pod rebuild required before Go-side bond/delegate handlers are exercised (Batches 3/4). (3) FlatBuffers regen (`bash types/generate.sh`) + rebuild before tests on Batches 2/6/8/9. (4) Batch 1.1 (commit-time authenticity) changes a hot path for every tx — verify sim throughput does not regress. (5) The storage-deposit count-sync (2.2) is determinism-critical: consensus and state MUST read the same validator count at the same commit point, or nodes diverge on `object.fees` — the count-match test must run with a grown validator set, not a static one.
