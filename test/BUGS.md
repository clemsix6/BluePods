# Known project bugs

This file is the living register of project bugs found by the scenario
corpus in `test/scenarios`. It is not a TODO list and not a backlog: a bug
here is a confirmed defect in the node, reproduced by a scenario that stays
red on purpose. Fixing these bugs is out of scope for the test-environment
cycle; the corpus is the instrument, not the fix.

## Protocol for adding an entry

Before registering a new bug, the burden of proof is on the harness: rule out
a race or a timeout in the test itself (rerun the scenario; check whether a
longer bound or a harness fix removes the failure). A flaky scenario is a
harness defect, always in scope to fix directly, never registered here.

Once a failure is confirmed as the project's:

- Give it a short title and the suspected subsystem (file/function).
- Name the scenario(s) that reproduce it.
- Record the event-journal (or other) evidence that pins it down.
- Leave the scenario red. Never skip, weaken, or work around the assertion
  that exposes it, and never fix the underlying defect in this cycle.

If a later scenario reproduces an already-registered bug, annotate the
existing entry ("reproduced by `<scenario name>`") instead of duplicating it.

## Entries

### 1. Multi-node fingerprint divergence, cause A: optimistic self-add races committed registration replay

**Subsystem:** `cmd/node/registration.go` (optimistic validator self-add) +
`internal/consensus/commit.go` `handleRegisterValidator`.

Registering a second validator leaves `epochAdditions` bookkeeping asymmetric
across nodes until the first epoch boundary: a node's optimistic local
self-add of its own registration races the committed replay of that same
registration arriving through consensus, so nodes disagree on `epochAdditions`
contents from the moment a second validator joins.

**Evidence:** confirmed by byte-level fingerprint inspection on a real 3-node
cluster — stable divergence across 40+ polls (`test/harness/cluster_test.go`
`TestClusterBasics`, run with `WithoutInvariants` for exactly this reason).
On a 5-node cluster the teardown convergence check observes five distinct
checksums at one identical committed round, stable across reruns (for
example round 278: `12746c30 / c0ecae56 / 6c5ec5eb / 2bb1bb20 / 853ec5d3`).

**Reproduced by:** `TestClusterBasics` (`test/harness`, pre-existing);
`TestScenarioBootstrap` is green (single node), and every multi-node
scenario's teardown convergence check is red on this entry:
`TestScenarioConsensusBasics`, `TestScenarioFees`, `TestScenarioAggregation`,
`TestScenarioEpochs`, `TestScenarioObjects`, `TestScenarioJoining`,
`TestScenarioStress`, and the adversarial corpus:
`TestScenarioCrash`, `TestScenarioAnchorCrash`, `TestScenarioJoinLoad`,
`TestScenarioPartition` (all three sub-scenarios)
(each red ONLY here — their in-scenario liveness, resync, plateau, heal and
zero-rollback assertions are green, with the divergent-checksums-at-one-round
sweep as the signature, stable across two runs each).

### 2. Multi-node fingerprint divergence, cause B: reward distribution sensitive to validator insertion order

**Subsystem:** `internal/consensus/epoch.go` `distributeEpochRewards`.

After an epoch transition, the founder's reward coin (the genesis reserve
coin) ends up with a node-dependent balance. Reward distribution over the
validator set appears sensitive to insertion order, so nodes that added
validators in different orders (or at different times) diverge on the
founder's coin balance once an epoch boundary lands.

**Evidence:** consistent with the same fingerprint divergence investigation
as bug 1; distinguished from cause A by manifesting specifically at and after
an epoch transition, rather than immediately on second-validator
registration.

**Reproduced by:** `TestScenarioPartition` (all sub-scenarios cross
reward-bearing epoch boundaries before partitioning; the teardown sweeps
repeatedly show most non-founder checksums agreeing while the founder's
diverges, for example symmetric at round 330: founder `7a0a7b7e` against
`16512932` on all four non-founders — the founder's reward coin is the
genesis reserve coin, a singleton whose content is fingerprinted).

### 3. A validator without a reward coin defers its liquid reward indefinitely

**Subsystem:** `internal/consensus/epoch.go` (reward crediting / carry-over
pool).

A validator that has not designated a reward coin has its liquid epoch
reward share folded into the carried-over pool rather than credited anywhere,
indefinitely deferring it. This is a fairness/liveness gap, not a supply
safety issue: the amount is never lost, only never paid to that validator
while it lacks a reward coin.

