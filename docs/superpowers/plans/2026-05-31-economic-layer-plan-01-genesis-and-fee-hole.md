# Economic Layer, Plan 01: Genesis-as-State and the Fee-less Hole

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the two genesis transactions (mint + register_validator) with directly-seeded initial state, then require a funded gas coin on every user transaction so the fee-less execution hole is eliminated at the root.

**Architecture:** Today the bootstrap node builds two `AttestedTransaction`s (`genesis.BuildTransactions`) and injects them into the first vertex via `consensus.WithGenesisTxs`; they execute through the normal commit path to create the initial coin and register the founding validator. Those transactions carry no gas coin, and `deductFees` treats "no gas coin" as "proceed for free", which any user transaction can exploit. This plan seeds the initial coin object and the founding validator directly into the state store and validator set at DAG construction (no genesis transactions), then changes `deductFees` to reject any transaction lacking a funded gas coin. With no legitimate fee-less transaction left, the exemption is removed.

**Tech Stack:** Go 1.26, FlatBuffers (`internal/types`), BLAKE3, Pebble-backed state, the existing `CoinStore`/`State`/`ValidatorSet` APIs.

**Spec:** `docs/superpowers/specs/2026-05-31-economic-layer-design.md` sections 11 (Genesis and fee integrity) and 5 (Fees). This is plan 1 of the 9-plan decomposition.

**Branch:** `economic-layer` (already checked out).

---

## Context the engineer needs

Current genesis flow:
- `cmd/node/init.go:139-152` builds `genesisCfg` (with `InitialMint`, `QUICAddress`, `BLSPubkey`, `SystemPodID`) and calls `genesis.BuildTransactions(genesisCfg)` → `consensus.WithGenesisTxs(txs)`.
- `internal/genesis/genesis.go:28-53` `BuildTransactions` returns `[mintTx, registerTx]` (both `AttestedTransaction` bytes, no gas coin).
- `internal/consensus/dag.go:131-135` `WithGenesisTxs` appends them to `d.pendingTxs`; the bootstrap node includes them in its first vertex (`dag.go:938-939`), which commits and executes them.
- The mint creates a Coin object: a singleton (`replication = 0`), `owner` = bootstrap pubkey, `content` = 8-byte little-endian balance (see the Coin codec in `internal/consensus/coins.go:21-90`). The object ID is currently `computeObjectID(txHash, 0)`.
- `register_validator` runs the system pod and the Go side adds the validator to the set (`ValidatorSet.Add(pubkey, quicAddr, blsPubkey)` in `internal/validators/validators.go`).

The fee-less hole:
- `internal/consensus/commit.go` `deductFees` (around lines 394-443): `gasCoinBytes := tx.GasCoinBytes(); if len(gasCoinBytes) != 32 { return FeeSplit{}, true }` — i.e. no gas coin ⇒ proceed with zero fee. `validateGasCoin` (around lines 445-476) checks `owner == sender`, `replication == 0`, sufficient balance.

State seeding APIs available:
- `State` exposes the object store; objects are written with the same `Object` FlatBuffer the Coin codec rebuilds (`internal/consensus/coins.go:59-90` shows building an `Object` with id/version/owner/replication/content/fees). The DAG holds `d.coinStore CoinStore` (`SetObject(data []byte)` / `GetObject(id) []byte`).
- `ValidatorSet.Add(pubkey Hash, quicAddr string, blsPubkey [48]byte) bool` adds a validator (`internal/validators/validators.go`).

Testing: unit tests live beside the package (`internal/consensus/*_test.go`, `internal/genesis/*_test.go`). Integration sims live in `test/integration/` (`TestSim*`). Per the project memory, run sims individually with a bounded `-timeout`.

---

## Task 1: Deterministic genesis object IDs

**Files:**
- Create: `internal/genesis/ids.go`
- Test: `internal/genesis/ids_test.go`

The seeded coin needs a deterministic, collision-free ID (no creating transaction exists anymore). Derive it from a fixed genesis tag and the owner pubkey.

- [ ] **Step 1: Write the failing test**

