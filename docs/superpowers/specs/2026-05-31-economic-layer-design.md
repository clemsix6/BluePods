# Economic Layer Design

**Goal:** Make the BluePods economy close the loop. Today fees are computed and deducted, but rewards are never paid, the burn destroys nothing, there is no issuance, no supply tracking, and no staking. This design fills those gaps and adds the missing primitives (adaptive issuance, real supply accounting, staking with delegation, stake-and-liveness rewards, sponsored transactions, and a future-proofed timestamp field) around the existing epoch, consensus, and off-chain-attestation machinery. It has been hardened through three adversarial review iterations; the decisions below reflect the resolutions.

This chantier is the dedicated tokenomics study the off-chain aggregation design deferred (it left the burn question and "a relayer or sponsor tier" open for exactly this work).

**Token identity:** BluePods is a decentralized cloud. The native token is utility-first and stability-oriented: a means to pay for compute and storage and to secure the network, not a speculative asset. The more stable its value, the better the product.

**Guiding principle:** Design the mechanism, parameterize the numbers. Two invariants protect against gaming. (1) Nothing that controls money depends on a manipulable clock: issuance is denominated in epoch events, not time. (2) No reward is a farmable multiplier: reward weight is `effective_stake x liveness`, and neither term scales with manufacturable activity (stake requires bonded capital; liveness is capped at one vertex per round). Verifying that holders actually store and serve their shard is an enforcement problem that has no sound at-launch solution from the artifacts the chain sees, so it is deferred to a dedicated branch (alongside slashing), and the spec does not claim it is solved.

This is one design of record, implemented as one chantier on the `economic-layer` branch, decomposed into ordered plans.

---

## 1. Stake-weighted consensus

Voting weight in the DAG is proportional to a validator's effective stake. This is the only Sybil defense: splitting stake across many keys yields the same total weight.

