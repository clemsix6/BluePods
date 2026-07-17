# Bug-Fix Campaign Implementation Plan

> **For agentic workers:** this plan is executed batch-by-batch by fresh
> subagents (one batch = one subagent, except where a batch says otherwise).
> Each subagent receives ONE batch section plus the Global Constraints below
> and nothing else. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** fix every fixable entry of `test/BUGS.md` (the 13-entry register
produced by the scenario-corpus campaign), on a single dedicated branch with
its own PR.

**Architecture:** one batch per bug, except where bugs share a code area and
are fixed together (batches 3 and 4). Batches 1–2 unlock the corpus-wide
signals (fingerprint convergence, commit liveness) that later batches need
for validation. Bugs 4 and 6 are design decisions and are parked in a final
decision batch for the user.

**Tech Stack:** Go node (`internal/`, `cmd/node/`, `pkg/`), scenario harness
(`test/harness`, `test/scenarios`), systematic-debugging discipline (root
cause before fix, failing test first).

## Global Constraints

- **Workspace:** ALL work happens in the worktree `.wt/bug-campaign`
  (branch `fix/bug-campaign`, forked from `origin/main` at `c44e97a`). Never
  touch the main checkout. No `cd`: absolute paths for edits, `-C` for every
  command (`git -C .wt/bug-campaign …`, `go -C .wt/bug-campaign …`).
- **One bug = one commit.** Batching never merges commits. Commit format:
  short title (no prefix), body lines prefixed `[+]/[&]/[!]/[-]`, NO footers,
  NO Co-Authored-By.
- **Every fix commit updates its `test/BUGS.md` entry** in the same commit:
  mark the entry `**Status: FIXED** (<short mechanism>, this commit)`, keep
  the evidence text. If a scenario carries a "known red" annotation for the
  entry (e.g. partition sub-tests for entry 9), remove the annotation in the
  same commit.
- **Code comments never cite the register.** `test/BUGS.md` is retired at
  the end of the campaign (user decision), so no code comment, test doc
  comment, or test failure message may reference "BUGS.md" or "entry N" —
  write them self-contained: state the invariant and the failure mechanism
  directly. Register references belong only in BUGS.md itself, commit
  messages, and the PR body.
- **Root cause before fix** (systematic-debugging). Entries whose root cause
  is already proven (1, 2, 11) go straight to TDD; the others start with a
  diagnosis task whose deliverable is a written root cause plus a failing
  test.
- **TDD:** failing unit test first whenever the bug is unit-reproducible.
  Run unit tests FOREGROUND with a bounded timeout
  (`go -C .wt/bug-campaign test ./<pkg>/ -run '<Name>' -count=1 -timeout 120s`).
  Subagents NEVER launch background test runs and NEVER run anything longer
  than ~8 minutes — scenario sims are the orchestrator's job, after the
  commit (a sim failure becomes a follow-up fix commit).
- **Batch green =** `go -C .wt/bug-campaign build ./... && go -C
  .wt/bug-campaign vet ./... && go -C .wt/bug-campaign test ./internal/...
  ./cmd/... ./pkg/... ./test/harness/ -count=1 -timeout 10m` all pass. If a
  batch touches `pods/` or `wasm-gas/`, their builds/tests must pass too
  (none of the batches below is expected to).
- **Events:** every NEW state mutation added by a fix gets an
  `internal/events` constructor; renaming/removing an event or attribute is
  a breaking change to call out in the commit (see `test/TESTING.md`).
- **Push after each batch**; the orchestrator updates the PR body (State
  checklist) after each push.
- **Model policy (user directive):** Sonnet for evident/mechanical fixes,
  Opus where diagnosis or design reflection is needed. Assignments are per
  batch below. The orchestrator (Fable) reviews every Opus batch's diff
  before push.

## Scenario validation reference (orchestrator only)

Scenarios run ONE AT A TIME, output redirected in full to a scratchpad file
(never piped through `tail`), wall-clock vs Go time compared to detect
machine-sleep contamination:

```bash
go -C .wt/bug-campaign test ./test/scenarios/ -run '^TestScenarioX$' -v -count=1 -timeout <bound> > <scratchpad>/X.log 2>&1
```

