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
| 8 | Whitepaper updates | 11 | 2 |

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
- `docs/WHITEPAPER.md` (batch 8).

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

**Spec:** §5, §7.

**Context (verified against main @ 397d6f9):** `buildVertex` (`internal/consensus/build.go:18-34`) signs `hashVertex(unsigned)` = BLAKE3 of the whole unsigned vertex (`hash.go:10`); `validateSignature` (`validate.go:117`) verifies over `HashBytes()`. The vertex `epoch` field is populated from the STATIC `d.epoch` construction-time field (`build.go:49/:77`; `cmd/node/init.go` passes 0) and `validateEpoch` (`validate.go:90`) compares against the same static field — while the `d.Epoch()` accessor returns the live `d.currentEpoch` (`dag.go:743`); this divergence is what Task 3.1 fixes. The vertex table (`types/vertex.fbs:61-92`) is hash/round/producer/signature/parents/transactions/epoch/fee_summary/timestamp. Parents link by vertex hash; gossip dedup hashes whole message bytes (`network/dedup.go:44`), so the identity change is contained to consensus. `buildFeeSummary` is in `build.go:89`, `validateFeeSummary` in `validate.go:257`.

**Breaking format:** the header hash changes every vertex's identity. Nodes upgrade in lockstep and data dirs are wiped (pre-mainnet, acceptable and stated); bump `snapshotVersion` (14 → 15) in this batch, since old snapshot vertices can no longer validate.

### Task 3.1: Detached provable header

**Files:** Modify `types/vertex.fbs` (append `frontier_round:uint64`, `index_root:[ubyte]`, `body_hash:[ubyte]` to `Vertex`); regenerate; modify `internal/consensus/build.go` (compute `bodyHash = blake3(parents || transactions || fee_summary || timestamp)` — round lives in the header only — then `hash = blake3(producer || round || epoch || frontier_round || index_root || bodyHash)`; sign `hash`; populate `epoch` with `d.currentEpoch`, read under `commitMu`) and `internal/consensus/validate.go` (`validateSignature` recomputes `bodyHash` and the header hash; `validateEpoch` reworked to accept `currentEpoch` and `currentEpoch-1` within the boundary window, else every vertex crossing a boundary in flight is rejected — add a boundary-skew test); Test `internal/consensus/build_test.go`.

**Interfaces — Produces:** the header hash **is** the vertex identity (parent links, store keys — unchanged code, changed meaning); `headerBytes(v *types.Vertex) []byte` and `computeBodyHash(v *types.Vertex) [32]byte` shared by build and validate (one function, two call sites, cannot drift).

- [ ] **Test:** a vertex verifies end-to-end; tampering with a transaction changes `bodyHash` and breaks the signature; tampering with `index_root` breaks it too; a light verification using only `{producer, round, epoch, frontier_round, index_root, bodyHash, signature}` (~200 bytes) accepts without the body; boundary skew: a vertex produced in epoch N arriving after the receiver transitioned to N+1 validates inside the window.
- [ ] **Run, expect FAIL → implement → PASS;** full consensus package tests.
- [ ] **Commit:** title `Detached provable vertex header`, body `[&] header hash becomes the vertex identity; epoch field populated`.

### Task 3.2: Producers anchor their committed frontier

**Files:** Modify `internal/consensus/build.go` (read `Manager.Root()` + frontier from the indexer interface at production; zero values when indexer unset); Test.

- [ ] **Test:** two nodes at the same committed frontier produce vertices carrying identical `(frontier_round, index_root)`.
- [ ] **Run, expect FAIL → implement → PASS.**
- [ ] **Commit:** title `Anchor the index root in every produced vertex`.

### Task 3.3: Ingress root check (stage 1)

**Files:** Create `internal/consensus/rootcheck.go`; modify `internal/consensus/validate.go` (`validateVertex`: if `v.FrontierRound() <= lastCommitted` and the indexer is set, require `RootAt(frontier) == index_root`; unverifiable-yet vertices pass); Test `internal/consensus/rootcheck_test.go`.

