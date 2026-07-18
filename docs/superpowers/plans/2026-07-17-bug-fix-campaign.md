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

## Remediation campaign — final-review findings (2026-07-17)

Two review passes over the full PR diff (an adversarial correctness review
and a conventions/hygiene review) plus the final corpus run produced the
findings below. The corpus came back 10 green / 7 red, and the red
signature (per-joiner fingerprint splits at epoch boundaries in
Epochs/EpochCrash/Churn/Partition, commit-liveness timeouts in
Stress/AnchorCrash, holder-routing divergence in Aggregation) empirically
confirms finding R1: the last-landed committee-freeze fix regressed
scenarios that had validated green earlier in the campaign.

Remediation rules (delta over Global Constraints):

- Remediation commits do NOT touch `test/BUGS.md` — the register is retired
  by the final batch; these findings are tracked here and in the PR body.
- Order is R1 → R2 → R3 → R4 → R5 → R6, then R7 (dead-code cleanup,
  appended below from the inventory report). R1 goes first because it is
  what broke the corpus: fixing it restores the campaign's measuring
  instrument before anything else is validated against it.

### Batch R1 — committee freeze forks after restart or sync in epoch >= 1 (Opus)

`committedMembers` is rebuilt only when `currentEpoch == 0`
(`internal/consensus/regime.go`, `restoreCommittedMembers` early-returns
otherwise) and is never persisted. A node that restarts or syncs in epoch
>= 1 comes back with an empty or partial set: the bootstrap founder path
re-seeds `{founder}`, a synced node re-fills from the first committed
registration it sees. The next `snapshotEpochHolders` then filters the
committee through that partial set, so every such node freezes a DIFFERENT
committee — execution sharding, routed reads and the BLS bitmap resolution
fork. This is exactly the corpus signature above.

- [ ] **Step 1 — failing unit test:** restart (and separately: sync) a DAG
  in epoch >= 1 with a multi-member committed set, drive the next boundary,
  assert the frozen `epochHolders` equals the pre-restart committee. Must
  fail today (founder-only or joiner-only freeze).
- [ ] **Step 2 — fix:** persist the committed member set durably alongside
  the live-validator snapshot at the commit cursor, restore it on restart
  in EVERY epoch (drop the epoch-0 guard), and carry it in the regime sync
  snapshot so a syncing node adopts the network-uniform set instead of
  reconstructing a partial one. If the sync snapshot schema must grow a
  field, that is an additive schema change — call it out in the commit
  body.
- [ ] **Step 3 — coverage hole:** extend `TestScenarioColdRestart` with an
  epochs-enabled leg (restart a node in epoch >= 1, cross a boundary,
  assert teardown convergence). This is the hole that let the regression
  through.
- [ ] **Step 4:** unit tests green; fix and scenario extension are separate
  commits.

**Orchestrator validation:** `TestScenarioEpochs`, `TestScenarioEpochCrash`,
`TestScenarioChurn`, `TestScenarioPartition` teardown convergence green;
`TestScenarioColdRestart` (new leg) green.

### Batch R2 — attested owner is not bound by the BLS proof (Opus)

The uniform-verdict fix made every node validate mutable-ref ownership of
replicated objects against the ATX's `owner` field
(`internal/consensus/commit.go`, `attestedReplicatedOwner`), but the BLS
proof only signs `ComputeObjectHash(content, version)`
(`internal/attest/hash.go`) — `owner` is attacker-controlled. An external
submitter can collect a legitimate read quorum for any replicated object,
rewrite `owner` to their own key in the ATX, sign the tx with their own
key, and steal the object: `transfer_object` performs no ownership
re-check. Before the campaign the holder-local content check rejected the
forgery; the fix removed the only authorization gate. This is GitHub issue
#7, now proven exploitable.

- [ ] **Step 1 — failing test:** at the commit-validation seam, build an
  ATX whose BLS proof is valid over `(content, version)` but whose `owner`
  field is rewritten to another key; assert every node rejects the tx.
  Must fail today (all nodes accept).
