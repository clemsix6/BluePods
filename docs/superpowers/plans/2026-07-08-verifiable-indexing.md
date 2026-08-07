# Verifiable Indexing Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development to implement this plan **one batch at a time** (one implementation subagent per batch, task-by-task with fresh subagents for heavy batches). Within a batch, tasks execute in order; **each task ends in one commit**. After a batch's tasks are all committed, **push** before starting the next batch. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Implement the verifiable-indexing spec: the parent/cascade object model with protocol-declared operations, four authenticated SMT indexes anchored in every vertex through a detached provable header, domain economics with rental and enforced namespaces, fail-closed verifiable sync, and the client/API surface with light-client verification.

**Architecture:** The deterministic-commit prerequisite (spec §2) has landed on `main` and been hardened by the scenario-corpus bug-fix campaign; this plan builds on it, bottom-up on `verifiable-indexing`: parent metadata and declared operations first (they define the write paths), then the SMT trees derived from that metadata, then the anchor that exposes the root, then economics, sync, and surface. The index is derived state: every structure is rebuilt from the tracker and domain store, never shipped.

**Tech Stack:** Go 1.26, FlatBuffers (`types/*.fbs` → `internal/types` via `bash types/generate.sh`, requires `flatc`), BLAKE3 (`github.com/zeebo/blake3`), Pebble-backed storage, Rust/WASM system pod (`pods/pod-system`), the scenario environment (`test/harness` + `test/scenarios`, document of record `test/TESTING.md`), structured events (`internal/events`).

**Spec:** `docs/superpowers/specs/2026-06-03-verifiable-indexing-design.md` (§2 landed; §3-§13 covered here as 8 batches).

**Branches:** All batches on `verifiable-indexing`, rebased onto `main` at the scenario-corpus base (397d6f9). Per the root `CLAUDE.md` pipeline, implementation runs in an isolated `.wt/<task>` worktree; open the draft PR after the first push and keep its body current.

## Batch 0 record (landed, for reference)

The prerequisite shipped as PR #3 (squash `ee7fa47`) and was hardened by the bug-fix campaign (PR #6, `397d6f9`). What later batches consume, as it exists today:

- Anchor machinery: `anchor.go` (designation over eligible committed producers), `anchor_decision.go` (vote-determined certify/blame, indirect rule, certification-impossibility and long-silence rules so undecided runs a dead producer left behind resolve), `eligible.go`, `scanner.go`.
- Regime: `regime.go` — `strictStartRound` latch armed on **committed stake** (`committedStakedMemberCount >= minValidators`, any epoch), persisted, carried in sync snapshots. Relaxed resolution waits until a round's evidence is view-independent (the old relaxed-skip determinism follow-up is fixed).
- Commit loop: `commit.go` — cursor-driven `commitNextRound` → `commitAnchorBatch` → `applyBatch`; per-vertex committed flags (`store_committed.go`); stall triggers `onCausalStall` (two-tick by-hash fetch) and `onWaitStall` (frontier fetch + `requestDeepGapRange` vertex-range backfill), served by `cmd/node/vertexfetch.go` (tags 0x15/0x16, 0x1B/0x1C).
- Epoch persistence: `epoch_persist.go` + `epoch_accumulators.go` (epoch state, holder/eligible snapshots, latch, settlement accumulators — one atomic batch with the cursor).
- Sync: consistent-cut snapshot (`snapshotVersion` 13) carrying committed flags, epoch state, latch, stakes and reward coins (`buildValidatorSetFromSnapshot` imports both).

The old `test/integration` `TestSim*` suite is gone. Validation runs on the scenario corpus (`test/scenarios`, `TestScenario*`) per `test/TESTING.md`.

## Global Constraints

- Go rules from the root `CLAUDE.md`: functions ≤ 25 lines, files ≤ 300 lines (split by responsibility), minimal exported API, docstrings everywhere, errors wrapped `fmt.Errorf("...:\n%w", err)`.
- Integer math on fees/supply/stake uses `safeMul`/`safeAdd` (`internal/consensus/fees.go:50/:64`), never raw operators on attacker-influenced values.
- After any `types/*.fbs` change: `bash types/generate.sh`, then rebuild. FlatBuffers fields are append-only; never renumber or remove existing fields (deprecate in place). Any snapshot format change bumps `snapshotVersion` (`internal/sync/snapshot.go:21`, currently 13) in the same commit.
- **Three-site lockstep:** any change to the canonical transaction body (new signed fields) must land in the same commit in `internal/genesis/transaction.go` (builder), `internal/validation/validate.go` (`rebuildUnsignedTx`), and `internal/consensus/txauth.go` (commit-path verify), which all delegate to `genesis.BuildUnsignedTxBytesWithRefs`.
- **Events:** every new mutation of persisted state gets a constructor in `internal/events`, called at the point of mutation, plus a row in `internal/events/catalog.go` AND in `test/TESTING.md`'s event table (same commit). Event names and existing attributes are stable — renaming or removing one is a breaking change to call out in the commit. New events this chantier adds: `state.object.reparented` (batch 1), `consensus.anchor.fault` (batch 3), `state.domain.renewed` and `state.domain.transferred` (batch 4); `state.domain.deleted` (already in the catalog, zero callers today) gains its first callers in batch 4.
- **Scenarios:** every feature batch extends or adds a scenario in `test/scenarios` (new scenario = corpus-table row in `test/TESTING.md`). Scenarios wait on events (`WaitEvent`/`WaitAll`), never sleep, and end under the automatic teardown invariants (convergence fingerprint, zero rollback, supply identity). The convergence fingerprint (`internal/sync/fingerprint.go`) must grow with the state this chantier adds — tracker parents (batch 1), domain owner/expiry leaves (batch 4) — in the same batch that adds the state, or every join scenario diverges.
- Scenario runs are **one at a time, bounded**: `go test ./test/scenarios/ -run TestScenarioX -v -count=1 -timeout <2m-10m>`. Unit tests: `go test ./internal/<pkg>/ -count=1 -timeout 120s`; fast gate: `go test -short ./... -timeout 120s`.
- Commits follow the repo convention: title line without prefix, body lines prefixed `[+] [-] [&] [!]`, no footers. Code comments, doc comments, and test failure messages are self-contained — never cite review findings, bug registers, or plan task numbers.
- Each batch leaves `go build ./... && go vet ./...` green with the new code wired and reachable; batches touching `pods/` or `wasm-gas/` also leave `make -C pods/pod-system release` green.

---

## Execution model (batches)

| Batch | Subsystem | Spec § | Tasks |
|---|---|---|---|
| 1 | Parent in model + tracker; declared operations; pod-output lockdown | 3, 6 | 9 |
| 2 | SMT primitive; domain/parent/children/validator trees; index manager | 4 | 6 |
| 3 | Detached provable header; anchoring; three-stage enforcement | 5, 7 | 6 |
| 4 | Domain declared ops; rental + term cap; expiry sweep; deposit term | 8 | 5 |
| 5 | Sync: index rebuild, fail-closed verification, join scenarios | 9 | 3 |
| 6 | QUIC + bpctl surface; light-client library; wallet switchover | 10 | 4 |
| 7 | Dead code removal | 11 | 2 |
| 8 | Whitepaper and VISION updates | 11 | 3 |

## Code-quality guardrails (enforced every batch)

- **Hot paths stay thin.** `executeTx`, the commit loop (`commitAnchorBatch`/`applyBatch`), and `validateVertex` gain one named-helper call each, never inline logic: declared ops in `ops.go`, root checks in `rootcheck.go`, index updates behind the `indexer` interface.
- **New packages:** `internal/index` (SMT + trees + manager) must not import `internal/consensus` (the DAG feeds it through a narrow interface), so the trees stay testable in isolation.
- **Unexported by default.** Export only what crosses a package boundary: `index.Tree`, `index.Proof`, `index.Verify`, `index.Manager`, the client library functions, the new QUIC message types.
- A batch is not done until its diff passes this checklist in the per-batch review, not just "tests pass".

## File map (created / modified across the plan)

- `internal/consensus/tracker.go` — parent + child count in the entry (55 bytes), `getParent`/`setParent`/`childCount` (batch 1).
- `internal/consensus/walk.go` (new, batch 1) — `controllerOf`, `controls`, `wouldCycle` over tracker metadata.
- `internal/consensus/ops.go` (new, batches 1, 4) — `handleDeclaredOps`: reparent/transfer/delete, then domain ops.
- `internal/consensus/commit.go` — `executeTx` routing to declared ops, created-parent validation, `settleDeclaredDeletions` extension, root re-check (batches 1, 3).
- `internal/consensus/rootcheck.go` (new, batch 3) — ingress/commit root verification, fault evidence.
- `internal/consensus/build.go` + `validate.go` + `hash.go` — detached header, epoch fix, ingress root check (batch 3).
- `internal/index/smt.go`, `proof.go`, `domain_tree.go`, `hierarchy_trees.go`, `validator_tree.go`, `manager.go` (new, batch 2; one tree family per file — a single `trees.go` would blow the 300-line rule).
- `internal/state/state.go` — creation-permission rule, pod-output lockdown (batch 1); `internal/state/domain.go` — leaf gains owner + expiry (batch 4).
- `internal/consensus/fees.go` + `epoch.go` — op fees, rental, `index_entry_fee`, expiry sweep, validator-tree rebuild (batches 2, 4).
- `internal/sync/fingerprint.go` + `fingerprint_hash.go`, `internal/consensus/fingerprint_export.go` — fingerprint covers parents (batch 1) and domain leaves (batch 4).
- `internal/sync/snapshot.go` + `types/snapshot.fbs` — tracker parents (batch 1), domain leaves (batch 4).
- `internal/events/` — new constructors + catalog rows (batches 1, 3, 4) mirrored in `test/TESTING.md`.
- `types/object.fbs` (+`parent_kind`), `types/transaction.fbs` (+`DeclaredOp`, `operations`), `types/vertex.fbs` (+`frontier_round`, `index_root`, `body_hash`), `types/podio.fbs` (deprecations).
- `internal/network/messages.go` — new client pairs starting at **0x1D** (0x15-0x1C are taken): 0x1D/0x1E anchor (batch 3), 0x1F/0x20 children, 0x21/0x22 ancestors, 0x23/0x24 validator tree (batch 6); `DomainResolve` extended in place; all added to `clientRequestTags` (`messages.go:130`).
- `cmd/node/indexhandlers.go` (new) — index query handlers (`clienthandlers.go` is at 569 lines); `cmd/node/sync.go` — index rebuild + fail-closed sync (batch 5).
- `pkg/client/` — declared-op builders (batch 1), `verify.go` light-client library + wallet switchover (batch 6).
- `cmd/cli/` — `bpctl` domain/object verbs: `main.go` dispatch, `object.go`, new `domain.go`, `tui/dispatch.go` (batch 6).
- `pods/pod-system/src/lib.rs` — remove `transfer`/`transfer_object` dispatch (batch 1); `pods/pod-sdk/src/domain_generated.rs` — deleted (batch 7).
- `test/scenarios/scenario_hierarchy_test.go` (new, batch 1), `scenario_domains_test.go` (new, batch 4), extensions to consensus/joining scenarios (batches 3, 5, 6); `test/TESTING.md` kept current throughout.
- `docs/WHITEPAPER.md` and `docs/VISION.md` (batch 8).