- [ ] **Test:** a wrong-root vertex whose frontier the receiver has committed is rejected; a vertex anchoring a future frontier is accepted; a frontier older than the retention window (no `RootAt` entry, no epoch checkpoint) is treated as unverifiable and passes (parents are always recent, so production never depends on stale roots); a zero-root vertex passes during the genesis epoch ONLY and is rejected like a wrong root from the first epoch boundary on (spec §5). The new rejection reason joins `consensus.vertex.rejected`'s fixed reason set (new value, e.g. `index_root` — update the catalog comment and `test/TESTING.md`'s reason list in the same commit).
- [ ] **Run, expect FAIL → implement → PASS.**
- [ ] **Commit:** title `Ingress root verification when the frontier is local`.

### Task 3.4: Verify-before-reference at production (stage 2) and commit re-check (stage 3)

**Files:** Modify `internal/consensus/dag.go` (`collectParents` filters to parents whose anchor is verified — `RootAt` match, or zero-root during the genesis epoch only); extend `rootcheck.go` with `recheckCommittedAnchor(v)` called from the commit loop's apply path; fault evidence persisted under a `fault/` Pebble prefix `{producer, round, claimed, computed, headerBytes, signature}` and emitted as a NEW event `consensus.anchor.fault` {producer, round, claimed, computed} (constructor in `internal/events/consensus.go` + catalog + `test/TESTING.md` rows, same commit); Test.

- [ ] **Test:** an honest producer never references a wrong-root vertex (it can win the round only when the liar is excluded); a committed vertex with a wrong root (forced through a crafted store) writes exactly one fault record with a verifying signature over the lying header, and emits the event.
- [ ] **Run, expect FAIL → implement → PASS.**
- [ ] **Commit:** title `Verify-before-reference and commit-path fault evidence`.

### Task 3.5: Quorum bundle assembly and `GetIndexAnchor`

**Files:** Modify `internal/network/messages.go` (tags `0x1D MsgTagGetIndexAnchor` / `0x1E MsgTagGetIndexAnchorResp` — 0x15-0x1C are taken by vertex fetch, fingerprint, test control, and range fetch — added to `clientRequestTags` at `messages.go:130` or the connection classifier will not route them); create the handler in `cmd/node/indexhandlers.go` (new file — `clienthandlers.go` is at 569 lines; serve the cached bundle: highest frontier where headers matching `(frontier, root)` reach the stake quorum within a 16-round sliding window; recompute lazily per committed batch); Test at the handler level.

**Interfaces — Produces:** response = `{frontier_round, index_root, headers: [~200B each], epoch}`; assembly reuses the capped-stake quorum test from `stake.go`.

- [ ] **Test:** with 4 simulated producers the bundle reaches quorum and verifies; a minority wrong-root producer is excluded from the bundle.
- [ ] **Run, expect FAIL → implement → PASS.**
- [ ] **Commit:** title `GetIndexAnchor quorum bundles`.

### Task 3.6: Scenario regression on the new vertex format

- [ ] **Extend** `TestScenarioConsensusBasics` with an `index_anchor_quorum` subtest: after traffic commits, `GetIndexAnchor` from every node returns bundles whose `(frontier_round, index_root)` agree and reach quorum.
- [ ] **Run, one at a time:** `TestScenarioConsensusBasics`, `TestScenarioAggregation`, `TestScenarioStress`, `TestScenarioEpochs`, `TestScenarioCrash` — bounded timeouts (5-10m); fix fallout (the identity change touches everything that stores or fetches vertices).
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

### Task 4.2: Rental and per-op fees, summary lockstep

**Files:** Modify `internal/consensus/fees.go` (`FeeParams` gains `RentalRatePerEpoch`, `MaxTermEpochs`, `GraceEpochs`, `ReparentFee`, `DeleteFee`, `IndexEntryFee`); `calculateTxFeeSplit` (`commit.go:1009`) adds `Σ op fees` to the consumed part, rent = `safeMul(rate, term)`; `CalculateFee` + `buildFeeSummary` + `validateFeeSummary` in the same commit; Test `internal/consensus/fees_test.go`.