- [ ] **Step 2 — fix:** bind the owner into the attested hash —
  `ComputeObjectHash(content, version, owner)` — and update every producer
  and verifier of that hash (holder attestation, `internal/aggregation`
  verification, `pkg/daemon` collection) so a rewritten owner invalidates
  the proof. Sweep ALL call sites so the hash stays network-uniform.
- [ ] **Step 3:** forged-owner test green, whole suite green, one commit
  (reference issue #7 in the commit body).

**Orchestrator validation:** `TestScenarioAggregation` fully green,
`attested_transfer` included.

### Batch R3 — replicated-deletion accounting never runs on non-holders (Opus)

The campaign's uniform-deletion-accounting fix lives in the execution path
(`internal/state/state.go`, `applyDeletedObjects`), but non-holders skip
execution for exactly the transactions it targets: a replicated-object
deletion is a holder-gated mutable ref with no created objects, so
`executeTx` (`internal/consensus/commit.go`) skips it. The fix is dead code
for its motivating case and effective only for singletons; harmless today
(no shipped pod deletes replicated objects), but the accounting must move.

- [ ] **Step 1 — failing unit test:** commit a replicated-object deletion
  through a DAG whose node is NOT a holder (execution skipped); assert
  `deposits`/`coins_total` move identically to a holder node. Must fail
  today.
- [ ] **Step 2 — fix:** run the tracker-driven deposit release/settlement
  from the commit loop over the committed deletion set (network-uniform
  metadata) on every node; holders keep content deletion in the execution
  path; make the accounting single-shot so holder nodes do not
  double-apply.
- [ ] **Step 3:** tests green, one commit.

**Orchestrator validation:** `TestScenarioObjects`,
`TestScenarioAggregation`.

### Batch R4 — the anchor silence rule rests on a timing assumption (Opus)

The wedge fix's cert-impossible predicate has two halves. The blamer half
is monotone: more information never flips the verdict. The silence half is
NOT: `silentHolders` shrinks as vertices arrive, so a LESS informed node is
MORE willing to declare certification impossible and act. The written
defense (`internal/consensus/anchor_decision.go` docstring) is that honest
producers do not back-fill and that `anchorSilenceSpanRounds` (20) exceeds
any honest delivery skew — the latter is a timing assumption, weakest under
partitions, which is where the rule matters most.

- [ ] **Step 1 — adversarial sim:** at a unit/harness seam, feed two honest
  nodes the same DAG except one is starved of a holder's vertices for more
  than `anchorSilenceSpanRounds` while the other receives them late; drive
  the anchor decision on both and assert they reach the SAME verdict.
- [ ] **Step 2 — if the sim splits the verdict:** harden the predicate so
  the silence half is evidence-based rather than time-based (for example,
  require the blame quorum alone, or an explicit absence proof anchored in
  committed rounds); re-run the sim.
- [ ] **Step 3 — if the sim holds:** keep the rule, extend the docstring's
  safety argument with the sim as its evidence, and keep the sim in the
  test suite as a regression guard. One commit either way.

**Orchestrator validation:** `TestScenarioAnchorCrash`,
`TestScenarioPartition`.

### Batch R5 — delegated stake silently leaves total_bonded at removal (Opus)

`applyPendingRemovals` drops the validator's whole `EffectiveStake` (self
plus delegated) from `total_bonded`, but `returnDeregisteredStake` only
credits the released SELF stake back to a coin. Outstanding delegated
positions keep existing while their amount has left every supply term: the
identity `coins_total + total_bonded + deposits + fees_in_flight ==
total_supply` runs deflationary until every delegator undelegates. Likely
pre-existing, but a real break.

- [ ] **Step 1 — failing unit test:** deregister a validator that carries
  delegated stake; assert the supply identity holds immediately after the
  removal while the delegations are still outstanding. Must fail today.
