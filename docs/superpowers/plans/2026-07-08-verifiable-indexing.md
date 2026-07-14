# Verifiable Indexing Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development to implement this plan **one batch at a time** (one implementation subagent per batch). Within a batch, the subagent executes its tasks in order; **each task ends in one commit**. After a batch's tasks are all committed, **push** before starting the next batch. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Implement the verifiable-indexing spec end to end: a deterministic commit rule (prerequisite fix), the parent/cascade object model with protocol-declared operations, four authenticated SMT indexes anchored in every vertex through a detached provable header, domain economics with rental and enforced namespaces, fail-closed verifiable sync, and the client/API surface with light-client verification.

**Architecture:** Batch 0 fixes consensus determinism on its own branch (anchor-per-round, causal batches) because every root computation depends on it. The chantier then builds bottom-up on `verifiable-indexing`: parent metadata and declared operations first (they define the write paths), then the SMT trees derived from that metadata, then the anchor that exposes the root, then economics, sync, and surface. The index is derived state: every structure is rebuilt from the tracker and domain store, never shipped.

**Tech Stack:** Go 1.26, FlatBuffers (`types/*.fbs` → `internal/types` via `bash types/generate.sh`, requires `flatc`), BLAKE3 (`github.com/zeebo/blake3`), Pebble-backed storage, Rust/WASM system pod (`pods/pod-system`), existing `DAG`/`State`/`ValidatorSet`/`objectTracker` APIs.

**Spec:** `docs/superpowers/specs/2026-06-03-verifiable-indexing-design.md` (all 13 sections covered; the section-13 batch order is refined here into 9 batches).

**Branches:** Batch 0 on `fix/deterministic-commit`, branched from `main`, PR, squash-merge to `main` first. Batches 1-8 on `verifiable-indexing`, rebased onto `main` after batch 0 merges. Per `.claude/CLAUDE.md`, implementation runs in an isolated `.wt/<task>` worktree; open the draft PR after the first push and keep its body current.

## Global Constraints

- Go rules from `.claude/CLAUDE.md`: functions ≤ 25 lines, files ≤ 300 lines (split by responsibility), minimal exported API, docstrings everywhere, errors wrapped `fmt.Errorf("...:\n%w", err)`.
- Integer math on fees/supply/stake uses `safeMul`/`safeAdd` (`internal/consensus/fees.go`), never raw operators on attacker-influenced values.
- After any `types/*.fbs` change: `bash types/generate.sh`, then rebuild. FlatBuffers fields are append-only; never renumber or remove existing fields (deprecate in place).
- **Three-site lockstep:** any change to the canonical transaction body (new signed fields) must land in the same commit in `internal/genesis/transaction.go` (builder), `internal/validation/validate.go` (`rebuildUnsignedTx`), and `internal/consensus/txauth.go` (commit-path verify), which all delegate to `genesis.BuildUnsignedTxBytesWithRefs`.
- Unit tests: `go test ./internal/<pkg>/ -count=1 -timeout 120s`. Integration sims: run `TestSim*` **individually** with a bounded `-timeout` (project memory; never the whole suite unbounded).
- Commits follow the repo convention: title line without prefix, body lines prefixed `[+] [-] [&] [!]`, no footers.
- Each batch leaves `go build ./... && go vet ./...` green with the new code wired and reachable; batches touching `pods/` or `wasm-gas/` also leave `make -C pods/pod-system release` green.

---

## Execution model (batches)

| Batch | Subsystem | Spec § | Tasks |
|---|---|---|---|
| 0 | Deterministic commit: anchor per round, causal batches (own branch) | 2 | 8 |
| 1 | Parent in model + tracker; declared operations; pod-output lockdown | 3, 6 | 8 |
| 2 | SMT primitive; domain/parent/children/validator trees; index manager | 4 | 5 |
| 3 | Detached provable header; anchoring; three-stage enforcement | 5, 7 | 6 |
| 4 | Domain declared ops; rental + term cap; expiry sweep; deposit term | 8 | 5 |
| 5 | Sync: snapshot fields, stake import, fail-closed verification | 9 | 4 |
| 6 | QUIC + bpctl surface; light-client library; wallet switchover | 10 | 4 |
| 7 | Dead code removal | 11 | 2 |
| 8 | Whitepaper updates | 11 | 2 |

## Code-quality guardrails (enforced every batch)

- **Hot paths stay thin.** `executeTx`, `commitRound`'s successor, and `validateVertex` gain one named-helper call each, never inline logic: anchor decisions in `anchor.go`, declared ops in `ops.go`, root checks in `rootcheck.go`, index updates behind the `indexer` interface.
- **New packages:** `internal/index` (SMT + trees + manager) must not import `internal/consensus` (the DAG feeds it through a narrow interface), so the trees stay testable in isolation.
- **Unexported by default.** Export only what crosses a package boundary: `index.Tree`, `index.Proof`, `index.Verify`, `index.Manager`, the client library functions, the new QUIC message types.
- A batch is not done until its diff passes this checklist in the per-batch review, not just "tests pass".

## File map (created / modified across the plan)

- `internal/consensus/anchor.go` (new, batch 0) — anchor designation, certificate/blame decision, causal batch.
- `internal/consensus/commit.go` — sequential anchor loop replaces round iteration (batch 0); declared-ops call, created-parent validation, root re-check (batches 1, 3).
- `internal/consensus/store.go` — per-vertex committed flag, `getByRoundProducer` (batch 0).
- `internal/consensus/tracker.go` — parent + child-count in the entry (51+4 bytes), `getParent`/`setParent`/`childCount` (batch 1).
- `internal/consensus/walk.go` (new, batch 1) — `controllerOf`, `controls`, `wouldCycle` over tracker metadata.
- `internal/consensus/ops.go` (new, batches 1, 4) — `handleDeclaredOps`: reparent/transfer/delete, then domain ops.
- `internal/consensus/rootcheck.go` (new, batch 3) — ingress/commit root verification, fault evidence.
- `internal/index/smt.go`, `proof.go`, `domain_tree.go`, `hierarchy_trees.go`, `validator_tree.go`, `manager.go` (new, batch 2; one tree family per file — a single `trees.go` would blow the 300-line rule).
- `internal/state/state.go` — output lockdown (`applyUpdatedObjects`, `validateOutput`), `DeleteObjectWithRefund` (batch 1).
- `internal/state/domain.go` — leaf gains owner + expiry (batch 4).
- `internal/consensus/fees.go` — op fees, `index_entry_fee` deposit term (batch 4).
- `internal/consensus/epoch.go` — expiry sweep hook, validator-tree rebuild (batches 2, 4).
- `types/object.fbs` (+`parent_kind`), `types/transaction.fbs` (+`DeclaredOp`, `operations`), `types/vertex.fbs` (+`frontier_round`, `index_root`, `body_hash`), `types/snapshot.fbs` (tracker/domain fields), `types/podio.fbs` (deprecations).
- `internal/network/messages.go` — four new pairs: 0x15/16 anchor, 0x17/18 children, 0x19/1A ancestors, 0x1B/1C validator tree; `DomainResolve` extended in place; all registered in `clientRequestTags` (batches 3, 6).
- `cmd/node/indexhandlers.go` (new) — the index query handlers (`clienthandlers.go` is at 479 lines); `cmd/node/sync.go` — stake import, fail-closed sync (batches 5, 6).
- `pkg/client/` — declared-op builders, `verify.go` light-client library, wallet switchover (batches 1, 6).
- `pods/pod-system/src/lib.rs` — remove `transfer`/`transfer_object` (batch 1).
- `pods/pod-sdk/src/domain_generated.rs` — deleted (batch 7).
- `docs/WHITEPAPER.md` (batch 8; batch 0 carries its own §5 edit).

---

# Batch 0 — Deterministic commit (branch `fix/deterministic-commit`)