- [ ] **Test:** a register-for-10-epochs tx pays `10×rate` into the epoch pool; a vertex whose summary omits op fees is rejected; an ops tx with no pod call pays `min_gas` compute.
- [ ] **Run, expect FAIL → implement → PASS;** supply-invariant tests extended and green.
- [ ] **Commit:** title `Rental and declared-operation fees in the summary lockstep`.

### Task 4.3: Expiry sweep at the epoch boundary; snapshot and fingerprint carry the leaf

**Files:** Modify `internal/consensus/epoch.go` (`transitionEpoch`: call a `sweepExpiredDomains(newEpoch)` hook after `applyPendingRemovals` and before the holder freeze, wired to state + index manager; each swept name emits `state.domain.deleted` with a `reason:"expired"` attribute — a new attribute on an existing event is compatible); modify `types/snapshot.fbs` (`SnapshotDomain` gains `owner:[ubyte]`, `expiry_epoch:uint64`), `internal/sync/snapshot.go` (encode/decode; bump `snapshotVersion` 15 → 16) and `internal/sync/fingerprint_hash.go` (`hashDomains` digests owner + expiry); Test `internal/consensus/epoch_test.go` + snapshot/fingerprint round-trips.

- [ ] **Test:** a name expired beyond grace is removed from store and tree on the boundary, deterministically on two nodes, with the event; within grace it stops resolving for registration purposes but the owner can still renew; the sweep touches only expired leaves (root changes only when something is swept); snapshot round-trips owner/expiry; a one-bit owner difference changes the fingerprint.
- [ ] **Run, expect FAIL → implement → PASS.**
- [ ] **Commit:** title `Deterministic domain expiry sweep; snapshot and fingerprint carry the leaf`.

### Task 4.4: Retire the pod domain path

**Files:** Modify `internal/state/state.go` (remove `applyRegisteredDomains`/`resolveDomainObjectID`; `validateOutput` rejects a non-empty `registered_domains`), `internal/consensus/commit.go:723` (drop the `MaxCreateDomains` term from the global-execution guard), `internal/consensus/fees.go` (remove the `max_create_domains × DomainFee` term from `CalculateFee` at `fees.go:169-170` AND from `buildFeeSummary`/`validateFeeSummary` in the same commit; drop `DomainFee` from `FeeParams`), `internal/genesis/transaction.go` (zero/deprecate the `maxCreateDomains` builder parameter — three-site lockstep applies), `types/podio.fbs` + `types/transaction.fbs` (mark `registered_domains` and `max_create_domains` deprecated in comments; fields stay for layout stability); pod SDK docs note; Test.

- [ ] **Test:** a pod emitting `registered_domains` reverts; domain refs in `ObjectRef` still resolve at execution (read path untouched).
- [ ] **Run, expect FAIL → implement → PASS.**
- [ ] **Commit:** title `Retire the pod domain write path`.

### Task 4.5: Index-entry deposit term; domain scenario

**Files:** Modify `internal/consensus/fees.go` (`StorageDeposit`, `fees.go:191`) **and** `internal/state/state.go` (`computeStorageDeposit`, `state.go:669`) — same formula, same commit: `storage_fee × effective_rep / total_validators + index_entry_fee`; refund keeps the existing 95/5 over the whole `fees` field (the `txLocksDeposits` gate for fee-exempt registrations is untouched); create `test/scenarios/scenario_domains_test.go` (`TestScenarioDomains`, 5 nodes, short `WithEpochLength`) + corpus-table row in `test/TESTING.md`.

- [ ] **Test (unit):** debit equals stamped deposit at creation; delete refunds 95% of (storage + index term); supply exact at the boundary.
- [ ] **Scenario:** register a name and wait `state.domain.registered` on every node (`WaitAll`); resolve from a different node; a second registration of the same name fails; renew moves expiry (`state.domain.renewed`); transfer hands the name (`state.domain.transferred`); let it expire and assert the sweep event lands on the boundary on every node and the name stops resolving; teardown invariants green (rent flows into the epoch pool; supply identity holds).
- [ ] **Run:** `TestScenarioDomains`, `TestScenarioEpochs`, `TestScenarioFees` one at a time, bounded.
- [ ] **Commit:** title `Flat index-entry term in the creation deposit; domain scenario`. **Push the batch.**