---

# Batch 1 — Parent, declared operations, pod-output lockdown

**Spec:** §3, §6.

**Context (verified against main @ 397d6f9):** the tracker entry is 18 bytes (`tracker.go:266 encodeValue`: version 8 + replication 2 + fees 8; struct at `tracker.go:21`); `trackObject` (`tracker.go:199`) is reached from the creation path via the `SetOnObjectCreated` callback (`cmd/node/aggregation.go:54` → `DAG.TrackObject`, `dag.go:612`). Deletion accounting is already network-uniform: the transaction declares deleted IDs in `tx.deleted_objects` (each must also appear in `mutable_refs`), and the commit loop settles them via `settleDeclaredDeletions` (`commit.go:797`) → `settleDeletion` (`commit.go:830`: `tracker.deleteObject` + 95/5 refund/burn + `events.ObjectDeleted`); `applyDeletedObjects` (`state.go:611`) only removes locally-held content. `applyUpdatedObjects` (`state.go:440`) persists pod output without an owner/parent check; `validateOutput` (`state.go:353`) already receives the tx. Ownership at commit: `validateMutableRefOwnership` (`commit.go:1099`) reads a replicated ref's owner from its attested ATX copy (`attestedReplicatedOwner`, `commit.go:1190`) and a singleton's from local content — the tracker walk replaces both sources with one global one. Creation transactions execute on every node (`commit.go:723`: `CreatedObjectsReplicationLength() == 0 && MaxCreateDomains() == 0 && !shouldExecute` guard). The staking precedent for protocol-parsed operations is `handleBond`/`handleDelegate` (`commit.go:1542/:1609`). Deposit stamping is gated on the tx carrying a gas coin (`txLocksDeposits`, `state.go:567`).

### Task 1.1: Schemas — `parent_kind`, `DeclaredOp`, canonical body coverage

**Files:** Modify `types/object.fbs` (append `parent_kind:ubyte` to `Object`; 0 = KeyRoot, 1 = ObjectParent; the existing `owner` bytes become the parent bytes), `types/transaction.fbs` (append `DeclaredOp` table and `operations:[DeclaredOp]` to `Transaction`); regenerate; modify `internal/genesis/transaction.go` + `internal/validation/validate.go` + `internal/consensus/txauth.go` in the same commit (three-site lockstep) so `operations` is covered by the canonical body hash — and therefore by the sponsor signature too (sponsored declared-op transactions must keep working; `TestScenarioSponsored` pins the sponsored transfer path).

```fbs
// DeclaredOp is a protocol-level operation applied at commit by every node
// without pod execution. kind: 0=reparent (transfer is a reparent to a
// KeyRoot), 1=delete, 2=domain_register, 3=domain_renew, 4=domain_update,
// 5=domain_transfer, 6=domain_delete.
table DeclaredOp {
    kind:ubyte;
    object_id:[ubyte];    // target object (reparent, delete, domain_register/update pointee)
    target_kind:ubyte;    // reparent: new parent kind (0=KeyRoot, 1=ObjectParent)
    target:[ubyte];       // reparent: new parent bytes; domain_transfer: new owner
    name:string;          // domain ops: the name
    term_epochs:uint32;   // domain_register/renew: rental term
}
```

**Relation to `tx.deleted_objects` (existing):** the two channels coexist by execution mode. `DeclaredOp` delete (kind 1) is the protocol-level delete for transactions with no pod call; `tx.deleted_objects` remains the declaration channel for pod-driven deletions inside globally-executed transactions (the `merge` carve-out, spec §3). Both funnel into the same settlement (Task 1.5).

- [ ] **Test:** a transaction carrying one reparent op round-trips through build → `rebuildUnsignedTx` → `verifyTxAuthenticity` (hash and signature verify); the same with a sponsor signature; a transaction without ops serializes byte-identically to one built before the field existed (absent-when-empty, same guarantee as sponsorship).
- [ ] **Run, expect FAIL** → regenerate (`bash types/generate.sh`), implement body coverage, **expect PASS**.
- [ ] **Commit:** title `DeclaredOp and parent_kind schemas under the canonical body hash`.

### Task 1.2: Tracker carries parent and child count

**Files:** Modify `internal/consensus/tracker.go`; Test `internal/consensus/tracker_test.go`.

**Interfaces — Produces:** `trackObject(objectID Hash, version uint64, replication uint16, fees uint64, parentKind byte, parent Hash)`; `getParent(objectID Hash) (kind byte, parent Hash, ok bool)`; `setParent(objectID Hash, kind byte, parent Hash)`; `childCount(parentID Hash) uint32` maintained on track/setParent/delete; `Export`/`Import` extended; entry layout: version 8 + replication 2 + fees 8 + kind 1 + parent 32 + childCount 4 = 55 bytes; a stored 18-byte value decodes as `KeyRoot` with zero parent (never panics — such objects are controlled by the zero key, i.e. frozen; acceptable only because pre-mainnet networks are recreated, state this in the docstring).