- [ ] **Step 2 — fix:** keep outstanding delegated amounts inside the
  bonded term until each delegator undelegates, or settle them back to
  delegator coins at removal — pick ONE, network-uniform, and justify the
  choice in the commit body. New mutation means an `internal/events`
  constructor if one appears.
- [ ] **Step 3 — scenario:** extend `TestScenarioStake` with a
  deregistration-with-delegators leg asserting the identity at teardown.
- [ ] **Step 4:** tests green; fix and scenario are separate commits.

**Orchestrator validation:** `TestScenarioStake`, `TestScenarioEpochs`.

### Batch R6 — hygiene: register citations, stale annotations, event catalog (Sonnet)

The late fix commits reintroduced register citations into scenario files,
some annotations claim open bugs that are now fixed, and the event catalog
misses an event added by the campaign.

- [ ] **Step 1 — purge the 20 register citations**, rewriting each as a
  self-contained statement of the invariant and failure mechanism (Global
  Constraints rule): `scenario_partition_test.go` (200-201, 205, 247),
  `scenario_stake_test.go` (72, 76, 82-83), `scenario_consensus_test.go`
  (406-407), `scenario_fees_test.go` (19, 23, 24, 25, 152, 168, and the
  RUNTIME failure string at 159 — highest priority),
  `scenario_churn_test.go` (53), `scenario_epochs_test.go` (34, 37),
  `scenario_aggregation_test.go` (55). Do not touch the four clean files
  (`scenario_sponsored_test.go`, `scenario_objects_test.go`,
  `scenario_epoch_crash_test.go`, `scenario_cold_restart_test.go`).
- [ ] **Step 2 — reconcile stale annotations** against the LATEST
  per-scenario validation logs (post R1-R5): a "known red" note whose
  scenario is now green is deleted; a still-red scenario is a triage for
  the orchestrator, never a comment rewrite.
- [ ] **Step 3:** add `stake.released` to `test/TESTING.md`'s event catalog
  table.
- [ ] **Step 4:** build and vet green, one commit.

**Orchestrator validation:** none beyond build and vet (comments and docs
only).

### Batch R7 — dead-code cleanup (Sonnet)

A repo-wide inventory (deadcode reachability from both mains, with and
without test roots; staticcheck U1000; cargo check on both pod crates and
wasm-gas; every candidate re-verified by hand) found 33 safely dead items,
a half-dead re-export shim, and two Rust vestiges. Test-only symbols
(harness machinery, test-seam DAG options `WithMinStake` /
`WithVotingCapMille` / `WithThermostat`, `StorageRefund`,
`isVertexCommitted`, `genesis.BuildSponsoredTx` / `BuildAttestedTx` /
`BuildDeregisterValidatorRawTx`, `logger.With`) are the designed test
surface — do NOT touch them.

- [ ] **Commit 1 — Go dead-symbol sweep.** Delete, verifying each still has
  zero callers at deletion time (post R1-R6 code may have changed):
  - `internal/consensus/dag.go` `WithCommissionBPS` (keep the
    `commissionBPS` field and its default — only the never-called setter
    goes);
  - `internal/consensus/dag.go` `DAG.isInTransition` (superseded by
    `isInTransitionOrBuffer`);
  - `internal/consensus/thermostat.go` `defaultThermostatParams`;
  - `internal/consensus/types.go` `quorumThreshold` const;
  - `internal/consensus/commit_test.go` `addQuorumVertices`;
  - `internal/state/state.go` `rebuildObjectWithID` AND
    `rebuildObjectCustomID` (dead chain — the latter's only caller is the
    former);
  - `internal/genesis/transaction.go` `BuildRegisterValidatorTx` (replaced
    by `BuildRegisterValidatorRawTx`, which stays);
  - `internal/logger/logger.go` `Timed`;
  - `internal/podvm/system_test.go` `buildCoinObject` (self-documented as
    replaced by `buildCoinObjectWithOwner`; do NOT confuse with the live
    `buildCoinObject` in `internal/state/state_test.go`);
  - `cmd/node/node.go` `defaultSyncBufferSec` const;
  - `internal/network/messages.go` `minClientTag` const;
  - `test/harness/cluster.go` `Cluster.Daemon`;
  - `test/harness/options.go` `WithMinValidators`, `WithGossipFanout`,
    `WithSyncBuffer`, `WithInitialMint`, `WithTransitionGrace`,
    `WithTransitionBuffer`, `WithStake` (the fields keep their defaults in
    `cluster.go`/`setup.go`; only the never-used overrides go);
  - `test/scenarios/helpers_test.go` `waitCommitted`,
    `requireCommittedReason`, `waitCommittedAll`, `waitOwner`.