---

# Batch 5 — Sync: index rebuild and fail-closed verification

**Spec:** §9.

**Context (verified against main @ 397d6f9):** the join flow is `performSync` (`cmd/node/sync.go:134`: buffer for `SyncBufferSec`, then `requestAndApplySnapshot` :203, then `initConsensusForValidator`/`initConsensusForListener` :290/:254, then `replayBufferedVertices` :395). `buildValidatorSetFromSnapshot` (`sync.go:337`) already imports stakes and reward coins. The snapshot already carries the tracker with parents (batch 1), domain leaves with owner/expiry (batch 4), committed flags, epoch state, and the latch; vertex history is `vertexHistoryRounds = 100` (`internal/sync/manager.go:132`).

### Task 5.1: Rebuild the trees on snapshot apply

**Files:** Modify `cmd/node/sync.go` (after snapshot apply: `Manager.BuildFromState(...)` from the imported tracker/domains/validator set, before consensus init); Test at the sync-manager level.

- [ ] **Test:** rebuilt root equals the root of a node that followed the same history live.
- [ ] **Run, expect FAIL → implement → PASS.**
- [ ] **Commit:** title `Rebuild the index from snapshot state`.

### Task 5.2: Fail-closed verification and the trusted checkpoint

**Files:** Modify `cmd/node/sync.go` (during replay, assemble anchors per frontier; go live only when a stake quorum matches the locally recomputed root for some frontier ≥ snapshot round; else abort with a typed error and emit `node.stopping` with `reason:"sync_unverified"`); add a `--trust-checkpoint epoch:rootHex` node flag, **mandatory for any non-genesis join** — without it the node refuses to sync; an explicit `--insecure-bootstrap` flag (loud warning) exists as an escape hatch. A default that trusts the snapshot's own validator set would let the bootstrap supply both the state and the judge, which is exactly the lie the spec promises to catch. **Harness wiring (same task):** `Cluster.Spawn`/`Restart` derive a real checkpoint from an alive node (`GetIndexAnchor` bundle → `epoch:root`) and pass `--trust-checkpoint`, so scenarios exercise the verified path, not the escape hatch; Test.

- [ ] **Test:** a tampered snapshot (one flipped tracker parent) fails sync with the event; an honest snapshot passes; no-quorum-in-history aborts rather than going live.
- [ ] **Run, expect FAIL → implement → PASS.**
- [ ] **Commit:** title `Fail-closed snapshot verification against the anchored root`.

### Task 5.3: Join scenarios on the verified path

