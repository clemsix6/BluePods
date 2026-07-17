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

**Status: FIXED** (epochAdditions gated on committed membership instead of
the live-set isNew; this branch).

Registering a second validator leaves `epochAdditions` bookkeeping asymmetric
across nodes until the first epoch boundary: a node's optimistic local
self-add of its own registration races the committed replay of that same
registration arriving through consensus, so nodes disagree on `epochAdditions`
contents from the moment a second validator joins.

Root cause proven by direct investigation (candidate fix parked on
`fix/multi-node-convergence`, not yet applied): the optimistic self-add puts
the node's own key in the LIVE validator set before its registration commits,
so when `handleRegisterValidator` replays the committed transaction,
`validators.Add` returns `isNew=false` on the self-registering node and
`isNew=true` everywhere else — and `isNew` gates the `epochAdditions` append
that the fingerprint hashes. A two-DAG unit diagnostic fed the identical
committed transaction shows `epochAdditions` empty on the self-added node and
populated elsewhere; gating on the committed-members set (never touched by
the optimistic add) instead of `isNew` makes both DAGs agree.

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
`TestScenarioStress`, `TestScenarioSponsored`, and the adversarial corpus:
`TestScenarioCrash`, `TestScenarioAnchorCrash`, `TestScenarioJoinLoad`,
`TestScenarioPartition` (all three sub-scenarios)
(each red ONLY here — their in-scenario liveness, resync, plateau, heal and
zero-rollback assertions are green, with the divergent-checksums-at-one-round
sweep as the signature, stable across two runs each). `TestScenarioSponsored`
(5-node, 4 non-founder registrations) shows the same signature: five distinct
checksums at identical committed round 215
(`023c8d62 / e5f5869f / 83b8df70 / c4a2a09a / c83b03be`), with both its
sponsored-transaction sub-tests green.

### 2. Multi-node fingerprint divergence, cause B: synced nodes drop every validator's RewardCoin, so epoch credits diverge from the bootstrap node

**Subsystem:** `cmd/node/sync.go` `buildValidatorSetFromSnapshot`.

**Status: FIXED** (RewardCoin now carried by buildValidatorSetFromSnapshot;
this branch). The deregistration-principal blast radius noted below gets
re-verified by the orchestrator at `TestScenarioEpochs`.

Root cause proven by direct investigation (candidate fix parked on
`fix/multi-node-convergence`, not yet applied). The original hypothesis here
(reward distribution sensitive to validator insertion order) was tested
directly and DISPROVEN: two DAGs fed identical validators in different
insertion orders credit identical balances. The real mechanism: every
joining, non-bootstrap node rebuilds its live validator set from the synced
snapshot through `buildValidatorSetFromSnapshot`, which calls
`vs.AddWithStake(...)` per validator and never `vs.SetRewardCoin(...)` — the
snapshot wire format carries RewardCoin correctly, but the rebuild silently
drops it. A non-founder's own coin gets repaired later by its explicit
`register_validator` replay; the founder never re-registers, so on every
synced node the founder's RewardCoin stays zero forever. Any epoch-boundary
credit that targets a RewardCoin then lands on the bootstrap node but is
skipped on synced nodes, and the fingerprints diverge.

**Evidence:** on a real 5-node cluster at the epoch-0 boundary, the
bootstrap node credited the undistributed reward remainder while all four
synced nodes carried it forward instead — deltas of exactly 4301 in founder
coin balance, `feesInFlight`, and `coinsTotal` simultaneously, with
`epoch.rewards.distributed` identical (pool=8000) on all five nodes. With
the one-line candidate fix (SetRewardCoin after AddWithStake) the same
cluster converges to a byte-identical checksum on all five nodes.