- [ ] **Commit 2 — retire the `internal/aggregation` re-export shim.** The
  package re-exports `internal/attest` function-for-function but production
  wires only five (`DeriveFromED25519`, `DecodeRequest`,
  `EncodePositiveResponse`, `EncodeNegativeResponse`,
  `IsAttestationRequest` — these stay). Delete the six dead re-exports
  (`GenerateBLSKeyFromSeed`, `Verify`, `AggregateSignatures`,
  `BuildSignerBitmap`, `EncodeRequest`, `BLSPublicKeySize`); migrate the
  package's own tests off the five test-only re-exports
  (`GenerateBLSKey`, `DecodePositiveResponse`, `DecodeNegativeResponse`,
  `GetMessageType`, `QuorumSize`) to direct `attest.*` calls, then delete
  those re-exports too (minimal-public-API rule: the shim's unfinished
  migration ends here).
- [ ] **Commit 3 — Rust vestiges.** Delete the unused
  `extern "C" fn gas(cost: u32)` declaration in `pods/pod-sdk/src/lib.rs`
  (gas metering is injected at the binary level by `wasm-gas`, which adds
  the `env.gas` import itself), and delete
  `pods/pod-system/src/functions/deregister_validator/args.rs` entirely
  plus its `mod args;` / `pub use args::Args;` lines (the empty `Args`
  struct is never constructed; the Go side confirms deregistration takes
  no arguments). This commit touches `pods/`: both crates' builds and
  tests must pass, plus the wasm build recipe.
- [ ] **Commit 4 — keep and document the domain-deletion scaffolding.**
  `internal/events/state.go` `DomainDeleted` and
  `internal/state/domain.go` `domainStore.delete` are pre-wired for the
  domain deletion operation the whitepaper documents as specified but not
  yet exposed by the system pod. Keep both; add one short doc-comment line
  to each stating exactly that (no campaign references).
- [ ] Each commit: build + vet + the touched packages' tests green,
  foreground, bounded.

**Deliberately NOT cleaned:** `genesis.BuildDeregisterValidatorRawTx`
duplicates `pkg/client`'s deregister construction path and survives only
through a `cmd/node` handler test — it is a legitimate test seam today,
but the duplication is a format-divergence risk to revisit when the client
transaction builders are next touched.

**Orchestrator validation:** none beyond the per-commit gates (pure
deletions and comments).

### Batch R8 — boundary-straddling liveness credits fork the reward split (Opus)

Diagnosed against the R1-validated corpus (Partition symmetric and
heal_under_traffic, Epochs teardown): the node that caught up through
replay reaches the right round with every fingerprint section identical
EXCEPT singletons — coin balances split by zero-sum deltas, the signature
of a conserved pool divided by divergent weights. Proven at vertex level:
`creditLiveness` (`internal/consensus/commit.go`, ~line 378) and the
`epochFees` accumulation (~line 365) credit the epoch that is CURRENT when
a vertex's batch commits, but the commit-vs-skip decision for a
boundary-adjacent anchor round is not network-uniform between the live
path and the catch-up path — a replay node committed round 99 as its own
anchor while live nodes skipped it and committed those vertices after the
round-100 boundary. The same vertices' liveness lands in epoch 1 on one
node and epoch 2 on another; when the producer's effective stake changes
across that boundary the reward weights differ and every subsequent split
diverges permanently. `epochFees` carries the identical latent
misattribution (not yet observed only because the straddling vertices held
no transactions).