- **`effective_stake` = self-stake + delegated stake.** It is the weight for the consensus quorum and for the reward (section 6).
- **Per-validator cap on VOTING POWER only.** A validator's voting weight is capped at a fraction of the total, so the global order stays decentralized even when delegation concentrates stake on the most reliable validators. Reward weight is NOT capped: reward is proportional to `effective_stake` (standard PoS, bigger stake earns proportionally more). The cap is a parameter calibrated to set size: loose while the set is small (a 10 percent cap with 3 validators makes a two-thirds quorum unreachable), tightened toward ~10 percent as the set grows. Honest scope: the cap bounds a single advertised identity; a determined entity can split across keys to reconstitute weight, so the cap bounds per-identity concentration (and costs the entity N independent nodes), not real-entity concentration.
- **Quorum is exact integer arithmetic** (`3 x cappedSum >= 2 x total` over capped effective stakes), never floating point, read from the epoch holder snapshot selected by the deterministic commit epoch (`HoldersForEpoch` / `commitEpochForRound`). The three counting sites (`ValidatorSet.QuorumSize`, `isRoundCommitted`'s distinct-producer count, `validateParentsQuorum`) plus the relaxed bootstrap literal move to capped-stake sums.
- **Per-object attestation stays equal-weight** (one holder, one vote, Rendezvous-assigned). This deliberately insulates the storage/holder layer from stake concentration.
- **Dual security model.** The global order is safe under a two-thirds-of-stake honest majority; each object's attestation is safe under a two-thirds-of-its-holders (count) honest majority. They compose: stake-weighting secures ordering, count-weighting secures per-object attestation.

**Interim deterrent without slashing.** Stake-weighting is kept even though slashing is deferred, because equal weight is Sybil-able and strictly worse. An attacker buying two-thirds of the stake pays an enormous capital cost and destroys its own holdings if it attacks; the cap bounds concentration; reward-withholding and removal handle misbehavior. This is the Sui/Aptos/Solana posture. Slashing adds economic punishment later (section 10).

**Rejected:** stake-age weighting; equal-weight quorum (Sybil-able); capping reward weight (collides with proportional staking).

---

## 2. Bonding and delegation

Bonding locks tokens as stake. Stake gives voting weight, reward weight, and (later) slashable collateral.

- **Self-stake** is a field on `ValidatorInfo`, not a flag on coins. Bonding debits a coin the validator owns and credits the stake. The field is carried in the copy-returning accessors (`Get` / `All`), set via a bonding mutation, rebuilt in the epoch snapshot helpers, and persisted by extending the validator encoder (`encodeValidators` / `decodeValidators` and the checksum); `Snapshot.validators` is a flat byte vector, so stake rides in it with no FlatBuffers schema change.
- **Minimum stake** to register: a parameter (slashable skin in the game, anti-spam). It interacts with validator-set size and therefore per-object holder-set size, so it is calibrated with both in mind.
- **Unbonding period:** withdrawn stake stays locked and slashable for a delay (tied to fraud-detection latency, finalized with slashing).

**Delegation (in scope, a precondition for the thermostat).** Without it, only validators bond, so on an adopted cloud (most supply is user working capital) the staking ratio is structurally capped and the thermostat saturates. Delegation lets the broad base stake idle balances.

- **Representation.** Each validator carries a maintained `delegated_total` aggregate (used for `effective_stake`, the cap, and `total_bonded`; persisted with self-stake). Each delegation is a stake-position object owned by the delegator (validator, amount).
- **Fixed commission (a governed parameter), not per-validator.** This avoids a commission field, its rate-limited change mechanics, and the rug-pull risk (a validator raising commission to 100 percent). Delegators choose validators on reliability. A per-validator commission market is a later refinement.
- **Reward sharing is a simple epoch-boundary proportional split (no accumulator).** Delegations take effect at the next epoch boundary (mirroring the existing validator-set churn-deferral). At the boundary, where the validator's reward is already computed (`distributeEpochRewards`), the fixed commission goes to the validator and the rest is split among its delegations pro-rata to their amounts, crediting each. At launch scale (few delegations) this O(delegations) iteration is the same order as the per-object/per-coin work the commit path already does. The Cosmos-F1 reward-per-share accumulator (O(1) but more complex) is deferred until delegator count makes per-epoch iteration costly.
- **Delegator unbonding:** undelegate destroys the position; after the delay, principal plus accrued reward is returned.
- **Jailing.** When a validator fails (is jailed/removed), its effective stake stops counting toward the quorum immediately (zeroed in the snapshot for that epoch) and its delegators stop accruing reward without needing to act; delegators can redelegate. A jailed validator does not carry voting weight it no longer earns. (Jail duration and auto-unjail are a small parameter; slashing of principal is deferred, section 10.)
- **Slashing hook** (deferred): when slashing lands, delegators share the slash pro-rata.

**Liquid staking is deferred** (widens the willing-to-lock base later). Basic delegation ships now.

---

## 3. Monetary policy: the thermostat (time-free, per-epoch)

Issuance is an adaptive control loop evaluated at each epoch boundary event, denominated in epoch events (not time), so it depends on no clock.

- **Target: a band around the staking ratio** (`total_bonded / total_supply`), ~25 to 35 percent of total supply, with a dead-band (inside the band the rate is held, preventing oscillation). The band is conservative and reachable with delegation; it errs low, because targeting too low is benign (rests at low inflation, easily raised) while targeting too high is catastrophic (saturates at the ceiling, dilutes forever). ~30 percent is the starting point; the denominator is `total_supply`.
- **Mechanism, per epoch boundary:** read the ratio on PRE-mint supply (so issuance cannot lower its own denominator); adjust the per-epoch rate `r` toward the band, bounded by `[floor, ceiling]`, capped step; mint `r x total_supply` into the reward pool. Issuance is minted even in a zero-fee epoch (it is the bootstrap incentive).
- **Why time-free is correct.** The loop targets the ratio (a dimensionless outcome), not a precise inflation number, so it self-compensates for the network's pace.
- **Pace manipulation is bounded and subsumed.** Closing epochs faster requires accelerating round production, which requires a two-thirds-stake quorum (the catastrophic threshold at which the attacker already controls consensus). Bounded by the ceiling, self-destructive. The oracle later re-denominates in real time, restoring a clean per-year bound.
- **Why not the clock or a VDF.** A median-of-timestamps clock invites a collective over-state-time bias the median cannot stop from a majority; a VDF measures hardware-dependent compute and does not fit a leaderless DAG.
- **Auto-restake.** A fraction of each reward is auto-restaked by default (opt-out), reflected in the NEXT epoch's pre-mint ratio, to relieve the treadmill where spent rewards lower the ratio. The step cap is calibrated with this feedback in mind.
- **Why the thermostat ships now while the dynamic fee does not.** Issuance is needed from genesis to pay and attract validators; the dynamic fee only matters under congestion a pre-traffic network cannot exhibit.

**Starting parameters.** Enforced per-epoch, expressed as the annual figures they approximate against an assumed epoch pace: floor ~1 percent (low perpetual security floor, not zero); ceiling ~20 percent (rarely reached); genesis ~8 to 10 percent; small per-epoch step cap. Annual readings are approximate until the oracle supplies true time. All governed.

---

## 4. No scarcity burn

A burn creates scarcity (appreciation pressure), against the stability goal. The token is left mildly inflationary.

- **The scarcity fee burn is removed.** One hundred percent of consumed fees go to validators (the off-chain design's 70/30 is superseded). The anti-gaming role the burn once played is now covered by the reward not being a farmable multiplier (section 6), not by a burn.
- **The 5 percent deletion burn on the storage deposit is kept** (anti-spam on create/delete churn).
- **Real price stability** is delivered later by the cycles/peg model, unblocked by the oracle.

---

## 5. Fees

- **5.1 Structure (implemented):** proportional to the fraction of nodes touched; a singleton caps and never grows with network size. Kept.
- **5.2 Distribution:** consumed fees (compute, transit, domain) go to the global epoch reward pool (rendezvous uniformity makes a storage fund unnecessary). The storage component is a refundable deposit (locked in the object's `fees`, 95 percent refunded on deletion, 5 percent burned), held, never pooled.
- **5.3 Level:** `gas_price` is a fixed governed constant for launch; a dynamic congestion fee is deferred (closes no gap here, not the acute-DDoS tool, uncalibratable without live traffic; simulations decide later).
- **5.4 DDoS in layers:** ingress hardening (microseconds, exists) + `min_gas` floor (instant, exists) absorb floods; the dynamic fee (minutes) is deferred and not needed at launch.

---

## 6. Reward distribution

This finishes the `TODO: credit to validator's reward_coin` in `epoch.go`.

- **The pool** for an epoch is `epochFees + thermostat issuance` (consumed fees; storage deposits held, not pooled).
- **Weight = `effective_stake x liveness participation`.** Liveness is `epochRoundsProduced / epochTotalRounds`, not farmable beyond uptime (one vertex per round; two is equivocation). Reward is NOT proportional to attestation count, which would be farmable by self-dealing (transacting against one's own held objects); making attestation count irrelevant to reward kills self-dealing.
- **Serving/storage enforcement is deferred (honest).** An earlier design gated reward on appearances in committed ATX signer bitmaps. That signal is unsound: the bitmap is assembled by the submitter (the daemon), who can omit an honest holder (failing it unfairly) and a relay holder that stores nothing can still appear (passing it). A sound serving check requires a protocol-issued challenge, which is the deferred storage-challenge problem (section 10). So the launch reward is `effective_stake x liveness`, and at launch the network does NOT economically punish a holder that under-serves or under-stores. This is the same posture as deferred slashing.
- **What protects the sharded layer at launch (instead of a gate):** the attestation quorum tolerates a minority of non-serving holders (two-thirds is needed, so up to a third can fail), replication means one holder dropping does not lose an object, and bonded capital is at stake (slashable once slashing lands). Beyond the quorum margin, availability degrades; full enforcement (storage challenges + slashing) is the dedicated future branch. The spec states this plainly rather than claiming freeloading is closed.
- **Crediting** is to a liquid `reward_coin` the validator designates at registration, via the consensus `creditCoin` primitive on `d.coinStore`. The validator's reward is split with its delegators by the epoch-boundary proportional split (fixed commission to the validator); a fraction is auto-restaked by default.
- **Cold objects are not under-rewarded** (reward depends on stake and liveness, not appearance count, so a cold-object holder earns the same as a hot-object holder at equal stake and uptime). Their durability at launch rests on replication alone (no economic enforcement until storage challenges).

---

## 7. Supply and bonded tracking

- **`total_supply`** is a maintained protocol counter (a dedicated key): set at genesis, increased by issuance, decreased by the deletion burn and future slashing. There is no user-callable mint (it would create unbacked supply); the only token creation is genesis seeding and protocol issuance. Maintained in the commit path, persisted, included in snapshots and the snapshot checksum. Feeds the thermostat denominator. Net-new concept. Invariant to property-test: `sum(coin balances) + total_bonded + sum(locked storage deposits) + fees_in_flight == total_supply`, where `fees_in_flight` is the epoch's accumulated consumed fees not yet credited to validators. It is zero immediately after an epoch boundary, so the invariant is asserted there as exact equality. The storage component of a fee is locked in the object's `fees` (debited from the gas coin, never pooled), so it stays accounted as a locked deposit until 95 percent is refunded and 5 percent burned on deletion.
- **`total_bonded`** is derived: sum of (`self-stake` + `delegated_total`) over the active set at the epoch boundary, O(validators).

---

## 8. Timestamp field (clock pipeline deferred)

A deterministic on-chain clock will be a useful pod primitive (subscriptions, time-locks, expirations), but it has no consumer at launch: issuance is per-epoch (section 3), and sponsored-tx `valid_until` is denominated in epochs (section 9), both deliberately clock-free.

- **Land only the field now.** Add a `timestamp` field to `vertex.fbs`, included in the signed/hashed body with a validity bound. This is the irreversible, consensus-breaking part; landing it now (no live mainnet) avoids a future consensus break. Producers populate it from their local clock.
- **Defer the pipeline.** The median-of-quorum derivation, monotonic coercion, and pod-context exposure are deferred to the branch that ships the first time-using pod. There is no point computing and exposing a clock nothing consumes.
- It will control no money (issuance is per-epoch), so the over-state-time bias never applies; the oracle later anchors it to a non-falsifiable wall-clock.

---

## 9. Sponsored transactions

A zero-balance new user cannot make a first transaction (every user tx needs a funded gas coin owned by the sender, section 11). The fix, and the resolution of the off-chain design's open relayer/sponsor tier, is native sponsored transactions, done the Sui way (a protocol-level fee payer), not the Base way (layered ERC-4337).

- **Schema.** `transaction.fbs` gains `fee_payer`, `sponsor_signature`, and `valid_until` (an epoch). These are optional, encoded as absent-when-empty (a sentinel/absent encoding) so a non-sponsored tx serializes byte-identically to today and its existing single-sender hash and signature still verify.
- **Mutual binding.** Both the sender and the sponsor sign the SAME canonical body hash, covering everything except the two signature fields. Neither field can be swapped after the other signs. The sponsor signature is verified in the commit path (`executeTx`), deterministically on every node, gated on `fee_payer`; the gas-coin ownership check becomes `owner == fee_payer`. The sender signature and the transaction hash are (re)verified at the same commit-path site, not only at client ingress: a transaction can reach commit via a gossiped vertex without passing local ingress validation, so authenticity must be enforced where every node agrees, closing the gap where inner transactions embedded in a (producer-signed) vertex were trusted unverified.
- **Replay** is prevented by the existing commit-once guard (the tx hash now covers the sponsor binding). `valid_until` (in epochs), checked against the commit epoch, kills stale sponsorships.
- **Sponsor exposure** is managed off-chain (the sponsor signs only what it will pay; the chain cannot overdraw its coin) and the sponsor is the natural submitter. Honest caveat: a sponsored tx that fails on a version conflict still charges the sponsor's gas, and a malicious holder of the signed artifact can submit it at an inconvenient time within the window; the sponsor prices this and bounds it with `valid_until`. On a sponsored create then delete, the storage refund goes to the owner-deleter, not the sponsor (accepted).
- **Coordination is off-chain;** the chain sees only the final doubly-signed tx. For a sponsored tx that also needs attestation, the submitter collects it last to avoid widening the version-race window.

---

## 10. Deferred enforcement (slashing, storage challenges)

In line with newer L1s, this chantier ships without principal slashing and without storage/serving enforcement.

- **Now:** reward-withholding (via jailing) and removal. Jailing zeroes quorum weight and stops reward immediately (section 2). No principal slash.
- **Stake is slashing-ready** (`Stake` reducible, `total_supply` decrementable); slashed stake is burned.
- **The dispute / fraud-proof / relay-resistant storage-challenge system** is a dedicated future branch. BluePods shards execution, so a Byzantine or freeloading holder has local shard authority; the full deterrent (a protocol-issued, relay-resistant challenge plus slashing) requires this system. In the interim the sharded layer has capability-based security (honest majority per object, the quorum tolerating a minority, replication) plus bonded capital, not economic punishment. This is stated as an accepted, deferred gap.

---

## 11. Genesis and fee integrity

- **Genesis is initial state, not transactions.** The initial coin allocation, founding validator set, and their initial bonded stake are the ledger's starting state, set at chain creation (a real bootstrap rewrite, not a relabel).
- **Every user transaction requires a funded gas coin** (or a sponsor) or it is rejected. The missing-gas-coin exemption is removed.
- **The faucet** becomes a `split` from a genesis-allocated reserve coin (a transfer only reassigns ownership; moving an amount to a new account is a split). Production has no faucet reserve.
- **The user-callable `mint` is removed.** It would create unbacked supply; the only token creation is genesis seeding and protocol issuance.
- **The founding validator's self-stake is genesis state**, locked from the genesis coin so its bonded weight has real backing (the stake-weighted quorum needs non-zero genesis stake to reach quorum).
- **Protocol actions** (issuance, reward crediting, fee deduction, future slashing) are consensus-driven state mutations, not transactions, so they need no gas coin.

---

## Parameters (starting values, calibration placeholders)

| Parameter | Starting value | Role |
|---|---|---|
| Staking-ratio target band | ~25 to 35% of total_supply (dead-band) | thermostat set point (err low) |
| Inflation floor / ceiling | ~1% / ~20% annual (per-epoch, approximate until oracle) | thermostat bounds |
| Genesis inflation | ~8 to 10% annual | bootstrap lever |
| Inflation per-epoch step cap | small | anti-lurch limit |
| Auto-restake fraction | a fraction, opt-out | relieves the treadmill |
| Scarcity fee burn | 0 | removed for stability |
| Deletion burn | 5% | anti-spam |
| gas_price | fixed, governed | dynamic deferred |
| Delegation commission | fixed (e.g. ~10%), governed | validator compensation, no rug-pull |
| Per-validator voting cap | loose then ~10%, calibrated to set size | anti-concentration (voting only) |
| Minimum stake | placeholder | registration bar |
| Unbonding / jail duration | short, parameterized | tied to future slashing |

---

## Out of scope (deferred, with triggers)

- **Storage/serving enforcement** (protocol-issued, relay-resistant challenges) and **slashing**. Trigger: a dedicated branch; required because execution is sharded.
- **The Cosmos-F1 reward accumulator.** Trigger: delegator count makes per-epoch proportional iteration costly. Epoch-boundary proportional split ships now.
- **Liquid staking.** Trigger: demand for stake liquidity.
- **Per-validator commission market.** Trigger: demand for differentiation (needs anti-rug-pull limits).
- **Dynamic fee.** Trigger: live traffic, decided by simulation.
- **The clock pipeline** (median derivation, monotonic coercion, pod-context exposure). Trigger: the first time-using pod. Only the `Vertex.timestamp` field lands now.
- **Cycles / USD-peg model and the data oracle.** Trigger: the planned oracle. Delivers real price stability, reverse gas, and real-time issuance denomination.

---

## Rejected alternatives

| Decision | Rejected | Why |
|---|---|---|
| Token identity | hybrid utility + value | a cloud wants a stable medium of payment |
| Issuance | fixed calendar / fee-indexed / clock or VDF denomination | guessing the trajectory / fragile feedback / clock-on-money bias |
| Reward weight | proportional to attestation count | farmable by self-dealing |
| Reward serving check | a gate read from committed signer bitmaps | unsound (submitter-assembled: fails honest holders, passes relays); needs a protocol-issued challenge (deferred) |
| Reward weight | capped | collides with proportional staking; cap voting only |
| Consensus weight | equal weight / stake-age | Sybil-able / attackable |
| Anti-concentration | defer the cap | delegation creates the concentration regime now |
| Commission | per-validator | rug-pull risk and change-mechanics |
| Delegation rewards | Cosmos-F1 accumulator now | over-built for launch scale; proportional split suffices |
| Pod clock | full pipeline now | no launch consumer; land only the field |
| Sponsored tx | Base-style ERC-4337 | a workaround for a constraint BluePods does not have |
| Fee-less transactions | genesis-authority exemption | eliminate the path at the root (genesis as state) |
| Stake representation | an `is_staked` flag on coins | record on `ValidatorInfo` |
| Storage payment | per-object storage fund | rendezvous uniformity makes the global pool equivalent |

---

## Doc impact

To update in place once implemented:

- **WHITEPAPER section 9 (Fees):** 70/30 to 100/0; drop the burn-as-anti-gaming argument; reward distribution prose (now `effective_stake x liveness`, not consensus participation, with serving enforcement deferred).
- **WHITEPAPER section 10 (Reward Distribution formula):** `stake x (rounds/total)` becomes `effective_stake x liveness`; add a delegation subsection (positions, fixed commission, epoch-boundary proportional split, unbonding, jailing) and a minimum-stake / voting-cap note and its interaction with section 4's holder-set sizing. Delegation is net-new, not an edit.
- **WHITEPAPER sections 10 / 5 (Validators, Consensus):** stake-weighted capped quorum and the dual security model replace the equal-weight framing and the "stake is equal (1)" note; genesis-as-state changes the genesis-epoch story.
- **WHITEPAPER section 1 (security headline):** "honest majority per object, not globally" must be reframed: honest majority per object for attestation, honest two-thirds-of-stake for ordering (count-based global security no longer holds after stake-weighting).
- **WHITEPAPER section 7 / 9 / 12 (Transaction Lifecycle, Gas Coin, Daemon trust):** sponsored transactions (`fee_payer`, sponsor signature, gas coin owned by the fee payer).
- **WHITEPAPER (new):** a `total_supply` accounting subsection.
- **VISION.md:** the utility-first, mildly-inflationary, no-burn stability stance is a positioning statement that belongs in VISION; add it.
- **`types/vertex.fbs`:** `FeeSummary.total_burned` becomes vestigial (zero, soft-deprecated); a new signed `Vertex.timestamp` field is added.
- **`types/transaction.fbs`:** `fee_payer`, `sponsor_signature`, `valid_until` added (absent-when-empty).
- **The off-chain aggregation spec** is superseded on the 70/30 split and its open relayer/sponsor item is resolved here.

---

## Relationship to the off-chain aggregation design

This chantier is the deferred tokenomics study; it resolves that design's open relayer/sponsor tier (section 9) and burn question (section 4); it reuses its machinery (the global-pool reasoning in 5.2; the holder snapshot and commit-epoch selection for the stake-weighted quorum in section 1); it supersedes its 70/30 split; it keeps and extends its deferrals (slashing, fraud proofs, and now storage/serving enforcement, all in one future branch).

---

## Implementation decomposition

Ordered plans, one design of record:

1. Genesis as initial state; require a gas coin (or sponsor) on every user transaction (close the fee-less hole).
2. `total_supply` counter (the invariant in section 7); remove the scarcity fee burn (100/0); the issuance hook mints even at zero fees.
3. Stake on `ValidatorInfo` (accessors, snapshot encoder, minimum stake, unbonding, jailing-zeroes-weight); `total_bonded` derivation.
4. Delegation: delegation-position objects, `delegated_total`, fixed commission, epoch-boundary proportional split (delegations effective at the next boundary), delegator unbonding.
5. Stake-weighted capped quorum (all counting sites, epoch-pinned) and the dual security model.
6. The thermostat (per-epoch issuance, pre-mint ratio, band with dead-band, auto-restake) at the epoch boundary.
7. Reward distribution: pool, `effective_stake x liveness`, crediting via `creditCoin`, delegator split, auto-restake.
8. Sponsored transactions (`fee_payer`, `sponsor_signature`, `valid_until`, dual signature over the shared body hash, absent-when-empty encoding).
9. The `Vertex.timestamp` field only (signed body, validity bound); the clock pipeline is deferred.

The whitepaper, VISION, and the FlatBuffers schemas are updated to match (see Doc impact) once the implementation lands.