**Evidence:** derived from reading `distributeEpochRewards` and the Task 3/4
prerequisite-fix work (fee conservation and reward-coin designation), which
established the pool-carry-over mechanism this gap is compatible with.

### 4. Deletion accounting for replicated objects runs only on holders

**Subsystem:** `internal/state/state.go` (existing `TODO`, deletion path).

`coins_total`/`total_supply` diverge across nodes if a pod deletes a
replicated object carrying a locked deposit: the refund/burn accounting for a
deletion currently only runs on the object's holders, not on every node, so
non-holder nodes never adjust their counters. Latent today because the only
shipped pod (the system pod) deletes singletons only, which every node holds.

### 5. The genesis reserve coin is never registered in the object tracker

**Subsystem:** `internal/genesis` (`SeedGenesisLedger`) vs. every other
object-creation path.

`SeedGenesisLedger` calls `SetObject` for the genesis reserve coin but never
calls `TrackObject`, unlike every other object-creation path in the protocol.
The reserve coin is therefore absent from the tracker used to compute
tracker-derived aggregates (deposits, tracked-object counts).

### 6. Spec-code gap: no domain update/delete capability

**Subsystem:** `internal/state/domain.go` (`domainStore.delete`).

The whitepaper describes domain updates and deletions, but the protocol
exposes no way to trigger them: `domainStore.delete` has zero callers. This
is a spec-code gap rather than a runtime defect (nothing crashes; the
capability is simply unreachable), noted here so `state.domain.updated` /
`state.domain.deleted` scenario coverage cannot be written until the gap is
closed.

### 7. Divergent commit verdicts for replicated-object transactions: non-holders reject with `ownership`

**Subsystem:** `internal/consensus/commit.go` `validateMutableRefOwnership`.

The commit-time mutable-ref ownership check resolves each referenced object
through the local state (`d.coinStore.GetObject`) and rejects the transaction
when the object is not found. A replicated object's content only exists on
its holders, so for an attested transaction mutating a replication>0 object,
holder nodes validate ownership and apply the mutation while every non-holder
node rejects the SAME committed transaction with the `ownership` failure.
The commit decision is supposed to be deterministic and network-uniform;
here the per-node verdict (and the `tx.committed` event, and the fail-reason
counters behind it) depends on local holdership.

State stays coherent by accident: fee deduction runs before the ownership
check (so the singleton gas coin is debited identically everywhere) and
execution sharding means non-holders would not have applied the object
mutation anyway. The divergence is in the commit verdict itself.

**Evidence:** on a 5-node cluster, a `transfer_object` of a replication-3
object produced the per-node verdict map
`[0:success 1:failed:ownership 2:success 3:failed:ownership 4:success]` —
exactly the 3 holders accepting and the 2 non-holders rejecting.
Reproducible on rerun.

**Reproduced by:** `TestScenarioConsensusBasics` (`object_create_transfer`),
which stays red on its uniform-verdict assertion. `TestScenarioAggregation`
asserts the attested path through holder reads instead, so the ATX path's
state-level value stays covered without duplicating this red.

### 8. Validator registration stamps a storage deposit no coin ever pays: supply inflates per registration

**Subsystem:** the register_validator commit flow — `internal/consensus/commit.go`
`deductFees` (registration is fee-exempt) together with the deposit stamping
for its created object (`internal/state` `applyCreatedObjects` /
`computeStorageDeposit`), and the system pod's `register_validator` creating
a replication-0 object.

A validator joining through the register_validator transaction creates a
replication-0 object owned by the validator, and the state layer stamps its
storage deposit (1000 at the default fee params) into the object tracker.
But register_validator is exempt from fee deduction (a joining validator has
no coin yet), so no coin is ever debited for that deposit: the locked-deposit
term grows with nothing leaving `coins_total`. The protocol supply identity
`coins_total + total_bonded + deposits + fees_in_flight == total_supply`
inflates by exactly one storage deposit per registered validator, on every
node, permanently.