**Spec:** §2. **Branch discipline:** `git checkout main && git pull`, branch `fix/deterministic-commit`, PR to `main`, squash-merge before batch 1.

**Context (verified against code):** `checkCommits` (`internal/consensus/commit.go:41`) advances `lastCommitted` strictly forward; `commitRound` (`commit.go:148`) iterates `d.store.getByRound(round)` (`store.go:90`), which returns whatever this node holds, in arrival order. A vertex arriving after its round committed locally is stored but never applied. `isRoundCommitted` (`commit.go:75`) counts stake over round+2 producers without any reachability requirement. Parents are strictly round N-1 (`validateParents`, `validate.go:106`). The epoch machinery keys on committed round numbers (`isEpochBoundary`, `transitionEpoch`) and is unaffected by batch composition.

### Task 0.1: Anchor designation and lookup

**Files:** Create `internal/consensus/anchor.go`; modify `internal/consensus/store.go` (add `getByRoundProducer`); Test `internal/consensus/anchor_test.go`.

**Interfaces — Produces:** `anchorProducerFor(round uint64) (Hash, bool)` (deterministic over the epoch holder snapshot selected by `commitEpochForRound(round)`); `store.getByRoundProducer(round uint64, producer Hash) ([]Hash, bool)` returning ALL locally-held vertex hashes of that producer at that round, stable order. An equivocator may have several; the decision rule of Task 0.3 arbitrates among them **by votes, never by a local tiebreak** — a locally-chosen candidate (e.g. lowest hash among *arrived* vertices) is view-dependent and forks the committed log (execution-discovered C1, first attempt reverted).

- [ ] **Test:** same validator set + same round → same anchor producer on two independent DAG instances; different rounds spread across producers; equivocation (two vertices, one producer, one round): `getByRoundProducer` returns both, in the same stable order on both instances regardless of insertion order.
- [ ] **Run, expect FAIL.**
- [ ] **Implement:**

```go
// anchorProducerFor returns the designated anchor producer for a round,
// chosen deterministically from the epoch holder snapshot so every node
// derives the same pivot without communication.
func (d *DAG) anchorProducerFor(round uint64) (Hash, bool) {
	holders, ok := d.HoldersForEpoch(d.commitEpochForRound(round))
	if !ok || holders.Len() == 0 {
		return Hash{}, false
	}
	keys := holders.SortedKeys() // add if absent: pubkeys sorted ascending
	var buf [8]byte
	binary.LittleEndian.PutUint64(buf[:], round)
	h := blake3.Sum256(append([]byte("bluepods/anchor/v1"), buf[:]...))
	idx := binary.LittleEndian.Uint64(h[:8]) % uint64(len(keys))
	return keys[idx], true
}
```

- [ ] **Run, expect PASS;** `go test ./internal/consensus/ -run TestAnchor -count=1 -timeout 120s`.
- [ ] **Commit:** title `Anchor designation for the deterministic commit rule`, body `[+] anchorProducerFor over the epoch holder snapshot` / `[+] store.getByRoundProducer returns all of a producer's round vertices, stable order (votes arbitrate, Task 0.3)`.

### Task 0.2: Per-vertex committed tracking and the causal batch walk

**Files:** Modify `internal/consensus/store.go` (committed flag, persisted under a `vc/` Pebble prefix, loaded at boot); extend `internal/consensus/anchor.go`; Test `internal/consensus/anchor_test.go`.

**Interfaces — Produces:** `store.isVertexCommitted(hash Hash) bool`, `store.markVertexCommitted(hash Hash)`; `causalBatch(anchor Hash) ([]Hash, bool)` returning all not-yet-committed vertices in the anchor's ancestry (anchor included), sorted by (round ascending, hash lexicographic ascending); `ok=false` if any referenced ancestor vertex is absent from the local store (caller waits).

- [ ] **Test:** a diamond DAG where a round-8 vertex is only referenced through one branch: the batch includes it exactly once, ordered before round-9 vertices; a missing ancestor returns `ok=false`; vertices already committed are excluded; two stores fed the same vertices in different orders return identical batches; after `ImportVertices` with a snapshot round R, no vertex at round ≤ R appears in any later batch.
- [ ] **Run, expect FAIL.**
- [ ] **Implement:** breadth-first walk over parent links (`extractLinkHash`), skipping committed vertices, collecting `(round, hash)` pairs, then `sort.Slice` by round then `bytes.Compare`. Persist the committed flag in `markVertexCommitted` so restart never re-applies a batch. `ImportVertices` (`store.go:191`) marks every imported vertex at rounds ≤ the snapshot's `lastCommittedRound` as committed at import time — the snapshot deliberately ships vertices up to the current round, and without the flag a synced node's first causal walk would re-apply the whole imported history onto snapshot state. The walk also takes a commit floor at the snapshot round as a second guard.
- [ ] **Run, expect PASS.**
- [ ] **Commit:** title `Causal batch walk with per-vertex committed tracking`.

### Task 0.3: Anchor decision rule (support / blame / indirect)

**Files:** Extend `internal/consensus/anchor.go`; Test `internal/consensus/anchor_test.go`.

**Interfaces — Produces:** `anchorStatus(round uint64) anchorDecision` where `anchorDecision` is `anchorCommit | anchorSkip | anchorWait`, plus the resolved anchor hash for `anchorCommit`.

**The rule (vote-determined candidate — an equivocating anchor producer cannot fork the verdict; both quorums over ONE stake set, so they intersect):** *support for a specific vertex V* = a round-N+1 vertex listing V among its **direct parents**. **CERTIFIED** when one vertex V of the designated producer reaches the stake quorum of round N+1 (exact integer test `3*cappedSum >= 2*total` from `stake.go` over the epoch snapshot); V is then the anchor — the quorum itself selects among an equivocator's vertices, never a local candidate (execution-discovered C1: a node holding both E1 and E2 that locally picked E1 blamed the round while E2-only nodes certified E2 — permanent fork from one Byzantine producer plus gossip asymmetry). A round-N+1 vertex citing a *different* vertex of the designated producer counts as ABSTAIN, never as blame. **BLAMED** only when round-N+1 producers citing **no vertex of the designated producer** carry that quorum. At most one V can reach the citation quorum, and certified and blamed cannot both hold (two ≥2/3 quorums over the same stake intersect in ≥1/3); both verdicts are durable: more vertices only add evidence, never flip a formed quorum. **INDIRECT rule (no deadlock):** if no direct verdict forms (split evidence, offline stake), the anchor is decided by the first LATER anchor that reaches a direct verdict: committed if it sits in that anchor's causal history, skipped otherwise. `anchorStatus` therefore scans forward past an undecided round; WAIT is returned only when no later direct verdict exists yet. **Epoch tail (I2):** the forward scan must be able to cross a boundary — freeze the next epoch's holder snapshot at the boundary (one epoch ahead) so `directAnchorVerdict` on a next-epoch round never WAITs solely because the snapshot does not exist yet; a split round at an epoch tail must not wedge.

- [ ] **Test:** direct certify; direct blame; a tiny-stake lone supporter does NOT certify (the round+2 transitive-reachability trap — assert the rule reads round+1 direct parents only); the C1 fork regression: designated producer equivocates E1/E2, quorum cites E2 → a node holding only E2 and a node holding E1+E2 BOTH certify E2; citations of a different vertex of the producer never contribute to blame; blame requires a quorum citing no vertex of the producer; split evidence resolves via the indirect rule both ways (in-history → commit, out-of-history → skip); a split round at the last round of an epoch resolves via a next-epoch anchor; WAIT only while no later anchor is decided.
- [ ] **Run, expect FAIL → implement → PASS.**
- [ ] **Commit:** title `Anchor support, blame, and indirect decision rules`.

### Task 0.4: Sequential anchor commit loop replaces round iteration

**Files:** Modify `internal/consensus/commit.go` (`checkCommits`, replace `commitRound` body with `commitAnchorBatch`); Test `internal/consensus/commit_test.go`.