Bounds: Bootstrap 4m, ConsensusBasics/Fees/Sponsored/Joining/Crash/AnchorCrash/JoinLoad 9m,
Stake/Aggregation/Churn 10m, Objects/Epochs/ColdRestart 12m, Stress/EpochCrash 14m, Partition 20m.

Until batch 4 lands, multi-node teardowns STAY RED on the supply identity
(entry 8's `+1000 × registrations`, entry 12's `-1000` per failed
created-object tx). Batch-level validation greps must therefore target the
signal the batch fixes, not overall scenario exit codes, until the end of
the campaign.

---

### Batch 1 — Bugs 1 + 2: multi-node fingerprint divergence (Sonnet)

Both root causes are PROVEN and both fixes are parked (uncommitted) on the
old worktree `.wt/fix-convergence` — this batch ports them properly, with a
regression test for each. Two commits.

**Files:**
- Modify: `internal/consensus/commit.go` (`handleRegisterValidator`, ~1084–1122)
- Create: `internal/consensus/commit_registration_test.go`
- Modify: `cmd/node/sync.go` (`buildValidatorSetFromSnapshot`, ~349–370)
- Modify: `cmd/node/sync_test.go`
- Reference (READ ONLY, do not copy the files themselves):
  `/Users/clement/BluePods/.wt/fix-convergence/internal/consensus/zz_diagnostic_test.go`
  (two-DAG diagnostic for bug 1 — crib its DAG construction),
  and the parked diffs reproduced verbatim below.

**Interfaces:** no public API change. `committedMembers` and
`recordCommittedMember` already exist in `internal/consensus`.

#### Commit 1 of 2 — bug 1 (epochAdditions gated on committed membership)

- [ ] **Step 1: write the failing regression test.** In
  `internal/consensus/commit_registration_test.go`, port the two-DAG
  diagnostic into a permanent test
  `TestHandleRegisterValidator_EpochAdditionsUniformAcrossSelfAdd`: build two
  DAGs with the same genesis validator and `epochLength > 0`; on DAG A only,
  perform the optimistic self-add (call `validators.Add` for the joining key
  before any commit, mirroring `cmd/node/registration.go`'s
  `selfAddToValidatorSet`); replay the IDENTICAL committed
  register_validator transaction through `handleRegisterValidator` on both;
  assert both DAGs end with the same `epochAdditions` contents (the joining
  key present exactly once on each). Use
  `.wt/fix-convergence/internal/consensus/zz_diagnostic_test.go` as the
  construction reference.