- [ ] **Test:** round-trip encode/decode both lengths; child counts follow reparent chains (A under K, B under A: `childCount(A)==1`; reparent B to K: `childCount(A)==0`); export/import preserves parents.
- [ ] **Run, expect FAIL → implement → PASS.** Update all `trackObject` call sites (creation path passes the created object's parent from its body — thread it through `SetOnObjectCreated` and `DAG.TrackObject`).
- [ ] **Commit:** title `Track parent and child count in the global object tracker`.

### Task 1.3: Snapshot and fingerprint carry parents

**Files:** Modify `types/snapshot.fbs` (`ObjectVersion` gains `parent_kind:ubyte`, `parent:[ubyte]`, `child_count:uint32`); regenerate; modify `internal/sync/snapshot.go` (encode/decode the new tracker fields; bump `snapshotVersion` 13 → 14), `internal/consensus/fingerprint_export.go` + `internal/sync/fingerprint_hash.go` (the convergence fingerprint digests parentKind/parent/childCount per tracker entry); Test `internal/sync/snapshot_test.go` + fingerprint tests.

**Why now, not batch 5:** the harness's teardown convergence check compares fingerprints on every scenario. If the tracker carries parents but the snapshot does not ship them, a joined node's tracker (and fingerprint) diverges from a founder's, and every join scenario goes red between this batch and batch 5.

- [ ] **Test:** snapshot round-trip preserves parents and child counts; checksum covers the new bytes; two nodes with identical trackers fingerprint identically, and a one-bit parent difference changes the fingerprint.
- [ ] **Run, expect FAIL → implement → PASS.**
- [ ] **Commit:** title `Snapshot and convergence fingerprint carry object parents`.

### Task 1.4: Permission walk over tracker metadata

**Files:** Create `internal/consensus/walk.go`; Test `internal/consensus/walk_test.go`.

**Interfaces — Produces:** `controllerOf(objectID Hash) (Hash, bool)` (terminal KeyRoot pubkey, walking ≤ 256 edges, `false` on depth overflow or missing entry); `controls(sender, objectID Hash) bool`; `wouldCycle(objectID Hash, newParentKind byte, newParent Hash) bool` (true if newParent is the object or any descendant — implemented as: walk up from newParent; if the walk meets objectID, cycle).

- [ ] **Test:** nested chain resolves to the root key; depth 257 fails closed; reparenting an ancestor under its descendant is detected; reparenting to a `KeyRoot` never cycles.
- [ ] **Run, expect FAIL → implement → PASS.**
- [ ] **Commit:** title `Cascade permission walk over global metadata`.

### Task 1.5: Reparent, transfer, and delete as commit-path operations

**Files:** Create `internal/consensus/ops.go`; modify `internal/consensus/commit.go` (`executeTx`: if `tx.OperationsLength() > 0`, route to `handleDeclaredOps` after fee deduction and version check, never into pod execution; reject a tx carrying both ops and a pod call at the same site); extend `settleDeclaredDeletions`' gates; add `events.ObjectReparented` constructor + catalog row + `test/TESTING.md` row; Test `internal/consensus/ops_test.go`.

**Interfaces — Consumes:** `controls`, `wouldCycle`, `setParent`, `settleDeletion` (`commit.go:830`), tracker version machinery. **Produces:** `handleDeclaredOps(tx *types.Transaction) bool` — ops apply **sequentially against the evolving state** (staged apply: mutations buffered, discarded wholesale on the first failure, fees kept). Per-op rules:

- **Reparent (kind 0):** requires `controls(sender, object)`; a `KeyRoot` target may be **any key** (that is a transfer — control of the object suffices); an `ObjectParent` target must additionally be sender-controlled; `!wouldCycle`; then `setParent` + version increment + `events.ObjectReparented(object, tx, kind, parent, version)`. The reparent effect also rewrites the stored body's owner bytes (and parent kind) everywhere a copy exists — the consensus-side coin store and, through a `SetOnObjectReparented` hook mirroring `SetOnObjectDeleted`, the state-held body — keeping every body-reading site (gas ownership, mutable-ref ownership, GetObject, pod execution) consistent with the tracker.
- **Delete (kind 1):** requires `controls(sender, object)`, the object in `mutable_refs` at its current version, and `childCount(object)==0`; settles through the existing `settleDeletion` (deposit release, 95/5 refund/burn, tracker removal, `events.ObjectDeleted`), decrements the parent's child count, and triggers the state hook so holders drop the body (wire a `SetOnObjectDeleted`-style hook mirroring `SetOnObjectCreated`).
- The pod carve-out channel (`tx.deleted_objects` in globally-executed transactions) gains the SAME `childCount==0` gate and parent-count decrement inside `settleDeclaredDeletions`, so `merge` keeps working and no channel can orphan children.

- [ ] **Test:** transfer to another key succeeds and only the root object's tracker entry changes; reparent under a non-controlled `ObjectParent` fails; cycle fails; version increments once per touched object; a dependent list (`delete X` then `reparent Y under X`) fails on the second op and applies nothing; sequential semantics: `reparent A under B` then `reparent C under A` succeeds in one tx; a tx with ops AND a pod function is rejected; deleting a leaf refunds 95% and burns 5% (assert supply delta); deleting a parent with children fails, through BOTH channels; a non-holder node processes the same delete without holding the body.
- [ ] **Run, expect FAIL → implement → PASS;** re-run the supply-invariant tests (`go test ./internal/consensus/ -run TestSupply -count=1 -timeout 120s`).
- [ ] **Commit:** title `Reparent, transfer, and delete as protocol-declared operations`.

### Task 1.6: Creation permission and pod-output lockdown

**Files:** Modify `internal/state/state.go`: `validateOutput` (`state.go:353`) gains the created-parent rule and the deletion restriction; `applyUpdatedObjects` (`state.go:440`) rejects parent changes; wire `SetParentValidator(fn func(kind byte, parent Hash, sender Hash, tx *types.Transaction) bool)` so state can ask consensus for the walk; Test `internal/state/state_test.go`.

**Interfaces — Produces:** a created object's parent must satisfy: KeyRoot == sender, or `controls(sender, parent)`, or parent is referenced by this tx through a domain ref (`ObjectRef.domain != ""`, the existing shared-access exemption). `updated_objects` whose owner/parent bytes differ from the input object's are rejected (input objects are in the ATX at the checked version — a pure local compare, no tracker needed). Pod-output `deleted_objects` (the `podio.fbs` field) is valid only when the tx is globally executed (`CreatedObjectsReplicationLength() > 0` or all mutable refs are singletons — the `merge` carve-out); otherwise the output is rejected.

- [ ] **Test:** creating under someone else's key succeeds (gift; deposit paid by creator); under an owned object succeeds; under a domain-referenced table succeeds; a pod flipping owner in `updated_objects` reverts the tx; `merge` (singleton coins) still deletes its source coin; a pod deleting a sharded object in a non-creating tx reverts.
- [ ] **Run, expect FAIL → implement → PASS.**
- [ ] **Commit:** title `Creation permission rule and pod-output lockdown`, body `[!] closes the third parent write path (pods) and spam-attach`.

### Task 1.7: Client builders for declared operations

**Files:** Modify `pkg/client/transactions.go` (add `Reparent` and `DeleteObject` builders; rebuild `TransferObject` — today a `transfer_object` pod call at `transactions.go:83` — and `Wallet.Transfer` on declared ops), keep `pkg/client/sponsored.go` working over ops txs; Test `pkg/client/client_test.go`.

**Interfaces — Produces:** `(w *Wallet) TransferObject(...)` and siblings; each puts the object in `mutable_refs` with its current version and one `DeclaredOp`; a pure transfer no longer executes WASM. Gas rules unchanged; sponsorship (fee_payer + sponsor_signature) works over ops.

- [ ] **Test:** builder output round-trips `verifyTxAuthenticity`; refs and op fields match inputs; a sponsored declared-op transfer verifies.
- [ ] **Run, expect FAIL → implement → PASS.**
- [ ] **Commit:** title `Client builders for reparent, transfer, delete`.

### Task 1.8: Retire pod transfers

**Files:** Modify `pods/pod-system/src/lib.rs` (drop the `transfer` and `transfer_object` dispatch entries, lines 23-24, and their function dirs); `make -C pods/pod-system release`; fix any scenario helper still building pod transfers.

- [ ] **Commit:** title `Retire pod-level transfer entrypoints`, body `[-] transfer/transfer_object from the system pod`.

### Task 1.9: Hierarchy scenario

**Files:** Create `test/scenarios/scenario_hierarchy_test.go` (`TestScenarioHierarchy`, ~5 nodes); add the corpus-table row in `test/TESTING.md` (same commit).

- [ ] **Scenario:** create a nested chain (object under key, object under object) and assert `state.object.created` carries the parent; transfer the root object to another key (declared op) and assert only the root's version moved; reparent with a cycle attempt is rejected (`tx.committed` success=false); delete a leaf and assert `state.object.deleted` + `fees.deposit.refunded` + the supply invariant at teardown; delete-with-children is rejected; a sponsored declared-op transfer commits with `fees.deducted` naming the sponsor's coin.
- [ ] **Run the batch's scenario battery, one at a time:** `TestScenarioHierarchy`, `TestScenarioObjects`, `TestScenarioSponsored`, `TestScenarioFees`, `TestScenarioConsensusBasics` — `go test ./test/scenarios/ -run TestScenarioX -v -count=1 -timeout 5m` each. Fix fallout (the retired pod transfer touches several scenarios' helpers).
- [ ] **Commit:** title `Hierarchy scenario: cascade ops under live consensus`. **Push the batch.**

---

# Batch 2 — SMT primitive and the four trees

**Spec:** §4. **Design:** `internal/index`, no import of `internal/consensus`; fed via interfaces. Reference semantics: Jellyfish Merkle Tree; binary SMT over BLAKE3(key), per-level default hashes for empty subtrees, only non-empty paths materialized. No new events (the index is derived state, not a new mutation class).

### Task 2.1: SMT core

**Files:** Create `internal/index/smt.go`; Test `internal/index/smt_test.go`.

**Interfaces — Produces:** `type SMT` with `Insert(key, value []byte)`, `Delete(key []byte)`, `Root() [32]byte`, `Get(key []byte) ([]byte, bool)`. Leaf hash `blake3(0x00 || keyHash || blake3(value))`, internal `blake3(0x01 || left || right)`, `defaultHash[depth]` precomputed; key position = `blake3(key)`, 256 levels, path compression (a subtree with one leaf collapses to that leaf, JMT-style).

- [ ] **Test:** insertion-order independence (same set, shuffled, same root); incremental insert+delete equals from-scratch rebuild; empty tree root equals `defaultHash[0]`; 10k-entry root stable across two builds.
- [ ] **Run, expect FAIL → implement → PASS.**
- [ ] **Commit:** title `Sparse Merkle Tree core`.

### Task 2.2: Inclusion and absence proofs

**Files:** Create `internal/index/proof.go`; Test `internal/index/proof_test.go`.