- [ ] **Step 1 — failing unit test:** two DAGs commit the same
  boundary-straddling vertices in different batches (one commits the
  adjacent round as its own anchor, one skips it and commits those
  vertices past the boundary); assert both settle IDENTICAL rewards and
  identical `epochRoundsProduced`/`epochFees` buckets. Must fail today.
- [ ] **Step 2 — attribute by round-owned epoch:** `creditLiveness` and
  the `epochFees` accumulation key their bucket by
  `commitEpochForRound(v.Round())` (`epoch.go`, ~line 542) instead of the
  current epoch, so the attribution is a pure function of the committed
  log.
- [ ] **Step 3 — defer each epoch's settlement until its rounds drain:**
  `transitionEpoch` keeps the membership work (removals, additions,
  holder freeze, epoch increment) at the boundary, but runs
  `runThermostat` + `distributeEpochRewards` for epoch E only once the
  cursor has passed `boundary(E) + EpochGraceRounds` (in practice: settle
  E at the NEXT boundary), reading E's now-final bucket. Carry the
  deferred bucket in the persisted accumulators
  (`epoch_accumulators.go`) and the sync regime blob (`regime_sync.go`)
  so a restart or joiner settles the deferred epoch identically.
- [ ] **Step 4 — scenario expectations:** the deferral shifts WHEN
  `epoch.rewards.distributed` fires (epoch E's rewards at boundary E+1).
  Check `scenario_epochs_test.go`'s expectations and adjust if the
  timing semantics legitimately changed — an event-timing change is a
  breaking change to call out in the commit body.
- [ ] **Step 5:** tests green; one commit (plus the scenario-expectation
  commit if step 4 changes files).

**Orchestrator validation:** `TestScenarioPartition` (symmetric and
heal_under_traffic now green, 5/5 total), `TestScenarioEpochs` full
teardown, `TestScenarioStake`.

### Batch R9 — consecutive dead-designated rounds wedge the relaxed regime (Opus)

Diagnosed (~99% deterministic on EpochCrash; same family as the
Partition/across_epoch_boundary stall — cursor frozen while production
races ahead). Mechanism: the cluster is still in the RELAXED bootstrap
regime when the kill lands (`strictStartRound = minValidators-commit +
grace + buffer` puts the whole run inside the relaxed window), and in
that regime BOTH wedge escapes are disabled — `verdictFromTally` never
blames a relaxed round and `anchorCertImpossible` short-circuits to
false. A round designated to a single dead producer still resolves
through the next certified anchor, but TWO CONSECUTIVE dead-designated
rounds block `anchorStatus`'s forward scan forever (first: undecided
queried round, continue; second: undecided, not impossible, wait), so
the certified anchor one round further is never reached. Every node
wedges identically at the same round.

- [ ] **Step 1 — failing unit test** (the diagnosis prototyped it):
  a relaxed-regime DAG with two consecutive rounds designated to
  producers that hold no vertex at their round and are silent across the
  deep span; the commit cursor must pass them and reach the certified
  anchor beyond. Must wedge today. Keep the strict-regime twin asserting
  the existing blame path still handles it.
- [ ] **Step 2 — regime-independent impossibility, integrated with the
  silence hardening:** extend `certImpossibility`
  (`internal/consensus/anchor_decision.go`) with a check that runs BEFORE
  the relaxed short-circuit: a designated producer with NO stored vertex
  at the round that is silent across the deep stored span
  (`silentHolders`) can never be certified — there is no candidate to
  cite. Classify it as the reversible (silence) grade, so
  `scanPastUndecided`'s adjacent-round guard keeps governing it, and
  teach `adjacentCertifyAgrees` the dead-pair case: when the QUERIED
  round's designated producer also holds no stored vertex and is
  span-silent, every resolver skips it on this node's evidence, so
  passing the adjacent silence-impossible round cannot flip the
  resolution. Justify the relaxed-regime safety argument in the
  docstring by the regime's own trust model: relaxed certification
  accepts a single supporter (no quorum intersection), so a span-silence
  skip is not a weaker guarantee than the regime's certification rule —
  and the strict regime keeps the full R4 intersection guard unchanged.