**Blast radius includes deregistration principal.** At
`TestScenarioEpochs`' teardown (10 nodes, two validators deregistered at a
boundary), synced node 1 reports
`coinsTotal(1000169008)+totalBonded(799222082216)+deposits(20000)+feesInFlight(0)=800222271224 != totalSupply(1000000000000)`
— short by almost exactly the two deregistered validators' ~100 B bonds:
released from `totalBonded`, credited to no coin the synced nodes know
about, while node 0's checksum (`6f5b79f2`) splits from the pack. The
deregistration-principal credit path needs re-verification once the
RewardCoin fix lands.

**Reproduced by:** `TestScenarioPartition` (teardown sweeps repeatedly show
non-founder checksums agreeing while the founder's diverges, for example
symmetric at round 330: founder `7a0a7b7e` against `16512932` on all four
non-founders), `TestScenarioStress` and `TestScenarioEpochCrash` (11 of 12
and 7 of 8 checksums identical, only the founder's out), and
`TestScenarioEpochs` (deregistration evidence above).

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

**Status: FIXED** (the reserve coin is now registered with the tracker right
after genesis seeding, via the already-exported `TrackObject`, matching every
transaction-driven creation path; this branch).

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

**Status: FIXED** (a replicated mutable ref's owner is now read from its
attested copy in the committed ATX, which every node holds identically, instead
of holder-only local content; singletons keep the local-content check; this
branch).

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

**Status: FIXED** (a transaction that references no gas coin locks a zero
storage deposit on the objects it creates, so the fee-exempt registration path
debits no coin and locks nothing; this branch).

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

Also reproduced at teardown of `TestScenarioSponsored` (5-node cluster, 4
non-founder registrations, no explicit `requireSupplyIdentity` call in the
scenario itself): `node 4:
coinsTotal(499999979000)+totalBonded(500000000000)+deposits(12000)+feesInFlight(13000)=1000000004000
!= totalSupply(1000000000000)` — again the exact predicted +4000, confirming
the leak is independent of the sponsored-transaction path exercised by that
scenario's own (green) sub-tests.

Also reproduced at teardown of `TestScenarioChurn` (2-node base cluster grown
by three `Spawn()`s, `WithMaxChurn(1)`, four non-founder registrations total:
the base cluster's own non-founder plus the three spawned nodes): `node 0:
coinsTotal(900000000000)+totalBonded(100000000000)+deposits(4000)+feesInFlight(0)=1000000004000
!= totalSupply(1000000000000)` — the exact predicted +4000 for 4
registrations, independent of churn limiting (registrations that a boundary's
churn cap defers still create their replication-0 object and stamp its
deposit at commit time, before admission to `epochHolders` is even decided).
Both of that scenario's in-scenario subtests (`additions_capped_per_boundary`,
`all_eventually_join`) are green; teardown convergence was also green on this
run (unlike the sibling entry 1, whose race window closes well before this
scenario's next check, since each registration here is separated from the
next by a full epoch boundary).

### 9. Network-wide commit wedge after two validators crash right after an epoch boundary

**Subsystem:** `internal/consensus` commit path — the anchor decision /
indirect resolution machinery around the crash window (`anchorStatus`,
`resolveIndirect`, and whatever vertex-recovery path should backfill a dead
producer's last vertices).

**Status: FIXED** (three layers, this branch: anchorStatus's forward scan passes certification-impossible rounds so resolveIndirect can decide wedged anchors; the impossibility predicate stops counting holders silent across a deep stored span above the frontier as potential supporters, so splits thinned by crashed or long-partitioned validators resolve; and the WAIT-stall recovery now fetches the parents the pending buffer is blocked on, so a vertex cut mid-broadcast no longer starves the visible frontier below quorum through its buffered descendants).

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

**Second trigger, no crash required:** a plain network partition spanning an
epoch boundary reproduces the same wedge intermittently. On a 5-node,
50-round-epoch, equal-stake cluster, isolating one node (4|1) while the
majority crosses a boundary wedged the MAJORITY's own commit cursor shortly
after the boundary (majority `lastCommitted` frozen at 164 while production
raced past 570, `designated` all-zero on every `consensus.round.advanced`
after the freeze), and the wedge PERSISTED after `Heal()` restored full
connectivity: all five nodes sat at `lastCommitted` 125-164 with rounds
569-571 until the scenario timed out. The immediately following rerun of the
identical scenario escaped the wedge entirely (isolate caught up to the
majority's epoch and round, all in-body assertions green), confirming the
timing dependence already noted for the kill trigger. Since no validator
ever leaves the set here, this rules out dead-producer vertex recovery as
the sole mechanism; the common factor across both triggers is unreachable
(not necessarily dead) peers across an epoch-boundary window.

**Third trigger, boundary not required:** repeated partition/heal cycles
wedge the cursor mid-epoch. Under `TestScenarioPartition/flapping_partitions`
(5 nodes, 4|1 cycles under background traffic), the second partition cycle
froze `lastCommitted` at 136-138 on ALL five nodes — 12 rounds short of the
next boundary at 150 — while production raced to 377-386, with every
`consensus.round.advanced` after the freeze carrying `designated` all-zero
(40 of 40 in the teardown dump) and background traffic ceasing to commit.
This narrows the mechanism: an epoch boundary is one way, but not the only
way, to arm the wedge; the recurring factor is anchor rounds whose producer
was unreachable when its round passed, which the decision/recovery machinery
then never resolves, even after connectivity returns.

**Fourth observation, a temporary form under load:**
`TestScenarioStress/double_spend_storm` (6 conflicting hand-built transfers
submitted right after the concurrent-traffic sub-test crosses an epoch
boundary on a 12-node cluster) timed out twice waiting 90 s for commit
verdicts — yet the same run's teardown minutes later found commits flowing
again and 11 of 12 checksums converged, and the sub-test run in isolation
passes in 0.16 s with zero submission errors. So the commit stall in this
trigger RECOVERED after multiple minutes, unlike the permanent wedges
above. Either the wedge has a self-healing variant, or heavy load plus a
boundary produces a distinct multi-minute commit outage with the same
external signature; discriminating the two belongs to the fix
investigation.

**Reproduced by:** `TestScenarioEpochCrash`, red in-scenario at the
post-kill boundary wait in the wedged runs (the dominant outcome), and red
only at teardown convergence (entries 1/2) in runs that escape the wedge.
Also `TestScenarioPartition/across_epoch_boundary`, red in-scenario at the
post-heal catch-up waits whenever the wedge engages (three failure shapes
across three runs: post-heal catch-up, majority boundary wait, and one
clean escape), `TestScenarioPartition/flapping_partitions` (2 of 2 runs,
mid-cycle traffic-progress wait), `TestScenarioPartition/symmetric` (1 run:
post-heal uniform split never got a verdict on one node), and
`TestScenarioStress/double_spend_storm` (2 of 2 full-scenario runs, the
temporary form above; green in isolation).

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

**Status: REPRODUCED** (was latent; see below).

**Evidence:** `TestScenarioColdRestart` funds the founder itself and has it
`Bond` 7,000,000 on top of its genesis self-stake (100,000,000,000 at this
cluster's default mint), stops the whole 5-node cluster gracefully, brings
the bootstrap node back first in bootstrap mode over its own data directory,
then resyncs the other four against it. Node 0's own fingerprint, taken
immediately before extinction and again once the cluster is back and past
its pre-extinction committed round, shows:

```
totalBonded: pre=500,007,000,000  post=100,000,000,000  (delta -400,007,000,000)
coinsTotal:  pre=499,992,977,000  post=499,992,977,000   (delta 0)
```

`coinsTotal` is exactly conserved (the coin debited to fund the bond never
comes back — "coin debits persist," confirmed) while `totalBonded` does not
land at "genesis minus the founder's 7,000,000 bond" as the entry's original
text would suggest — it collapses all the way down to the founder's bare
genesis self-stake, 100,000,000,000, as if no other validator had ever
bonded anything. This confirms the entry's own "suspected ... missing
validator-set persistence path" co-factor is real and NOT founder-specific:
`buildValidatorSet` (`cmd/node/init.go`) starts every process run — restart
included, bootstrap or not — from an in-memory validator set containing
nothing but the local identity (`consensus.NewValidatorSet(nil)` for a
non-bootstrap restart, a single-entry set for a bootstrap one), and nothing
outside `SeedGenesisValidator` (founder-only, genesis values only) restores
anyone else into it. The other four validators' bonds are recovered only to
the extent their own restart/re-registration and node 0's sync snapshot
happen to reconstruct them, which this run's teardown shows landing
inconsistently: the five nodes reported five different validator COUNTS
(1/2/3/3/3) shortly after the cold restart, and the teardown supply-identity
check failed with a ~4*10^11 gap (`node 0:
coinsTotal(499992973000)+totalBonded(100000000000)+deposits(15000)+feesInFlight(16000)
=599993004000 != totalSupply(1000000000000)`) — far larger than entry 8's
usual few-thousand leak, and consistent with this same validator-set-loss
mechanism rather than a new defect.

**Reproduced by:** `TestScenarioColdRestart`, red in the body
(`founder_stake_preserved`, the intended live reproduction, not a test
defect) and at teardown (convergence, per entry 1's mechanism repeating as
four non-founders re-register against a cold-started bootstrap; supply
identity, dominated by this entry's cluster-wide validator-stake loss rather
than entry 8's smaller leak). No prior scenario ever restarted the bootstrap
node, so this path was previously untested, as noted below.

Latent before this: no prior scenario mutated the founder's stake or reward
coin before restarting the bootstrap node, so nothing in the corpus
exercised this path.

### 11. Inter-node GetObject omits LocalOnly, so a globally-absent object cascades to a client timeout instead of a prompt not-found

**Subsystem:** `cmd/node/clienthandlers.go` `fetchObjectFromHolder` /
`requestObjectFrom`.

**Status: FIXED** (inter-node probes now request LocalOnly, a global miss is
N direct not-founds; this branch).

`fetchObjectFromHolder`'s own doc comment states it "asks each holder for the
object locally (the remote handler returns not-found rather than re-routing),
preventing cascades," but `requestObjectFrom` builds the inter-node request as
`network.GetObjectRequest{ObjectID: id}`, never setting `LocalOnly: true`. A
holder that also lacks the object therefore does not answer not-found
directly: it re-enters `handleGetObject` on the non-local path and itself
calls `fetchObjectFromHolder`, probing every other validator (including the
original requester) the same way. For an object no node holds (for example a
deleted singleton), this fans out into a multi-hop cascade across the mesh
instead of the single round-trip per holder the comment promises, and a
client's own `GetObject` (routed, non-local) blows past its 8-second QUIC
request timeout waiting for the cascade to unwind, surfacing as "deadline
exceeded" rather than a prompt not-found.

**Evidence:** `TestScenarioStake/undelegate_returns_principal` deletes a
delegation position (`handleUndelegate` calls `coinStore.DeleteObject`, a
singleton every one of the 5 validators held and now none do) and then calls
`cli.GetObject` on it to confirm it is gone. Reproduced on two consecutive
runs: the call fails with `get object:\nget object:\ndeadline exceeded` after
8.06s and 9.92s respectively, both comfortably past `quicRequestTimeout`
(8s, `pkg/client/quic.go`) and consistent with a multi-hop cascade rather than
one bounded remote probe.

**Reproduced by:** `TestScenarioStake` (`undelegate_returns_principal`), red
in the scenario body, not just at teardown. No prior scenario had exercised
`GetObject` (non-local) against an object absent from every node, so this
path was previously untested.

### 12. A failed pod execution debits the fee's storage component from the coin but credits it nowhere: a deflationary supply leak

**Subsystem:** `internal/consensus/commit.go` `deductFees` / `calculateTxFeeSplit`
(the storage/consumed fee split) together with `internal/state/state.go`
`applyCreatedObjects` (the only place that credits a storage deposit).

A transaction that declares `created_objects_replication` (any
created-object-bearing call: `split`, `create_object`) has its full fee —
consumed plus storage — computed from that DECLARED header field and debited
from the gas coin unconditionally, before execution, regardless of whether
execution actually succeeds. When the debit is fully covered, `deductFees`
pools only the "consumed" portion into the epoch reward pool
(`SplitFee(consumed, ...)`); the storage portion is deliberately excluded
because it is meant to become a locked deposit against the object the pod
creates, credited via `applyCreatedObjects`'s `events.DepositLocked` /
`onObjectCreated` callback. But `state.Execute` only reaches
`processOutput`/`applyCreatedObjects` on a SUCCESSFUL pod execution — an
error from `s.pods.Execute` returns before either runs. So when execution
fails (for example `split`'s `ERR_INSUFFICIENT_BALANCE`) after fees were
already deducted, the storage portion has already left the gas coin but is
credited NEITHER to the epoch fee pool NOR to the deposit tracker: it simply
vanishes from every term of the protocol supply identity
`coins_total + total_bonded + deposits + fees_in_flight == total_supply`,
breaking it in the DEFLATIONARY direction — the mirror image of entry 8's
inflationary registration-deposit leak.

**Evidence:** on a 5-node cluster, `TestScenarioFees`'s
`split_exceeds_balance_is_execution_error` funds a coin with 100,000, splits
the full 100,000 (guaranteeing `ERR_INSUFFICIENT_BALANCE` once the fee has
already reduced the balance), and takes a `Fingerprint` immediately before
and after the commit: `coinsTotal` drops by exactly 2000 (the split's full
consumed+storage fee, confirmed fully covered via `fees.deducted`), while
`deposits + fees_in_flight` together rise by only 1000 (the consumed
portion) — a 1000-unit shortfall, exactly the storage component
(`StorageDeposit(0, 5, 1000) = 1000`) that never lands anywhere. Reproduced
identically on two consecutive runs (`coin lost 2000, deposits+fees_in_flight
only gained 1000` both times). Consistent with entry 8's independently
derived per-registration mechanism: this same cluster's next subtest
(`underfunded_gas_coin_pools_partial`, red on entry 8 alone before this
scenario had an execution-failing step) now reports a supply-identity delta
of +3000 instead of the +4000 documented in entry 8's own evidence for this
file — exactly entry 8's +4000 (four non-founder registrations) minus this
entry's -1000 (one failed created-object transaction), the two leaks
superimposed on the same ledger.

**Reproduced by:** `TestScenarioFees` (`split_exceeds_balance_is_execution_error`,
red in the scenario body via a direct before/after `Fingerprint` delta check,
not just at teardown). No prior scenario had exercised the `execution_error`
path for a transaction that declares created objects, so this leak was
previously unreachable by the corpus.

### 13. An attested transfer that never lands wedges its object: every recollection is refused quorum-impossible across epochs

**Subsystem:** `pkg/daemon` attestation collection / quorum assembly, and
the routed object-read path serving the client (`cmd/node/clienthandlers.go`
neighborhood, same as entry 11), against `internal/consensus` ATX commit.

**Status: FIXED in two layers** (read layer: the routed read served the serving
node's own stale copy of a replicated object it no longer held, instead of
probing the holders that carried the post-transfer version; this branch.
Aggregation layer: `handleGetValidators` served the daemon the LIVE validator
set, so the daemon assembled attestation quorums over holders that differed from
the epoch-frozen snapshot the chain verifies and executes against, and the proof
was rejected at commit; `handleGetValidators` now serves the epoch-frozen holder
snapshot, this branch). A residual `internal/consensus` divergence remains (node
4's frozen epoch snapshot forks from the other nodes'), documented below and out
of the aggregation layer's scope.

A first `TransferObject` through the daemon is accepted at submission, but
the routed `GetObject` poll never observes the ownership change (20 s
bound). Every subsequent recollection attempt — each one made in a FRESH
epoch, so attestation staleness cannot explain it — is refused by the
daemon with the typed error `attestation quorum impossible`, persistently,
for at least three consecutive epochs. From the submitting client's
perspective the object is wedged: the first transfer neither lands nor
frees the collection path.

The mechanism proven was the read-path one, adjacent to entry 11. The first
ATX did apply on the holders (they emitted `state.object.updated version:1`),
so the alternative — an ATX dropped at commit leaving holder attestation state
the daemon can never re-assemble a quorum from — is ruled out. The wedge was
that the routed read through the serving node returned that node's own stale
copy: storage and execution are sharded by rendezvous holdership, and the
holder set is re-frozen each epoch from the evolving validator set, so a node
that held the object at create time and lost holdership by transfer time keeps
a lazily-retained stale copy. `handleGetObject` trusted any local copy before
probing holders, so the client kept seeing the pre-transfer owner and retried a
transfer of an object it no longer owned — after which `attestation quorum
impossible` was the daemon correctly refusing to re-collect against the moved
object. In isolation the create and transfer land inside one epoch with the
serving node's holdership stable, so the stale copy never arises.

**Evidence:** `TestScenarioAggregation/attested_transfer`, deterministic in
three consecutive full-scenario runs (84 s = 4 stale attempts of 20 s, then
red), including one run where each retry explicitly waited out an epoch
boundary before recollecting: attempt 1 `submitted tx 40c79adb...`, then
attempts 2-4 all `collection error: ... attestation quorum impossible`.
The same run's sibling sub-tests (`version_race`, `epoch_boundary_grace`,
`cold_holder`) drive the identical create-and-transfer machinery and pass,
and the sub-test run in isolation passes in 0.69 s with the holders
emitting `state.object.updated` immediately — the failure is specific to
some property of the full-scenario run (object identity / holder-set
placement relative to the serving node is the leading suspect: the routed
reads and the failing collections go through `Client(0)`).

**Reproduced by:** `TestScenarioAggregation` (`attested_transfer`), red in
the scenario body; green in isolation (`-run
'TestScenarioAggregation/attested_transfer'`), which is itself part of the
signature.

**Second layer (aggregation).** With the read fix in place the wedge still
reproduced with a new shape: the routed read (now truthful) kept showing the OLD
owner, recollections were refused quorum-impossible, and at teardown node 4's
fingerprint diverged alone with no fault injected. Driving many
create-then-transfer cycles on one cluster and dumping each node's commit verdict
proved the mechanism: the SAME ATX (identical bytes: aggregate signature,
objects, bitmap, attestation epoch) was accepted at commit by exactly one node
and rejected by all the others with `proof 0: aggregated BLS signature invalid`,
so that one node applied the transfer to `version:1` while the rest stayed at the
old owner. Even the holders the daemon collected the signatures FROM rejected the
proof carrying their own signatures. Aggregated BLS verification is a pure
function of (signature, message, public keys); the signature and message are
identical on every node, so a split verdict means the nodes resolved the signer
bitmap to different public keys, i.e. they disagreed on the object's holder set.

The trigger is in the aggregation layer and in scope: the daemon computed object
holders and assembled the quorum from the response of `handleGetValidators`,
which served `ValidatorsInfo()` — the LIVE validator set — while the chain
shards storage and execution, serves routed reads, and verifies ATX proofs
against the epoch-FROZEN holder snapshot (`EpochHolders` / `HoldersForEpoch`).
When the live set and the frozen snapshot diverge (a boundary frozen mid
bootstrap, validator churn), the daemon builds a proof over the wrong holders and
the chain rejects it, so the transfer never lands and every recollection is
quorum-impossible. The fix serves the epoch-frozen holder snapshot from
`handleGetValidators`, so the daemon's holder set is identical to the verifier's.
A 40-iteration create-and-transfer loop went from repeated wedges to 0 wedges
with the fix.

The residual is a distinct `internal/consensus` defect, not the aggregation
layer's: node 4's frozen epoch-1 holder snapshot forks from the other four nodes'
(a split verdict is only possible if the frozen snapshots themselves differ
across nodes). After the aggregation fix the transfer lands on the majority
holders, but node 4 — which stored the object under its own divergent holder view
and then rejected the transfer — keeps a stale copy and diverges in fingerprint.
That divergence is the same family as entry 15 (node 4 converges in round but not
in fingerprint) and belongs to the consensus snapshot/validator-set path, out of
this fix's surface.

**Second-layer reproduced by:** `internal/aggregation`
`TestATXVerifierRejectsProofBuiltOverDifferentHolderSet` — a quorum proof
assembled over one holder set is rejected by a verifier reconstructing holders
from a different (frozen) snapshot, and accepted when reassembled over the
verifier's snapshot, capturing the split verdict at the unit seam.

### 14. Deregistration principal is released from total_bonded but credited to no coin

**Subsystem:** the epoch-boundary deregistration processing
(`internal/consensus/epoch.go` neighborhood, the deferred deregistration
path that releases a departing validator's bond).

When a validator's deregistration is applied at an epoch boundary, its bond
leaves `total_bonded` but the principal is never credited to any coin: the
supply identity loses the full bond amount. This was entry 2's "blast
radius" paragraph; the RewardCoin fix (entry 2, fixed on this branch) made
the loss NETWORK-UNIFORM instead of bootstrap-vs-synced divergent, proving
it is a distinct defect in the credit path itself, not a snapshot-rebuild
artifact.

**Evidence:** `TestScenarioEpochs` teardown after the batch-1 fixes: every
node now reports the SAME failing identity, node 0 included —
`coinsTotal(1000169008)+totalBonded(799222082216)+deposits(20000)+feesInFlight(0)=800222271224
!= totalSupply(1000000000000)` — short ~199.78 B, almost exactly the two
deregistered validators' ~100 B bonds, with teardown convergence green
(entries 1/2 fixed). Before those fixes only synced nodes showed this sum
while the bootstrap node diverged.

**Reproduced by:** `TestScenarioEpochs` (teardown supply identity;
`deregister_deferred_to_boundary` is green in-body, so the deferral works —
only the principal credit is missing).

### 15. A healed (formerly partitioned) node converges in round but not in fingerprint

**Subsystem:** the post-heal catch-up path — how an isolated node that could
not commit during a partition replays the majority's committed rounds after
connectivity returns (`internal/sync` / `internal/consensus` commit replay),
suspected against the live-commit path it must byte-match.

After every partition/heal cycle, the formerly isolated node reaches the
SAME committed round as the majority but with a DIFFERENT state fingerprint,
and never reconverges within the teardown bound. The majority nodes are
byte-identical among themselves (the batch-1 fixes hold); only the healed
node splits. Zero rollback holds throughout.

**Evidence:** two independent `TestScenarioPartition` runs on this branch,
one WITH the wedge fix (bc6db89) and one WITHOUT it (d27f5f2), show the
identical pattern, proving the defect pre-exists the wedge fix and was
previously masked by the five-distinct-checksums noise of entries 1/2:
minority at d27f5f2: nodes 0-3 all `24db9459` at round 320, isolated node 4
`8e3e5645` at the same round 320; symmetric and heal_under_traffic show the
same single-node split (`91191c80` x4 vs `7633966a`, `5225a427` x4 vs
`7fb35b04`).

**Reproduced by:** `TestScenarioPartition` — every sub-test's teardown
convergence check (minority, symmetric, heal_under_traffic,
across_epoch_boundary, flapping_partitions), deterministic across both runs.