**Interfaces — Produces:** `Prove(key []byte) Proof`, `Verify(root [32]byte, key, value []byte, p Proof) bool` (absence: `value == nil` verifies against the default/other-leaf at the key's position). `Proof` is `{Siblings [][32]byte, Leaf []byte}` serializable with FlatBuffers-free plain encoding (length-prefixed), since clients re-implement it.

- [ ] **Test:** inclusion verifies; absence verifies for a missing key; a tampered value, wrong root, or truncated sibling list fails; proof size ~`log2(n)` siblings.
- [ ] **Run, expect FAIL → implement → PASS.**
- [ ] **Commit:** title `SMT inclusion and absence proofs`.

### Task 2.3: Domain, parent, and children trees

**Files:** Create `internal/index/domain_tree.go` and `internal/index/hierarchy_trees.go`; Test `internal/index/trees_test.go`.

**Interfaces — Produces:** typed wrappers with leaf codecs: `DomainTree` (key `blake3(name)`, leaf `{name, objectID, owner, expiryEpoch}`), `ParentTree` (key `blake3(childID)`, leaf `{childID, parentKind, parentBytes}`), `ChildrenTree` (two-level: top key `blake3(parentID)` → child-subtree root; subtree key `childID` → `present`); `SetEdge(child, kind, parent)` / `RemoveEdge` update ParentTree and ChildrenTree together (the two views of one edge set).

- [ ] **Test:** enumeration completeness (subtree root recomputed from streamed leaves matches the top-tree leaf); an ancestry walk over ParentTree leaves terminates at a KeyRoot kind; edge move (reparent) updates both trees consistently.
- [ ] **Run, expect FAIL → implement → PASS.**
- [ ] **Commit:** title `Domain, parent, and children trees over the SMT`.

### Task 2.4: Validator tree and the combined root

**Files:** Create `internal/index/validator_tree.go` (`ValidatorTree`: key `blake3(pubkey)`, leaf `{pubkey, cappedStake, blsKey, status}`; during the genesis epoch the tree tracks the live registration set, first frozen at the first boundary — spec §4); create `CombinedRoot(domain, parent, children, validator [32]byte) [32]byte = blake3(d || p || c || v)`; Test.

- [ ] **Test:** rebuild from a validator snapshot is deterministic; combined root changes when any sub-root changes.
- [ ] **Run, expect FAIL → implement → PASS.**
- [ ] **Commit:** title `Validator tree and combined index root`.

### Task 2.5: Index manager wired to the commit path

**Files:** Create `internal/index/manager.go`; modify `internal/consensus/commit.go` + `epoch.go` (feed the manager: object created/reparented/deleted from the apply and settle paths, domain writes, epoch validator snapshot at `transitionEpoch`'s holder-freeze step, `epoch.go:47`) behind a narrow `indexer` interface field on `DAG` (nil-safe: no-op when unset); modify `cmd/node/init.go` to construct and inject it, **and on a restart with an existing data dir call `BuildFromState` from the persisted tracker, domain store, and current epoch holders BEFORE production starts** (a restarted node with an empty index would anchor wrong roots and be silently excluded by peers — add a restart test); Test `internal/index/manager_test.go`.

**Interfaces — Produces:** `Manager` with `ApplyEdge(child Hash, kind byte, parent Hash)`, `RemoveObject(child Hash)`, `ApplyDomain(...)`/`RemoveDomain(name)`, `RebuildValidators(entries []ValidatorLeaf)`, `Root() [32]byte`, `RootAt(round uint64) ([32]byte, bool)` (bounded history: 1,000 rounds + one per epoch), `SetFrontier(round uint64)` called once per committed batch. `BuildFromState(trackerEntries, domainEntries, validatorEntries)` for boot/sync rebuild.

- [ ] **Test:** applying a synthetic committed stream vs `BuildFromState` on the final state → identical root; `RootAt` returns historical roots inside the window and `false` outside; restart rebuild matches the never-restarted twin.
- [ ] **Run, expect FAIL → implement → PASS;** `go build ./... && go vet ./...`; fast gate `go test -short ./... -timeout 120s`.
- [ ] **Commit:** title `Index manager fed by the commit path`.

### Task 2.6: Incremental SMT behind the same API

**Why (review-mandated, 2026-07-20):** the task-2.1 core computes `Root()` as a full O(n) functional recompute; spec §7 promises "each committed batch rehashes only the SMT paths its transactions touched (a few thousand BLAKE3 hashes, sub-millisecond)". The manager cannot reduce the complexity class from outside the tree, and batch 3 puts `Root()` on the consensus hot path (every committed batch). The functional core stays — it is the differential oracle.

**Files:** Extend `internal/index/smt.go` (or a sibling `smt_inc.go` under the 300-line rule); Test `internal/index/smt_diff_test.go`.

**Interfaces — unchanged:** the public API (`Insert/Delete/Get/Root/Prove/Verify`) does not move; callers never change. Internally the tree materializes non-empty paths and memoizes subtree hashes so a mutation dirties only its root-to-leaf path; `Root()` rehashes dirty paths only; `Prove` reuses the materialized path (no full-set re-sort per call — the anchor handler generates proofs on the query path).

- [ ] **Test (differential, the real "incremental == rebuild"):** a seeded randomized sequence of insert/overwrite/delete (≥5k steps) asserting after EVERY step that the incremental root equals a from-scratch functional recompute (the 2.1 oracle, kept callable from tests); the 2.1/2.2 suite passes unchanged; a benchmark demonstrating a single-key update at 100k entries costs O(log n) hashes (assert a hash-count or wall bound, not vibes); the negative absence-proof and oversized-proof guards still pass.
- [ ] **Run, expect FAIL → implement → PASS;** full package + fast gate.
- [ ] **Commit:** title `Incremental SMT: dirty-path rehash behind the unchanged API`. **Push the batch.**

---

# Batch 3 — Detached header, anchoring, three-stage enforcement

**Spec:** §5, §7.

**Context (verified against main @ 397d6f9):** `buildVertex` (`internal/consensus/build.go:18-34`) signs `hashVertex(unsigned)` = BLAKE3 of the whole unsigned vertex (`hash.go:10`); `validateSignature` (`validate.go:117`) verifies over `HashBytes()`. The vertex `epoch` field is populated from the STATIC `d.epoch` construction-time field (`build.go:49/:77`; `cmd/node/init.go` passes 0) and `validateEpoch` (`validate.go:90`) compares against the same static field — while the `d.Epoch()` accessor returns the live `d.currentEpoch` (`dag.go:743`); this divergence is what Task 3.1 fixes. The vertex table (`types/vertex.fbs:61-92`) is hash/round/producer/signature/parents/transactions/epoch/fee_summary/timestamp. Parents link by vertex hash; gossip dedup hashes whole message bytes (`network/dedup.go:44`), so the identity change is contained to consensus. `buildFeeSummary` is in `build.go:89`, `validateFeeSummary` in `validate.go:257`.

**Breaking format:** the header hash changes every vertex's identity. Nodes upgrade in lockstep and data dirs are wiped (pre-mainnet, acceptable and stated); bump `snapshotVersion` (14 → 15) in this batch, since old snapshot vertices can no longer validate.

### Task 3.1: Detached provable header

**Files:** Modify `types/vertex.fbs` (append `frontier_round:uint64`, `index_root:[ubyte]`, `body_hash:[ubyte]` to `Vertex`); regenerate; modify `internal/consensus/build.go` (compute `bodyHash = blake3(parents || transactions || fee_summary || timestamp)` — round lives in the header only — then `hash = blake3(producer || round || epoch || frontier_round || index_root || bodyHash)`; sign `hash`; populate `epoch` with the live epoch (atomic mirror of `currentEpoch`, written under `commitMu` at each transition — a direct `commitMu` read on the submit path couples client latency to commit batches)) and `internal/consensus/validate.go` (`validateSignature` recomputes `bodyHash` and the header hash; `validateEpoch` reworked to a receiver-independent consistency window: accept `v.Epoch()` within ±1 of `commitEpochForRound(v.FrontierRound())`, reject everything else; with `epochLength == 0` require epoch 0. Rationale (third design, scenario-proven): the epoch field is stamped from the producer's COMMIT clock (its live epoch) while a round-derived bound validates it against the PRODUCTION clock — two clocks that diverge unboundedly whenever commit lag exceeds one epoch length. Three witnesses from the batch-3 battery: a partition-healed node (commit 153, round 546, epoch 1) had every vertex rejected `wrong_epoch` network-wide; a cold-restarted holder resuming production below its cursor hits the same wall (the exact shape the first review flagged); Stress with epochLength 50 makes the lag trivial under load. The frontier and the epoch come from the same commit state, so validating them against each other is bounded, receiver-independent, honest under any lag, and it is what Task 3.5 already consumes (bundle epoch re-derived from the frontier). Earlier iterations, both unsound: `currentEpoch`-relative (terminal-rejects tip vertices a lagging node must buffer) and round-derived ±1 (rejects honest production whenever |commit − round| > epochLength); Test `internal/consensus/build_test.go`.

**Interfaces — Produces:** the header hash **is** the vertex identity (parent links, store keys — unchanged code, changed meaning); `headerBytes(v *types.Vertex) []byte` and `computeBodyHash(v *types.Vertex) [32]byte` shared by build and validate (one function, two call sites, cannot drift).

- [ ] **Test:** a vertex verifies end-to-end; tampering with a transaction changes `bodyHash` and breaks the signature; tampering with `index_root` breaks it too; a light verification using only `{producer, round, epoch, frontier_round, index_root, bodyHash, signature}` (~200 bytes) accepts without the body; boundary skew: a vertex produced in epoch N arriving after the receiver transitioned to N+1 validates inside the window.
- [ ] **Run, expect FAIL → implement → PASS;** full consensus package tests.
- [ ] **Commit:** title `Detached provable vertex header`, body `[&] header hash becomes the vertex identity; epoch field populated`.

### Task 3.2: Producers anchor their committed frontier

**Files:** Modify `internal/consensus/build.go` (anchor the pair from the indexer seam's `CommittedFrontier()` — added as-built to `internal/consensus/indexer.go` and `internal/index/manager.go`, returning `(frontier, root-at-frontier)` atomically under its own leaf lock, because no read seam existed and the commit loop mutates trees between `SetFrontier` calls. `Manager.Root()` is the LIVE uncommitted root: it must never be anchored into a vertex nor compared against a received one — 3.4 in particular must not reach for it; zero values when indexer unset); Test.

- [ ] **Test:** two nodes at the same committed frontier produce vertices carrying identical `(frontier_round, index_root)`.
- [ ] **Run, expect FAIL → implement → PASS.**
- [ ] **Commit:** title `Anchor the index root in every produced vertex`.

### Task 3.3: Ingress root check (stage 1)

**Files:** Create `internal/consensus/rootcheck.go`; modify `internal/consensus/validate.go` (`validateVertex`: if the frontier is at or below the receiver's committed index frontier and the indexer is set, require `RootAt(frontier) == index_root`; unverifiable-yet vertices pass. As-built: the gate reads the seam's `CommittedFrontier()`, not the DAG cursor — provably the same rejection set since retained rounds never exceed the seam frontier, and it avoids both the next-vs-last cursor trap and `commitMu` on the gossip path); modify `cmd/node/sync.go` — both sync-side construction paths (`initConsensusForListener`/`initConsensusForValidator`) call `initIndex` after snapshot apply, pulled forward from Task 5.1: once stage 1 is active, the harness's bootstrap-joined nodes 1..N-1 anchor `(0, zero)` and node 0 would reject every peer vertex past the first epoch boundary, wedging every epoch-enabled scenario (Task 5.1 keeps the fail-closed verification and the trusted checkpoint); modify `internal/index/manager.go` — `RootAt` reads `history`/`epochCheckpoints` with no lock, safe only while every caller sits on the commit path; this task puts it on ingress goroutines concurrent with the commit loop's writes (a fatal concurrent map access), so bring those maps under `frontierMu` (write-lock on the commit path, `RLock` in `RootAt`); Test `internal/consensus/rootcheck_test.go`.

- [ ] **Test:** a wrong-root vertex whose frontier the receiver has committed is rejected; a vertex anchoring a future frontier is accepted; a frontier older than the retention window (no `RootAt` entry, no epoch checkpoint) is treated as unverifiable and passes (parents are always recent, so production never depends on stale roots); a zero-root vertex passes during the genesis epoch ONLY and is rejected like a wrong root from the first epoch boundary on (spec §5) — and the genesis tolerance keys on the VERTEX's own round (`commitEpochForRound(v.Round()) == 0`), never on the receiver's current epoch: receiver-relative rules are the unsoundness class the 3.1 adjudication removed (a genesis vertex buffered across the first boundary by deep-gap recovery or late gossip must not be terminally rejected). The new rejection reason joins `consensus.vertex.rejected`'s fixed reason set (new value, e.g. `index_root` — update the catalog comment and `test/TESTING.md`'s reason list in the same commit).
- [ ] **Run, expect FAIL → implement → PASS.**
- [ ] **Commit:** title `Ingress root verification when the frontier is local`.

### Task 3.4: Verify-before-reference at production (stage 2) and commit re-check (stage 3)

**Files:** Modify `internal/consensus/dag.go` (`collectParents` filters to parents whose anchor is verified — `RootAt` match, or zero-root during the genesis epoch only); extend `rootcheck.go` with `recheckCommittedAnchor(v)` called from the commit loop's apply path; fault evidence persisted under a `fault/` Pebble prefix `{producer, round, claimed, computed, headerBytes, signature}` and emitted as a NEW event `consensus.anchor.fault` {producer, round, claimed, computed} (constructor in `internal/events/consensus.go` + catalog + `test/TESTING.md` rows, same commit); Test.

- [ ] **Test:** an honest producer never references a wrong-root vertex (it can win the round only when the liar is excluded); a committed vertex with a wrong root (forced through a crafted store) writes exactly one fault record with a verifying signature over the lying header, and emits the event.
- [ ] **Run, expect FAIL → implement → PASS.**
- [ ] **Commit:** title `Verify-before-reference and commit-path fault evidence`.

**As-built (landed 6d7f6d5):** (a) stage 2 is a denylist of PROVEN liars, not the allowlist above — measured: the allowlist kept 0 of 3 parents at frontier skew 3-vs-9 and a parentless vertex is rejected, silencing lagging producers; (b) the fault record is `header(120)‖signature(64)‖computed(32)` under 38-byte `fault/` keys, producer/round/claimed derived from the header at the normative offsets; (c) stage 2 also applies in sync mode, composed after `trustedParents`; (d) `referenceableParents` also drops a candidate whose vertex the store cannot read. Scenario exemption: `consensus.anchor.fault` and `consensus.vertex.quarantined` both need a byzantine producer the harness does not offer; covered by crafted-store package tests (incl. the end-to-end wedge regression), scenarios deferred to the byzantine harness axis.

**Fix wave (review finding, spec §5 amended):** terminal ingress rejection of wrong-root vertices is a partition lever — a vertex some nodes refuse to store, smuggled into committed causal history through references (an honest laggard suffices as-built; two colluders suffice even under the allowlist, since stage 2 checks the direct parent's anchor, not its ancestry), stalls the refusing nodes' commit forever (causal batch incompletable, refetch re-rejected, `RootAt` retention never slides). Resolution: QUARANTINE — a proven-wrong vertex is stored and served on request but never relayed and never referenced; stage 3 then convicts it on every node when laggard/colluder references commit it. Requirements: the quarantine verdict reuses `anchorLie`; the event taxonomy change is called out (the vertex is stored, not dropped — rename or re-attribute `consensus.vertex.rejected reason=index_root` accordingly, TESTING.md same commit); regression test = the wedge shape end to end (ahead-node DAG completes a causal batch containing a quarantined lie, writes exactly one fault record); the lagging-producer liveness test and the 11-case stage-1 table stay green with REJECT cases becoming QUARANTINE. Cosmetics folded in: `makeFaultKey` uses `make+copy` like sibling key builders; the standalone-evidence test asserts `BLAKE3(0x01‖storedBytes[0:120])` directly.

### Task 3.5: Quorum bundle assembly and `GetIndexAnchor`

**Files:** Modify `internal/network/messages.go` (tags `0x1D MsgTagGetIndexAnchor` / `0x1E MsgTagGetIndexAnchorResp` — 0x15-0x1C are taken by vertex fetch, fingerprint, test control, and range fetch — added to `clientRequestTags` at `messages.go:130` or the connection classifier will not route them); create the handler in `cmd/node/indexhandlers.go` (new file — `clienthandlers.go` is at 569 lines; serve the cached bundle: highest frontier where headers matching `(frontier, root)` reach the stake quorum within a 16-round sliding window; recompute lazily per committed batch); Test at the handler level.

**Interfaces — Produces:** response = `{frontier_round, index_root, headers: [~200B each], epoch}`; assembly reuses the capped-stake quorum test from `stake.go`. The header wire layout is the NORMATIVE contract pinned in `internal/consensus/header.go` (golden-vector tested); bundle assembly re-derives the expected epoch from the frontier round (`commitEpochForRound`) rather than trusting the header field, which the 3.1 window bounds only to ±1. The 16-round sliding window is anchored at the serving node's own `CommittedFrontier()`, never at the highest CLAIMED frontier seen — otherwise one absurd-future anchor (e.g. 10^9) drags the window where no quorum exists and denies bundles to every client. **As-built (7085295):** assembly lives in `internal/consensus` (`AnchorBundle`, `(*DAG) IndexAnchorBundle()`) reusing the package-private capped-stake helpers — there is NO exported "header ‖ signature from a vertex" accessor; the batch-6 light client decodes the 184-byte record positionally (`producer@0:32, round@32:40, epoch@40:48, frontier@48:56, root@56:88, bodyHash@88:120, sig@120:184`) per `internal/consensus/header.go`'s normative layout. `Headers` carries one record per distinct MATCHING producer (non-members weigh zero; the client recomputes stake from the epoch set).

- [ ] **Test:** with 4 simulated producers the bundle reaches quorum and verifies; a minority wrong-root producer is excluded from the bundle.
- [ ] **Run, expect FAIL → implement → PASS.**
- [ ] **Commit:** title `GetIndexAnchor quorum bundles`.

### Task 3.6: Scenario regression on the new vertex format

- [ ] **Extend** `TestScenarioConsensusBasics` with an `index_anchor_quorum` subtest: after traffic commits, `GetIndexAnchor` from every node returns bundles whose `(frontier_round, index_root)` agree and reach quorum. This needs a new `pkg/client` verb for tag `0x1D` (nothing speaks it yet — `pkg/client/quic.go` has `Fingerprint()`/`TestControl()` as the pattern): add `GetIndexAnchor()` returning the decoded bundle, minimal surface, same task.
- [ ] **Run, one at a time:** `TestScenarioConsensusBasics`, `TestScenarioAggregation`, `TestScenarioStress`, `TestScenarioEpochs`, `TestScenarioPartition`, `TestScenarioCrash` — bounded timeouts (5-10m); fix fallout (the identity change touches everything that stores or fetches vertices). Epochs and Partition are the two scenarios that reproduce stalled-production-across-an-epoch-boundary, the shape the 3.1 epoch window exists for.
- [ ] **Commit:** title `Scenarios green on the anchored vertex format`. **Push the batch.**

---

# Batch 4 — Domain operations, rental, sweep, deposit term

**Spec:** §8.

**Context (verified against main @ 397d6f9):** the `domainStore` leaf is a bare 32-byte objectID (`internal/state/domain.go`: `get` :27, `set` :48, `delete` :57 — the delete is pre-wired scaffolding with no production caller). `applyRegisteredDomains` (`state.go:416-418`) is the only writer and already emits `events.DomainRegistered/DomainUpdated`; `events.DomainDeleted` is defined (`internal/events/state.go:41`) with zero callers. Fee formula: `CalculateFee` (`fees.go:138`, still carrying the `MaxCreateDomains × DomainFee` term at :169-170), `calculateTxFeeSplit` (`commit.go:1009`), summary lockstep `buildFeeSummary` (`build.go:89`) / `validateFeeSummary` (`validate.go:257`). `FeeParams` (`fees.go:10-18`): GasPrice, MinGas, TransitFee, StorageFee, DomainFee, BurnBPS, StorageRefundBPS. Epoch boundary work in `transitionEpoch` (`epoch.go:47`): deferred settlement → churn removals → holder freeze → eligible freeze → clear → epoch++ → event. The convergence fingerprint already digests domains (`hashDomains`, `internal/sync/fingerprint.go:52`), and the snapshot ships them as `SnapshotDomain{name, object_id}` (`snapshot.fbs:60-66`) — both change shape with the leaf and must land in this batch.

### Task 4.1: Domain store leaf and declared-op handlers

**Files:** Modify `internal/state/domain.go` (leaf `{objectID 32, owner 32, expiryEpoch 8}`, length-tolerant decode for old 32-byte values); extend `internal/consensus/ops.go` with kinds 2-6: register (absent name only; dotted name requires `sender == owner(immediateParentName)`; `system.*` rejected), renew (owner-only; also during grace), update/transfer/delete (owner-only); all feed the index manager and emit events: register/update/delete use the existing constructors (`DomainDeleted` gains its first caller), renew and transfer get NEW constructors `state.domain.renewed` {name, expiry, tx} and `state.domain.transferred` {name, owner, tx} (+ catalog + `test/TESTING.md` rows, same commit); Test `internal/consensus/ops_test.go`.

**Interfaces — Consumes:** domain reads through a narrow state accessor `DomainLeaf(name) (objectID, owner Hash, expiry uint64, ok bool)`. **Produces:** `expiry = max(currentExpiry, currentEpoch) + term`, and an op whose result would exceed `currentEpoch + maxTermEpochs` **reverts** (never clamps: the fee is `rate × term_epochs` from the header, and a clamped term would charge a fee that no longer matches the declared field). Domain ops carry NO object refs and increment NO object version (spec §3); `domain_register` and `domain_update` require `controls(sender, pointedObject)` — without it, anyone could alias a victim's object and reach it mutably through the domain-ref ownership exemption. Resolution (execution-time and queries) treats a name past `expiry_epoch` as absent, even during grace; grace only reserves the owner's renewal right.

- [ ] **Test:** FCFS on roots; `x.y` without owning `y` fails; renewal by a non-owner fails; a term pushing past the cap reverts and charges nothing but the base fee; registering a name pointing at a non-controlled object fails; an expired-in-grace name does not resolve at execution but renews for the owner; update repoints; transfer hands renewal rights; register on an existing live name fails; each successful op emits its event.
- [ ] **Run, expect FAIL → implement → PASS.**
- [ ] **Commit:** title `Domain declared operations with ownership and namespaces`.

**As-built (0ab4b97):** handlers live in `domainops.go`/`staged_domain.go` (300-line rule), not `ops.go`; `maxTermEpochs uint64 = 256` is a package constant that Task 4.2 MUST move into `FeeParams` AND rewire `domainExpiry` to read — otherwise the cap that reverts and the rent priced as `rate × term` drift apart under a governed parameter change; zero `term_epochs` is rejected; the seam is `DomainLeaf(name) (objectID, owner [32]byte, expiry uint64, ok bool)`; the composition-root wiring (`SetDomainStore` + `SetEpochSource`, `cmd/node/aggregation.go:80-81`) was written by this task — 4.4 must not remove or re-add it blindly. The tree leaf hashes all four fields, so the anchored root covers owner and expiry.

### Task 4.2: Rental and per-op fees, summary lockstep

**Files:** Modify `internal/consensus/fees.go` (`FeeParams` gains `RentalRatePerEpoch`, `MaxTermEpochs`, `GraceEpochs`, `ReparentFee`, `DeleteFee`, `IndexEntryFee`; `domainops.go`'s package constant `maxTermEpochs` is REPLACED by `FeeParams.MaxTermEpochs` and `domainExpiry` reads it — the cap and the pricing must move together); `calculateTxFeeSplit` (`commit.go:1009`) adds `Σ op fees` to the consumed part, rent = `safeMul(rate, term)`; `CalculateFee` + `buildFeeSummary` + `validateFeeSummary` in the same commit; Test `internal/consensus/fees_test.go`.

- [ ] **Test:** a register-for-10-epochs tx pays `10×rate` into the epoch pool; a vertex whose summary omits op fees is rejected; an ops tx with no pod call pays `min_gas` compute.
- [ ] **Run, expect FAIL → implement → PASS;** supply-invariant tests extended and green.
- [ ] **Commit:** title `Rental and declared-operation fees in the summary lockstep`.

**As-built (b9b7788):** fees are a pure function of the DECLARED header, validated at ingress — a domain op that later REVERTS at commit still pays `rate × declared term` (the summary cannot depend on commit-time outcomes; same semantics as a reverting pod call paying its gas, and it keeps cap-probing expensive). Task 4.1's earlier "charges nothing but the base fee" test wording was unimplementable and is superseded by this rule. Signatures later tasks build on: `CalculateFee(..., maxCreateDomains, ops []genesis.DeclaredOp, opsOnly bool, totalValidators, params)`; `domainExpiry(current, epoch, term, maxTerm)`; `newStagedView(ot, ds, epoch, maxTerm)`; the cap reads `(*DAG).maxTermEpochs()` with a 256 fallback when no fee system is wired — and the class that runs unwired in production is the LISTENER construction path (`initConsensusForListener` never calls `initAggregation`, so a listener has no domain store and no fee params: it reverts every domain op validators apply, and prices the cap from the fallback — a permanent index-root divergence plus a governed-parameter fork armed to fire the day `MaxTermEpochs` moves; listener mode is unreachable-broken in production today, which is why this is recorded rather than fixed here). This commit also promotes `feeParams` from "affects balances" to "affects the domain tree and thus the anchored index root". `GraceEpochs` is declared and 4.3's sweep fires at `expiry + GraceEpochs`.

### Task 4.3: Expiry sweep at the epoch boundary; snapshot and fingerprint carry the leaf

**Files:** Modify `internal/consensus/epoch.go` (`transitionEpoch`: call a `sweepExpiredDomains(newEpoch)` hook after `applyPendingRemovals` and before the holder freeze, wired to state + index manager; each swept name emits `state.domain.deleted` with a `reason:"expired"` attribute — a new attribute on an existing event is compatible); modify `types/snapshot.fbs` (`SnapshotDomain` gains `owner:[ubyte]`, `expiry_epoch:uint64`), `internal/sync/snapshot.go` (encode/decode; bump `snapshotVersion` 15 → 16) and `internal/sync/fingerprint_hash.go` (`hashDomains` digests owner + expiry); Test `internal/consensus/epoch_test.go` + snapshot/fingerprint round-trips.

- [ ] **Test:** a name expired beyond grace is removed from store and tree on the boundary, deterministically on two nodes, with the event; within grace it stops resolving for registration purposes but the owner can still renew; the sweep touches only expired leaves (root changes only when something is swept); snapshot round-trips owner/expiry; a one-bit owner difference changes the fingerprint.
- [ ] **Run, expect FAIL → implement → PASS.**
- [ ] **Commit:** title `Deterministic domain expiry sweep; snapshot and fingerprint carry the leaf`.

**As-built (b6f2a52 + 3f4f7c7):** tests live in `internal/consensus/domain_sweep_test.go` (file-size rule), not `epoch_test.go`; the determinism test is three sequential in-process DAGs leaning on Go map-iteration randomization (adequate — map order is the only entropy source; two-NODE agreement is proven by 4.5's scenario); "stops resolving within grace" is enforced by `State.ResolveDomain` since 4.1/4.2, not re-asserted here; `DomainStore` gained `ExportDomains()`; `DomainDeleted` gained a `reason` param (empty for owner deletes, `"expired"` + zero tx for sweeps). Sweep rule: `newEpoch > safeAdd(expiry, grace)`, strict, saturating.

### Task 4.4: Retire the pod domain path

**Files:** Modify `internal/state/state.go` (remove `applyRegisteredDomains`/`resolveDomainObjectID` AND `validateDomainName` in `internal/state/output_validation.go:80` — the pod path's weaker name rule must not survive the path; since 4.1 the pod path also writes unowned zero-expiry leaves no declared op can ever touch, one more reason it dies whole; `validateOutput` rejects a non-empty `registered_domains`), `internal/consensus/commit.go:723` (drop the `MaxCreateDomains` term from the global-execution guard), `internal/consensus/fees.go` (remove the `max_create_domains × DomainFee` term from `CalculateFee` at `fees.go:169-170` AND from `buildFeeSummary`/`validateFeeSummary` in the same commit; drop `DomainFee` from `FeeParams`), `internal/genesis/transaction.go` (zero/deprecate the `maxCreateDomains` builder parameter — three-site lockstep applies), `types/podio.fbs` + `types/transaction.fbs` (mark `registered_domains` and `max_create_domains` deprecated in comments; fields stay for layout stability); pod SDK docs note; Test.

- [ ] **Test:** a pod emitting `registered_domains` reverts; domain refs in `ObjectRef` still resolve at execution (read path untouched).
- [ ] **Run, expect FAIL → implement → PASS.**
- [ ] **Commit:** title `Retire the pod domain write path`.

### Task 4.5: Index-entry deposit term; domain scenario

**Files:** Modify `internal/consensus/fees.go` (`StorageDeposit`, `fees.go:191`) **and** `internal/state/state.go` (`computeStorageDeposit`, `state.go:669`) — same formula, same commit: `storage_fee × effective_rep / total_validators + index_entry_fee`; refund keeps the existing 95/5 over the whole `fees` field (the `txLocksDeposits` gate for fee-exempt registrations is untouched); REMOVE the dead pod-path domain wiring left by 4.4 (`cmd/node/indexing.go` `SetOnDomainRegistered` closure — its only caller died with `applyRegisteredDomains`, and if a caller ever returned it would write a zero-owner zero-expiry leaf into the anchored tree; plus the `SetOnDomainRegistered`/`onDomainRegistered` pair in `internal/state/state.go` — 4.4 review obligation); create `test/scenarios/scenario_domains_test.go` (`TestScenarioDomains`, 5 nodes, short `WithEpochLength`) + corpus-table row in `test/TESTING.md`.

- [ ] **Test (unit):** debit equals stamped deposit at creation; delete refunds 95% of (storage + index term); supply exact at the boundary.
- [ ] **Scenario:** register a name and wait `state.domain.registered` on every node (`WaitAll`); resolve from a different node; a second registration of the same name fails; renew moves expiry (`state.domain.renewed`); transfer hands the name (`state.domain.transferred`); let it expire and assert the sweep event lands on the boundary on every node and the name stops resolving; teardown invariants green (rent flows into the epoch pool; supply identity holds).
- [ ] **Run:** `TestScenarioDomains`, `TestScenarioEpochs`, `TestScenarioFees` one at a time, bounded.
- [ ] **Commit:** title `Flat index-entry term in the creation deposit; domain scenario`. **Push the batch.**

**As-built (2cfe5aa + a68d650 + fix wave):** `CalculateFee`'s storage term DELEGATES to `StorageDeposit` — an avoided regression, in scope per spec §8's own "same shared function" sentence (without it, the split's storage would exceed the fee's total; on a common 1-object tx the un-delegated version would have silently moved `IndexEntryFee` out of the pool). The batch's one consensus-visible behavior change: every created object costs +`IndexEntryFee` (25), stamped into the deposit, settled 95/5 on delete; refunds read the STAMPED deposit so parameter changes between create and delete cannot break them. Minimal client domain builders landed in `pkg/client/domain.go` (kind constants duplicated from `transactions.go` — batch 6 consolidates). `cmd/node/aggregation.go` was edited by this task (forced arity update — the batch closer carries its own integration). Listener-mode gap widens by one item (no `initFeeSystem` ⇒ zero-stamped deposits) — owned by Task 5.1 (d).

---

# Batch 5 — Sync: index rebuild and fail-closed verification

**Spec:** §9.

**Context (verified against main @ 397d6f9):** the join flow is `performSync` (`cmd/node/sync.go:134`: buffer for `SyncBufferSec`, then `requestAndApplySnapshot` :203, then `initConsensusForValidator`/`initConsensusForListener` :290/:254, then `replayBufferedVertices` :395). `buildValidatorSetFromSnapshot` (`sync.go:337`) already imports stakes and reward coins. The snapshot already carries the tracker with parents (batch 1), domain leaves with owner/expiry (batch 4), committed flags, epoch state, and the latch; vertex history is `vertexHistoryRounds = 100` (`internal/sync/manager.go:132`).

### Task 5.1: Rebuild the trees on snapshot apply

**Largely LANDED in batch 3 (commit 61e8fc7, the 3.3 fix wave):** both sync-side construction paths call `initIndex` after snapshot apply; the sync-shaped twin test in `cmd/node/index_test.go` proves rebuilt root == live root at the same frontier (via the restart-twin composition), with wrong-seed and missing-tracker mutations discriminating. Remaining for this task: (a) the DOMAIN leg of the twin (the batch-3 fixture had no domains — extend it once batch 4's leaves exist); (b) a joiner-holds-no-quarantine-marks note where the rebuild is documented (snapshot-imported vertices carry no `vq/` mark — correct, the joiner is in the "could not check" class, but the quarantine set is not reconstructible from a snapshot); (c) adjudicate the `WithIndexer` construction option (closes the `d.indexer` happens-before gap AND the decide-before-wire window — review carry-over); (d) the composition root must wire everything consensus-visible on BOTH construction paths — domain store, fee params, epoch source — and the parameter-accessor fallbacks (`maxTermEpochs()` 256, `graceEpochs()` 8) are then removed (fail-loud instead): a node class that commits the log and builds the index while reverting the ops validators apply is a permanent index-root divergence (today's listener path; unreachable-broken in production, but the next construction path inherits the same trap silently).

- [ ] **Test:** domain-leg twin (rebuilt root includes domain leaves, equals live).
- [ ] **Commit:** title `Index rebuild covers domains; indexer wired at construction`.

### Task 5.2: Fail-closed verification and the trusted checkpoint

**Files:** Modify `cmd/node/sync.go` (during replay, assemble anchors per frontier; go live only when a stake quorum matches the locally recomputed root for some frontier ≥ snapshot round; else abort with a typed error and emit `node.stopping` with `reason:"sync_unverified"`); add a `--trust-checkpoint epoch:rootHex` node flag, **mandatory for any non-genesis join** — without it the node refuses to sync; an explicit `--insecure-bootstrap` flag (loud warning) exists as an escape hatch. A default that trusts the snapshot's own validator set would let the bootstrap supply both the state and the judge, which is exactly the lie the spec promises to catch. **Harness wiring (same task):** `Cluster.Spawn`/`Restart` derive a real checkpoint from an alive node (`GetIndexAnchor` bundle → `epoch:root`) and pass `--trust-checkpoint`, so scenarios exercise the verified path, not the escape hatch; Test.

- [ ] **Test:** a tampered snapshot (one flipped tracker parent) fails sync with the event; an honest snapshot passes; no-quorum-in-history aborts rather than going live.
- [ ] **Run, expect FAIL → implement → PASS.**
- [ ] **Commit:** title `Fail-closed snapshot verification against the anchored root`.

**As-built (141199d, review-adjudicated):** the joiner's checkpoint pins `(epoch, VALIDATOR-SET root)`, not the combined index root (spec §5 amended with the two-checkpoint distinction — the light client keeps the full triple; the joiner cannot recompute a past frontier's combined root and a weakened pin is copyable). The harness derives the checkpoint from the source's newest `epoch.validators.frozen` event, NOT from a `GetIndexAnchor` bundle. New surface: `AnchorQuorumSince` on the DAG, `epoch.validators.frozen` event, `node.stopping reason=sync_unverified`, `--trust-checkpoint`/`--insecure-bootstrap` (hatch confined to harness genesis-committee formation). The gate has no freshness bound beyond frontier ≥ snapshot round (an eclipsing source can serve a genuine but old snapshot — documented, plan rule).

### Task 5.3: Join scenarios on the verified path

**Files:** Extend `test/scenarios/scenario_joining_test.go` (a joined node's `GetIndexAnchor` root matches the founders'; joining refuses a wrong `--trust-checkpoint` — assert the `node.stopping` reason and that the cluster's alive set is unaffected, `WithoutInvariants` NOT needed since the refused node never joins the convergence set).

- [ ] **Run, one at a time, bounded:** `TestScenarioJoining`, `TestScenarioJoinLoad`, `TestScenarioColdRestart`, `TestScenarioChurn`, plus `TestScenarioCrash`, `TestScenarioEpochCrash`, `TestScenarioAnchorCrash`. Intermittent history on join scenarios: loop Joining and JoinLoad ≥3 passes each; loop ColdRestart ≥3.

**As-built (batch-5 join/restart semantics, 0f8da59 + fix wave):** the 5.2 gate applies to FOREIGN state only. A node whose directory holds state it already ADOPTED resumes locally (initConsensus + ordinary gossip/backfill catch-up, new `runResume` + `node.resumed` event) — the first resurrector after a full extinction can never see a live quorum, so gating every restart made recovery impossible by construction. "Adopted" = durable marker `m:stateAdopted` (plus cursor and live set), written ONLY where the state became this node's own: after `verifySyncedState` returns nil in performSync, and in runBootstrap gated on `cfg.Bootstrap` (a refused join's residue never marks, and starting that residue without the upstream flag must not launder it). `ApplySnapshot` writes only exactly-32-byte non-consensus-prefix keys (a hostile snapshot could previously write ANY key, including the marker — the rule would be theatre). Empty-directory joins are bit-unchanged: gate, mandatory `--trust-checkpoint`, insecure hatch confined to harness genesis formation.
- [ ] **Commit:** title `Join scenarios cover verified snapshots`. **Push the batch.**

---

# Batch 6 — Client and API surface

**Spec:** §10.

**Context (verified against main @ 397d6f9):** client tags end at 0x1C; new pairs start at 0x1D (0x1D/0x1E used by batch 3). Handlers live in `cmd/node/clienthandlers.go` (569 lines; `handleDomainResolve` :525) and `cmd/node/indexhandlers.go` (batch 3). The CLI is `bpctl` (`cmd/cli/main.go`, dispatch :132-140; `object.go` subcommands create/show/set/transfer/holders; interactive console in `cmd/cli/tui/dispatch.go` with faucet/transfer/split/object/coins/objects/balance/pubkey verbs). The wallet already persists tracked object IDs (`pkg/client/wallet.go`, `walletFile.Objects`).

### Task 6.1: Query messages with proofs

**Files:** Modify `internal/network/messages.go` (extend the existing `DomainResolve` response **in place** with `{leaf, proof, frontier_round, index_root}` — no parallel proved message, minimal API on a pre-launch network; new tags `0x1F/0x20 ListChildren`, `0x21/0x22 GetAncestors`, all in `clientRequestTags`); handlers in `cmd/node/indexhandlers.go`; `ListChildren` always returns the top-tree proof plus the raw child-leaf stream (one mechanism at every size: the client rebuilds the subtree and checks its root — no threshold); Test handler-level.

**Interfaces — Produces:** `ResolveDomain(name) -> {leaf, proof}` (inclusion or absence); `ListChildren(parentID) -> {topProof, leaves}`; `GetAncestors(objectID) -> {edges: [{childID, kind, parent, proof}]}`; all responses carry `{frontier_round, index_root}` and are verified against the `GetIndexAnchor` bundle.

- [x] **Test:** resolve/enumerate/ancestry round-trips verify against the bundle; absence proof for an unregistered name verifies; a truncated leaf stream fails the client-side subtree-root check.
- [x] **Run, expect FAIL → implement → PASS.**
- [x] **Commit:** title `Proved index queries over QUIC`.

**As-built (32c4e5c, review-adjudicated):**

- Responses carry `ProvedIndexAnchor{Anchored, FrontierRound, IndexRoot, DomainRoot, ParentRoot, ChildrenRoot, ValidatorRoot}` — the four component roots, not `index_root` alone. A proof folds to one component root and the quorum signs only the combined root, so without the other three components no served proof is verifiable at all.
- `Anchored=false` is the answer between a tree mutation and the `SetFrontier` that closes the commit batch: the manager keeps no versioned trees, so a proof can only be taken against the live tree, and serving a `(round, root)` pair no frontier recorded would be unverifiable by construction. The client retries (spec §5's live unproven read). Anchor and proofs are read in the same critical section.
- Proved values travel as the raw leaf bytes the SMT hashed; `index.DecodeDomainLeaf`/`DecodeParentLeaf` exported so a verifier can read what the proof covered.
- `index.Manager` gained an exclusive `treeMu` (SMT `Root`/`Prove` memoize dirty-node hashes, so concurrent readers write shared state). Lock order: `commitMu` → `treeMu` → `frontierMu`, acyclic; no handler holds it across a network write.
- `handleDomainResolve` lives in `cmd/node/indexqueries.go` with the two new handlers. An expired-but-unswept name answers `Found=false` plus an inclusion proof whose leaf carries the expiry; the authenticated answer is the leaf, not `Found`.
- Ancestor walk bounded at 256, mirroring consensus's `walkDepthLimit` (constant duplicated: `internal/index` imports nothing from the repo by design).
- `ListChildren` travels in one frame under the transport's frame cap; a parent whose child set exceeds it is unserveable through this verb and the client sees a refused stream, not a defined answer. Known ceiling to keep in view when 6.3 makes `ListChildren(pubkey)` the wallet's recovery path.
- `pkg/client` gained NO transport verbs for `ListChildren`/`GetAncestors`, and its `DomainResolve` verb still returns only `(objectID, found)`, discarding the proof. Task 6.2 adds the proved verbs; its library docs must also state the freshness rule: a query answers at the node's committed frontier while the bundle serves the highest quorate frontier, so the client matches `IndexRoot`, not `FrontierRound`, and waits for the bundle when the index just moved.

### Task 6.2: Light-client verification library

**Files:** Create `pkg/client/verify.go` (checkpoint struct, `VerifyAnchor(bundle, validatorTree)`, `VerifyProof(root, key, value, proof)`, epoch walking via `GetValidatorTree` — add tags `0x23/0x24` + `clientRequestTags` + handler in the same commit); the library also exposes the spec §5 freshness choice: `WaitForFrontier(round)` (poll bundles until frontier ≥ round) vs an explicit unproven live read; Test `pkg/client/verify_test.go`.

- [ ] **Test:** a full flow against a fixture: checkpoint at epoch N → bundle whose headers carry epoch N+1 within the boundary window verified through the epoch-N validator tree (the spec §5 handoff rule) → the first N+1-attested root proves the new set → a domain proof verified against the bundle; a forged bundle below quorum fails; `WaitForFrontier` returns once a bundle covers the requested round.
- [ ] **Run, expect FAIL → implement → PASS.**
- [x] **Commit:** title `Light-client verification: checkpoint, epoch walk, proofs`.

**As-built (29dc0b6, review-adjudicated):**

- `VerifyProof` is a method on `VerifiedAnchor` taking `(anchor, component, key, value, proof)`: the combination check (`index.CombinedRoot == attested IndexRoot`) runs inside, where it cannot be forgotten. `bind` calls the `internal/index` function itself, so the combination order cannot drift.
- `WaitForFrontier(round, timeout)` takes an explicit bound.
- `GetValidatorTree` serves only the epoch the node's index tree currently describes: the manager keeps no versioned validator trees (`RebuildValidators` replaces wholesale), so this is forced, not chosen. User-visible property: a client more than one boundary behind cannot walk forward and needs a fresh checkpoint — weak subjectivity, stated on the wire doc.
- The served set is the whole leaf list with no Merkle proof: the quorum denominator is the committee's capped total and no inclusion proof authenticates a total; the client rebuilds the tree and matches the anchor's `ValidatorRoot`. `Manager.ValidatorSet()` reads set+anchor under one `treeMu` hold; the 6.1 lock order is preserved.
- `Checkpoint.authenticate` tries the strong link (components combine to the pinned index root) and falls back to the pinned `ValidatorSetHash` once the chain has moved past the checkpointed root — the ordinary case. Spec §5's light-client sentence amended to match; the security chain (out-of-band committee hash + capped-stake quorum) is the standard PoS light-client construction.
- The epoch walk is opportunistic: re-pin when the attested root proves the next committee, keep the old checkpoint otherwise; hard refusal at a full epoch of distance (window `{N, N+1}`, exact).
- Spec §5's "frontier falls within the boundary window" clause is NOT client-enforceable: nothing on the wire carries the epoch length. Adjudicated bounded — the hard stop at N+2 caps exposure at one epoch of stale-committee weighing, which the capped-churn overlap argument covers.
- The proved `ListChildren`/`GetAncestors` client verbs and their verification landed here (authorised by 6.1's as-built record); tests split `verify_test.go` / `lightclient_test.go`.
- The `GetValidatorTreeResponse` epoch label is unauthenticated by design (the set is authenticated by its root); task 6.4 must not assert on that field.

### Task 6.3: bpctl verbs and wallet switchover

**Files:** Modify `cmd/cli/main.go` dispatch + new `cmd/cli/domain.go` + `cmd/cli/object.go` + `cmd/cli/tui/dispatch.go` (`domain register|renew|update|transfer|delete|resolve`, `objects [owner|parent]`, `object parent <id>`, `object reparent|delete`; `object transfer` rebuilt on the batch-1 builder); `domain register` on a freshly created object is the named home of the spec §8 two-transaction saga (create, wait for commit, register); `pkg/client/wallet.go` becomes a cache: `objects` reads `ListChildren(pubkey)` recursively (depth-limited) and reconciles the local file; Test client-level.

- [x] **Test:** wallet recovery from a bare key repopulates tracked objects from the index; console verbs build valid declared-op transactions.
- [x] **Run, expect FAIL → implement → PASS.**
- [x] **Commit:** title `bpctl index verbs and wallet recovery from the index`.

**As-built (4afafcd, review-adjudicated):**

- Recovery lives in `pkg/client/recovery.go` (`Wallet.RecoverObjects`, `Client.EnumerateSubtree`, depth bound mirroring the consensus walk limit), not inside `wallet.go`; it never writes the wallet file itself — the console persists on exit, one-shot `bpctl` commands rebuild from the key and discard the recovered set.
- `childrenSource` seam: the walk accepts either `*LightClient` (verified) or `*Client` (unproven); both return `(nil, nil)` for a childless parent, so the two walks agree at the seam.
- Unproven CLI reads are permanent as shipped: nothing in `cmd/cli` can construct a `LightClient` (no checkpoint flag, no trust-on-first-use). Adjudicated OUT OF PLAN — spec §10 settles verification as a library function and 6.4's proved reads run through `pkg/client` directly. Known issue, supervisor decision.
- The saga is library-shaped (`Wallet.RegisterNewObjectDomain` + `Client.WaitForTx`); bpctl's `domain register` is flag-shaped, the console's is positional and exposes no saga.
- `tui/dispatch.go` split into three files (object/domain siblings) under the size rule; kind constants for domain ops consolidated in `pkg/client/domain.go`, `transactions.go`'s reparent/delete constants left in place.

### Task 6.4: End-to-end proved scenarios

**Files:** Extend `TestScenarioDomains` and `TestScenarioHierarchy` (batches 4 and 1) with the proved read path.

- [ ] **Scenario extensions:** register `demo.config` → resolve WITH proof verification from a *different* node against its `GetIndexAnchor` bundle; transfer an object subtree → `ListChildren` from the new owner's bare key returns the subtree with a verifying completeness proof; `GetAncestors` walks to the `KeyRoot`; wallet bare-key recovery repopulates from the live cluster.
- [ ] **Run, one at a time, bounded:** `TestScenarioDomains`, `TestScenarioHierarchy`, `TestScenarioBootstrap`, `TestScenarioStress`.
- [ ] **Commit:** title `End-to-end proved indexing scenarios`. **Push the batch.**

---

# Batch 7 — Dead code removal

**Spec:** §11.

### Task 7.1: Remove the dead trie registry

**Files:** Delete `pods/pod-sdk/src/domain_generated.rs` (verified still present); scrub any `TrieNode`/`DomainRegistry` reference; `make -C pods/pod-system release` green.

- [ ] **Commit:** title `Remove the abandoned on-chain-trie registry`, body `[-] domain_generated.rs (unreferenced)`.

### Task 7.2: Repo-wide regression

- [ ] **Run:** `go build ./... && go vet ./... && go test -short ./... -timeout 300s`; then one scenario per family, one at a time, bounded: `TestScenarioBootstrap`, `TestScenarioConsensusBasics`, `TestScenarioCrash`.
- [ ] **Commit:** title `Post-removal regression pass`. **Push the batch.**

---

# Batch 8 — Documentation

**Spec:** §11 (whitepaper consequences and the VISION retouch; the §5 commit-rule text landed with the prerequisite).

### Task 8.1: Whitepaper sections

**Files:** Modify `docs/WHITEPAPER.md`: object model (parent, cascade, creation rule), domains (authenticated index, owner, namespaces, rental + cap, lifecycle), transaction lifecycle (declared operations, either-ops-or-pod), consensus (detached provable header), fees (op fees, `index_entry_fee`, 95/5 unchanged), network (new messages), sync (fail-closed snapshot, trusted checkpoint); the "18 bytes per object" tracker figures become the new entry size. Follow the root `CLAUDE.md` doc conventions (one document of record, sober register, no em dashes).

- [ ] **Commit:** title `Whitepaper: verifiable indexing`.

### Task 8.2: VISION composability wording

**Why:** the chantier's doctrine is "synchronous atomic composability within a transaction, off-chain orchestration across transactions" (spec §1 non-goals, §11), but VISION's literal wording promises more than the protocol offers: "any pod can call any other pod atomically... in a single finalized step" and "there is no asynchronous boundary between applications" describe an inter-pod call primitive that does not exist — a transaction targets one pod and one function, the host interface has no cross-pod call, and a chain of dependent transactions is a client-orchestrated saga by design (spec §1). External copy (the landing) inherits its claims from VISION, so the overpromise propagates until VISION carries the doctrine.

**Files:** Modify `docs/VISION.md` only (spec §11 records this retouch):

- Cardinal properties, the "Global synchronous atomic composability" paragraph: the guarantee is per-transaction — a single transaction touches its declared objects (up to the reference cap) atomically and consistently in one finalized step, regardless of which holders own them; across transactions the client orchestrates, and zero rollback makes each finalized step solid ground that needs no compensating logic. Drop "any pod can call any other pod" and "no asynchronous boundary between applications".
- Positioning versus ICP: mirror the same correction — the contrast is holder-independent atomic multi-object transactions plus zero rollback, versus asynchronous, non-atomic cross-subnet calls with application-level rollback; do not claim synchronous cross-application calls.
- Sweep the rest of VISION for wording that leans on the old claim (the non-goals and Sui paragraphs reference "the synchronous composability above" and "uniform atomic composability") and keep them consistent with the reworded property.
- Keep VISION's register (opinionated, positioning; sentence-case headings, straight quotes, no em dashes) per the root `CLAUDE.md` conventions. The wedge stands: no fragmented blockchain offers holder-independent atomicity plus zero rollback.

- [ ] **Commit:** title `VISION: composability stated as the per-transaction guarantee`.

### Task 8.3: Final review

- [ ] **Check** `test/TESTING.md` is fully current: corpus table (2 new scenarios), event table (4 new events + new attributes/reasons), maintenance rule respected across the branch.
- [ ] **Run** the final whole-branch review (most capable model); fix findings autonomously; mark the PR ready when CI is green.
- [ ] **Commit** fixes if any. **Push.**

---

## Self-review (updated 2026-07-18 for the scenario-corpus base)

- **Spec coverage:** §2 landed on `main`; §3→batch 1; §4→batch 2; §5/§7→batch 3; §6→batches 1-2; §8→batch 4; §9→batch 5; §10→batch 6; §11→batches 7-8; §12's testing strategy is distributed into per-task tests and the scenario battery (determinism 2.1, proofs 2.2/6.1, anchoring 3.x, cascade/ops 1.x, economics 4.x, sync 5.x, removal 7.x).
- **Type consistency:** `controllerOf`/`controls`/`wouldCycle` (1.4) consumed by 1.5-1.6 and 4.1; `Manager.RootAt` (2.5) consumed by 3.3/3.5; `DomainLeaf` accessor (4.1) consumed by 4.3; header helpers (3.1) shared by build/validate; `settleDeletion` (existing) consumed by 1.5.
- **Re-integration deltas (2026-07-18), after the test-environment and bug-fix-campaign merges:** batch 0 removed (landed; see the record above — the relaxed-skip determinism follow-up formerly tracked here was fixed by the campaign's view-independence rule). Client tags renumbered from 0x1D (0x15-0x1C taken). Deletion accounting discovered already network-uniform (`tx.deleted_objects` + `settleDeclaredDeletions`): Task 1.5 reuses it instead of rebuilding it, and the DeclaredOp/`deleted_objects` split is stated (protocol delete vs pod carve-out). Snapshot AND fingerprint coverage move into the batches that change state (1.3, 4.3) because the harness's teardown convergence check would otherwise redden every join scenario mid-chantier. All validation converted from the retired `TestSim*` suite to named scenarios with per-run bounds; two scenarios added to the corpus (`TestScenarioHierarchy`, `TestScenarioDomains`); every new mutation emits a cataloged event (`state.object.reparented`, `consensus.anchor.fault`, `state.domain.renewed`, `state.domain.transferred`, first callers for `state.domain.deleted`), mirrored in `test/TESTING.md`.
- **Historical record:** the first implementation attempt (reverted 2026-07-13) and its lessons (C1-C3, I1-I8) shaped the landed batch 0 and are preserved in `main`'s git history and the batch-0 PR; they are no longer restated here.
- **Known deferred items (out of scope, from the spec):** committed-tx-hash pruning, `SyncBufferSec` scaling rules, slashing consumption of the batch-3 fault evidence.