**Files:** Extend `test/scenarios/scenario_joining_test.go` (a joined node's `GetIndexAnchor` root matches the founders'; joining refuses a wrong `--trust-checkpoint` — assert the `node.stopping` reason and that the cluster's alive set is unaffected, `WithoutInvariants` NOT needed since the refused node never joins the convergence set).

- [ ] **Run, one at a time, bounded:** `TestScenarioJoining`, `TestScenarioJoinLoad`, `TestScenarioColdRestart`, `TestScenarioChurn`. Intermittent history on join scenarios: loop the first two ≥3 passes each.
- [ ] **Commit:** title `Join scenarios cover verified snapshots`. **Push the batch.**

---

# Batch 6 — Client and API surface

**Spec:** §10.

**Context (verified against main @ 397d6f9):** client tags end at 0x1C; new pairs start at 0x1D (0x1D/0x1E used by batch 3). Handlers live in `cmd/node/clienthandlers.go` (569 lines; `handleDomainResolve` :525) and `cmd/node/indexhandlers.go` (batch 3). The CLI is `bpctl` (`cmd/cli/main.go`, dispatch :132-140; `object.go` subcommands create/show/set/transfer/holders; interactive console in `cmd/cli/tui/dispatch.go` with faucet/transfer/split/object/coins/objects/balance/pubkey verbs). The wallet already persists tracked object IDs (`pkg/client/wallet.go`, `walletFile.Objects`).

### Task 6.1: Query messages with proofs

**Files:** Modify `internal/network/messages.go` (extend the existing `DomainResolve` response **in place** with `{leaf, proof, frontier_round, index_root}` — no parallel proved message, minimal API on a pre-launch network; new tags `0x1F/0x20 ListChildren`, `0x21/0x22 GetAncestors`, all in `clientRequestTags`); handlers in `cmd/node/indexhandlers.go`; `ListChildren` always returns the top-tree proof plus the raw child-leaf stream (one mechanism at every size: the client rebuilds the subtree and checks its root — no threshold); Test handler-level.

**Interfaces — Produces:** `ResolveDomain(name) -> {leaf, proof}` (inclusion or absence); `ListChildren(parentID) -> {topProof, leaves}`; `GetAncestors(objectID) -> {edges: [{childID, kind, parent, proof}]}`; all responses carry `{frontier_round, index_root}` and are verified against the `GetIndexAnchor` bundle.

- [ ] **Test:** resolve/enumerate/ancestry round-trips verify against the bundle; absence proof for an unregistered name verifies; a truncated leaf stream fails the client-side subtree-root check.
- [ ] **Run, expect FAIL → implement → PASS.**
- [ ] **Commit:** title `Proved index queries over QUIC`.

### Task 6.2: Light-client verification library

**Files:** Create `pkg/client/verify.go` (checkpoint struct, `VerifyAnchor(bundle, validatorTree)`, `VerifyProof(root, key, value, proof)`, epoch walking via `GetValidatorTree` — add tags `0x23/0x24` + `clientRequestTags` + handler in the same commit); the library also exposes the spec §5 freshness choice: `WaitForFrontier(round)` (poll bundles until frontier ≥ round) vs an explicit unproven live read; Test `pkg/client/verify_test.go`.

- [ ] **Test:** a full flow against a fixture: checkpoint at epoch N → bundle whose headers carry epoch N+1 within the boundary window verified through the epoch-N validator tree (the spec §5 handoff rule) → the first N+1-attested root proves the new set → a domain proof verified against the bundle; a forged bundle below quorum fails; `WaitForFrontier` returns once a bundle covers the requested round.
- [ ] **Run, expect FAIL → implement → PASS.**
- [ ] **Commit:** title `Light-client verification: checkpoint, epoch walk, proofs`.

### Task 6.3: bpctl verbs and wallet switchover

**Files:** Modify `cmd/cli/main.go` dispatch + new `cmd/cli/domain.go` + `cmd/cli/object.go` + `cmd/cli/tui/dispatch.go` (`domain register|renew|update|transfer|delete|resolve`, `objects [owner|parent]`, `object parent <id>`, `object reparent|delete`; `object transfer` rebuilt on the batch-1 builder); `domain register` on a freshly created object is the named home of the spec §8 two-transaction saga (create, wait for commit, register); `pkg/client/wallet.go` becomes a cache: `objects` reads `ListChildren(pubkey)` recursively (depth-limited) and reconciles the local file; Test client-level.

- [ ] **Test:** wallet recovery from a bare key repopulates tracked objects from the index; console verbs build valid declared-op transactions.
- [ ] **Run, expect FAIL → implement → PASS.**
- [ ] **Commit:** title `bpctl index verbs and wallet recovery from the index`.

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

**Spec:** §11 (whitepaper consequences; the §5 commit-rule text landed with the prerequisite).

### Task 8.1: Whitepaper sections

**Files:** Modify `docs/WHITEPAPER.md`: object model (parent, cascade, creation rule), domains (authenticated index, owner, namespaces, rental + cap, lifecycle), transaction lifecycle (declared operations, either-ops-or-pod), consensus (detached provable header), fees (op fees, `index_entry_fee`, 95/5 unchanged), network (new messages), sync (fail-closed snapshot, trusted checkpoint); the "18 bytes per object" tracker figures become the new entry size. Follow the root `CLAUDE.md` doc conventions (one document of record, sober register, no em dashes).

- [ ] **Commit:** title `Whitepaper: verifiable indexing`.

### Task 8.2: Final review

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