**Evidence:** on a 2-node cluster with stake setup disabled (registration is
the ONLY operation), both nodes report `deposits=1000, fees=0, coins and
bonded unchanged, delta=+1000`, with the journal showing the registration's
`state.object.created` (replication 0, owned by the joining validator) and
`fees.deposit.locked amount=1000` and NO `fees.deducted`. On a 5-node
cluster (4 non-founder registrations) the identity is off by exactly +4000.
A single-node cluster (no tx-path registration) holds the identity exactly
(delta 0) through faucet, split, and underfunded-split — so fee pooling
(including the Task 3 partial-coverage fix) is not the leak.

**Reproduced by:** `TestScenarioFees` (`underfunded_gas_coin_pools_partial`
asserts the per-node supply identity and stays red on it: +4000 with 4
registrations) and `TestScenarioEpochs` (`supply_identity_across_boundary`:
+9000 with 9 registrations, stable across epoch boundaries and reward
distributions).

Also reproduced at teardown of `TestScenarioConsensusBasics` (5-node
cluster, 4 non-founder registrations), now that `CheckInvariants` runs the
supply check even when the convergence check (entry 1) disagrees rather than
aborting the invariant pass on it: `node 4:
coinsTotal(499999968790)+totalBonded(500000000000)+deposits(15600)+feesInFlight(19610)=1000000004000
!= totalSupply(1000000000000)` — the exact predicted +4000 for 4
registrations. Before that harness fix this teardown check was reachable in
every multi-node scenario but always masked by entry 1's convergence
`t.Fatalf` aborting the invariant pass first.

### 9. Network-wide commit wedge after two validators crash right after an epoch boundary

**Subsystem:** `internal/consensus` commit path — the anchor decision /
indirect resolution machinery around the crash window (`anchorStatus`,
`resolveIndirect`, and whatever vertex-recovery path should backfill a dead
producer's last vertices).

On a 10-node, 50-round-epoch, equal-stake cluster, SIGKILLing two
non-founder validators immediately after an epoch boundary can wedge the
COMMIT CURSOR of every surviving node permanently, while round production
races on unhindered. Survivor stake is ~80 percent of the total, so the 2/3
quorum arithmetic guarantees liveness on paper; the wedge is in the
decision/recovery machinery, not the arithmetic. This is a liveness bug,
not a safety one: zero rollback holds throughout (no contradicted anchors,
strictly increasing per segment).

**Evidence:** `TestScenarioEpochCrash` kills nodes 8 and 9 right after
`epoch.transitioned` epoch 6 (boundary round 300) fires on node 0. In the
wedged runs, EVERY surviving node freezes at `lastCommitted` 307-309 (the
kill lands around round 305) while the production round races past 680:
final status on all 8 survivors reads `round=683-684, epoch=6,
lastCommitted=309` with 7 of 8 fingerprints byte-identical at committed
round 309. Every `consensus.round.advanced` event after the wedge carries
`designated` all-zero (`anchorProducerFor` failing for far-ahead rounds
whose epoch snapshot cannot exist yet), and only 2 `consensus.round.skipped`
events fire in the entire run, so the skip path never clears the wedged
rounds. Reproduced twice consecutively on the current code; one earlier run
escaped the wedge and completed (restart, epoch reconvergence, uniform
commit all green), so the trigger is timing-dependent within the
kill-at-boundary window.

**Reproduced by:** `TestScenarioEpochCrash`, red in-scenario at the
post-kill boundary wait in the wedged runs (the dominant outcome), and red
only at teardown convergence (entries 1/2) in runs that escape the wedge.

### 10. Bootstrap restart reverts founder self-stake and reward coin to genesis values while coin debits persist

**Subsystem:** `cmd/node/init.go` (genesis seeding on startup), suspected
together with a missing validator-set persistence path;
`internal/consensus/dag.go` `SeedGenesisValidator`.

`SeedGenesisValidator` runs on every bootstrap start, restart included
(unlike `SeedGenesisLedger`, which is genesis-only), and its
`SetSelfStake`/`SetRewardCoin` calls overwrite rather than merge. A bootstrap
node that restarts over its own data directory therefore re-seeds the
founder's self-stake and reward coin designation back to their GENESIS
values, even if the founder's live self-stake or reward coin has since
diverged (through bonding, delegation, or reward crediting) — while any coin
debits already applied against the old values persist in the durable ledger.
The two are not kept consistent across a restart.

Latent today: no current scenario mutates the founder's stake or reward coin
before restarting the bootstrap node, so nothing in the corpus exercises
this path yet.