- [ ] **Step 3:** existing wedge and silence-sim tests stay green
  (the R4 fork sim especially — the new escape must not weaken it);
  full suite green; one commit.

**Orchestrator validation:** `TestScenarioEpochCrash`,
`TestScenarioPartition` (across_epoch_boundary), `TestScenarioChurn`.

### Batch R10 — reward-coin designation diverges between live and replay (Opus)

Diagnosed with per-section fingerprint sub-hashes: the healed node splits
ONLY in the singleton (coin) section — tracker, validator, supply and
counter sections all match, so the reward share is identical but its
liquid credit lands on a DIFFERENT coin. Mechanism: when a registration
carries no explicit reward-coin arg, `setRewardCoinFromArgs`
(`internal/consensus/commit.go`) falls back to `ownedGasCoin`, a LIVE
coin-store read at the moment the registration is applied — a
non-committed, timing-dependent input that resolves differently on the
live path and the replay path. The divergence hides because
`hashValidators` (`internal/sync/fingerprint_hash.go`) omits
`RewardCoin`, and `creditCoin` is version-neutral, so nothing shows
until epoch credits land. This is the replay-path sibling of the
sync-path reward-coin loss fixed at the start of the campaign.

- [ ] **Step 1 — failing unit test:** apply the same committed
  registration (zero reward-coin arg) through the live-commit path and
  through a replay, assert both designate the IDENTICAL reward coin.
  Must fail today.
- [ ] **Step 2 — fix:** designate the reward coin only from committed,
  network-uniform inputs: the explicit arg, else a coin identified by
  the registration transaction's own committed data (e.g. its gas
  coin), else zero — the coinless path already compounds rewards into
  self-stake deterministically. No live store read at apply time.
- [ ] **Step 3 — close the blind spot:** include `RewardCoin` in
  `hashValidators`' per-validator hash. Breaking fingerprint change —
  call it out in the commit body.
- [ ] **Step 4:** full suite green, one commit.

**Orchestrator validation:** `TestScenarioPartition` (heal_under_traffic,
across_epoch_boundary), `TestScenarioEpochCrash`.

### Batch R11 — deep-gap ancestry backfill too slow for reintegration (Opus)

