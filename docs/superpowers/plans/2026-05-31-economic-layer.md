# Economic Layer Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development to implement this plan **one batch at a time** (one implementation subagent per batch). Within a batch, the subagent executes its tasks in order; **each task ends in one commit**. After a batch's tasks are all committed, **push** before starting the next batch. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Close the BluePods economic loop end to end so the spec is 100% implemented: genesis-as-state, real supply accounting, no scarcity burn, staking with delegation, stake-weighted capped consensus, the adaptive issuance thermostat, stake-and-liveness reward payout, sponsored transactions, and the future-proofed signed timestamp field — then bring the docs and schemas in line.

**Architecture:** Build the primitives bottom-up so each batch ships working, testable software and later batches build on earlier ones. Money never depends on a clock (issuance is per-epoch-event); no reward is a farmable multiplier (`effective_stake x liveness`). `total_supply` is owned by `state.State` (it already owns the coin store and the deletion-burn path); stake lives on `validators.ValidatorInfo` (rides the snapshot's flat validator byte vector); consensus quorum reads capped effective stake from the epoch holder snapshot.

**Tech Stack:** Go 1.26, FlatBuffers (`types/*.fbs` → `internal/types` via `bash types/generate.sh`, requires `flatc`), BLAKE3 (`github.com/zeebo/blake3`), Pebble-backed storage, Rust/WASM system pod (`pods/pod-system`), the existing `CoinStore`/`State`/`ValidatorSet`/`DAG` APIs.

**Spec:** `docs/superpowers/specs/2026-05-31-economic-layer-design.md` (one design of record; all 11 sections + Parameters + Doc impact are covered here).

**Branch:** `economic-layer` (already checked out).

---

## Execution model (batches)

This is one plan. Work is grouped into **batches**; each batch is a coherent, independently-pushable unit dispatched to a single implementation subagent.

- A batch has 1–10 tasks. **One task = one commit.** **Push after each batch.**
- Batches are ordered: a later batch may use symbols defined by an earlier one. Do not reorder.
- Test discipline: run unit tests for the touched package with a bounded timeout. Run integration sims (`TestSim*`) **individually** with `-timeout` (per project memory: never the whole suite unbounded).
- After a schema change (`types/*.fbs`), regenerate with `bash types/generate.sh` and rebuild before testing.

| Batch | Subsystem | Spec § | Tasks |
|---|---|---|---|
| 1 | Genesis as state; close the fee-less hole | 11, 5 | 7 |
| 2 | `total_supply` accounting; remove the scarcity burn | 7, 4 | 6 |
| 3 | Stake on `ValidatorInfo`; `total_bonded`; jailing | 2, 1 | 7 |
| 4 | Delegation (positions, commission, epoch-boundary split) | 2 | 7 |
| 5 | Stake-weighted capped quorum; dual security model | 1 | 6 |
| 6 | The thermostat (per-epoch adaptive issuance) | 3 | 6 |
| 7 | Reward distribution (`effective_stake x liveness`, payout) | 6, 5.2 | 5 |
| 8 | Sponsored transactions | 9 | 7 |
| 9 | `Vertex.timestamp` field (pipeline deferred) | 8 | 4 |
| 10 | Docs and schema comments | Doc impact | 4 |

---

## File map (created / modified across the plan)

- `internal/genesis/ids.go` (new) — deterministic genesis object IDs.
- `internal/genesis/state.go` (new) — `BuildInitialState` / `InitialState`.
- `internal/genesis/genesis.go` — drop `BuildTransactions` (Batch 1); add `BuildTransferTx` (Batch 1).
- `internal/consensus/dag.go` — `WithGenesisState`, drop `WithGenesisTxs`; new fields (supply seed, stake/thermostat params).
- `internal/consensus/coins.go` — extend `CoinStore` with supply methods (Batch 2).
- `internal/consensus/supply.go` (new) — supply helpers on the DAG side (Batch 2/6).
- `internal/state/state.go` — own `total_supply`, decrement on deletion burn (Batch 2).
- `internal/consensus/fees.go` — `BurnBPS=0` (Batch 2); thermostat math lives in `thermostat.go`.
- `internal/consensus/stake.go` (new) — effective stake, capped voting weight, `total_bonded` (Batch 3/5).
- `internal/validators/validators.go` — stake/jail fields + mutators (Batch 3).
- `internal/sync/snapshot.go` — encode/decode stake + supply; bump `snapshotVersion` (Batch 2/3).
- `internal/consensus/delegation.go` (new) — delegation positions + split (Batch 4).
- `internal/consensus/commit.go` — `handleBond`/`handleUnbond`/`handleDelegate`/`handleUndelegate`; gas-coin owner==fee_payer (Batch 3/4/8); close fee-less hole (Batch 1).
- `internal/consensus/epoch.go` — capped snapshot weight, reward payout, thermostat call (Batch 3/5/6/7).
- `internal/consensus/thermostat.go` (new) — issuance control loop (Batch 6).
- `pods/pod-system/src/lib.rs` — `bond`/`unbond`/`delegate`/`undelegate` entries (Batch 3/4).
- `types/transaction.fbs` — `fee_payer`, `sponsor_signature`, `valid_until` (Batch 8).
- `types/vertex.fbs` — `timestamp` (Batch 9).
- `cmd/node/init.go`, `cmd/node/clienthandlers.go` — genesis + faucet wiring (Batch 1).
- `docs/WHITEPAPER.md`, `docs/VISION.md` (Batch 10).

---

# Batch 1 — Genesis as state; close the fee-less hole

**Spec:** §11 (genesis and fee integrity), §5 (fees). Replace the two genesis transactions with directly-seeded initial state, then require a funded gas coin on every user transaction.

**Context:** Today `genesis.BuildTransactions` returns `[mintTx, registerTx]`; `cmd/node/init.go buildConsensusOpts` injects them via `consensus.WithGenesisTxs` into `d.pendingTxs`; they commit through the normal path to create the initial coin and register the founding validator. They carry no gas coin, and `deductFees` (`commit.go:399-403`) treats "no gas coin" as "proceed for free". The Coin codec lives in `internal/consensus/coins.go` (content = 8-byte LE balance, singleton `replication=0`). `state.State` is the `CoinStore` (`SetFeeSystem(n.state, ...)` in `cmd/node/aggregation.go:99`).

### Task 1.1: Deterministic genesis object IDs

**Files:** Create `internal/genesis/ids.go`; Test `internal/genesis/ids_test.go`.

- [ ] **Test** (`ids_test.go`):

```go
package genesis

import "testing"

func TestGenesisCoinID_Deterministic(t *testing.T) {
	var owner [32]byte
	owner[0] = 1

	if GenesisCoinID(owner) != GenesisCoinID(owner) {
		t.Fatal("GenesisCoinID not deterministic")
	}

	var other [32]byte
	other[0] = 2
	if GenesisCoinID(other) == GenesisCoinID(owner) {
		t.Fatal("different owners must yield different coin IDs")
	}
}
```

- [ ] **Run, expect FAIL:** `go test ./internal/genesis/ -run TestGenesisCoinID_Deterministic` → undefined `GenesisCoinID`.
- [ ] **Implement** (`ids.go`):

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

### Task 1.2: Build the initial state as data

**Files:** Create `internal/genesis/state.go`; Test `internal/genesis/state_test.go`.

- [ ] **Test:**

```go
package genesis

import (
	"crypto/ed25519"
	"encoding/binary"
	"testing"

	"BluePods/internal/types"
)

func TestBuildInitialState_Coin(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(nil)
	var owner [32]byte
	copy(owner[:], pub)

	st := BuildInitialState(Config{InitialMint: 1234, QUICAddress: "127.0.0.1:9000", BLSPubkey: make([]byte, 48)}, owner)

	obj := types.GetRootAsObject(st.Coin, 0)
	if got := binary.LittleEndian.Uint64(obj.ContentBytes()[:8]); got != 1234 {
		t.Fatalf("coin balance = %d, want 1234", got)
	}
	if obj.Replication() != 0 {
		t.Fatalf("coin must be a singleton, got replication %d", obj.Replication())
	}
	if string(obj.OwnerBytes()) != string(owner[:]) {
		t.Fatal("coin owner mismatch")
	}
	if st.Supply != 1234 {
		t.Fatalf("supply = %d, want 1234", st.Supply)
	}
}
```

- [ ] **Run, expect FAIL** → undefined `BuildInitialState` / `InitialState`.
- [ ] **Implement** (`state.go`):

```go
package genesis

import (
	"encoding/binary"

	flatbuffers "github.com/google/flatbuffers/go"

	"BluePods/internal/types"
)

// InitialState is the genesis ledger state, seeded directly (no transactions).
type InitialState struct {
	Coin   []byte   // Coin is the serialized initial Coin object (a singleton).
	CoinID [32]byte // CoinID is its deterministic object ID.
	Pubkey [32]byte // Pubkey is the founding validator's Ed25519 key.
	QUIC   string   // QUIC is the founding validator's QUIC address.
	BLS    []byte   // BLS is the founding validator's 48-byte BLS key.
	Supply uint64   // Supply is the initial total supply (== InitialMint).
}

// BuildInitialState constructs the genesis state for the bootstrap owner.
func BuildInitialState(cfg Config, owner [32]byte) InitialState {
	coinID := GenesisCoinID(owner)

	content := make([]byte, 8)
	binary.LittleEndian.PutUint64(content, cfg.InitialMint)

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
		Coin:   b.FinishedBytes(),
		CoinID: coinID,
		Pubkey: owner,
		QUIC:   cfg.QUICAddress,
		BLS:    cfg.BLSPubkey,
		Supply: cfg.InitialMint,
	}
}
```

- [ ] **Run, expect PASS.**
- [ ] **Commit:** `git add internal/genesis/state.go internal/genesis/state_test.go && git commit -m "[+] BuildInitialState: genesis coin + validator as seeded data"`

### Task 1.3: Seed state and validator at DAG construction

**Files:** Modify `internal/consensus/dag.go` (add `genesisSupply uint64` field + `WithGenesisState` Option); Test `internal/consensus/genesis_state_test.go`.

- [ ] **Test:** build a DAG with a stub `CoinStore` and empty `ValidatorSet`, apply `WithGenesisState(is)`, assert: `d.coinStore.GetObject(is.CoinID)` non-nil with balance `is.Supply`; `d.validators.Get(is.Pubkey)` present; `d.genesisSupply == is.Supply`; `len(d.pendingTxs) == 0`. (Reuse the existing fee-test stub `CoinStore` and DAG test constructor in `internal/consensus/*_test.go`.)
- [ ] **Run, expect FAIL** → undefined `WithGenesisState`.
- [ ] **Implement** in `dag.go` (add the field to the `DAG` struct near `pendingTxs`, add the import `"BluePods/internal/genesis"`):

```go
// genesisSupply holds the initial total supply seeded at genesis. Batch 2 routes
// this into the persisted state.State counter; kept as a field so this batch ships alone.
genesisSupply uint64
```

```go
// WithGenesisState seeds the initial ledger state directly: the genesis coin
// object into the coin store, the founding validator into the validator set,
// and the initial total supply. Genesis is state, not transactions, so no
// genesis transaction enters the DAG.
func WithGenesisState(is genesis.InitialState) Option {
	return func(d *DAG) {
		d.coinStore.SetObject(is.Coin)

		var bls [48]byte
		copy(bls[:], is.BLS)
		d.validators.Add(is.Pubkey, is.QUIC, bls)

		d.genesisSupply = is.Supply
	}
}
```

- [ ] **Run, expect PASS.**
- [ ] **Commit:** `git add internal/consensus/dag.go internal/consensus/genesis_state_test.go && git commit -m "[+] WithGenesisState seeds coin + validator at DAG construction"`

### Task 1.4: Switch bootstrap to seeded state; remove genesis transactions

**Files:** Modify `cmd/node/init.go` (`buildConsensusOpts`); Modify `internal/genesis/genesis.go` (delete `BuildTransactions`); Modify `internal/consensus/dag.go` (delete `WithGenesisTxs`).

- [ ] **Rewire** `buildConsensusOpts`: derive `owner` from `n.cfg.PrivateKey.Public().(ed25519.PublicKey)`, call `is := genesis.BuildInitialState(genesisCfg, owner)`, and replace `consensus.WithGenesisTxs(txs)` with `consensus.WithGenesisState(is)`. Drop the `genesis.BuildTransactions(genesisCfg)` call and its error handling.
- [ ] **Build:** `go build ./...` → `BuildTransactions` / `WithGenesisTxs` now unused.
- [ ] **Delete dead code:** remove `genesis.BuildTransactions` (`genesis.go:26-53`) and `consensus.WithGenesisTxs` (`dag.go:131-137`). Keep `BuildMintTx` (faucet uses it until Task 1.6).
- [ ] **Run the bootstrap sim:** `go test ./test/integration/ -run TestSimBootstrap -count=1 -timeout 5m`. Expect PASS (node bootstraps; genesis coin exists as seeded state). If a sub-case asserted a genesis *transaction* committed, change it to assert seeded state.
- [ ] **Commit:** `git add -A && git commit -m "[&] Bootstrap from seeded genesis state; remove genesis transactions"`

### Task 1.5: Close the fee-less hole in deductFees

**Files:** Modify `internal/consensus/commit.go` (`deductFees`, ~399-403); Test `internal/consensus/commit_test.go`.

- [ ] **Test** `TestDeductFees_RejectsMissingGasCoin`: a tx whose `GasCoinBytes()` length != 32 must return `proceed == false`. (Reuse the fee-test DAG helper.)
- [ ] **Run, expect FAIL** (currently `proceed == true`).
- [ ] **Implement** — change the no-gas-coin branch in `deductFees`:

```go
// No gas coin: reject. Genesis is seeded state (not a transaction) and protocol
// actions (issuance, reward crediting, slashing) are not transactions, so every
// user transaction must reference a funded gas coin.
gasCoinBytes := tx.GasCoinBytes()
if len(gasCoinBytes) != 32 {
	return FeeSplit{}, false
}
```

- [ ] **Run** `go test ./internal/consensus/ -count=1 -timeout 120s`. Fix any unit test that relied on the fee-less path by giving its tx a funded gas coin.
- [ ] **Commit:** `git add internal/consensus/commit.go internal/consensus/commit_test.go && git commit -m "[!] Reject transactions without a funded gas coin (close fee-less hole)"`

### Task 1.6: Faucet transfers from the genesis reserve

**Files:** Modify `internal/genesis/transaction.go` (add `BuildTransferTx`); Modify `cmd/node/clienthandlers.go` (`buildFaucetTx`); Test `internal/genesis/transfer_test.go`.

The faucet currently calls `genesis.BuildMintTx` (a fee-less mint). With the hole closed it must be a normal, fee-paying transfer from the genesis-seeded bootstrap coin (the reserve) to the requester.

- [ ] **Test** `TestBuildTransferTx_HasGasCoin`: `BuildTransferTx(privKey, systemPod, gasCoinID, toOwner, amount)` builds a `transfer` system-pod ATX whose inner tx `GasCoinBytes()` length is 32 and `FunctionName()` is `"transfer"`.
- [ ] **Run, expect FAIL** → undefined `BuildTransferTx`.
- [ ] **Implement** `BuildTransferTx` (mirror `BuildMintTx`: encode the system pod's `transfer` args for `(toOwner, amount)`, pass `gasCoinID[:]` as the `gasCoin` argument to `BuildAttestedTx`, and reference the reserve coin as a mutable ref so the system pod debits it). Rewire `buildFaucetTx` to call it with the bootstrap reserve coin `genesis.GenesisCoinID(bootstrapOwner)` paying its own gas, and reject faucet requests once the reserve is exhausted (transfer fails on insufficient balance).
- [ ] **Verify:** `go test ./internal/genesis/ -count=1` and `go test ./test/integration/ -run TestSimConsensus -count=1 -timeout 5m`. Expect PASS.
- [ ] **Commit:** `git add -A && git commit -m "[&] Faucet transfers from the genesis reserve (no fee-less mint)"`

### Task 1.7: Verify Batch 1 end to end, then push

- [ ] **Unit gate:** `go test ./internal/... ./client/... -count=1 -timeout 180s`. Expect PASS.
- [ ] **Sims:** `go test ./test/integration/ -run TestSimBootstrap -count=1 -timeout 5m`; `go test ./test/integration/ -run TestSimConsensus -count=1 -timeout 5m`. Expect PASS.
- [ ] **Push:** `git push -u origin economic-layer`

---

# Batch 2 — `total_supply` accounting; remove the scarcity burn

**Spec:** §7 (supply/bonded tracking, the invariant), §4 (no scarcity burn). `state.State` owns the coin store and the deletion-burn path (`applyDeletedObjects`, refund 95% / burn 5%, `state.go:533-584`), so `total_supply` lives there. The DAG reaches it through the `CoinStore` interface (same concrete `*state.State`).

### Task 2.1: total_supply counter in state.State

**Files:** Modify `internal/state/state.go`; Test `internal/state/supply_test.go`.

- [ ] **Test:** a fresh `State` has `TotalSupply()==0`; `AddSupply(100)` → 100; `SubSupply(30)` → 70; `SubSupply(1000)` floors at 0; `SetTotalSupply(500)` → 500; after a reopen on the same storage the value persists.
- [ ] **Run, expect FAIL** → undefined methods.
- [ ] **Implement:** add `totalSupply uint64` field (guarded by the existing state mutex), a storage key `var prefixSupply = []byte("m:supply")` (8-byte BE value), load it in the `State` constructor, and:

```go
// TotalSupply returns the current total token supply.
func (s *State) TotalSupply() uint64 { /* lock; return s.totalSupply */ }

// SetTotalSupply sets the supply (genesis seeding) and persists it.
func (s *State) SetTotalSupply(v uint64) { /* lock; s.totalSupply = v; persist */ }

// AddSupply increases total supply (mint, issuance) and persists it.
func (s *State) AddSupply(amount uint64) { /* lock; saturating add; persist */ }

// SubSupply decreases total supply (burn, slashing), flooring at 0, and persists it.
func (s *State) SubSupply(amount uint64) { /* lock; floor-0 sub; persist */ }
```

Use a single unexported `persistSupplyLocked()` writing `prefixSupply` via the storage handle the `State` already holds.

- [ ] **Run, expect PASS.**
- [ ] **Commit:** `git add internal/state/state.go internal/state/supply_test.go && git commit -m "[+] total_supply counter persisted in state.State"`

### Task 2.2: Expose supply on the CoinStore interface; seed at genesis

**Files:** Modify `internal/consensus/coins.go` (extend `CoinStore`); Modify `internal/consensus/dag.go` (`WithGenesisState` seeds the real counter); update the consensus test stub `CoinStore`.

- [ ] **Test:** extend the genesis-state test from Task 1.3 to assert `d.coinStore.TotalSupply() == is.Supply` after `WithGenesisState`.
- [ ] **Run, expect FAIL.**
- [ ] **Implement:** add to the `CoinStore` interface:

```go
// TotalSupply returns the current total token supply.
TotalSupply() uint64
// SetTotalSupply seeds the supply at genesis.
SetTotalSupply(v uint64)
// AddSupply increases supply (issuance/mint).
AddSupply(amount uint64)
// SubSupply decreases supply (burn/slashing), flooring at 0.
SubSupply(amount uint64)
```

In `WithGenesisState`, replace `d.genesisSupply = is.Supply` with `d.coinStore.SetTotalSupply(is.Supply)` and delete the `genesisSupply` field. Add the four methods to every test stub `CoinStore` in `internal/consensus/*_test.go`.

- [ ] **Run, expect PASS;** `go build ./...`.
- [ ] **Commit:** `git add -A && git commit -m "[&] Expose total_supply via CoinStore; seed it at genesis"`

### Task 2.3: Decrement supply on the deletion burn

**Files:** Modify `internal/state/state.go` (`applyDeletedObjects`); Test `internal/state/supply_test.go`.

- [ ] **Test:** seed supply 10000, delete an object with `fees=1000` and `storageRefundBPS=9500`: the owner is refunded 950 and `TotalSupply()` drops by the 50 burned → 9950.
- [ ] **Run, expect FAIL.**
- [ ] **Implement:** in `applyDeletedObjects`, where the refund is computed (`refund := objFees * s.storageRefundBPS / 10000`), compute `burned := objFees - refund` and call `s.SubSupply(burned)` (the locked deposit was part of supply; refund returns 95% to circulation, 5% is destroyed).
- [ ] **Run, expect PASS.**
- [ ] **Commit:** `git add internal/state/state.go internal/state/supply_test.go && git commit -m "[&] Decrement total_supply by the 5% deletion burn"`

### Task 2.4: Remove the scarcity fee burn (100/0)

**Files:** Modify `internal/consensus/fees.go` (`DefaultFeeParams`, doc comments); Test `internal/consensus/fees_test.go`.

- [ ] **Test** `TestSplitFee_NoScarcityBurn`: with `DefaultFeeParams()`, `SplitFee(1000, params)` returns `Burned==0` and `Epoch==1000`.
- [ ] **Run, expect FAIL** (currently 30% burned).
- [ ] **Implement:** in `DefaultFeeParams`, set `BurnBPS: 0` and `EpochBPS: 10000`. Update the `BurnBPS`/`EpochBPS` field comments and the `FeeSplit.Burned` comment to note the scarcity burn is removed (100% of consumed fees go to the epoch pool). `validateFeeSummary` and `buildFeeSummary` already use `SplitFee`, so `total_burned` automatically becomes 0 and stays consistent.
- [ ] **Run** `go test ./internal/consensus/ -count=1 -timeout 120s`.
- [ ] **Commit:** `git add internal/consensus/fees.go internal/consensus/fees_test.go && git commit -m "[&] Remove the scarcity fee burn: 100% of fees to validators"`

### Task 2.5: Persist total_supply in the snapshot

**Files:** Modify `types/snapshot.fbs` (add `total_supply:uint64`); regenerate; Modify `internal/sync/snapshot.go` (carry it through `CreateSnapshot`/checksum/`ApplySnapshot`); Modify the sync wiring so a synced node restores it into `state.State`. Test `internal/sync/snapshot_test.go`.

- [ ] **Schema:** add to the `Snapshot` table: `// total_supply is the protocol token supply at this snapshot.\n    total_supply:uint64;`. Bump `snapshotVersion` (in `snapshot.go`) to 7.
- [ ] **Regenerate:** `bash types/generate.sh && go build ./...`.
- [ ] **Test:** round-trip a snapshot with `total_supply=12345` through build → `ApplySnapshot`, asserting the value survives and is included in the checksum (a tampered supply fails `verifyChecksum`).
- [ ] **Run, expect FAIL.**
- [ ] **Implement:** thread `totalSupply uint64` into `CreateSnapshot`/`buildSnapshot` (its caller passes `state.TotalSupply()`), add it to `computeChecksumWithInfo` (write the 8 BE bytes), set `types.SnapshotAddTotalSupply`, read it back in `ApplySnapshot`, and have the apply path call `state.SetTotalSupply(snapshot.TotalSupply())`. Update the `CreateSnapshot` call site to pass the supply.
- [ ] **Run** `go test ./internal/sync/ -count=1 -timeout 120s`.
- [ ] **Commit:** `git add -A && git commit -m "[&] Persist total_supply in snapshots (version 7)"`

### Task 2.6: Supply invariant property test; verify; push

**Files:** Test `internal/consensus/supply_invariant_test.go`.

- [ ] **Test** `TestSupplyInvariant`: drive a small sequence (genesis seed, a fee-paying transfer, an object create then delete) through a test DAG/state and assert `sum(coin balances) + total_bonded + sum(locked storage deposits) == total_supply` holds after every commit. (`total_bonded` is 0 until Batch 3; the helper that computes it returns 0 here.) Iterate coin balances via the state object store.
- [ ] **Run, expect PASS.**
- [ ] **Commit:** `git add internal/consensus/supply_invariant_test.go && git commit -m "[+] Property test: supply invariant after every commit"`
- [ ] **Verify + push:** `go test ./internal/... -count=1 -timeout 180s`; `go test ./test/integration/ -run TestSimConsensus -count=1 -timeout 5m`; then `git push`.

---

# Batch 3 — Stake on ValidatorInfo; total_bonded; jailing

**Spec:** §2 (bonding, jailing), §1 (effective stake). Stake is a field on `ValidatorInfo` (not a flag on coins); it rides the snapshot's flat validator byte vector (`encodeValidators`/`decodeValidators`), so no `.fbs` change for validators. Bonding/unbonding mirror `register_validator`: recognized Go-side in the commit path, debiting/crediting a coin and mutating stake.

### Task 3.1: Stake and jail fields on ValidatorInfo

**Files:** Modify `internal/validators/validators.go`; Test `internal/validators/validators_test.go`.

- [ ] **Test:** new `ValidatorInfo` zero value has `SelfStake==0`, `DelegatedTotal==0`, `Jailed==false`; `Get`/`All` return copies that include these fields.
- [ ] **Run, expect FAIL.**
- [ ] **Implement:** add fields with docs:

```go
SelfStake      uint64 // SelfStake is the validator's own bonded stake.
DelegatedTotal uint64 // DelegatedTotal is the sum of stake delegated to this validator.
Jailed         bool   // Jailed is true when the validator is jailed (zero voting weight, no reward accrual).
```

Carry all three in the copy constructors inside `Get` and `All`.

- [ ] **Run, expect PASS.**
- [ ] **Commit:** `git add internal/validators/ && git commit -m "[+] Stake and jail fields on ValidatorInfo"`

### Task 3.2: Stake/jail mutators on ValidatorSet

**Files:** Modify `internal/validators/validators.go`; Test `internal/validators/validators_test.go`.

- [ ] **Test:** `SetSelfStake(pk, 100)` then `Get(pk).SelfStake==100`; `AddDelegated(pk,50)`/`SubDelegated(pk,20)` → `DelegatedTotal==30` (floor 0); `Jail(pk)`/`Unjail(pk)` toggle `Jailed`; each returns false for an unknown pubkey.
- [ ] **Run, expect FAIL.**
- [ ] **Implement** (under the write lock, mutating the stored `*ValidatorInfo`):

```go
// SetSelfStake sets a validator's self-stake. Returns false if not found.
func (vs *ValidatorSet) SetSelfStake(pubkey Hash, stake uint64) bool { /* ... */ }

// AddDelegated increases a validator's delegated total. Returns false if not found.
func (vs *ValidatorSet) AddDelegated(pubkey Hash, amount uint64) bool { /* ... */ }

// SubDelegated decreases a validator's delegated total, flooring at 0. Returns false if not found.
func (vs *ValidatorSet) SubDelegated(pubkey Hash, amount uint64) bool { /* ... */ }

// Jail marks a validator jailed. Returns false if not found.
func (vs *ValidatorSet) Jail(pubkey Hash) bool { /* ... */ }

// Unjail clears the jailed flag. Returns false if not found.
func (vs *ValidatorSet) Unjail(pubkey Hash) bool { /* ... */ }
```

- [ ] **Run, expect PASS.**
- [ ] **Commit:** `git add internal/validators/ && git commit -m "[+] Stake and jail mutators on ValidatorSet"`

### Task 3.3: Persist stake in the snapshot encoder

**Files:** Modify `internal/sync/snapshot.go` (`encodeValidators`/`decodeValidators`); bump `snapshotVersion` to 8; Test `internal/sync/snapshot_test.go`.

- [ ] **Test:** encode validators with `SelfStake`/`DelegatedTotal`/`Jailed` set, decode, assert the values survive; a snapshot round-trip preserves them and the checksum covers them.
- [ ] **Run, expect FAIL.**
- [ ] **Implement:** extend the per-validator record to append `SelfStake` (8 BE), `DelegatedTotal` (8 BE), and `Jailed` (1 byte) after the existing `pubkey | u16 quic_len | quic | bls(48)`. Update `decodeValidators` to read them (guarding lengths, mirroring the existing break-on-short pattern). Bump `snapshotVersion` to 8. `computeChecksumWithInfo` already hashes `encodeValidators(validators)`, so stake is covered automatically.
- [ ] **Run** `go test ./internal/sync/ -count=1 -timeout 120s`.
- [ ] **Commit:** `git add internal/sync/snapshot.go internal/sync/snapshot_test.go && git commit -m "[&] Persist validator stake and jail flag in snapshots (version 8)"`

### Task 3.4: Effective stake and total_bonded helpers

**Files:** Create `internal/consensus/stake.go`; Test `internal/consensus/stake_test.go`.

- [ ] **Test:** `EffectiveStake(&ValidatorInfo{SelfStake:100, DelegatedTotal:50})==150`; a jailed validator's effective stake is 0; `d.totalBonded()` sums `EffectiveStake` over the active set, skipping jailed validators.
- [ ] **Run, expect FAIL.**
- [ ] **Implement:**

```go
// EffectiveStake returns a validator's consensus/reward weight: self-stake plus
// delegated stake. A jailed validator contributes zero.
func EffectiveStake(v *ValidatorInfo) uint64 {
	if v == nil || v.Jailed {
		return 0
	}
	return v.SelfStake + v.DelegatedTotal
}

// totalBonded sums effective stake over the active validator set (O(validators)).
func (d *DAG) totalBonded() uint64 {
	var total uint64
	for _, v := range d.validators.All() {
		total += EffectiveStake(v)
	}
	return total
}
```

- [ ] **Run, expect PASS.**
- [ ] **Commit:** `git add internal/consensus/stake.go internal/consensus/stake_test.go && git commit -m "[+] EffectiveStake and total_bonded derivation"`

### Task 3.5: bond / unbond system-pod entries (Rust)

**Files:** Modify `pods/pod-system/src/lib.rs`; build the system pod.

- [ ] **Read** the existing `register_validator` entry in `pods/pod-system/src/lib.rs` to mirror its dispatch shape.
- [ ] **Implement** `bond` and `unbond` functions: `bond` takes an amount and validates the sender owns the referenced coin (the actual stake mutation is applied Go-side in Task 3.6, mirroring how `register_validator` adds to the validator set Go-side); `unbond` takes an amount and starts the unbonding delay. Keep them minimal — they exist so the function name dispatches and the tx is well-formed.
- [ ] **Build:** the project's pod build step (e.g. `cargo build --target wasm32-unknown-unknown` in `pods/pod-system`, then the gas-instrumentation step the repo uses). Confirm the system-pod WASM rebuilds.
- [ ] **Commit:** `git add pods/pod-system/ && git commit -m "[+] System pod: bond / unbond entries"`

### Task 3.6: Go-side bond/unbond handling + min stake + jailing weight

**Files:** Modify `internal/consensus/commit.go` (add `handleBond`/`handleUnbond`, call them in `executeTx` next to `handleRegisterValidator`); add a `minStake` field + `WithMinStake` Option in `dag.go`; Modify `internal/consensus/epoch.go` (zero jailed weight in `snapshotEpochHolders`). Test `internal/consensus/bond_test.go`.

- [ ] **Test:** a `bond` tx from a registered validator referencing its coin debits the coin by the amount and sets `SelfStake`; a `bond` below `minStake` (when `SelfStake` would stay under the bar) is rejected; an `unbond` reduces `SelfStake`; a jailed validator is excluded from the epoch holder snapshot's effective weight.
- [ ] **Run, expect FAIL.**
- [ ] **Implement:** `handleBond` mirrors `handleRegisterValidator` (guard `isBondTx` on system pod + function name `"bond"`); read the amount from args; debit the sender's coin via `deductCoinFee(d.coinStore, coinID, amount)` (full-cover required) and `d.validators.SetSelfStake(sender, existing+amount)`; enforce `minStake` at register/bond time. `handleUnbond` reduces self-stake (the coin credit-back after the unbonding delay is wired with delegation in Batch 4 / slashing later; for now reduce stake and credit immediately, leaving a `// TODO: enforce unbonding delay (Batch 10/slashing branch)`). In `snapshotEpochHolders`, when copying validators into `epochHolders`, carry stake but set effective weight to 0 for jailed validators (the snapshot copies `SelfStake`/`DelegatedTotal`; jailing is read via `EffectiveStake`, so ensure jailed validators are copied with `Jailed=true`). Add `WithMinStake(uint64) Option`.
- [ ] **Run** `go test ./internal/consensus/ -count=1 -timeout 120s`.
- [ ] **Commit:** `git add -A && git commit -m "[+] Go-side bond/unbond, minimum stake, jailing zeroes weight"`

### Task 3.7: Verify Batch 3; push

- [ ] **Verify:** `go test ./internal/... -count=1 -timeout 180s`; `go test ./test/integration/ -run TestSimBootstrap -count=1 -timeout 5m`.
- [ ] **Push:** `git push`

---

# Batch 4 — Delegation

**Spec:** §2 (delegation). Each delegation is a stake-position object owned by the delegator `(validator, amount)`; each validator carries a maintained `DelegatedTotal` (Batch 3). Fixed commission (governed parameter). Rewards use a simple epoch-boundary proportional split at the boundary where `distributeEpochRewards` already runs. Delegations take effect at the next epoch boundary (mirroring validator-set churn deferral).

### Task 4.1: Delegation-position object codec

**Files:** Create `internal/consensus/delegation.go`; Test `internal/consensus/delegation_test.go`.

- [ ] **Test:** `encodeDelegation(validator, amount)` → bytes; `decodeDelegation` round-trips `(validator [32]byte, amount uint64)`; a deterministic `DelegationID(delegator, validator)` is stable and distinct per pair.
- [ ] **Run, expect FAIL.**
- [ ] **Implement:** a delegation position is an `Object` whose `owner` is the delegator and whose `content` is `validator(32) || amount(8 LE)`. Provide `DelegationID(delegator, validator [32]byte) [32]byte` (BLAKE3 of `"bluepods/delegation/v1" || delegator || validator`), `encodeDelegationContent(validator [32]byte, amount uint64) []byte`, and `decodeDelegationContent([]byte) (validator [32]byte, amount uint64, err error)`.
- [ ] **Run, expect PASS.**
- [ ] **Commit:** `git add internal/consensus/delegation.go internal/consensus/delegation_test.go && git commit -m "[+] Delegation-position object codec"`

### Task 4.2: delegate / undelegate system-pod entries (Rust)

**Files:** Modify `pods/pod-system/src/lib.rs`; build.

- [ ] **Implement** `delegate(validator, amount)` and `undelegate(validator)` mirroring `bond`/`unbond` (minimal; the Go side applies the position + `DelegatedTotal` change in Task 4.3).
- [ ] **Build** the system pod.
- [ ] **Commit:** `git add pods/pod-system/ && git commit -m "[+] System pod: delegate / undelegate entries"`

### Task 4.3: Go-side delegate/undelegate

**Files:** Modify `internal/consensus/commit.go` (`handleDelegate`/`handleUndelegate`); Test `internal/consensus/delegation_test.go`.

- [ ] **Test:** a `delegate` tx debits the delegator's coin, creates the delegation position object (owner = delegator), and increases the target validator's `DelegatedTotal`; `undelegate` removes the position and decreases `DelegatedTotal`; delegating to an unknown validator is rejected.
- [ ] **Run, expect FAIL.**
- [ ] **Implement** `handleDelegate`/`handleUndelegate` mirroring `handleBond`: validate the target is a known validator; debit/credit the delegator's coin; write/delete the delegation object via `d.coinStore.SetObject`; `d.validators.AddDelegated`/`SubDelegated`. Like validator churn, the effective-weight change applies from the next epoch boundary (the snapshot already freezes per epoch).
- [ ] **Run, expect PASS.**
- [ ] **Commit:** `git add internal/consensus/commit.go internal/consensus/delegation_test.go && git commit -m "[+] Go-side delegate/undelegate updating delegated_total"`

### Task 4.4: Fixed commission parameter

**Files:** Modify `internal/consensus/dag.go` (add `commissionBPS uint64` + `WithCommissionBPS` Option, default ~1000 = 10%); Test inline in Task 4.5.

- [ ] **Implement** the field and Option with a doc comment (governed fixed commission, not per-validator; default 1000 BPS).
- [ ] **Build** `go build ./...`.
- [ ] **Commit:** `git add internal/consensus/dag.go && git commit -m "[+] Fixed delegation commission parameter"`

### Task 4.5: Epoch-boundary proportional reward split

**Files:** Create the split helper in `internal/consensus/delegation.go`; Test `internal/consensus/delegation_test.go`.

- [ ] **Test** `TestSplitValidatorReward`: given a validator reward of 1000, commission 1000 BPS (10%), and two delegations of amounts 100 and 300 against `SelfStake=600` (so total effective = 1000), the validator keeps its self-stake share + commission, and each delegation receives its pro-rata share of the post-commission remainder. Assert exact integer amounts and that the sum of all credited shares equals the input reward (remainder to the validator).
- [ ] **Run, expect FAIL.**
- [ ] **Implement** a pure function:

```go
// delegatorShare is one delegator's slice of an epoch reward.
type delegatorShare struct {
	Delegator [32]byte // Delegator is the position owner credited.
	Amount    uint64   // Amount is the tokens credited to this delegator.
}

// splitValidatorReward divides a validator's epoch reward between the validator
// and its delegators. The validator keeps the reward on its self-stake plus a
// fixed commission on the delegated portion; the rest is split among delegations
// pro-rata to amount. Integer math; any rounding remainder goes to the validator.
func splitValidatorReward(reward, selfStake, commissionBPS uint64, dels []delegatorShare) (validatorAmount uint64, delegatorAmounts []delegatorShare) { /* ... */ }
```

(`dels` carries each delegation's amount in `.Amount` on input; the function returns the credited amounts.)

- [ ] **Run, expect PASS.**
- [ ] **Commit:** `git add internal/consensus/delegation.go internal/consensus/delegation_test.go && git commit -m "[+] Epoch-boundary proportional reward split (fixed commission)"`

### Task 4.6: Enumerate a validator's delegations at the boundary

**Files:** Modify `internal/consensus/delegation.go` (a helper to list delegation positions for a validator); Test `internal/consensus/delegation_test.go`.

- [ ] **Test:** after two `delegate` txs to validator V, the enumerator returns both positions `(delegator, amount)` for V.
- [ ] **Run, expect FAIL.**
- [ ] **Implement** an iterator over the state object store filtering delegation-position objects whose decoded `validator == V`. Expose it via the state object store the DAG already holds (add a minimal `IterateObjects(func(id [32]byte, data []byte))` to the `CoinStore` interface if not present, backed by `state.State`'s existing `db.Iterate`; only add it if no equivalent exists). Keep it O(objects) — acceptable at launch scale; `// TODO: index delegations per validator when count grows`.
- [ ] **Run, expect PASS.**
- [ ] **Commit:** `git add -A && git commit -m "[+] Enumerate delegation positions per validator"`

### Task 4.7: Verify Batch 4; push

- [ ] **Verify:** `go test ./internal/... -count=1 -timeout 180s`.
- [ ] **Push:** `git push`

---

# Batch 5 — Stake-weighted capped quorum; dual security model

**Spec:** §1. Voting weight is capped effective stake; quorum is exact integer arithmetic (`3 x cappedSum >= 2 x total`) read from the epoch holder snapshot. The three counting sites — `ValidatorSet.QuorumSize`, `isRoundCommitted`'s distinct-producer count, `validateParentsQuorum` — plus the relaxed bootstrap literal move to capped-stake sums. Per-object attestation stays equal-weight (untouched).

### Task 5.1: Capped voting weight (pure integer math)

**Files:** Modify `internal/consensus/stake.go`; Test `internal/consensus/stake_test.go`.

- [ ] **Test** `TestCappedWeight`: with a cap of 100 (per-mille of total, say 10% expressed as 100‰), a validator at 50% of total is capped to 10%; below the cap it is unchanged; with 3 validators a 10% cap is widened so a 2/3 quorum stays reachable (the cap is `max(perValidatorCapMille * total / 1000, total/len)` — never below an equal share).
- [ ] **Run, expect FAIL.**
- [ ] **Implement:**

```go
// cappedWeight returns a validator's voting weight: its effective stake capped at
// the per-validator ceiling. The ceiling is the larger of the configured fraction
// of total stake and an equal share, so a small set keeps a reachable 2/3 quorum.
func cappedWeight(effective, total uint64, capMille uint64, setSize int) uint64 {
	if setSize <= 0 || total == 0 {
		return effective
	}
	ceiling := total * capMille / 1000
	if equal := total / uint64(setSize); ceiling < equal {
		ceiling = equal
	}
	if effective > ceiling {
		return ceiling
	}
	return effective
}
```

Add `votingCapMille uint64` + `WithVotingCapMille` Option to `dag.go` (default e.g. 100‰ = 10%).

- [ ] **Run, expect PASS.**
- [ ] **Commit:** `git add internal/consensus/stake.go internal/consensus/stake_test.go internal/consensus/dag.go && git commit -m "[+] Capped voting weight (integer, equal-share floor)"`

### Task 5.2: Capped quorum sum over a holder snapshot

**Files:** Modify `internal/consensus/stake.go`; Test `internal/consensus/stake_test.go`.

- [ ] **Test** `TestQuorumReached`: a helper `quorumReached(cappedSum, total)` returns true iff `3*cappedSum >= 2*total`; and `cappedStakeOf(set, producers)` sums `cappedWeight` over the producers present in the set.
- [ ] **Run, expect FAIL.**
- [ ] **Implement:**

```go
// quorumReached reports whether a capped-stake sum meets the 2/3 BFT threshold,
// using exact integer arithmetic (never floating point).
func quorumReached(cappedSum, total uint64) bool { return 3*cappedSum >= 2*total }

// cappedStakeOf sums the capped voting weight of the given producers within a
// holder set. The cap uses the set's uncapped total and size.
func cappedStakeOf(set *ValidatorSet, producers map[Hash]bool) (cappedSum, total uint64) { /* ... */ }
```

- [ ] **Run, expect PASS.**
- [ ] **Commit:** `git add internal/consensus/stake.go internal/consensus/stake_test.go && git commit -m "[+] Capped-stake quorum sum (exact integer 3*sum>=2*total)"`

### Task 5.3: Stake-weight ValidatorSet.QuorumSize callers via the snapshot

**Files:** Modify `internal/consensus/commit.go` (`isRoundCommitted`); Test `internal/consensus/commit_test.go`.

- [ ] **Test:** with two validators holding 90% and 10% stake, a round with only the 10% producer does NOT commit; with the 90% producer it does (quorum is stake-weighted, not count). Use the epoch holder snapshot for the round (`HoldersForEpoch(commitEpochForRound(round))`).
- [ ] **Run, expect FAIL.**
- [ ] **Implement:** in `isRoundCommitted`, replace the distinct-producer count check (`len(producers) >= requiredQuorum`) with a stake-weighted check: select the holder set via `d.HoldersForEpoch(d.commitEpochForRound(round))`, compute `cappedSum, total := cappedStakeOf(set, producers)`, and commit when `quorumReached(cappedSum, total)`. Preserve the relaxed paths (init `< minValidators` and transition) as count-of-1 (a single producer), since stake is not yet meaningful during bootstrap.
- [ ] **Run** `go test ./internal/consensus/ -count=1 -timeout 120s`.
- [ ] **Commit:** `git add internal/consensus/commit.go internal/consensus/commit_test.go && git commit -m "[&] Stake-weight the round commit quorum"`

### Task 5.4: Stake-weight production quorum

**Files:** Modify `internal/consensus/dag.go` (`hasQuorumFromRound`, `QuorumSize` use); Test `internal/consensus/dag_test.go`.

- [ ] **Test:** `hasQuorumFromRound` returns true once the producers at the round carry a 2/3 capped-stake majority of the holder snapshot, not a count majority.
- [ ] **Run, expect FAIL.**
- [ ] **Implement:** replace the `count >= required` logic in `hasQuorumFromRound` with the stake-weighted `cappedStakeOf` + `quorumReached` against the current epoch holder snapshot, keeping the transition/bootstrap relaxations. Leave `ValidatorSet.QuorumSize()` for the logging/relaxed paths but stop using it as the authority for commit/production.
- [ ] **Run, expect PASS.**
- [ ] **Commit:** `git add internal/consensus/dag.go internal/consensus/dag_test.go && git commit -m "[&] Stake-weight the production quorum"`

### Task 5.5: Keep validateParentsQuorum sound under stake-weighting

**Files:** Modify `internal/consensus/validate.go` (`validateParentsQuorum`); Test `internal/consensus/validate_test.go`.

- [ ] **Test:** `validateParentsQuorum` still accepts a vertex whose parents include at least one known validator (the minimal sanity check), unchanged in spirit; add a comment clarifying it is intentionally a presence check, not the stake-weighted quorum (which is enforced at production/commit). Confirm a vertex with zero known parent producers is rejected.
- [ ] **Run** (should mostly pass; adjust the comment + any count assumption).
- [ ] **Implement:** keep the at-least-one-known-parent check; update the docstring to state the authoritative stake-weighted quorum is enforced in `hasQuorumFromRound`/`isRoundCommitted`, and that receiving nodes cannot recompute another node's stake-quorum during convergence.
- [ ] **Commit:** `git add internal/consensus/validate.go internal/consensus/validate_test.go && git commit -m "[&] Clarify validateParentsQuorum under stake-weighting"`

### Task 5.6: Verify Batch 5; push

- [ ] **Verify:** `go test ./internal/consensus/ -count=1 -timeout 180s`; `go test ./test/integration/ -run TestSimConsensus -count=1 -timeout 5m`; `go test ./test/integration/ -run TestSimBootstrap -count=1 -timeout 5m`. Stake is seeded at genesis (the founding validator's self-stake) — if sims have validators with zero stake, give the genesis/registration path a default self-stake so quorum is reachable; otherwise commit stalls. Verify carefully.
- [ ] **Push:** `git push`

---

# Batch 6 — The thermostat (per-epoch adaptive issuance)

**Spec:** §3. Issuance is an adaptive control loop at each epoch boundary, denominated in epoch events (no clock). Target band ~25–35% of `total_supply` (dead-band), bounded `[floor, ceiling]`, capped step, ratio read on PRE-mint supply, mint into the reward pool, auto-restake a fraction. Runs in `transitionEpoch` before reward distribution.

### Task 6.1: Thermostat parameters

**Files:** Create `internal/consensus/thermostat.go`; Modify `dag.go` (params + Option); Test `internal/consensus/thermostat_test.go`.

- [ ] **Implement** a `thermostatParams` struct (all in per-mille / per-epoch integer units): `targetLowMille`, `targetHighMille` (250/350), `floorRateMicro`, `ceilingRateMicro` (per-epoch rate in micro-units approximating ~1%/~20% annual), `genesisRateMicro` (~8–10% annual), `stepCapMicro` (small), `autoRestakeMille` (fraction). Add `WithThermostat(thermostatParams)` and store the current per-epoch rate `d.issuanceRateMicro uint64` (seeded to `genesisRateMicro`). Document each field. Persist `issuanceRateMicro` in the snapshot (extend the snapshot scalar set like `total_supply`; bump `snapshotVersion` to 9) so the loop is continuous across restarts.
- [ ] **Build + commit:** `git add -A && git commit -m "[+] Thermostat parameters and per-epoch rate state"`

### Task 6.2: Ratio and rate-adjustment math (pure)

**Files:** Modify `internal/consensus/thermostat.go`; Test `internal/consensus/thermostat_test.go`.

- [ ] **Test** `TestAdjustRate`: ratio below the band raises the rate by at most `stepCapMicro` (clamped to ceiling); ratio above the band lowers it (clamped to floor); ratio inside the dead-band holds the rate unchanged. `stakingRatioMille(bonded, supply)` returns `bonded*1000/supply` (0 when supply==0).
- [ ] **Run, expect FAIL.**
- [ ] **Implement:**

```go
// stakingRatioMille returns total_bonded / total_supply in per-mille (0..1000).
func stakingRatioMille(bonded, supply uint64) uint64 {
	if supply == 0 {
		return 0
	}
	return bonded * 1000 / supply
}

// adjustRate moves the per-epoch issuance rate toward the target band. Inside the
// band (dead-band) the rate is held. Outside, it steps by at most stepCapMicro and
// is clamped to [floor, ceiling]. Pure integer arithmetic, no clock.
func adjustRate(rate, ratioMille uint64, p thermostatParams) uint64 { /* ... */ }
```

- [ ] **Run, expect PASS.**
- [ ] **Commit:** `git add internal/consensus/thermostat.go internal/consensus/thermostat_test.go && git commit -m "[+] Thermostat ratio and rate-adjustment math"`

### Task 6.3: Compute issuance from pre-mint supply

**Files:** Modify `internal/consensus/thermostat.go`; Test `internal/consensus/thermostat_test.go`.

- [ ] **Test** `TestComputeIssuance`: `issuanceFor(rateMicro, supply)` returns `rateMicro * supply / 1_000_000`; with rate 0 returns 0.
- [ ] **Run, expect FAIL.**
- [ ] **Implement** `issuanceFor(rateMicro, supply uint64) uint64` (saturating multiply via the existing `safeMul`). Document that supply is the PRE-mint value so issuance never lowers its own denominator.
- [ ] **Run, expect PASS.**
- [ ] **Commit:** `git add internal/consensus/thermostat.go internal/consensus/thermostat_test.go && git commit -m "[+] Issuance from pre-mint supply"`

### Task 6.4: Run the thermostat at the epoch boundary

**Files:** Modify `internal/consensus/epoch.go` (call the thermostat at the start of `transitionEpoch`, before `distributeEpochRewards`); Test `internal/consensus/epoch_test.go`.

- [ ] **Test:** at an epoch boundary, with bonded below the band, `issuanceRateMicro` rises (capped) and the epoch reward pool grows by `issuanceFor(newRate, preMintSupply)`, with `total_supply` increased by the minted amount; with a zero-fee epoch, issuance is still minted (bootstrap incentive).
- [ ] **Run, expect FAIL.**
- [ ] **Implement** a method `runThermostat() uint64` that: reads `preMint := d.coinStore.TotalSupply()` and `bonded := d.totalBonded()`; `ratio := stakingRatioMille(bonded, preMint)`; `d.issuanceRateMicro = adjustRate(d.issuanceRateMicro, ratio, d.thermostat)`; `issuance := issuanceFor(d.issuanceRateMicro, preMint)`; `d.coinStore.AddSupply(issuance)`; returns `issuance`. Call it in `transitionEpoch` before `distributeEpochRewards`, and stash the issuance so `distributeEpochRewards` adds it to the pool (Batch 7 consumes it). Remove the `epochFees == 0` early-return guard in `distributeEpochRewards` (issuance must pay even at zero fees).
- [ ] **Run** `go test ./internal/consensus/ -count=1 -timeout 120s`.
- [ ] **Commit:** `git add internal/consensus/epoch.go internal/consensus/epoch_test.go && git commit -m "[+] Run the thermostat at each epoch boundary (mints even at zero fees)"`

### Task 6.5: Persist the issuance rate; restore on sync

**Files:** Modify `internal/sync/snapshot.go` + the sync wiring; Test `internal/sync/snapshot_test.go`.

- [ ] **Test:** the issuance rate round-trips through a snapshot and is restored.
- [ ] **Run, expect FAIL.**
- [ ] **Implement:** carry `issuance_rate_micro:uint64` in `snapshot.fbs` (already bumped to version 9 in Task 6.1; add the field there if not yet done and regenerate), thread it through `CreateSnapshot`/checksum/`ApplySnapshot`, and restore it into the DAG on import (a `WithIssuanceRate` Option or the existing import path).
- [ ] **Run** `go test ./internal/sync/ -count=1 -timeout 120s`.
- [ ] **Commit:** `git add -A && git commit -m "[&] Persist and restore the thermostat issuance rate"`

### Task 6.6: Verify Batch 6; push

- [ ] **Verify:** `go test ./internal/... -count=1 -timeout 180s`.
- [ ] **Push:** `git push`

---

# Batch 7 — Reward distribution (`effective_stake x liveness`)

**Spec:** §6, §5.2. Finishes the `TODO: credit to validator's reward_coin` in `epoch.go`. Pool = `epochFees + thermostat issuance`. Weight = `effective_stake x liveness`, liveness = `epochRoundsProduced / epochTotalRounds`. NOT proportional to attestation count. Crediting via `creditCoin` on `d.coinStore` to a `reward_coin` the validator designates at registration; the validator's reward is split with delegators (Batch 4); a fraction is auto-restaked.

### Task 7.1: reward_coin designation on ValidatorInfo

**Files:** Modify `internal/validators/validators.go` (add `RewardCoin [32]byte` field, carried in copies + snapshot encoder); Modify `internal/consensus/commit.go` (`handleRegisterValidator` reads the reward-coin from args, default to the validator's gas/genesis coin). Test accordingly.

- [ ] **Test:** registering with a reward-coin arg sets `Get(pk).RewardCoin`; the snapshot preserves it (bump `snapshotVersion` to 10; extend `encodeValidators`/`decodeValidators` to append the 32-byte reward coin).
- [ ] **Run, expect FAIL.**
- [ ] **Implement** the field + encoder extension + the registration read (fall back to `genesis.GenesisCoinID(pubkey)` / the validator's known coin when absent).
- [ ] **Run, expect PASS.**
- [ ] **Commit:** `git add -A && git commit -m "[+] reward_coin designation on validators (snapshot version 10)"`

### Task 7.2: Reward weight = effective_stake x liveness

**Files:** Modify `internal/consensus/epoch.go` (`distributeEpochRewards`); Test `internal/consensus/epoch_test.go`.

- [ ] **Test** `TestRewardWeight`: two validators with equal stake but different `epochRoundsProduced` get rewards proportional to rounds produced; two with equal liveness but different stake get rewards proportional to stake; a validator with zero rounds gets nothing; attestation count is irrelevant (not an input).
- [ ] **Run, expect FAIL.**
- [ ] **Implement:** compute each validator's weight as `EffectiveStake(v) * epochRoundsProduced[pubkey]` (liveness is `rounds/epochTotalRounds`; multiplying by the common `epochTotalRounds` denominator cancels, so `effective_stake * rounds` is the integer weight). `totalWeight = sum`. `share = pool * weight / totalWeight`. `pool = epochFees + issuance` (from Task 6.4).
- [ ] **Run, expect PASS.**
- [ ] **Commit:** `git add internal/consensus/epoch.go internal/consensus/epoch_test.go && git commit -m "[&] Reward weight = effective_stake x liveness"`

### Task 7.3: Credit rewards (validator + delegator split + auto-restake)

**Files:** Modify `internal/consensus/epoch.go` (`distributeEpochRewards`); Test `internal/consensus/epoch_test.go`.

- [ ] **Test** `TestRewardCrediting`: a validator's `share` is split via `splitValidatorReward` (Batch 4); the validator's portion is credited to its `RewardCoin` via `creditCoin`; each delegator's portion is credited to the delegator's coin; an `autoRestakeMille` fraction of the validator portion increments `SelfStake` instead of crediting the coin (reflected in next epoch's pre-mint ratio).
- [ ] **Run, expect FAIL.**
- [ ] **Implement:** for each validator, enumerate its delegations (Task 4.6), call `splitValidatorReward(share, v.SelfStake, d.commissionBPS, dels)`, credit the delegator amounts to their owner coins, split the validator amount into an auto-restake part (`amount * autoRestakeMille / 1000` → `SetSelfStake(self+restake)`) and a liquid part (`creditCoin(d.coinStore, v.RewardCoin, liquid)`). Replace the `TODO: credit to validator's reward_coin` block.
- [ ] **Run** `go test ./internal/consensus/ -count=1 -timeout 120s`.
- [ ] **Commit:** `git add internal/consensus/epoch.go internal/consensus/epoch_test.go && git commit -m "[+] Credit epoch rewards: validator/delegator split + auto-restake"`

### Task 7.4: Reward conservation property test

**Files:** Test `internal/consensus/epoch_test.go`.

- [ ] **Test** `TestRewardConservation`: the sum of all credited reward amounts (validator liquid + auto-restake + delegator shares) over an epoch equals the pool (`epochFees + issuance`) up to integer-division remainder, and the supply invariant (Batch 2.6) still holds after distribution (issuance already added to supply in Task 6.4; crediting moves it into coins/stake, not creating new supply).
- [ ] **Run, expect PASS.**
- [ ] **Commit:** `git add internal/consensus/epoch_test.go && git commit -m "[+] Property test: epoch reward conservation"`

### Task 7.5: Verify Batch 7; push

- [ ] **Verify:** `go test ./internal/... -count=1 -timeout 180s`; `go test ./test/integration/ -run TestSimConsensus -count=1 -timeout 5m`.
- [ ] **Push:** `git push`

---

# Batch 8 — Sponsored transactions

**Spec:** §9. Native fee payer (Sui-style). `transaction.fbs` gains `fee_payer`, `sponsor_signature`, `valid_until`, encoded absent-when-empty so a non-sponsored tx serializes byte-identically. Both parties sign the SAME canonical body hash. The gas-coin owner check becomes `owner == fee_payer`. Replay via the existing commit-once guard; `valid_until` (epochs) checked against the commit epoch.

### Task 8.1: Schema fields (absent-when-empty)

**Files:** Modify `types/transaction.fbs`; regenerate; build. Test `internal/genesis/transaction_test.go`.

- [ ] **Implement:** add to the `Transaction` table: `fee_payer:[ubyte];`, `sponsor_signature:[ubyte];`, `valid_until:uint64;`. Regenerate: `bash types/generate.sh && go build ./...`.
- [ ] **Test** `TestNonSponsoredTxByteIdentical`: building a tx with no fee_payer through the existing builder produces bytes identical to the pre-change builder (the new fields are absent, not zero-filled) and its hash/signature verify unchanged. (Assert by confirming `FeePayerBytes()` is empty and the existing genesis tx tests still pass.)
- [ ] **Run, expect PASS** (after regenerate).
- [ ] **Commit:** `git add types/transaction.fbs internal/types/ && git commit -m "[+] Schema: fee_payer, sponsor_signature, valid_until (absent-when-empty)"`

### Task 8.2: Canonical body hash covers the new fields

**Files:** Modify `internal/genesis/transaction.go` (`BuildUnsignedTxBytesWithRefs` and the hashing path); Test `internal/genesis/transaction_test.go`.

- [ ] **Test:** the unsigned body includes `fee_payer` and `valid_until` (when present) but never the two signature fields; two bodies differing only in `fee_payer` hash differently; an absent `fee_payer` yields the legacy body bytes (backward-compatible).
- [ ] **Run, expect FAIL.**
- [ ] **Implement:** include `fee_payer` and `valid_until` in `BuildUnsignedTxBytesWithRefs` (conditionally, only when set, mirroring the `gasCoin` absent-when-empty pattern). The body must exclude `signature` and `sponsor_signature`. Add a `BuildSponsoredUnsignedTxBytes(...)` wrapper if needed for the sponsor path.
- [ ] **Run, expect PASS.**
- [ ] **Commit:** `git add internal/genesis/transaction.go internal/genesis/transaction_test.go && git commit -m "[+] Canonical body hash binds fee_payer and valid_until"`

### Task 8.3: Build a doubly-signed sponsored tx

**Files:** Modify `internal/genesis/transaction.go` (a builder for sponsored txs); Test `internal/genesis/transaction_test.go`.

- [ ] **Test:** `BuildSponsoredTx(senderKey, sponsorKey, ...)` produces a tx where both `signature` (sender) and `sponsor_signature` (sponsor) verify against the same body hash, `fee_payer == sponsor pubkey`, and `valid_until` is set.
- [ ] **Run, expect FAIL.**
- [ ] **Implement** the builder: compute the body hash once, sign it with both keys, set `signature`, `sponsor_signature`, `fee_payer`, `valid_until`.
- [ ] **Run, expect PASS.**
- [ ] **Commit:** `git add internal/genesis/transaction.go internal/genesis/transaction_test.go && git commit -m "[+] Build doubly-signed sponsored transactions"`

### Task 8.4: Verify the sponsor signature; gate gas coin on fee_payer

**Files:** Modify `internal/consensus/validate.go` (or where tx signatures verify) + `internal/consensus/commit.go` (`validateGasCoin`); Test `internal/consensus/commit_test.go`.

- [ ] **Test:** a sponsored tx whose gas coin is owned by the `fee_payer` (not the sender) passes `validateGasCoin`; a sponsored tx with an invalid `sponsor_signature` is rejected; a non-sponsored tx is unaffected (`owner == sender`).
- [ ] **Run, expect FAIL.**
- [ ] **Implement:** when `len(tx.FeePayerBytes()) == 32`, verify `sponsor_signature` against the body hash using `fee_payer` as the key, and change `validateGasCoin` to require `owner == fee_payer` (fall back to `owner == sender` when no fee_payer). Verify the sender `signature` as today.
- [ ] **Run** `go test ./internal/consensus/ -count=1 -timeout 120s`.
- [ ] **Commit:** `git add -A && git commit -m "[+] Verify sponsor signature; gas coin owned by fee_payer"`

### Task 8.5: valid_until enforced against the commit epoch

**Files:** Modify `internal/consensus/commit.go` (`executeTx` / `deductFees`); Test `internal/consensus/commit_test.go`.

- [ ] **Test:** a sponsored tx with `valid_until` < the commit epoch is rejected (stale sponsorship); `valid_until` >= commit epoch proceeds; `valid_until == 0` with a fee_payer means "no bound" (or is rejected — pick and document; default: 0 means unbounded only for non-sponsored, required for sponsored).
- [ ] **Run, expect FAIL.**
- [ ] **Implement:** in the commit path where the commit epoch is known (`d.commitEpochForRound(commitRound)`), reject sponsored txs whose `valid_until` is below it. Document the `valid_until == 0` rule.
- [ ] **Run, expect PASS.**
- [ ] **Commit:** `git add internal/consensus/commit.go internal/consensus/commit_test.go && git commit -m "[+] Enforce sponsored-tx valid_until against the commit epoch"`

### Task 8.6: Client/SDK support for sponsored submission

**Files:** Modify `client/` (Go client) to build/submit a sponsored tx; optionally `cmd/cli` (`bpctl`). Test in `client/`.

- [ ] **Test:** the client can assemble a sponsored tx from a sender-signed body + a sponsor signature and submit it; the round-trip commits and charges the sponsor's coin.
- [ ] **Run, expect FAIL.**
- [ ] **Implement** the minimal client helper(s); wire a `bpctl` subcommand only if it is a small addition (else `// TODO`).
- [ ] **Run** the client unit tests + `go test ./test/integration/ -run TestSimConsensus -count=1 -timeout 5m`.
- [ ] **Commit:** `git add -A && git commit -m "[+] Client support for sponsored transactions"`

### Task 8.7: Verify Batch 8; push

- [ ] **Verify:** `go test ./internal/... ./client/... -count=1 -timeout 180s`; `go test ./test/integration/ -run TestSimConsensus -count=1 -timeout 5m`.
- [ ] **Push:** `git push`

---

# Batch 9 — `Vertex.timestamp` field (pipeline deferred)

**Spec:** §8. Land ONLY the field now (the consensus-breaking part). The median derivation, monotonic coercion, and pod exposure are deferred to the first time-using pod. Producers populate the field from their local clock; it is part of the signed/hashed body.

### Task 9.1: Add the timestamp field

**Files:** Modify `types/vertex.fbs`; regenerate; build.

- [ ] **Implement:** add to the `Vertex` table: `// timestamp is the producer's local wall-clock at production (unix nanos); part of the signed body. The median-derivation pipeline and pod exposure are deferred.\n    timestamp:uint64;`. Regenerate: `bash types/generate.sh && go build ./...`.
- [ ] **Commit:** `git add types/vertex.fbs internal/types/ && git commit -m "[+] Schema: signed Vertex.timestamp field"`

### Task 9.2: Producers populate the timestamp; include it in the signed body

**Files:** Modify `internal/consensus/build.go` (`buildVertex` and the hashed body); Test `internal/consensus/build_test.go`.

- [ ] **Test:** a produced vertex has a non-zero `Timestamp()`; the timestamp is covered by the vertex hash (two vertices identical except timestamp hash differently); the signature still verifies.
- [ ] **Run, expect FAIL.**
- [ ] **Implement:** set `types.VertexAddTimestamp(builder, uint64(time.Now().UnixNano()))` in `buildVertex`, and include the timestamp in the unsigned body that feeds the vertex hash (so it is signed). Keep the validity bound minimal: reject a vertex whose timestamp is absurd (e.g. zero) only if it complicates nothing; otherwise accept any value (the pipeline that interprets it is deferred). Document the deferral.
- [ ] **Run** `go test ./internal/consensus/ -count=1 -timeout 120s`.
- [ ] **Commit:** `git add internal/consensus/build.go internal/consensus/build_test.go && git commit -m "[+] Producers stamp and sign Vertex.timestamp"`

### Task 9.3: Confirm no money path reads the clock

**Files:** Test `internal/consensus/` (a guard test or a code comment).

- [ ] **Test/assert:** add a short test or doc note confirming issuance (Batch 6) and `valid_until` (Batch 8) are epoch-based and never read `Vertex.timestamp`, so the over-state-time bias never applies. (This is a documentation/guard task; assert that `runThermostat` takes no timestamp input.)
- [ ] **Commit:** `git add -A && git commit -m "[&] Assert money never reads the vertex clock"`

### Task 9.4: Verify Batch 9; push

- [ ] **Verify:** `go test ./internal/... -count=1 -timeout 180s`; `go test ./test/integration/ -run TestSimBootstrap -count=1 -timeout 5m` (timestamp is consensus-breaking — confirm sims still converge).
- [ ] **Push:** `git push`

---

# Batch 10 — Docs and schema comments

**Spec:** Doc impact section. Bring the docs of record in line now that the implementation has landed. VISION owns the why; WHITEPAPER owns the how (one document of record per subject; edit in place; no em dashes; straight quotes).

### Task 10.1: WHITEPAPER fees, reward, supply

**Files:** Modify `docs/WHITEPAPER.md`.

- [ ] **Edit** §9 (Fees): 70/30 → 100/0; drop the burn-as-anti-gaming argument; describe reward as `effective_stake x liveness` with serving enforcement deferred. §10 (Reward formula): `stake x (rounds/total)` → `effective_stake x liveness`; add a delegation subsection (positions, fixed commission, epoch-boundary proportional split, unbonding, jailing) and the minimum-stake / voting-cap note. Add a new `total_supply` accounting subsection.
- [ ] **Commit:** `git add docs/WHITEPAPER.md && git commit -m "[&] Whitepaper: 100/0 fees, effective_stake x liveness, delegation, supply"`

### Task 10.2: WHITEPAPER consensus and security headline

**Files:** Modify `docs/WHITEPAPER.md`.

- [ ] **Edit** §§10/5 (Validators, Consensus): stake-weighted capped quorum and the dual security model replace the equal-weight framing and the "stake is equal (1)" note; genesis-as-state changes the genesis-epoch story. §1 (security headline): reframe "honest majority per object" to "honest majority per object for attestation, honest two-thirds-of-stake for ordering". §§7/9/12: sponsored transactions (`fee_payer`, sponsor signature, gas coin owned by the fee payer).
- [ ] **Commit:** `git add docs/WHITEPAPER.md && git commit -m "[&] Whitepaper: stake-weighted quorum, dual security, sponsored tx"`

### Task 10.3: VISION positioning

**Files:** Modify `docs/VISION.md`.

- [ ] **Edit:** add the utility-first, mildly-inflationary, no-burn stability stance as a positioning statement (the why), without duplicating the whitepaper's mechanics.
- [ ] **Commit:** `git add docs/VISION.md && git commit -m "[&] Vision: utility-first, mildly-inflationary, no-burn stance"`

### Task 10.4: Schema-comment soft-deprecations; final verify; push

**Files:** Modify `types/vertex.fbs` (comment), and run a full gate.

- [ ] **Edit** the `FeeSummary.total_burned` comment in `types/vertex.fbs` to note it is vestigial (always 0 since the scarcity burn was removed), soft-deprecated. Regenerate if the comment change is in a doc-only position that does not require it; otherwise `bash types/generate.sh`.
- [ ] **Final verify:** `go test ./internal/... ./client/... -count=1 -timeout 180s`; `go test ./test/integration/ -run TestSimBootstrap -count=1 -timeout 5m`; `go test ./test/integration/ -run TestSimConsensus -count=1 -timeout 5m`.
- [ ] **Commit + push:** `git add -A && git commit -m "[&] Soft-deprecate total_burned; final economic-layer verification"` then `git push`.

---

## Self-review

**Spec coverage.** §1 stake-weighted capped quorum + dual security → Batch 5 (+ headline doc in 10.2). §2 bonding/delegation/jailing → Batches 3–4. §3 thermostat → Batch 6. §4 no scarcity burn → Batch 2.4. §5 fees (100/0 distribution, structure kept) → Batch 2.4 + 7. §6 reward `effective_stake x liveness` → Batch 7. §7 supply/bonded tracking + invariant → Batches 2 (+3.4 for bonded, 2.6/7.4 property tests). §8 timestamp field only → Batch 9. §9 sponsored tx → Batch 8. §10 deferred enforcement → respected (no slashing/storage-challenge tasks; unbonding delay left as a documented TODO). §11 genesis-and-fee integrity → Batch 1. Parameters table → seeded as Options across Batches 2–6. Doc impact → Batch 10.

**Deferred-by-design (not gaps).** The Cosmos-F1 accumulator (proportional split ships instead), liquid staking, per-validator commission, the dynamic fee, the clock pipeline, slashing, and storage/serving challenges — all explicitly out of scope per the spec. The unbonding delay is stubbed with a TODO (tied to the slashing branch); flagged in Task 3.6.

**Forward references.** Symbols used by later batches are defined earlier in this plan: `CoinStore` supply methods (2.2) before the thermostat (6); `EffectiveStake` (3.4) before quorum (5) and reward (7); `splitValidatorReward` + delegation enumeration (4.5/4.6) before reward crediting (7.3); the body-hash change (8.2) before sponsor verification (8.4). No symbol is referenced before its defining task.

**Risk notes.** (1) Batch 5 makes commit/production stake-weighted — sims must seed the founding validator with non-zero self-stake or quorum stalls; Task 5.6 calls this out. (2) Snapshot version is bumped repeatedly (7→10); each bump must update both `encode`/`decode` and the checksum, and old snapshots are incompatible (acceptable pre-mainnet). (3) Batches 3/4 touch the Rust system pod — the WASM must be rebuilt before the Go-side handlers are exercised. (4) Schema regeneration (Batches 8/9) must run `bash types/generate.sh` and rebuild before tests.