- [ ] **Step 2: run it, expect FAIL** (DAG A's `epochAdditions` empty):
  `go -C .wt/bug-campaign test ./internal/consensus/ -run 'TestHandleRegisterValidator_EpochAdditionsUniformAcrossSelfAdd' -count=1 -timeout 120s`
- [ ] **Step 3: apply the proven fix** to `handleRegisterValidator` — exactly
  this parked diff:

```diff
@@ internal/consensus/commit.go, in handleRegisterValidator @@
 	}

+	// Read committed membership BEFORE recordCommittedMember below admits pubkey
+	// to it. A node that optimistically self-added its own registration to the
+	// LIVE validator set (cmd/node/registration.go selfAddToValidatorSet, called
+	// before the registration it just submitted ever commits) sees isNew=false
+	// from validators.Add below for THIS SAME committed transaction, while every
+	// other node sees isNew=true — an asymmetric epochAdditions bookkeeping
+	// (test/BUGS.md entry 1: the fingerprint hashes epochAdditions verbatim, so
+	// this alone forks the checksum from the moment a second validator joins).
+	// committedMembers is admitted ONLY through this committed-only path, never
+	// through an optimistic self-add (recordCommittedMember's own guarantee), so
+	// "was already a committed member" is identical on every node and is the
+	// correct gate for epochAdditions instead of the live-set isNew.
+	wasCommittedMember := d.committedMembers[pubkey]
+
 	isNew := d.validators.Add(pubkey, quicAddr, blsPubkey)
 	events.ValidatorRegistered(pubkey, quicAddr)
@@
-	// Track mid-epoch additions for churn limiting
-	if isNew && d.epochLength > 0 {
+	// Track mid-epoch additions for churn limiting. Gated on committed membership
+	// (wasCommittedMember, captured above), not the live-set isNew: every node
+	// agrees on which registrations were already committed, regardless of any
+	// node's own optimistic self-add.
+	if !wasCommittedMember && d.epochLength > 0 {
 		d.epochAdditions = append(d.epochAdditions, pubkey)
 	}

 	// Retry pending vertices — some may be from this newly registered producer.
-	// Run async to avoid blocking the commit path.
+	// Run async to avoid blocking the commit path. isNew (the live-set add) is the
+	// right gate here: it fires whenever THIS node's local set actually gained the
+	// producer just now, whether via this commit or (having already gained it
+	// through an earlier optimistic self-add) not at all — a redundant retry on a
+	// node that already knew the producer is harmless, so no symmetry is required.
 	if isNew {
 		go d.processPendingVertices()
 	}
```

- [ ] **Step 4: run the test again, expect PASS**, then the full package:
  `go -C .wt/bug-campaign test ./internal/consensus/ -count=1 -timeout 10m`
- [ ] **Step 5: update `test/BUGS.md` entry 1** (Status: FIXED) and commit:

```
Gate epochAdditions on committed membership, not the live-set add

[!] handleRegisterValidator: optimistic self-add made isNew=false on the
    self-registering node only, forking epochAdditions and the fingerprint
    (test/BUGS.md entry 1)
[+] regression test: two DAGs, one self-added, identical committed replay,
    equal epochAdditions
[&] BUGS.md entry 1 marked fixed
```

#### Commit 2 of 2 — bug 2 (RewardCoin carried from the synced snapshot)

- [ ] **Step 1: port the parked regression test**
  `TestBuildValidatorSetFromSnapshot_CarriesRewardCoin` into
  `cmd/node/sync_test.go` (parked version below — port as-is):

```go
func TestBuildValidatorSetFromSnapshot_CarriesRewardCoin(t *testing.T) {
	n := &Node{}

	var founder, rewardCoin consensus.Hash
	founder[0] = 0xAA
	rewardCoin[0] = 0xBB

	synced := []*consensus.ValidatorInfo{
		{Pubkey: founder, QUICAddr: "quic://founder:9000", SelfStake: 1000, RewardCoin: rewardCoin},
	}

	vs := n.buildValidatorSetFromSnapshot(synced)

	got := vs.Get(founder)
	if got == nil {
		t.Fatal("founder missing from rebuilt validator set")
	}
	if got.RewardCoin != rewardCoin {
		t.Errorf("RewardCoin dropped rebuilding validator set from snapshot: got %x, want %x", got.RewardCoin, rewardCoin)
	}
}
```

  (Keep the parked doc comment explaining why the founder is the permanent
  victim: non-founders repair their coin via their own register_validator
  replay; the founder never re-registers.)
- [ ] **Step 2: run it, expect FAIL**:
  `go -C .wt/bug-campaign test ./cmd/node/ -run 'TestBuildValidatorSetFromSnapshot_CarriesRewardCoin' -count=1 -timeout 120s`
- [ ] **Step 3: apply the proven one-line fix + comment** in
  `buildValidatorSetFromSnapshot` (`cmd/node/sync.go`): after
  `vs.AddWithStake(v.Pubkey, v.QUICAddr, v.BLSPubkey, v.SelfStake,
  v.DelegatedTotal, v.Jailed)`, add `vs.SetRewardCoin(v.Pubkey,
  v.RewardCoin)`, with the parked explanatory comment (AddWithStake doubles
  as the epoch-holder-snapshot constructor which intentionally omits
  RewardCoin; dropping it here zeroes the founder's designation on every
  syncing node — BUGS.md entry 2).
- [ ] **Step 4: run the test (PASS), then the package**:
  `go -C .wt/bug-campaign test ./cmd/node/ -count=1 -timeout 10m`
- [ ] **Step 5: update `test/BUGS.md` entry 2** (Status: FIXED; note that the
  deregistration-principal blast radius gets re-verified by the orchestrator
  at TestScenarioEpochs) and commit:

```
Carry RewardCoin when rebuilding the validator set from a synced snapshot

[!] buildValidatorSetFromSnapshot dropped every validator's RewardCoin;
    founder's designation stayed zero forever on synced nodes, forking
    epoch credits and the fingerprint (test/BUGS.md entry 2)
[+] regression test TestBuildValidatorSetFromSnapshot_CarriesRewardCoin
[&] BUGS.md entry 2 marked fixed
```

**Orchestrator validation (after push):** `TestScenarioJoining` (9m) and
`TestScenarioConsensusBasics` (9m) in background — expected signal: teardown
convergence check GREEN (identical checksums across nodes); supply identity
still red (+4000, entry 8 — expected until batch 4). `TestScenarioEpochs`
(12m): founder checksum no longer splits; check whether the deregistration
principal gap (entry 2 blast radius) is gone — if not, file it as its own
follow-up entry. Then remove the old worktree:
`git -C /Users/clement/BluePods worktree remove --force .wt/fix-convergence
&& git -C /Users/clement/BluePods branch -D fix/multi-node-convergence`.

---

### Batch 2 — Bug 9: network-wide commit wedge (Opus)

The dominant liveness bug: the commit cursor freezes permanently while round
production races on, under four documented triggers (double SIGKILL after a
boundary; partition across a boundary, persisting after Heal; partition/heal
cycles mid-epoch; a temporary multi-minute form under load). One commit
(plus follow-ups if scenario sims disagree).

**Files (starting points, diagnosis may widen):**
- Read: `internal/consensus/` commit path — `anchorStatus`,
  `resolveIndirect`, `anchorProducerFor`, the round-skip path
  (`consensus.round.skipped` emission), epoch snapshot lookup.
- Modify: wherever the root cause lands (expected: anchor
  decision / indirect resolution / skip machinery).
- Create: a unit test in `internal/consensus/` reproducing the wedge.

**Known evidence (from `test/BUGS.md` entry 9 — read the full entry first):**
- Signature: `lastCommitted` frozen, production rounds race ahead by
  hundreds, EVERY `consensus.round.advanced` after the freeze carries
  `designated` all-zero (`anchorProducerFor` failing for far-ahead rounds
  whose epoch snapshot cannot exist yet), and `consensus.round.skipped`
  almost never fires — the skip path never clears the wedged rounds.
- The common factor across triggers: anchor rounds whose producer was
  UNREACHABLE (not necessarily dead) when the round passed; the
  decision/recovery machinery never resolves them, even after connectivity
  returns. No validator ever leaves the set in triggers 2 and 3, so
  dead-producer vertex recovery is not the sole mechanism.
- Zero rollback holds throughout — fix must not weaken safety to buy
  liveness. Quorum arithmetic: `3 × capped_sum >= 2 × total`.

#### Task 2.1 — root cause + failing unit test

- [ ] Read entry 9 in full, then the anchor decision code. Answer in
  writing, with file:line references: (a) exactly why `anchorProducerFor`
  returns zero for rounds past the wedge point; (b) why the skip path never
  fires for the unresolved anchor rounds; (c) what SHOULD resolve an anchor
  whose producer's vertices were missed during unreachability, and why it
  doesn't after connectivity returns; (d) whether the temporary form
  (Stress/double_spend_storm) is the same mechanism with eventual
  resolution or a distinct outage.
- [ ] Build a DAG-level unit test reproducing the wedge deterministically
  (no real network): drive two-or-more DAG instances to an epoch boundary,
  withhold one producer's vertices around an anchor round (simulating
  unreachability), deliver them late, and assert the commit cursor
  eventually passes the withheld round. Expect FAIL.