Diagnosed: NOT the anti-fork guard (`adjacentCertifyAgrees` returned true
at every gate) and not a permanent wedge — the reintegrated node does
converge given time. The causal backfill (`requestMissingAncestors`,
gated by `stallTicks >= 2` in the stall handlers) recovers roughly one
missing frontier layer per 50 ms stall tick, so a node healing across a
multi-hundred-round gap closes it over dozens of seconds; bounded
scenario waits (and EpochCrash's tx-commit window) expire first. The
`TODO` at `internal/consensus/commit.go:191` already flags the missing
batching.

- [ ] **Step 1 — failing unit test:** a DAG missing a deep ancestry gap
  (tens of vertices across many rounds) must close it within a small
  bounded number of fetch iterations. Count fetch rounds through a seam;
  must fail today (one layer per iteration).
- [ ] **Step 2 — fix:** on a causal stall, request the FULL missing
  ancestry breadth (batched, deduplicating in-flight requests, bounded
  message size — split into chunks if needed), and trigger without
  waiting extra ticks when the gap is deep. Keep the fetch targeted at
  committed-anchor ancestry so it cannot flood.
- [ ] **Step 3:** full suite green, one commit. The convergence oracle
  stays STRICT — no harness window loosening in this batch; if
  revalidation still misses windows, that is a follow-up decision with
  data, not a silent widening.

**Orchestrator validation:** `TestScenarioPartition` (all five),
`TestScenarioEpochCrash`, `TestScenarioStress`.

### Batch R12 — the partition scenarios never tested the strict regime (Opus)

Final diagnosis of the last two reds (symmetric, heal_under_traffic):
both die at `requirePlateau` BEFORE `Heal()` is called — the catch-up
path the earlier batches hardened is never even reached in these legs.
Root cause: the strict-regime latch is only evaluated during epoch 0,
and the harness's sequential bootstrap commits the five founding
registrations at rounds ~0/24/50/76/102 — across THREE 50-round epochs —
so committed membership never reaches minValidators inside the epoch-0
window and the whole scenario runs in the relaxed single-supporter
regime, where a partition halts neither side (each certifies disjoint
rounds alone; zero-rollback checks cannot see it). The scenario's
premise comment is simply false. The node behaves correctly for a
relaxed cluster; the node-side latch limitation is filed as issue #8.

- [ ] **Step 1:** raise `partitionEpochLength` so epoch 0 spans the whole
  founding bootstrap (150 verified empirically: all five registrations
  commit in epoch 0, the latch fires at ~round 100 with strictStart
  ~130, boundary 1 at 150 still applies the equal-stake arithmetic).
  Update the scenario's premise comment to state the real constraint:
  epoch 0 must outlast the sequential bootstrap or the strict regime
  never arms (issue #8).
- [ ] **Step 2:** make `requirePlateau` race-free: capture the
  production frontier F0 at the moment of the cut and fail only if the
  committed round REACHES F0 — a no-quorum side can legitimately drain
  commits below F0 (certifications for rounds >= F0 cannot exist
  locally), so the drain tail stops flaking the assertion while genuine
  post-cut quorum progress still fails it. The oracle stays strict.
- [ ] **Step 3:** `go vet ./test/scenarios/` green; one commit; the
  orchestrator validates `TestScenarioPartition` in full — with the
  strict regime finally governing, the plateau assertions become
  meaningful and the post-heal catch-up path gets its first real
  exercise.

### Batch R13 — post-heal recovery: frontier re-broadcast, range catch-up, grace retry (Opus)

The first-ever strict-regime partition runs surfaced two real node bugs
and one scenario flake (closure corpus: 15/17).

**Commit 1 — symmetric heal deadlock (node).** After a symmetric freeze
each side holds only its own subset of the frozen round's vertices.
Forward gossip sends a vertex ONCE at production and never again; the
pull path walks parent links backward from held vertices and can never
discover the other side's childless sibling leaves. Both sides wedge
forever (`cannot produce: no quorum` 183 times in 90 s, zero vertices
received). Fix: in `tryProduceVertex`'s cannot-produce branch
(`internal/consensus/dag.go`), re-broadcast this node's own latest
produced vertex (`getByRoundProducer` → `sendVertex`) on the liveness
tick — idempotent, already-signed, safety-neutral. Failing test first:
two DAG halves frozen on disjoint round-N vertex sets must reconverge
once vertices flow again.

**Commit 2 — deep-behind reintegration (node).** The batched backfill
fixed the `onCausalStall` path only; a far-behind running validator
wedges on the `onWaitStall` path, which still surfaces one round-layer
per fetch cycle (~6 rounds per 90 s measured; store promotion is
all-or-nothing until the whole gap bridges). Fix: a vertex-RANGE
catch-up request in the sync protocol (`internal/sync/protocol.go`,
served from the store by round range, chunked) invoked from the
wait-stall handler when the gap between the commit cursor and the
highest buffered round exceeds a threshold — the gap closes in a few
round-trips. Failing test first: a DAG behind by tens of rounds with the
far frontier buffered must close the gap in a bounded number of fetch
iterations.

**Commit 3 — grace-leg retry contract (scenario).** ~1-in-3 flake:
`transferWithRetry`'s pre-submit routed read can hit a transient
deadline exactly at the epoch transition (sequential holder probes at
5 s each vs the client's 8 s budget) and the leg fatals on any non-typed
error. Fix in `test/scenarios/scenario_aggregation_test.go`: treat a
transient timeout like a stale collection (log, continue the retry
loop), and schedule the retry a beat past the transition rather than at
it. The retry budget already exists; no oracle weakening.

**Orchestrator validation:** `TestScenarioPartition` (full),
`TestScenarioAggregation` (×2 runs), `TestScenarioJoinLoad` (regression
guard for the sync protocol change).

### Batch R14 — the relaxed regime must not survive the bootstrap (Opus)

Found by the CI smoke on slow hardware, reproduced locally under CPU
load with the exact CI signature AND a zero-rollback contradiction
(round 155 committed with two different anchors on different camps).
Mechanism, traced end to end: the harness forces grace=100/buffer=100
on clusters >= 10 (`bigClusterTransition`), pushing strictStartRound to
~244 while every bond commits by ~round 100 — the whole traffic phase
runs in the relaxed single-supporter regime, where the commit-vs-skip
verdict is view-dependent under delivery skew. Transient splits then
become permanent state forks through measured channels: a gossip-
duplicated tx first-committed in different epochs on different camps
(fees land in different settlement buckets → same pool total, different
coin bytes), a backlog swept across a boundary (frozen producedMembers
differ → anchor designation forks → the double anchor). Fast local
hardware never opens the window, which is why the whole campaign
validated green.

- [ ] **Step 1 — failing test:** at the unit seam, a relaxed-window DAG
  where one camp receives a laggard's backlog before deciding a round
  and the other decides first must NOT reach opposite commit-vs-skip
  verdicts once the fix holds (encode the run-1 interleaving; must fail
  today). Reuse the diagnosis artifacts in the scratchpad
  (`run1.log`, `run3.log`, tmpdir copies) for the exact shape.
- [ ] **Step 2 — stake-aware strict latch** (`internal/consensus/regime.go`):
  arm `strictStartRound` from COMMITTED state — the round where the
  minValidators committed members all hold non-zero committed stake —
  plus the node-default grace/buffer (20+10), instead of a fixed margin.
  The inflated margins exist only because pre-bond strict would wedge on
  zero stakes; a stake-aware latch removes the dead window without that
  wedge. Keep the latch's epoch-0 evaluation scope unchanged (the
  cross-epoch limitation stays issue #8).
- [ ] **Step 3 — harness margins** (`test/harness/cluster.go`,
  `bigClusterTransition`): drop the forced 100/100 to the node defaults
  now that the latch arms itself on committed stake; scenarios must
  still pass their own bootstrap.
- [ ] **Step 4 — relaxed resolveIndirect never skips a resolvable round**
  (`internal/consensus/anchor_decision.go`): in the relaxed regime,
  WAIT instead of skipping when the queried round's candidate is absent
  locally but the producer is not span-silent, and when the candidate is
  stored but omitted by the resolver while a single supporter remains
  possible. (Validated under load by the diagnosis: reduces the run-1
  catastrophe to one residual split; defense in depth under fix 2.)
- [ ] **Step 5:** full suite green including the R9 dead-pair wedge test
  and the R4 fork sim; one commit per concern (latch+margins, resolver
  hardening) or one combined — implementer's judgment, called out.

**Orchestrator validation:** Stress under CI (the goal loop), plus local
`TestScenarioStress`, `TestScenarioJoinLoad`, `TestScenarioChurn`,
`TestScenarioPartition`.

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
- Remediation batches follow the same rules. Order: R1 → R2 → R3 → R4,
  then R8 (it gates the remaining Partition/Epochs reds), then R5, R9,
  R6, R7; the final batch (corpus + register retirement + PR ready)
  closes the campaign after them.