```go
package genesis

import "testing"

func TestGenesisCoinID_Deterministic(t *testing.T) {
	var owner [32]byte
	owner[0] = 1

	a := GenesisCoinID(owner)
	b := GenesisCoinID(owner)
	if a != b {
		t.Fatalf("GenesisCoinID not deterministic: %x vs %x", a, b)
	}

	var other [32]byte
	other[0] = 2
	if GenesisCoinID(other) == a {
		t.Fatal("different owners must yield different coin IDs")
	}
}
```

- [ ] **Step 2: Run it, expect FAIL** — `go test ./internal/genesis/ -run TestGenesisCoinID_Deterministic` → undefined `GenesisCoinID`.

- [ ] **Step 3: Implement**

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

- [ ] **Step 4: Run it, expect PASS** — `go test ./internal/genesis/ -run TestGenesisCoinID_Deterministic`.

- [ ] **Step 5: Commit** — `git add internal/genesis/ids.go internal/genesis/ids_test.go && git commit -m "[+] Deterministic genesis coin ID derivation"`

---

## Task 2: Build the initial state (coin object + validator) instead of transactions

**Files:**
- Modify: `internal/genesis/genesis.go` (add `BuildInitialState`, keep `BuildTransactions` for now until Task 4 removes its caller)
- Create: `internal/genesis/state.go`
- Test: `internal/genesis/state_test.go`

Produce the genesis state as data: the serialized initial Coin object and the founding validator descriptor.

- [ ] **Step 1: Write the failing test**

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

	st := BuildInitialState(Config{
		PrivateKey:  ed25519.NewKeyFromSeed(make([]byte, 32)),
		InitialMint: 1234,
		QUICAddress: "127.0.0.1:9000",
		BLSPubkey:   make([]byte, 48),
	}, owner)

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
}
```

- [ ] **Step 2: Run it, expect FAIL** — `go test ./internal/genesis/ -run TestBuildInitialState_Coin` → undefined `BuildInitialState` / `InitialState`.

- [ ] **Step 3: Implement** in `internal/genesis/state.go`

```go
package genesis

import (
	"encoding/binary"

	flatbuffers "github.com/google/flatbuffers/go"

	"BluePods/internal/types"
)