#### Task 2.2 — minimal fix

- [ ] Fix at the root cause (candidate directions the diagnosis must
  confirm or refute: the anchor decision must be re-evaluated when late
  vertices arrive; the skip decision must be reachable for rounds whose
  producer never showed; `anchorProducerFor` must not depend on an epoch
  snapshot that cannot exist yet). ONE mechanism, no defensive patches
  stacked on top.
- [ ] Unit test from 2.1 passes; full `./internal/consensus/` suite passes
  (10m bound). Commit (title e.g. "Resolve wedged anchor rounds when late
  vertices arrive"; body `[!]` entry 9 + `[+]` unit test + `[&]` BUGS.md
  entry 9 marked fixed + remove the "known red" annotations in
  `test/scenarios/scenario_partition_test.go`).

**Orchestrator validation (after push):** `TestScenarioEpochCrash` (14m),
`TestScenarioPartition` (20m), `TestScenarioStress` (14m), sequentially in
background. Expected: post-kill boundary wait, post-heal catch-up waits and
flapping traffic-progress waits all green; `double_spend_storm` verdict
waits green in the full run. Convergence green (batch 1). Supply identity
still red (entry 8) — expected.

---

### Batch 3 — Bugs 11 + 13 + 7: object read path and ATX verdicts

Three bugs on the object/ATX path, fixed in this order because 11's cascade
and 13's leading suspect (stale routed reads) share the routed `GetObject`
path. Three commits.