**Interfaces — Consumes:** `anchorStatus`, `causalBatch`, `store.markVertexCommitted`. **Produces:** unchanged external behavior: `processTransactions(v, commitRound, verdicts)` is called per batch vertex in batch order; `transitionEpoch`/`isEpochBoundary` still key on the decided round number; `lastCommitted` advances on both COMMIT and SKIP.

- [ ] **Test:** feed two DAG instances the same vertices in different arrival orders (including one vertex delivered after its round was decided): identical committed logs (assert on the ordered list of committed vertex hashes); a late vertex is applied exactly once, in the batch where first referenced; a never-referenced vertex is applied on neither instance; a SKIP round advances without applying anything and its stragglers ride the next COMMIT batch.
- [ ] **Run, expect FAIL.**
- [ ] **Implement:** `checkCommits` loops: `status := anchorStatus(lastCommitted+1)`; on WAIT stop; on SKIP advance; on COMMIT get `causalBatch`, on `ok=false` stop (ancestry still in flight; the pending-parent buffer delivers it), else apply each vertex through the existing `verifyRoundProofs`/`processTransactions` path in batch order, mark committed, advance. Keep the function ≤ 25 lines by delegating to `commitAnchorBatch(round, anchor)`. **Side-effect inventory (C3):** the replaced round-quorum path was `markFullQuorumAchieved`'s only caller; the new loop MUST call it from the strict-regime direct-certification branch, or `fullQuorumAchieved` never latches → production quorum accepts a single vertex forever AND `validateVertex` skips `validateParents` forever, so a vertex citing a FABRICATED parent hash enters certified causal history and wedges every node's causal walk on an unfetchable vertex (execution-discovered C3; also the likely cause of the first attempt's 2x sim slowdown). Inventory every caller and side effect of the code the new loop replaces and re-wire each one. **Resolve-before-transition gate (from the 0.3 review):** the epoch-tail proxy (`nextEpochHolders`) weighs a tail round only until `transitionEpoch` installs the real churned holders, so a DECIDED round must never be re-evaluated afterwards — the persisted cursor is the guarantee: the loop decides rounds strictly in cursor order, `transitionEpoch` fires only from the commit path once the boundary round is decided, and a restarted node reads past decisions from the cursor, never re-derives them.
- [ ] **Run, expect PASS;** full package `go test ./internal/consensus/ -count=1 -timeout 300s` and fix tests that assumed arrival-order commits. Assert that after the first strict-regime direct certification, `fullQuorumAchieved` is set (production quorum and parent validation leave the relaxed posture). Assert the resolve-before-transition gate: decide an epoch-tail round via the proxy, transition the epoch with churned holders, re-open the node — the round's decision comes from the persisted cursor and is never re-derived differently.
- [ ] **Commit:** title `Sequential anchor batches replace arrival-order round commits`, body `[!] late vertices commit exactly once instead of silently diverging`.

### Task 0.5: Relaxed-regime anchoring and restart determinism

**Files:** Extend `internal/consensus/anchor.go` + `internal/consensus/epoch.go` (persist epoch holder snapshots) + `internal/consensus/dag.go` (persist/load `lastCommitted`); Test `internal/consensus/anchor_test.go`.

**Context (verified):** `isCommitQuorumRelaxed` (`commit.go:105-115`) commits on a single known producer during init and the transition/buffer windows; `HoldersForEpoch` (`epoch.go:410`) serves only the current and previous epoch and the snapshots are in-memory only; `lastCommitted` is not persisted (only `WithLastCommittedRound` from sync). Under the new rule the anchor choice determines batch content and order, so any per-node fallback divergence means divergent logs.

- [ ] **Test:** during relaxed rounds the vote-determined candidate rule of Task 0.3 decides under the relaxed certificate (bootstrap still converges; two honest nodes with a 1-vertex delivery gap decide identically — the I1 regression); a node restarted mid-epoch resumes from its persisted `lastCommitted` with the persisted holder snapshot and produces the same decisions as an uninterrupted twin; a node syncing AFTER the first epoch boundary imports the epoch state and commits past the boundary (the C2 regression); `anchorProducerFor` on an epoch with no snapshot returns `false` and the caller WAITs (never silently falls back to the live set).
- [ ] **Run, expect FAIL.**
- [ ] **Implement:** the regime is a pure function of committed history, NEVER of local atomics (`isCommitQuorumRelaxed`'s `transitionRound`/`fullQuorumAchieved` are per-node join-timeline state and made `TestSimProgressiveJoining` diverge — execution-discovered, resolved 2026-07-08): `strictStartRound` = the committed round crossing `minValidators` + grace + buffer windows, a persisted monotone latch carried in sync snapshots, derived from COMMITTED registrations only and clamped to the commit cursor at latch-fire (never retroactively below already-relaxed-decided rounds — I8); round R is relaxed iff the latch is unset or R < latch. Relaxed rounds keep the existing relaxed certificate but the candidate is vote-determined exactly as in Task 0.3 — NEVER "lowest hash among arrived vertices", which is view-dependent and forked two honest nodes on a 1-vertex gap (I1). At the latch, freeze the registered set (with stakes) as the **genesis holder snapshot** used by `anchorProducerFor` and the stake quorums until the first epoch boundary; the freeze reads committed registrations only — an uncommitted self-add must never enter the frozen set, or the registering node freezes a different set than everyone else (I7); `HoldersForEpoch` never falls back to the live set in the anchor path. Strict-regime stake quorums divide by the CAPPED total, never the raw total (execution-discovered: a raw-total denominator makes the strict quorum unreachable under stake concentration). **Persistence and sync (C2, I3, I4):** persist the current and previous `epochHolders` snapshots under a Pebble key at each boundary and load them at init; persist `lastCommitted` alongside the store's latest-round key, and persist the cursor and the epoch state (currentEpoch + both holder snapshots) in ONE batched write — a crash between the two restarts into a wedge. Sync snapshots carry the latch AND the full epoch state (currentEpoch, both holder snapshots), imported at join — a joiner landing after the first boundary with zero epoch state WAITs forever (C2). The snapshot provider reads cursor, validators, vertices, tracker, latch, and holders under `commitMu` as ONE consistent cut (I3), and the exported cursor semantic matches the importer exactly — the first attempt exported next-to-decide where `WithLastCommittedRound` expected last-decided, silently skipping a round on every join (I4). **Carry-forwards from the 0.4 review (all three mandatory):** (1) at boot, restore `currentEpoch` and the holder snapshots from persistence BEFORE the cursor drives any decision — Task 0.4 persists the cursor alone, so today a plain restart past the first epoch boundary either wedges (`HoldersForEpoch` false → WAIT forever) or silently uses the genesis fallback; (2) REMOVE Task 0.4's genesis-only live-set fallback in `HoldersForEpoch` (epoch-0 tail reads `d.validators`, a mutating set — the I1/I7 multi-node fork channel), replaced by the frozen genesis holder snapshot; (3) rebuild the store's in-memory `byRound` index at boot from the persisted `r:` entries — `newStore` loads only latestRound/committed flags/floor, so a restarted node's `getByRoundProducer` returns empty for pre-crash undecided rounds and spuriously blames or skips where a live peer commits: the committed SET converges but the ORDER diverges, and order is state (a re-gossiped pre-crash vertex is rejected by `add` as already-in-db and never re-indexed, so the amputation is permanent).
- [ ] **Regression tests (0.4 carry-forwards):** restart a node with a cursor past the first epoch boundary — it resumes committing (epoch state restored, no wedge, no fallback); two nodes at the epoch-0 tail with different in-flight registration states decide the boundary round identically (the fallback fork channel is closed); a node restarted with undecided in-flight rounds produces the same committed log ORDER as a never-crashed twin (byRound rebuilt at boot).
- [ ] **Run, expect PASS.**
- [ ] **Commit:** title `Relaxed-regime anchoring and persisted commit cursor`.

### Task 0.6: Missing-ancestor fetch

**Files:** Modify `internal/network/messages.go` (mesh-tier vertex request pair), `internal/consensus/dag.go` (request missing ancestry when a decided anchor's `causalBatch` returns `ok=false` for two consecutive commit ticks); create `cmd/node/vertexfetch.go` (`meshVertexFetcher` implementing the consensus fetcher interface + the serving handler); modify `cmd/node/clienthandlers.go` (route the request tag) and `cmd/node/init.go` + `cmd/node/sync.go` (call `SetVertexFetcher` after EVERY DAG construction site — bootstrap, synced validator, synced listener); Test at the `cmd/node` level through the real wiring.

**Context (verified):** the pending buffer (`dag.go:1119-1141`) is a passive retry queue with no fetch protocol, and `validateParents` is skipped for transition-window rounds and unknown-producer parents, so a stored vertex can have permanently absent ancestry. Today's round-cursor loop cannot block on that; the causal walk can — so the fetch is required for liveness, not an optimization. **Wiring is the deliverable:** the first attempt shipped the wire protocol, the server method, and the consensus-side fetch fully unit-tested against a stub — and never called `SetVertexFetcher`, so the entire feature was dead code and joiners stalled ~50% of runs (execution-discovered; a DAG-level test with a stubbed transport cannot catch this). The fetch is asynchronous and in-flight-deduped: the commit loop calls it under `commitMu` and must never block on network I/O; the fetched vertex enters through the normal `AddVertex` path and is picked up on a later tick. The receiver compares the returned vertex hash against the requested hash and discards mismatches — a malicious peer must not satisfy a request with a different valid vertex while the real hash stays missing (I5). Known deferral (I6, tracked): a gap on an UNDECIDED round has no fetch trigger; residual ~1/10 `TestSimEpochs` parallel-startup flake, pre-existing, out of scope.

- [ ] **Test:** a node holding an anchor whose parent was never gossiped to it requests and receives the missing vertex **through the real `cmd/node` wiring** (assert `SetVertexFetcher` is non-nil on every construction path), then commits the identical batch as a fully-fed twin; a response whose hash differs from the request is discarded and the retry reaches another peer.
- [ ] **Run, expect FAIL → implement → PASS.**
- [ ] **Commit:** title `Fetch missing ancestry for decided anchors`.

### Task 0.7: Divergence regression and sim validation

**Files:** Test `internal/consensus/divergence_test.go` (new); run existing sims.

- [ ] **Test (new):** property-style: 3 simulated validators, 20 rounds of generated vertices, random per-node delivery permutations with one delayed vertex per round; assert byte-identical committed logs and identical tracker exports across nodes.
- [ ] **Run, expect PASS** (guards the batch; it must pass now).
- [ ] **Run sims individually:** `go test ./test/integration/ -run TestSimConsensus -count=1 -timeout 600s`, then `TestSimBootstrap`, `TestSimEpochs`, `TestSimFees`, and `TestSimProgressiveJoining` (the synced-node path exercises the import commit floor of Task 0.2) the same way. **`TestSimProgressiveJoining` and `TestSimEpochs` run in a loop (≥5 passes each):** the first attempt's stall was ~50% intermittent and a single pass proves nothing. Compare `TestSimConsensus` wall time against `main` (~114s): a 2x slowdown signals the C3 relaxed-production posture, not load. Fix fallout.
- [ ] **Commit:** title `Divergence regression: identical logs under delivery permutations`.

### Task 0.8: Whitepaper §5 commit-rule paragraph

**Files:** Modify `docs/WHITEPAPER.md` (section 5, Commit Rule and Finality).

- [ ] **Edit:** describe the anchor designation, the vote-determined certificate/blame rule, causal batches ordered (round, hash), and late admission (append-only, zero rollback preserved). Say "the first later *certified* anchor" for the indirect rule, and do not claim "a round advances only once a BFT supermajority is reached" unless the strict posture of Task 0.4 makes it true. Keep the sober register; no other section changes here.
- [ ] **Build check:** `go build ./... && go vet ./...` still green (doc-only, sanity).
- [ ] **Commit:** title `Document the anchor commit rule`; then **push the branch, open the PR to main, squash-merge once CI is green**. Rebase `verifiable-indexing` onto `main` before batch 1.

---

# Batch 1 — Parent, declared operations, pod-output lockdown

**Spec:** §3, §6. **Branch:** `verifiable-indexing` (rebased on main).

**Context (verified against code):** the tracker entry is 18 bytes (`tracker.go:266 encodeValue`: version 8 + replication 2 + fees 8); `trackObject` (`tracker.go:199`) is called from the creation path; `deleteObject` (`tracker.go:207`) has zero callers. `applyUpdatedObjects` (`state.go:410`) persists pod output unchecked; `applyDeletedObjects` (`state.go:573`) skips non-local objects; `validateOutput` (`state.go:334`) already receives the tx. Ownership at commit reads the local body (`validateMutableRefOwnership`, `commit.go:601`) and fails on non-holders — the tracker walk replaces it. Creation transactions execute on every node (`commit.go:400`: `CreatedObjectsReplicationLength() == 0 && MaxCreateDomains() == 0` guard). The staking ops precedent for protocol-parsed operations is `handleBond`/`handleDelegate` (`commit.go:958/:1023`).

### Task 1.1: Schemas — `parent_kind`, `DeclaredOp`, canonical body coverage

**Files:** Modify `types/object.fbs` (append `parent_kind:ubyte` to `Object`; 0 = KeyRoot, 1 = ObjectParent; the existing `owner` bytes become the parent bytes), `types/transaction.fbs` (append `DeclaredOp` table and `operations:[DeclaredOp]` to `Transaction`), `types/snapshot.fbs` (`ObjectVersion` gains `parent_kind:ubyte`, `parent:[ubyte]`, `child_count:uint32`); regenerate; modify `internal/genesis/transaction.go` + `internal/validation/validate.go` + `internal/consensus/txauth.go` in the same commit (three-site lockstep) so `operations` is covered by the canonical body hash.

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

- [ ] **Test:** a transaction carrying one reparent op round-trips through build → `rebuildUnsignedTx` → `verifyTxAuthenticity` (hash and signature verify); a transaction without ops serializes byte-identically to one built before the field existed (absent-when-empty, same guarantee as sponsorship).
- [ ] **Run, expect FAIL** → regenerate (`bash types/generate.sh`), implement body coverage, **expect PASS**.
- [ ] **Commit:** title `DeclaredOp and parent_kind schemas under the canonical body hash`.

### Task 1.2: Tracker carries parent and child count

**Files:** Modify `internal/consensus/tracker.go`; Test `internal/consensus/tracker_test.go`.

**Interfaces — Produces:** `trackObject(objectID Hash, version uint64, replication uint16, fees uint64, parentKind byte, parent Hash)`; `getParent(objectID Hash) (kind byte, parent Hash, ok bool)`; `setParent(objectID Hash, kind byte, parent Hash)`; `childCount(parentID Hash) uint32` maintained on track/setParent/delete; `Export`/`Import` extended; entry layout: version 8 + replication 2 + fees 8 + kind 1 + parent 32 + childCount 4 = 55 bytes; a stored 18-byte value decodes as `KeyRoot` with zero parent (never panics — such objects are controlled by the zero key, i.e. frozen; acceptable only because pre-mainnet networks are recreated, state this in the docstring).

- [ ] **Test:** round-trip encode/decode both lengths; child counts follow reparent chains (A under K, B under A: `childCount(A)==1`; reparent B to K: `childCount(A)==0`); export/import preserves parents.
- [ ] **Run, expect FAIL → implement → PASS.** Update all `trackObject` call sites (creation path passes the created object's parent from its body).
- [ ] **Commit:** title `Track parent and child count in the global object tracker`.

### Task 1.3: Permission walk over tracker metadata

**Files:** Create `internal/consensus/walk.go`; Test `internal/consensus/walk_test.go`.

**Interfaces — Produces:** `controllerOf(objectID Hash) (Hash, bool)` (terminal KeyRoot pubkey, walking ≤ 256 edges, `false` on depth overflow or missing entry); `controls(sender, objectID Hash) bool`; `wouldCycle(objectID Hash, newParentKind byte, newParent Hash) bool` (true if newParent is the object or any descendant — implemented as: walk up from newParent; if the walk meets objectID, cycle).

- [ ] **Test:** nested chain resolves to the root key; depth 257 fails closed; reparenting an ancestor under its descendant is detected; reparenting to a `KeyRoot` never cycles.
- [ ] **Run, expect FAIL → implement → PASS.**
- [ ] **Commit:** title `Cascade permission walk over global metadata`.

### Task 1.4: Reparent and transfer as commit-path operations

**Files:** Create `internal/consensus/ops.go`; modify `internal/consensus/commit.go` (`executeTx`: if `tx.OperationsLength() > 0`, route to `handleDeclaredOps` after fee deduction and version check, never into pod execution; reject a tx carrying both ops and a pod call at the same site); Test `internal/consensus/ops_test.go`.

**Interfaces — Consumes:** `controls`, `wouldCycle`, `setParent`, tracker version machinery. **Produces:** `handleDeclaredOps(tx *types.Transaction) bool` — ops apply **sequentially against the evolving state** (staged apply: mutations buffered, discarded wholesale on the first failure, fees kept). Per-op rule: reparent requires `controls(sender, object)`; a `KeyRoot` target may be **any key** (that is a transfer — control of the object suffices); an `ObjectParent` target must additionally be sender-controlled; `!wouldCycle`; then `setParent` + version increment.

- [ ] **Test:** transfer to another key succeeds and only the root object's tracker entry changes; reparent under a non-controlled `ObjectParent` fails; cycle fails; version increments once per touched object; a dependent list (`delete X` then `reparent Y under X`) fails on the second op and applies nothing; sequential semantics: `reparent A under B` then `reparent C under A` succeeds in one tx; a tx with ops AND a pod function is rejected.
- [ ] **Run, expect FAIL → implement → PASS.**
- [ ] **Commit:** title `Reparent and transfer as protocol-declared operations`.

### Task 1.5: Delete as a declared operation with refund

**Files:** Extend `internal/consensus/ops.go`; modify `internal/state/state.go` (add `DeleteObjectWithRefund(id Hash, gasCoin Hash) error` wrapping the existing `settleDeletionDeposit` 95/5 path and local body removal); wire a `SetOnObjectDeleted`-style hook mirroring `SetOnObjectCreated` (`state.go:83`); Test both packages.

**Interfaces — Produces:** delete op requires `controls(sender, object)` and `childCount(object)==0`; calls `tracker.deleteObject` (first real caller), decrements the parent's child count, triggers the state hook (holders drop the body; refund credits the tx's gas coin; 5% burn via the existing supply path).

- [ ] **Test:** deleting a leaf refunds 95% and burns 5% (assert supply delta); deleting a parent with children fails; the tracker forgets the entry; a non-holder node processes the same delete without holding the body (no error, tracker+refund consistent) — this is the divergence the old pod path had.
- [ ] **Run, expect FAIL → implement → PASS;** re-run `go test ./internal/consensus/ -run TestSupplyInvariant -count=1 -timeout 120s`.
- [ ] **Commit:** title `Delete as a declared operation with deterministic refund`.

### Task 1.6: Creation permission and pod-output lockdown

**Files:** Modify `internal/state/state.go`: `validateOutput` gains the created-parent rule and the deletion restriction; `applyUpdatedObjects` rejects parent changes; wire `SetParentValidator(fn func(kind byte, parent Hash, sender Hash, tx *types.Transaction) bool)` so state can ask consensus for the walk; Test `internal/state/state_test.go`.

**Interfaces — Produces:** a created object's parent must satisfy: KeyRoot == sender, or `controls(sender, parent)`, or parent is referenced by this tx through a domain ref (`ObjectRef.domain != ""`, the existing shared-access exemption). `updated_objects` whose owner/parent bytes differ from the input object's are rejected (input objects are in the ATX at the checked version — a pure local compare, no tracker needed). `deleted_objects` valid only when the tx is globally executed (`CreatedObjectsReplicationLength() > 0` or all mutable refs are singletons — the `merge` carve-out); otherwise the output is rejected. The carve-out delete runs the SAME effects as the declared delete: `childCount == 0` required, tracker + index removal, 95/5 deposit settlement.

- [ ] **Test:** creating under someone else's key fails; under an owned object succeeds; under a domain-referenced table succeeds; a pod flipping owner in `updated_objects` reverts the tx; `merge` (singleton coins) still deletes its source coin; a pod deleting a sharded object in a non-creating tx reverts; a pod deleting a parented object (children > 0) through the carve-out reverts.
- [ ] **Run, expect FAIL → implement → PASS.**
- [ ] **Commit:** title `Creation permission rule and pod-output lockdown`, body `[!] closes the third parent write path (pods) and spam-attach`.

### Task 1.7: Client builders for declared operations

**Files:** Modify `pkg/client/transactions.go` (add `Reparent`, `TransferObject` (rebuilt on ops), `DeleteObject` builders producing signed declared-op transactions with correct refs and version); Test `pkg/client/client_test.go`.

**Interfaces — Produces:** `(c *Client) TransferObject(objectID [32]byte, newOwner [32]byte) (txHash [32]byte, err error)` and siblings; each puts the object in `mutable_refs` with its current version and one `DeclaredOp`; gas rules unchanged.

- [ ] **Test:** builder output round-trips `verifyTxAuthenticity`; refs and op fields match inputs.
- [ ] **Run, expect FAIL → implement → PASS.**
- [ ] **Commit:** title `Client builders for reparent, transfer, delete`.

### Task 1.8: Retire pod transfers; sims green

**Files:** Modify `pods/pod-system/src/lib.rs` (drop `transfer`/`transfer_object` dispatch entries and their function dirs); update integration sims that used them to the new client builders; `make -C pods/pod-system release`.

- [ ] **Implement + test:** `go test ./test/integration/ -run TestSimObjects -count=1 -timeout 600s`, then `TestSimForgedTxAuth`, `TestSimFees` individually. Fix fallout.
- [ ] **Commit:** title `Retire pod-level transfer entrypoints`, body `[-] transfer/transfer_object from the system pod`. **Push the batch.**

---

# Batch 2 — SMT primitive and the four trees

**Spec:** §4. **Design:** `internal/index`, no import of `internal/consensus`; fed via interfaces. Reference semantics: Jellyfish Merkle Tree; binary SMT over BLAKE3(key), per-level default hashes for empty subtrees, only non-empty paths materialized.

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

**Files:** Create `internal/index/manager.go`; modify `internal/consensus/commit.go` + `epoch.go` (feed the manager: object created/reparented/deleted, domain writes, epoch validator snapshot) behind a narrow `indexer` interface field on `DAG` (nil-safe: no-op when unset); modify `cmd/node/init.go` to construct and inject it, **and on a restart with an existing data dir call `BuildFromState` from the persisted tracker, domain store, and current epoch holders BEFORE production starts** (a restarted node with an empty index would anchor wrong roots and be silently excluded by peers — add a restart test); Test `internal/index/manager_test.go`.

**Interfaces — Produces:** `Manager` with `ApplyEdge(child Hash, kind byte, parent Hash)`, `RemoveObject(child Hash)`, `ApplyDomain(...)`/`RemoveDomain(name)`, `RebuildValidators(entries []ValidatorLeaf)`, `Root() [32]byte`, `RootAt(round uint64) ([32]byte, bool)` (bounded history: 1,000 rounds + one per epoch), `SetFrontier(round uint64)` called once per committed batch. `BuildFromState(trackerEntries, domainEntries, validatorEntries)` for boot/sync rebuild.

- [ ] **Test:** applying a synthetic committed stream vs `BuildFromState` on the final state → identical root; `RootAt` returns historical roots inside the window and `false` outside.
- [ ] **Run, expect FAIL → implement → PASS;** `go build ./... && go vet ./...`.
- [ ] **Commit:** title `Index manager fed by the commit path`. **Push the batch.**

---

# Batch 3 — Detached header, anchoring, three-stage enforcement

**Spec:** §5, §7. **Context (verified):** the producer signs BLAKE3 of the whole unsigned vertex (`build.go:24-33`); `validateSignature` (`validate.go:82`) verifies over `HashBytes`; the vertex `epoch` field is written from `d.epoch`, the static construction-time value (`cmd/node/init.go` passes 0) — `transitionEpoch` advances `d.currentEpoch` only; parents link by vertex hash. Gossip dedup hashes whole message bytes (`network/dedup.go:43`), so the identity change is contained to consensus.

**Breaking format:** the header hash changes every vertex's identity. Nodes upgrade in lockstep and data dirs are wiped (pre-mainnet, acceptable and stated); bump `snapshotVersion` in this batch, since old snapshot vertices can no longer validate.

### Task 3.1: Detached provable header

**Files:** Modify `types/vertex.fbs` (append `frontier_round:uint64`, `index_root:[ubyte]`, `body_hash:[ubyte]` to `Vertex`); regenerate; modify `internal/consensus/build.go` (compute `bodyHash = blake3(parents || transactions || fee_summary || timestamp)` — round lives in the header only — then `hash = blake3(producer || round || epoch || frontier_round || index_root || bodyHash)`; sign `hash`; populate `epoch` with `d.currentEpoch`, read under `commitMu`) and `internal/consensus/validate.go` (`validateSignature` recomputes `bodyHash` and the header hash; `validateEpoch` reworked to accept `currentEpoch` and `currentEpoch-1` within the boundary window, else every vertex crossing a boundary in flight is rejected — add a boundary-skew test); Test `internal/consensus/build_test.go`.

**Interfaces — Produces:** the header hash **is** the vertex identity (parent links, store keys — unchanged code, changed meaning); `headerBytes(v *types.Vertex) []byte` and `computeBodyHash(v *types.Vertex) [32]byte` shared by build and validate (one function, two call sites, cannot drift).

- [ ] **Test:** a vertex verifies end-to-end; tampering with a transaction changes `bodyHash` and breaks the signature; tampering with `index_root` breaks it too; a light verification using only `{producer, round, epoch, frontier_round, index_root, bodyHash, signature}` (~200 bytes) accepts without the body.
- [ ] **Run, expect FAIL → implement → PASS;** full consensus package tests.
- [ ] **Commit:** title `Detached provable vertex header`, body `[&] header hash becomes the vertex identity; epoch field populated`.

### Task 3.2: Producers anchor their committed frontier

**Files:** Modify `internal/consensus/build.go` (read `Manager.Root()` + frontier from the indexer interface at production; zero values when indexer unset); Test.

- [ ] **Test:** two nodes at the same committed frontier produce vertices carrying identical `(frontier_round, index_root)`.
- [ ] **Run, expect FAIL → implement → PASS.**
- [ ] **Commit:** title `Anchor the index root in every produced vertex`.

### Task 3.3: Ingress root check (stage 1)

**Files:** Create `internal/consensus/rootcheck.go`; modify `internal/consensus/validate.go` (`validateVertex` step 7: if `v.FrontierRound() <= lastCommitted` and the indexer is set, require `RootAt(frontier) == index_root`; unverifiable-yet vertices pass); Test `internal/consensus/rootcheck_test.go`.

- [ ] **Test:** a wrong-root vertex whose frontier the receiver has committed is rejected; a vertex anchoring a future frontier is accepted; a frontier older than the retention window (no `RootAt` entry, no epoch checkpoint) is treated as unverifiable and passes (parents are always recent, so production never depends on stale roots); a zero-root vertex passes during the genesis epoch ONLY and is rejected like a wrong root from the first epoch boundary on (spec §5).
- [ ] **Run, expect FAIL → implement → PASS.**
- [ ] **Commit:** title `Ingress root verification when the frontier is local`.

### Task 3.4: Verify-before-reference at production (stage 2) and commit re-check (stage 3)

**Files:** Modify `internal/consensus/dag.go` (`collectParents` filters to parents whose anchor is verified — `RootAt` match, or zero-root during the genesis epoch only); extend `rootcheck.go` with `recheckCommittedAnchor(v)` called from the batch loop of Task 0.4; fault evidence persisted under a `fault/` Pebble prefix `{producer, round, claimed, computed, headerBytes, signature}`; Test.

- [ ] **Test:** an honest producer never references a wrong-root vertex (it can win the round only when the liar is excluded); a committed vertex with a wrong root (forced through a crafted store) writes exactly one fault record with a verifying signature over the lying header.
- [ ] **Run, expect FAIL → implement → PASS.**
- [ ] **Commit:** title `Verify-before-reference and commit-path fault evidence`.

### Task 3.5: Quorum bundle assembly and `GetIndexAnchor`

**Files:** Modify `internal/network/messages.go` (tags `0x15 MsgTagGetIndexAnchor` / `0x16 MsgTagGetIndexAnchorResp`, added to `clientRequestTags` at `messages.go:99` or the connection classifier will not route them); create the handler in `cmd/node/indexhandlers.go` (new file — `clienthandlers.go` is at 479 lines; serve the cached bundle: highest frontier where headers matching `(frontier, root)` reach the stake quorum within a 16-round sliding window; recompute lazily per committed batch); Test at the handler level.

**Interfaces — Produces:** response = `{frontier_round, index_root, headers: [~200B each], epoch}`; assembly reuses the capped-stake quorum test from `stake.go`.

- [ ] **Test:** with 4 simulated producers the bundle reaches quorum and verifies; a minority wrong-root producer is excluded from the bundle.
- [ ] **Run, expect FAIL → implement → PASS.**
- [ ] **Commit:** title `GetIndexAnchor quorum bundles`.

### Task 3.6: Sim regression

- [ ] **Run individually:** `TestSimConsensus`, `TestSimAggregation`, `TestSimStress` with bounded timeouts; fix fallout (vertex format change touches gossip fixtures).
- [ ] **Commit:** title `Sims green on the anchored vertex format`. **Push the batch.**

---

# Batch 4 — Domain operations, rental, sweep, deposit term

**Spec:** §8. **Context (verified):** `domainStore` leaf is a bare 32-byte objectID (`domain.go:48 set`); `validateOutput` collision check makes updates impossible today; fee formula in `CalculateFee`/`calculateTxFee` (`fees.go`), summary lockstep `buildFeeSummary`/`validateFeeSummary`; epoch boundary work in `transitionEpoch` (`epoch.go:37`).

### Task 4.1: Domain store leaf and declared-op handlers

**Files:** Modify `internal/state/domain.go` (leaf `{objectID 32, owner 32, expiryEpoch 8}`, length-tolerant decode for old 32-byte values); extend `internal/consensus/ops.go` with kinds 2-6: register (absent name only; dotted name requires `sender == owner(immediateParentName)`; `system.*` rejected), renew (owner-only; also during grace), update/transfer/delete (owner-only); all feed the index manager; Test `internal/consensus/ops_test.go`.

**Interfaces — Consumes:** domain reads through a narrow state accessor `DomainLeaf(name) (objectID, owner Hash, expiry uint64, ok bool)`. **Produces:** `expiry = max(currentExpiry, currentEpoch) + term`, and an op whose result would exceed `currentEpoch + maxTermEpochs` **reverts** (never clamps: the fee is `rate x term_epochs` from the header, and a clamped term would charge a fee that no longer matches the declared field). Domain ops carry NO object refs and increment NO object version (spec §3); `domain_register` and `domain_update` require `controls(sender, pointedObject)` — without it, anyone could alias a victim's object and reach it mutably through the domain-ref ownership exemption. Resolution (execution-time and queries) treats a name past `expiry_epoch` as absent, even during grace; grace only reserves the owner's renewal right.

- [ ] **Test:** FCFS on roots; `x.y` without owning `y` fails; renewal by a non-owner fails; a term pushing past the cap reverts and charges nothing but the base fee; registering a name pointing at a non-controlled object fails; an expired-in-grace name does not resolve at execution but renews for the owner; update repoints; transfer hands renewal rights; register on an existing live name fails.
- [ ] **Run, expect FAIL → implement → PASS.**
- [ ] **Commit:** title `Domain declared operations with ownership and namespaces`.

### Task 4.2: Rental and per-op fees, summary lockstep

**Files:** Modify `internal/consensus/fees.go` (`FeeParams` gains `RentalRatePerEpoch`, `MaxTermEpochs`, `GraceEpochs`, `ReparentFee`, `DeleteFee`, `IndexEntryFee`; `calculateTxFeeSplit` adds `Σ op fees` to the consumed part, rent = `safeMul(rate, term)`); `buildFeeSummary` + `validateFeeSummary` in the same commit; Test `internal/consensus/fees_test.go`.

- [ ] **Test:** a register-for-10-epochs tx pays `10×rate` into the epoch pool; a vertex whose summary omits op fees is rejected; ops tx with no pod call pays `min_gas` compute.
- [ ] **Run, expect FAIL → implement → PASS;** supply invariant test extended and green.
- [ ] **Commit:** title `Rental and declared-operation fees in the summary lockstep`.

### Task 4.3: Expiry sweep at the epoch boundary

**Files:** Modify `internal/consensus/epoch.go` (`transitionEpoch` step: call a `sweepExpiredDomains(epoch)` hook before the snapshot step) wired to state + index manager; Test `internal/consensus/epoch_test.go`.

- [ ] **Test:** a name expired beyond grace is removed from store and tree on the boundary, deterministically on two nodes; within grace it stops resolving for registration purposes but the owner can still renew; the sweep touches only expired leaves (root changes only when something is swept).
- [ ] **Run, expect FAIL → implement → PASS.**
- [ ] **Commit:** title `Deterministic domain expiry sweep`.

### Task 4.4: Retire the pod domain path

**Files:** Modify `internal/state/state.go` (remove `applyRegisteredDomains`/`resolveDomainObjectID`; `validateOutput` rejects a non-empty `registered_domains`), `internal/consensus/commit.go:400` (drop the `MaxCreateDomains` term from the global-execution guard), `internal/consensus/fees.go` (remove the `max_create_domains x DomainFee` term from `CalculateFee` at `fees.go:170` AND from `buildFeeSummary`/`validateFeeSummary` in the same commit), `internal/genesis/transaction.go` (zero/deprecate the `maxCreateDomains` builder parameter — three-site lockstep applies), `types/podio.fbs` + `types/transaction.fbs` (mark `registered_domains` and `max_create_domains` deprecated in comments; fields stay for layout stability); pod SDK docs note; Test.

- [ ] **Test:** a pod emitting `registered_domains` reverts; domain refs in `ObjectRef` still resolve at execution (read path untouched).
- [ ] **Run, expect FAIL → implement → PASS.**
- [ ] **Commit:** title `Retire the pod domain write path`.

### Task 4.5: Index-entry deposit term

**Files:** Modify `internal/consensus/fees.go` (`StorageDeposit`) **and** `internal/state/state.go` (`computeStorageDeposit`) — same formula, same commit: `storage_fee × effective_rep / total_validators + index_entry_fee`; refund keeps the existing 95/5 over the whole `fees` field; Test both packages + supply invariant.

- [ ] **Test:** debit equals stamped deposit at creation; delete refunds 95% of (storage + index term); supply exact at the boundary.
- [ ] **Run, expect FAIL → implement → PASS.**
- [ ] **Commit:** title `Flat index-entry term in the creation deposit`. **Push the batch.**

---

# Batch 5 — Sync

**Spec:** §9. **Context (verified):** snapshot ships tracker/domains/vertices/supply (`internal/sync/snapshot.go`); `cmd/node/sync.go buildValidatorSetFromSnapshot` calls `vs.Add` **without stake** (drops it); vertex history is 100 rounds (`internal/sync/manager.go:146-150`).

### Task 5.1: Snapshot carries parents, domain leaves, and imports stakes

**Files:** `types/snapshot.fbs` fields from Task 1.1 already exist; modify `internal/sync/snapshot.go` (encode/decode tracker parent+childCount and full domain leaves; bump `snapshotVersion`), `cmd/node/sync.go` (import stakes into the validator set — fixes the drop); Test `internal/sync/snapshot_test.go`.

- [ ] **Test:** snapshot round-trip preserves parents, child counts, domain owners/expiries, stakes; checksum covers the new bytes.
- [ ] **Run, expect FAIL → implement → PASS.**
- [ ] **Commit:** title `Snapshot carries parents and domain leaves; stakes imported`, body `[!] joining nodes no longer rebuild the validator set unstaked`.

### Task 5.2: Rebuild the trees on snapshot apply

**Files:** Modify `cmd/node/sync.go` (after snapshot apply: `Manager.BuildFromState(...)` from the imported tracker/domains/validator set); Test at the sync-manager level.

- [ ] **Test:** rebuilt root equals the root of a node that followed the same history live.
- [ ] **Run, expect FAIL → implement → PASS.**
- [ ] **Commit:** title `Rebuild the index from snapshot state`.

### Task 5.3: Fail-closed verification and the trusted checkpoint

**Files:** Modify `cmd/node/sync.go` (during replay, assemble anchors per frontier; go live only when a stake quorum matches the locally recomputed root for some frontier ≥ snapshot round; else abort with a typed error); add a `--trust-checkpoint epoch:rootHex` node flag, **mandatory for any non-genesis join** — without it the node refuses to sync; an explicit `--insecure-bootstrap` flag (loud warning) exists for dev sims only. A default that trusts the snapshot's own validator set would let the bootstrap supply both the state and the judge, which is exactly the lie the spec promises to catch; Test.

- [ ] **Test:** a tampered snapshot (one flipped tracker parent) fails sync; an honest snapshot passes; no-quorum-in-history aborts rather than going live.
- [ ] **Run, expect FAIL → implement → PASS.**
- [ ] **Commit:** title `Fail-closed snapshot verification against the anchored root`.

### Task 5.4: Sync sim

- [ ] **Run/extend:** `TestSimProgressiveJoining` and `TestSimBatchJoining` (`test/integration/sim_progressive_test.go`) individually with bounded timeouts; add a joining-node case asserting the rebuilt root matches and a lying-bootstrap case asserting sync refusal.
- [ ] **Commit:** title `Sync sims cover verified snapshots`. **Push the batch.**

---

# Batch 6 — Client and API surface

**Spec:** §10. **Context (verified):** client tags end at `0x14` (`internal/network/messages.go`); handlers in `cmd/node/clienthandlers.go`; wallet in `pkg/client/wallet.go`; console/CLI under `cmd/cli` and the node console.

### Task 6.1: Query messages with proofs

**Files:** Modify `internal/network/messages.go` (extend the existing `DomainResolve` response **in place** with `{leaf, proof, frontier_round, index_root}` — no parallel proved message, minimal API on a pre-launch network; new tags `0x17/0x18 ListChildren`, `0x19/0x1A GetAncestors`, all registered in `clientRequestTags`); handlers in `cmd/node/indexhandlers.go`; `ListChildren` always returns the top-tree proof plus the raw child-leaf stream (one mechanism at every size: the client rebuilds the subtree and checks its root — no threshold); Test handler-level.

**Interfaces — Produces:** `ResolveDomain(name) -> {leaf, proof}` (inclusion or absence); `ListChildren(parentID) -> {topProof, leaves}`; `GetAncestors(objectID) -> {edges: [{childID, kind, parent, proof}]}`; all responses carry `{frontier_round, index_root}` and are verified against the `GetIndexAnchor` bundle of Task 3.5.

- [ ] **Test:** resolve/enumerate/ancestry round-trips verify against the bundle; absence proof for an unregistered name verifies; a truncated leaf stream fails the client-side subtree-root check.
- [ ] **Run, expect FAIL → implement → PASS.**
- [ ] **Commit:** title `Proved index queries over QUIC`.

### Task 6.2: Light-client verification library

**Files:** Create `pkg/client/verify.go` (checkpoint struct, `VerifyAnchor(bundle, validatorTree)`, `VerifyProof(root, key, value, proof)`, epoch walking via `GetValidatorTree` — add tag `0x1B/0x1C` + `clientRequestTags` + handler in the same commit); the library also exposes the spec §5 freshness choice: `WaitForFrontier(round)` (poll bundles until frontier ≥ round) vs an explicit unproven live read; Test `pkg/client/verify_test.go`.

- [ ] **Test:** a full flow against a fixture: checkpoint at epoch N → bundle whose headers carry epoch N+1 within the boundary window verified through the epoch-N validator tree (the spec §5 handoff rule) → the first N+1-attested root proves the new set → a domain proof verified against the bundle; a forged bundle below quorum fails; `WaitForFrontier` returns once a bundle covers the requested round.
- [ ] **Run, expect FAIL → implement → PASS.**
- [ ] **Commit:** title `Light-client verification: checkpoint, epoch walk, proofs`.

### Task 6.3: bpctl verbs and wallet switchover

**Files:** Modify `cmd/cli` command dispatch + console (`domain register|renew|update|transfer|delete|resolve`, `objects [owner|parent]`, `object parent <id>`, `object reparent|delete|transfer`); `domain register` on a freshly created object is the named home of the spec §8 two-transaction saga (create, wait for commit, register); `pkg/client/wallet.go` becomes a cache: `objects` reads `ListChildren(pubkey)` recursively (depth-limited) and reconciles the local file; Test client-level.

- [ ] **Test:** wallet recovery from a bare key repopulates tracked objects from the index; console verbs build valid declared-op transactions.
- [ ] **Run, expect FAIL → implement → PASS.**
- [ ] **Commit:** title `bpctl index verbs and wallet recovery from the index`.

### Task 6.4: End-to-end sim

- [ ] **Extend** an integration sim: register `demo.config` → resolve with proof from a *different* node → transfer an object subtree → enumerate from the new owner's bare key; run individually with `-timeout 600s`.
- [ ] **Commit:** title `End-to-end proved indexing sim`. **Push the batch.**

---

# Batch 7 — Dead code removal

**Spec:** §11.

### Task 7.1: Remove the dead trie registry

**Files:** Delete `pods/pod-sdk/src/domain_generated.rs`; scrub any `TrieNode`/`DomainRegistry` reference; `make -C pods/pod-system release` green.

- [ ] **Commit:** title `Remove the abandoned on-chain-trie registry`, body `[-] domain_generated.rs (unreferenced)`.

### Task 7.2: Repo-wide regression

- [ ] **Run:** `go build ./... && go vet ./... && go test ./internal/... ./pkg/... ./cmd/... -count=1 -timeout 600s`; one representative sim per family individually.
- [ ] **Commit:** title `Post-removal regression pass`. **Push the batch.**

---

# Batch 8 — Documentation

**Spec:** §11 (whitepaper consequences; §5 commit rule already landed with batch 0).

### Task 8.1: Whitepaper sections

**Files:** Modify `docs/WHITEPAPER.md`: §2 object model (parent, cascade, creation rule), §3 domains (authenticated index, owner, namespaces, rental + cap, lifecycle), §7 lifecycle (declared operations, either-ops-or-pod), §9 fees (op fees, `index_entry_fee`, 95/5 unchanged), §11 network (new messages, detached header), sync section (fail-closed snapshot, trusted checkpoint). Follow `.claude/CLAUDE.md` doc conventions (one document of record, sober register, no em dashes).

- [ ] **Commit:** title `Whitepaper: verifiable indexing`.

### Task 8.2: Final review

- [ ] **Run** `/code-review` on the branch diff at medium effort; fix findings autonomously; mark the PR ready when CI is green.
- [ ] **Commit** fixes if any. **Push.**

---

## Self-review (plan time, then hardened through a two-lens adversarial review)

- **Spec coverage:** §2→batch 0; §3→batch 1; §4→batch 2; §5/§7→batch 3; §6→batches 1-2; §8→batch 4; §9→batch 5; §10→batch 6; §11→batches 7-8; §12 testing strategy is distributed into the per-task tests (determinism 0.7/2.1, proofs 2.2/6.1, anchoring 3.x, cascade/ops 1.x, economics 4.x, sync 5.x, removal 7.x).
- **Type consistency:** `controllerOf`/`controls`/`wouldCycle` (1.3) consumed by 1.4-1.6 and 4.1; `Manager.RootAt` (2.5) consumed by 3.3/3.5; `DomainLeaf` accessor (4.1) consumed by 4.3; header helpers (3.1) shared by build/validate.
- **Review resolutions baked in (2026-07-08, two adversarial reviewers):** support-based anchor rule with same-round quorum intersection plus the indirect decision (0.3); relaxed-regime anchoring and persisted commit cursor (0.5); missing-ancestor fetch (0.6); snapshot-import commit floor (0.2); boot-time index rebuild (2.5); epoch-field fix is `d.currentEpoch` + `validateEpoch` boundary window (3.1); zero-root bounded to the genesis epoch (3.3/3.4); term-cap reverts, domain ops carry no refs, pointee control required (4.1); DomainFee remnants removed (4.4); mandatory trust checkpoint (5.3); `DomainResolve` extended in place, single ListChildren mechanism, freshness API (6.1/6.2); real sim names (0.7/1.8/5.4).
- **Execution-decided rule (2026-07-14, from the TestSimEpochs stall debug):** anchor designation eligibility = committed members with ≥1 committed vertex (full-snapshot fallback while empty). A member's registration commits ~5-25 rounds before its node first produces; a relaxed-regime designation landing in that gap wedges the cursor forever (absent producers are never relaxed-blamed, and the scan cannot pass a later-undecided round) — 6/9 sim failures. Eligibility is a pure function of committed history, threaded through the frozen snapshots (changes only at committed events).
- **First-attempt lessons baked in (implementation reverted 2026-07-13; branch `fix/deterministic-commit@439cf40` deleted after a Fable FIX_FIRST review):** vote-determined equivocation rule, no local candidate (0.1/0.3, C1); epoch state in sync snapshots + atomic cursor/epoch persist (0.5, C2); `markFullQuorumAchieved` re-wired from the strict commit path (0.4, C3); order-free relaxed candidate, committed-registrations-only freeze, latch cursor clamp (0.5, I1/I7/I8); capped-total strict quorum denominator (0.5); consistent snapshot cut + cursor semantic (0.5, I3/I4); epoch-tail rule for the indirect scan (0.3, I2); fetcher wired at every DAG construction site with hash-checked responses and a real-wiring test (0.6, I5); undecided-round gap deferred and tracked (0.6, I6); looped sim validation with a wall-time check (0.7).
- **Known deferred items (explicitly out of scope, from the spec):** committed-tx-hash pruning, `SyncBufferSec` scaling rules, slashing consumption of fault evidence, the undecided-round fetch gap (I6).