// InitialState is the genesis ledger state, seeded directly (no transactions).
type InitialState struct {
	Coin    []byte   // Coin is the serialized initial Coin object (a singleton).
	CoinID  [32]byte // CoinID is its deterministic object ID.
	Pubkey  [32]byte // Pubkey is the founding validator's Ed25519 key.
	QUIC    string   // QUIC is the founding validator's address.
	BLS     []byte   // BLS is the founding validator's 48-byte BLS key.
	Supply  uint64   // Supply is the initial total supply (== InitialMint).
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

- [ ] **Step 4: Run it, expect PASS** — `go test ./internal/genesis/ -run TestBuildInitialState_Coin`.

- [ ] **Step 5: Commit** — `git add internal/genesis/state.go internal/genesis/state_test.go && git commit -m "[+] BuildInitialState: genesis coin + validator as seeded data"`

---

## Task 3: Seed the state and validator set at DAG construction

**Files:**
- Modify: `internal/consensus/dag.go` (add `WithGenesisState(InitialState)` Option that seeds `d.coinStore` and `d.validators` instead of appending txs)
- Test: `internal/consensus/genesis_state_test.go`

- [ ] **Step 1: Write the failing test**

```go
package consensus

// TestWithGenesisState_SeedsCoinAndValidator builds a DAG with WithGenesisState
// and asserts the coin object is retrievable from the coin store and the
// validator is in the set, with no entries in pendingTxs.
func TestWithGenesisState_SeedsCoinAndValidator(t *testing.T) {
	// construct a DAG with a stub CoinStore + empty ValidatorSet, apply
	// WithGenesisState(is), then assert:
	//   d.coinStore.GetObject(is.CoinID) != nil and balance == is.Supply
	//   d.validators.Get(is.Pubkey) present with QUIC/BLS set
	//   len(d.pendingTxs) == 0
}
```

(Flesh out using the existing DAG test constructor pattern in `internal/consensus/*_test.go`; reuse the stub `CoinStore` already used by the fee tests.)

- [ ] **Step 2: Run it, expect FAIL** — `go test ./internal/consensus/ -run TestWithGenesisState_SeedsCoinAndValidator` → undefined `WithGenesisState`.

- [ ] **Step 3: Implement** in `internal/consensus/dag.go`, alongside `WithGenesisTxs`:

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
		d.setTotalSupply(is.Supply) // see Plan 2; for now store on the DAG field
	}
}
```

Note: `setTotalSupply` / the `total_supply` counter is Plan 2. For Plan 1, store `is.Supply` on a plain `d.genesisSupply uint64` field and wire the real counter in Plan 2. Add the import of `BluePods/internal/genesis` to `dag.go`.

- [ ] **Step 4: Run it, expect PASS** — `go test ./internal/consensus/ -run TestWithGenesisState_SeedsCoinAndValidator`.

- [ ] **Step 5: Commit** — `git add internal/consensus/dag.go internal/consensus/genesis_state_test.go && git commit -m "[+] WithGenesisState seeds coin + validator at DAG construction"`

---

## Task 4: Switch the bootstrap node to seeded state; remove genesis transactions

**Files:**
- Modify: `cmd/node/init.go:139-152` (use `BuildInitialState` + `WithGenesisState` instead of `BuildTransactions` + `WithGenesisTxs`)
- Modify: `internal/genesis/genesis.go` (delete `BuildTransactions` once unused)
- Modify: `internal/consensus/dag.go` (delete `WithGenesisTxs` once unused)
- Test: existing `TestSimBootstrap` covers the bootstrap path end to end.

- [ ] **Step 1: Change the wiring** in `cmd/node/init.go`: derive `owner` from `n.cfg.PrivateKey.Public()`, call `is := genesis.BuildInitialState(genesisCfg, owner)`, and pass `consensus.WithGenesisState(is)` in the DAG options where `WithGenesisTxs(txs)` was. Drop the `genesis.BuildTransactions` call.

- [ ] **Step 2: Build** — `go build ./...`. Expect `BuildTransactions` / `WithGenesisTxs` now unused.

- [ ] **Step 3: Delete the dead code** — remove `genesis.BuildTransactions` (`internal/genesis/genesis.go:28-53`) and `consensus.WithGenesisTxs` (`internal/consensus/dag.go:131-135`). Keep `genesis.BuildMintTx` (still used by the faucet until Task 6).

- [ ] **Step 4: Run the bootstrap sim** — `go test ./test/integration/ -run TestSimBootstrap -count=1 -timeout 5m`. Expect PASS: the node bootstraps, the genesis coin exists (seeded), and a faucet/mint still works (faucet rewired in Task 6). If a sub-case asserted a genesis *transaction* was committed, update it to assert seeded state instead.

- [ ] **Step 5: Commit** — `git add -A && git commit -m "[&] Bootstrap from seeded genesis state; remove genesis transactions"`

---

## Task 5: Close the fee-less hole in deductFees

**Files:**
- Modify: `internal/consensus/commit.go` (the `deductFees` no-gas-coin branch, ~lines 399-403)
- Test: `internal/consensus/commit_test.go` (or the existing fee test file)

- [ ] **Step 1: Write the failing test**

```go
// TestDeductFees_RejectsMissingGasCoin asserts a user tx with no gas coin is
// rejected (proceed == false), where today it returns proceed == true.
func TestDeductFees_RejectsMissingGasCoin(t *testing.T) {
	d := newTestDAG(t) // existing helper used by fee tests
	tx := buildTxNoGasCoin(t) // a tx whose GasCoinBytes() length != 32
	atx := wrapATX(t, tx)
	_, proceed := d.deductFees(tx, atx, Hash{})
	if proceed {
		t.Fatal("a transaction without a funded gas coin must be rejected")
	}
}
```

- [ ] **Step 2: Run it, expect FAIL** — `go test ./internal/consensus/ -run TestDeductFees_RejectsMissingGasCoin` → currently `proceed == true`.

- [ ] **Step 3: Implement** — change the branch in `deductFees`:

```go
// No gas coin: reject. Genesis is seeded state (not a transaction) and
// protocol actions (issuance, reward crediting) are not transactions, so
// every user transaction must reference a funded gas coin.
gasCoinBytes := tx.GasCoinBytes()
if len(gasCoinBytes) != 32 {
	return FeeSplit{}, false
}
```

- [ ] **Step 4: Run tests, expect PASS** — `go test ./internal/consensus/ -run TestDeductFees -count=1`. Also run the full consensus unit gate: `go test ./internal/consensus/ -count=1 -timeout 120s`. Fix any test that relied on the old fee-less behavior by giving its tx a funded gas coin.

- [ ] **Step 5: Commit** — `git add internal/consensus/commit.go internal/consensus/commit_test.go && git commit -m "[!] Reject transactions without a funded gas coin (close fee-less hole)"`

---

## Task 6: Faucet transfers from a genesis reserve instead of a fee-less mint

**Files:**
- Modify: `cmd/node/clienthandlers.go` (`handleFaucet` / `buildFaucetTx`, around lines 334-367)
- Modify: `cmd/node/init.go` (the genesis `InitialMint` becomes the bootstrap account that the faucet transfers from on testnet)
- Test: `internal/genesis/` unit test for a transfer-from-reserve builder + `TestSimConsensus` faucet path.

The faucet currently calls `genesis.BuildMintTx` (a fee-less mint). With the hole closed, the faucet must be a normal, fee-paying transfer from the genesis-seeded bootstrap coin (the reserve) to the requester.

- [ ] **Step 1: Write the failing test** for a `genesis.BuildTransferTx(privKey, systemPod, gasCoinID, fromCoinID, toOwner, amount)` helper that produces a `transfer` system-pod tx referencing `gasCoinID` as the gas coin (so it pays a fee). Assert the built tx's `GasCoinBytes()` length is 32 and `FunctionName()` is `"transfer"`.

- [ ] **Step 2: Run it, expect FAIL** — undefined `BuildTransferTx`.

- [ ] **Step 3: Implement** `genesis.BuildTransferTx` (mirror `BuildMintTx` but call the system pod `transfer` function with the split/transfer args the system pod expects, and pass `gasCoinID[:]` as the `gasCoin` argument to `BuildAttestedTx`). Rewire `buildFaucetTx` to call it, transferring from the bootstrap reserve coin (`genesis.GenesisCoinID(bootstrapOwner)`) and paying gas from that same coin. Reject faucet requests once the reserve is exhausted.

- [ ] **Step 4: Verify** — `go test ./internal/genesis/ -count=1` and `go test ./test/integration/ -run TestSimConsensus -count=1 -timeout 5m` (its faucet + transfer scenario). Expect PASS.

- [ ] **Step 5: Commit** — `git add -A && git commit -m "[&] Faucet transfers from the genesis reserve (no fee-less mint)"`

---

## Task 7: Verify the whole plan end to end

- [ ] **Step 1: Unit gate** — `go test ./internal/... ./client/... -count=1 -timeout 180s`. Expect PASS.
- [ ] **Step 2: Representative sims** — `go test ./test/integration/ -run TestSimBootstrap -count=1 -timeout 5m` and `go test ./test/integration/ -run TestSimConsensus -count=1 -timeout 5m`. Expect PASS.
- [ ] **Step 3: Manual smoke (optional)** — start a bootstrap node, confirm via `bpctl status`/`bpctl coin` that the genesis coin exists, that a `bpctl` object/coin operation referencing a funded gas coin succeeds, and that one without a gas coin is rejected.

---

## Self-review

- **Spec coverage:** section 11 (genesis as state, gas coin required, faucet from reserve, protocol-actions-not-transactions) is covered by Tasks 1-6. The "protocol actions are not transactions" invariant is preserved because issuance/reward crediting (Plans 2/6/7) mutate state directly and never go through `deductFees`. Section 5's fee structure is unchanged here (only the no-gas-coin branch).
- **Placeholder scan:** the one forward-reference is `d.setTotalSupply` (Task 3), explicitly deferred to Plan 2 with an interim `d.genesisSupply` field; not a silent placeholder.
- **Type consistency:** `GenesisCoinID([32]byte) [32]byte`, `BuildInitialState(Config,[32]byte) InitialState`, `WithGenesisState(InitialState) Option` are used consistently across Tasks 1-4.
- **Risk:** Task 4 is the invasive one (bootstrap path). Keep `BuildMintTx` until Task 6 rewires the faucet, then it can be removed in a later plan if fully unused. Verify `TestSimBootstrap` after Task 4 before proceeding.