#### Task 3.1 — bug 11: inter-node GetObject must set LocalOnly (Sonnet)

**Files:**
- Modify: `cmd/node/clienthandlers.go` (`requestObjectFrom`,
  `fetchObjectFromHolder`)
- Test: `cmd/node/clienthandlers_test.go` (create if absent)

- [ ] **Step 1: failing test.** Unit-test that the inter-node request built
  by `requestObjectFrom` carries `LocalOnly: true` (test at whatever seam is
  cheapest: construct the request struct via the same code path, or a
  handler-level test asserting a holder lacking the object answers
  not-found WITHOUT re-entering `fetchObjectFromHolder`). Expect FAIL.
- [ ] **Step 2: fix.** Set `LocalOnly: true` in the
  `network.GetObjectRequest` built by `requestObjectFrom` — this makes the
  code match `fetchObjectFromHolder`'s own doc comment ("the remote handler
  returns not-found rather than re-routing, preventing cascades").
- [ ] **Step 3:** test passes; `./cmd/...` suite passes. Update BUGS.md
  entry 11 (FIXED), commit:

```
Set LocalOnly on inter-node GetObject probes

[!] requestObjectFrom omitted LocalOnly, so a globally-absent object
    fanned out into a mesh-wide cascade and a client timeout instead of a
    prompt not-found (test/BUGS.md entry 11)
[+] regression test on the inter-node request
[&] BUGS.md entry 11 marked fixed
```

**Orchestrator validation:** `TestScenarioStake` (10m) —
`undelegate_returns_principal` green (prompt not-found well under the 8s
QUIC timeout).

#### Task 3.2 — bug 13: attested transfer wedges its object (Opus)

**Files (diagnosis decides):** `pkg/daemon` (collection/quorum assembly),
`cmd/node/clienthandlers.go` (routed reads), `internal/consensus` ATX
commit; scenario `test/scenarios/scenario_aggregation_test.go`.

- [ ] Read entry 13 in full. Discriminate the two candidate mechanisms.
  **Evidence hint from the campaign's diagnostic run:** after attempt 1's
  submission, the three HOLDER nodes emitted `state.object.updated
  version:1` for the object — the first ATX DID apply on the holders while
  the routed poll through `Client(0)` (a non-holder) never observed the
  ownership change. This points to mechanism A: the routed read path serves
  a stale owner, the client then retries a transfer of an object it no
  longer owns, and the daemon's `attestation quorum impossible` is CORRECT
  behavior against a moved object. Confirm (or refute) this with a targeted
  unit/integration test at the routed-read seam: a non-holder serving
  `GetObject` for a replicated object another node updated must return the
  post-update owner/version.
- [ ] Fix at the confirmed root (if mechanism A: the non-holder read path —
  stale cache, missing version check, or reading tracker/local state
  instead of probing holders; note task 3.1's LocalOnly fix changes probe
  semantics — build on it, don't fight it). Failing test first.
- [ ] Update BUGS.md entry 13 (FIXED, with the discriminated mechanism
  written down), commit (`[!]` + `[+]` test + `[&]` register).

**Orchestrator validation:** `TestScenarioAggregation` full run (10m) —
`attested_transfer` green in the FULL run (isolation was already green;
the full-run context is the signature).

#### Task 3.3 — bug 7: uniform commit verdicts for replicated objects (Opus)

**Files:** `internal/consensus/commit.go` (`validateMutableRefOwnership`,
~732) and whatever deterministic ownership source the fix settles on;
scenario `test/scenarios/scenario_consensus_test.go`
(`object_create_transfer` uniform-verdict assertion).

- [ ] Read entry 7. The commit verdict must be deterministic and
  network-uniform; today holders validate ownership from local content
  while non-holders reject the same committed tx with `ownership`. Decide
  the uniform rule from the code (candidates: validate against
  network-uniform object METADATA — the tracker every node maintains — 
  instead of holder-only content; or restrict the content-based check to
  holders and make non-holders accept the holders' deterministic outcome).
  Constraint: the verdict, the `tx.committed` event and the fail-reason
  counters must be identical on every node, holders or not, and the chosen
  source must itself be provably uniform.
- [ ] Failing unit test: two DAGs (one holder, one non-holder) must produce
  the SAME verdict for the same committed replicated-object mutation.
- [ ] Fix, test green, `./internal/...` suite green. Update BUGS.md entry 7
  (FIXED), commit.

**Orchestrator validation:** `TestScenarioConsensusBasics` (9m) —
`object_create_transfer` verdict map uniform across the 5 nodes.

---

### Batch 4 — Bugs 8 + 12 + 14 + 3: fee, deposit, bond and reward accounting (Opus)

The two supply-identity leaks are mirror images in the same accounting path
(`internal/consensus/commit.go` `deductFees`/`calculateTxFeeSplit`,
`internal/state` `applyCreatedObjects`/`computeStorageDeposit`), and bugs
14 and 3 live in the adjacent boundary/reward code
(`internal/consensus/epoch.go`). Four commits. The supply identity
`coins_total + total_bonded + deposits + fees_in_flight == total_supply`
must hold EXACTLY after each commit.

#### Task 4.1 — bug 8: registration stamps a deposit no coin pays

- [ ] Read entry 8. The register_validator tx is fee-exempt (a joining
  validator has no coin), yet `applyCreatedObjects` stamps a 1000 storage
  deposit for its created replication-0 object — the deposit term grows
  with nothing leaving `coins_total`: +1000 supply per registration,
  permanently, on every node. Decide the accounting rule: either the
  fee-exempt registration's created object carries a ZERO storage deposit
  (deposit stamping follows fee payment — nothing paid, nothing locked), or
  the deposit is genuinely funded by some payer. Prefer the first unless
  the code/whitepaper gives the deposit a load-bearing role for this
  object; check `docs/WHITEPAPER.md`'s fees section and keep it accurate
  (one document of record).
- [ ] Failing unit test at the state/commit seam: committing a
  registration must leave the supply identity exact (delta 0).
- [ ] Fix, tests green. Update BUGS.md entry 8 (FIXED), commit.

#### Task 4.2 — bug 12: failed execution leaks the storage fee component

- [ ] Read entry 12. Fees (consumed + storage, from the DECLARED
  created-objects header) are debited before execution; on success the
  storage part becomes a locked deposit via `applyCreatedObjects`; on pod
  FAILURE that code never runs and the storage part vanishes from every
  supply term (deflationary leak, exactly -1000 in the repro). Decide the
  conservation rule (candidates: on execution failure, fold the already-
  debited storage portion into the epoch fee pool exactly like the consumed
  portion — simplest, conserves supply, keeps "declared fees are always
  charged" semantics; or refund it to the gas coin — check which one the
  whitepaper's fee semantics implies before choosing). Symmetry with 4.1's
  rule matters: one coherent story for "storage component when no object
  ends up created".
- [ ] Failing unit test: a committed tx that declares created objects and
  fails execution leaves the supply identity exact, and the storage portion
  lands in the chosen term (pool or coin), with the matching
  `internal/events` emission (new mutation ⇒ new event constructor if the
  chosen path creates one).
- [ ] Fix, tests green. Update BUGS.md entry 12 (FIXED), commit.

#### Task 4.3 — bug 14: deregistration principal credited to no coin

Filed during batch-1 validation (entry 14 in `test/BUGS.md`): at an epoch
boundary, a deregistered validator's bond leaves `total_bonded` but the
principal is never credited to any coin — the supply identity loses the
full bond (~199.78 B for two validators at `TestScenarioEpochs`' teardown,
now network-uniform since the entry-2 fix).

- [ ] Read entry 14. Find the deferred-deregistration application path in
  `internal/consensus/epoch.go` and establish where the released principal
  is SUPPOSED to land (the validator's reward coin is the natural target
  now that entry 2 guarantees synced nodes know it; decide what happens
  when the validator has none — one coherent rule with task 4.4's choice).
- [ ] Failing unit test: applying a deregistration at a boundary must keep
  the supply identity exact — the released bond lands in a coin
  (`coins_total` grows by exactly the principal), with the matching
  `internal/events` emission.
- [ ] Fix, tests green. Update BUGS.md entry 14 (FIXED), commit.

#### Task 4.4 — bug 3: reward deferred indefinitely without a reward coin

- [ ] Read entry 3. A validator with no designated reward coin has its
  liquid epoch share folded into the carry-over pool forever (fairness gap,
  not a supply gap — nothing is lost). Choose the smallest coherent fix
  (candidates: credit the accumulated share the moment the validator
  designates a coin — needs per-validator deferral tracking; or pay the
  share into the validator's bonded stake instead of the pool; or make a
  reward-coin designation mandatory at registration so the state is
  unreachable). Pick what `distributeEpochRewards`' existing structure
  supports most simply; document the choice in the BUGS.md entry.
- [ ] Failing unit test on `distributeEpochRewards` for the no-coin case,
  asserting the chosen behavior.
- [ ] Fix, tests green. Update BUGS.md entry 3 (FIXED), commit.

**Orchestrator validation (after push):** `TestScenarioFees` (9m):
`split_exceeds_balance_is_execution_error` AND
`underfunded_gas_coin_pools_partial` green, including the per-node supply
identity. `TestScenarioEpochs` (12m): `supply_identity_across_boundary`
green (was +9000) AND the teardown supply identity exact (was short
~199.78 B on entry 14's deregistration principal). From this batch on,
multi-node teardowns are expected
FULLY green (convergence from batch 1, liveness from batch 2, supply from
this batch) — any residual red is a new finding to triage, not to ignore.

---

### Batch 5 — Bug 10: validator set lost across restart (Opus)

**Files (starting points):** `cmd/node/init.go` (`buildValidatorSet`),
`internal/consensus/dag.go` (`SeedGenesisValidator`), the epoch/holder
snapshot persistence (`internal/consensus/epoch_persist.go` neighborhood) as
the likely durable source; scenario
`test/scenarios/scenario_cold_restart_test.go`.

- [ ] Read entry 10 in full. Two co-factors, both must be fixed: (a)
  `buildValidatorSet` starts every process run from an essentially empty
  in-memory validator set (self-only), so a restart forgets every other
  validator's stake — `totalBonded` collapsed from 500 G to 100 G in the
  repro, five nodes reported five different validator counts; (b)
  `SeedGenesisValidator` runs on every bootstrap start and OVERWRITES the
  founder's self-stake and reward coin back to genesis values while coin
  debits persist in the durable ledger. Investigate what durable state
  already exists locally (holder snapshots, synced snapshots, committed
  ledger) that a restarting node can rebuild its LIVE validator set from,
  and write the root-cause note down before fixing.
- [ ] Failing unit test at the init seam: build a validator set, persist
  whatever the fix decides is the durable source, simulate a restart
  (fresh `buildValidatorSet` over the same data dir), assert stakes and
  reward coins survive; plus a test that `SeedGenesisValidator` on a
  NON-EMPTY restored set merges (or no-ops) instead of overwriting.
- [ ] Fix, tests green (`./cmd/...` + `./internal/...`). Update BUGS.md
  entry 10 (FIXED), commit.

**Orchestrator validation:** `TestScenarioColdRestart` (12m) —
`founder_stake_preserved` green, teardown convergence AND supply identity
green (batches 1/2/4 all landed by now, so a clean run is the bar).

---

### Batch 6 — Bug 5: genesis reserve coin absent from the object tracker (Sonnet)

**Files:** `internal/genesis` (`SeedGenesisLedger`); its test file.

- [ ] **Step 1: failing test.** After `SeedGenesisLedger`, the tracker must
  contain the genesis reserve coin (mirror whatever every other
  object-creation path registers via `TrackObject`). Expect FAIL.
- [ ] **Step 2: fix.** Add the `TrackObject` call next to the existing
  `SetObject`, matching the arguments every other creation path uses.
- [ ] **Step 3 (CHECK, do not skip):** genesis seeding runs on the
  bootstrap node only; syncing nodes rebuild their tracker from the synced
  snapshot. Verify the snapshot wire format carries tracked objects such
  that synced nodes end up with the reserve coin tracked too — if it does
  not, the fix as-is would FORK the fingerprint between bootstrap and
  synced nodes; in that case extend the snapshot path in the same commit
  and say so in the commit body.
- [ ] **Step 4:** tests green. Update BUGS.md entry 5 (FIXED), commit.

**Orchestrator validation:** `TestScenarioBootstrap` (4m) +
`TestScenarioConsensusBasics` (9m): teardown convergence still green
(catches the fork risk of step 3), tracker-derived aggregates coherent.

---

### Batch 7 — Bugs 4 + 6: resolved decisions (delegated to the orchestrator)

The user delegated both decisions to the campaign (2026-07-17). Chosen:

- **Bug 4 → fix it properly (Opus, one commit):** run the refund/burn
  accounting for deleted replicated objects on EVERY node from tracker
  metadata (the deposit amount is network-uniform), so the supply terms
  move identically everywhere. Failing unit test first: a deletion of a
  replicated object must move `deposits`/`coins_total` identically on a
  holder DAG and a non-holder DAG. Resolves the `TODO` in
  `internal/state/state.go`'s deletion path. New mutation on non-holders ⇒
  reuse the existing deletion/refund events; add an `internal/events`
  constructor only if a genuinely new mutation appears.
- **Bug 6 → close the spec-code gap on the docs side (one docs commit):**
  the whole domain surface (register/update/delete) is unreachable from any
  client; exposing it is feature work for a later cycle. Add a short,
  factual note to `docs/WHITEPAPER.md`'s naming/domains section stating the
  capability is specified but not yet exposed by the system pod, so the
  whitepaper stays accurate as the document of record.

---

### Batch 8 — Bug 15: healed-node fingerprint divergence (Opus)

Filed during batch-2 validation (entry 15 in `test/BUGS.md`): after every
partition/heal cycle, the formerly isolated node reaches the majority's
committed round with a DIFFERENT fingerprint and never reconverges. Proven
pre-existing (identical pattern with and without the wedge fix); previously
masked by entries 1/2. Scheduled after the batch-2 kill-shape follow-up
lands (shared `internal/consensus` files).

- [ ] Diagnose: how does an isolated node catch up after heal — live commit
  replay of gossiped vertices, or the sync/snapshot path? Diff the healed
  node's state against a majority node's at the same committed round in a
  unit/harness seam (the fingerprint components: coins, objects, validator
  set, epoch bookkeeping) to identify WHICH component splits, then trace
  where the catch-up path diverges from the live-commit path. Candidates:
  state the isolate mutated optimistically while alone; epoch bookkeeping
  (additions/holders) rebuilt differently on catch-up; a sync path that
  drops a field (entry 2's class of defect, different field).
- [ ] Failing test at the smallest seam that shows the split component.
- [ ] Fix at the root; scenario validation (orchestrator):
  `TestScenarioPartition` all five sub-tests' teardown convergence green.
- [ ] Update BUGS.md entry 15 (FIXED), commit.

### Final batch — full-corpus validation (orchestrator)

- [ ] Re-run the FULL corpus, one scenario at a time with the bounds table
  above (adapt the campaign script to `.wt/bug-campaign`), full logs to the
  scratchpad, wall-vs-Go time checked.
- [ ] Expected: every scenario green except what batch 7's decisions leave
  open. Any red: triage per `test/TESTING.md` (harness flake → fix harness;
  project bug → new BUGS.md entry + follow-up fix commit on this branch).
- [ ] Retire the register (user request): DELETE `test/BUGS.md` in the
  final commit — the fixed-entry history lives in git and in this
  campaign's commits. Update `test/TESTING.md`'s triage protocol in the
  same commit: a confirmed project bug is now fixed on a branch (or filed
  as an issue), no longer registered in a standing file; drop every
  BUGS.md reference (TESTING.md, scenario comments, CLAUDE.md's project
  layout line).
- [ ] Update the PR body (all State boxes ticked), mark ready for review,
  final review pass on the whole diff by Fable before merge.

## Execution notes

- Batch order is the validated priority: 1 → 2 → 3 → 4 → 5 → 6 → 7.
  Batches 5 and 6 are independent of 3/4 and may be dispatched in parallel
  with them if the orchestrator wants wall-clock savings — they touch
  disjoint files; rebase-order their commits at push time.
- Fresh subagent per batch (per task for the multi-task batches 3 and 4 —
  each task is its own commit and its own agent). Subagents receive: this
  file's Global Constraints + their batch section + the relevant BUGS.md
  entry text. They do NOT get the whole plan.
- Every scenario-level validation belongs to the orchestrator (runs exceed
  the subagent watchdog), runs in background, and a red after a pushed
  commit becomes a follow-up fix commit — never an amend.
