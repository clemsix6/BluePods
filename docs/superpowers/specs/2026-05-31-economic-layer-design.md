# Economic Layer Design

**Goal:** Make the BluePods economy close the loop. Today fees are computed and deducted, but rewards are never paid, the burn destroys nothing, there is no issuance, no supply tracking, and no staking. This design fills those gaps and adds the missing primitives (adaptive issuance, real supply accounting, staking with delegation, gated attestation-based rewards, sponsored transactions, and a deterministic pod-facing clock) around the existing epoch, consensus, and off-chain-attestation machinery. It has been hardened through two adversarial review iterations; the decisions below reflect the resolutions.

This chantier is the dedicated tokenomics study that the off-chain aggregation design deferred (it left the burn question and "a relayer or sponsor tier" open for exactly this work).

**Token identity:** BluePods is a decentralized cloud. The native token is utility-first and stability-oriented: a means to pay for compute and storage and to secure the network, not a speculative asset. The more stable its value, the better the product. This identity drives every decision below.

**Guiding principle:** Design the mechanism, parameterize the numbers. Two invariants protect against gaming: nothing that controls money depends on a manipulable clock (issuance is denominated in epoch events, not time), and the reward signal is a GATE, not a farmable multiplier (you cannot earn more by manufacturing more of it). Concrete values are governed, calibrated parameters.

This is one design of record. Per the maintainer's decision, it is implemented as one chantier on the `economic-layer` branch (not split), decomposed into ordered plans.

---

## 1. Stake-weighted consensus

Voting weight in the DAG is proportional to a validator's effective stake (its own bonded stake plus stake delegated to it). This is the only Sybil defense: splitting stake across many keys yields the same total weight, so faking nodes buys nothing.

- **`effective_stake` = self-stake + delegated stake.** It is the weight for both the consensus quorum and the reward (section 6). The resulting per-validator reward is then split with delegators (section 2).
- **Per-validator voting-power cap.** A single validator's voting weight is capped at a fraction of the total, regardless of how much stake or delegation it holds. This is added now (not deferred) because delegation creates a winner-take-most concentration regime (delegators pile onto the biggest/most reliable validator); the cap keeps the global order decentralized even when stake concentrates. The cap is a parameter calibrated to the validator-set size: loose while the set is small (a 10 percent cap with 3 validators would make a two-thirds quorum unreachable, since 3 x 10 < 67), tightened toward ~10 percent as the set grows.
- **Quorum is exact integer arithmetic** (`3 x sum >= 2 x total` over capped effective stakes), never floating point. The stake total is read from the epoch holder snapshot selected by the deterministic commit epoch (reusing `HoldersForEpoch` / `commitEpochForRound`), so commit determinism and the attestation grace window agree. Equal-weight counting lives in three sites (`ValidatorSet.QuorumSize`, `isRoundCommitted`'s distinct-producer count, `validateParentsQuorum`) plus the relaxed bootstrap literal (`requiredQuorum = 1`); all move to capped-stake sums.
- **Per-object attestation stays equal-weight** (one holder, one vote). Holders are assigned by Rendezvous Hashing over the validator set, not by stake.
- **Dual security model (stated explicitly).** The global order is safe under a two-thirds-of-stake honest majority; each object's attestation is safe under a two-thirds-of-its-holders (count) honest majority. These are two different honest-majority models that compose: stake-weighting secures ordering, count-weighting secures per-object attestation, and neither weakens the other.

**Interim deterrent without slashing.** Stake-weighting is kept even though slashing is deferred, because the alternative (equal weight) is Sybil-able and strictly worse. An attacker who buys two-thirds of the stake to control consensus pays an enormous capital cost and destroys the value of its own holdings if it attacks; the cap bounds concentration, and reward-withholding plus removal handle misbehavior. This is the posture of Sui, Aptos, and Solana, which run with weak or no principal slashing. Slashing adds economic punishment later (section 10).

**Rejected:** stake-age weighting (Peercoin's deprecated coin-age: enables an accumulation attack, rewards inactivity, entrenches early holders).

---

## 2. Bonding and delegation

Bonding locks tokens as stake. Stake gives voting weight, reward weight, and (later) slashable collateral.

- **Self-stake representation.** Stake is a field on `ValidatorInfo`, not a flag on coins. Bonding is a transaction referencing a coin the validator owns; the protocol debits it and credits the stake. The field must be carried in the copy-returning accessors (`ValidatorSet.Get` / `All`), set through a bonding mutation, rebuilt in the epoch snapshot helpers (`snapshotEpochHolders` / `snapshotOf`), and persisted by extending the validator encoder (`encodeValidators` / `decodeValidators` and the checksum); `Snapshot.validators` is a flat byte vector, so stake rides in it without a FlatBuffers schema change.
- **Minimum stake** to register: a parameter (slashable skin in the game, anti-spam). Note it interacts with validator-set size and therefore per-object holder-set size, so it is calibrated with both in mind.
- **Unbonding period.** Withdrawn stake stays locked and slashable for a delay before becoming liquid. Length is a security parameter tied to fraud-detection latency, finalized with the future slashing system.

**Delegation (in scope, a precondition for the thermostat).** Without it, only validators can bond, so on an adopted cloud, where most supply is user working capital, the staking ratio is structurally capped and the thermostat saturates. Delegation lets the broad base stake idle balances, which makes the target reachable.

- **Representation, two levels.** Each validator carries a maintained `delegated_total` aggregate (used for `effective_stake`, the cap, and `total_bonded`; persisted with self-stake), so `total_bonded` stays O(validators). Each delegation is a stake-position object owned by the delegator (validator, amount, reward-accumulator entry), used for reward sharing and unbonding.
- **Commission is a fixed governed parameter**, not per-validator. This avoids a per-validator commission field, the rate-limited change mechanics it needs, and the rug-pull risk (a validator raising commission to 100 percent to drain delegators). Delegators then choose validators on reliability, not on a commission race. A per-validator commission market is a later refinement.
- **Reward sharing via an accumulator (Cosmos F1 style), the one non-trivial piece.** Instead of paying each delegator every epoch (O(delegators)), a validator keeps a running reward-per-unit-delegated-stake accumulator. When it earns, the fixed commission goes to its `reward_coin` and the rest bumps the accumulator. A delegator's share = its amount x (accumulator now minus accumulator at delegation), realized on undelegate. O(1) per delegate/undelegate. Commission is charged on rewards only, never on principal.
- **Delegator unbonding.** Undelegate destroys the position; after the unbonding delay, principal plus accrued reward is returned.
- **Slashing hook** (deferred): when slashing lands, delegators share the slash pro-rata through the accumulator.

**Liquid staking is deferred** (a transferable staked-position derivative removes the unbonding-delay friction for delegators who need spend-ability; it widens the willing-to-lock base and lets the target rise). Basic delegation ships now.

---

## 3. Monetary policy: the thermostat (time-free, per-epoch)

Issuance is an adaptive control loop, evaluated at each epoch boundary event, denominated in epoch events (not time), so it depends on no clock.

- **Target: a band around the effective staking ratio** (`total_bonded / total_supply`), roughly 25 to 35 percent of total supply, with a dead-band (while the ratio is inside the band, the rate is held, which prevents oscillation). The band is set conservatively and reachable with basic delegation; it errs low, because targeting too low is benign (rests at low inflation, slightly under-secured, easily raised) while targeting too high is catastrophic (saturates at the ceiling and dilutes forever). Governance raises it later as data and liquid staking widen the stakeable base. About 30 percent of total supply is the starting point; the denominator is `total_supply` (not "circulating", which adds complexity for no benefit since issuance is minted over time, not pre-locked).
- **Mechanism, per epoch boundary:** read the ratio on the PRE-mint supply (measuring on post-mint supply would let issuance lower its own denominator and bias the loop upward); adjust the per-epoch rate `r` toward the band, bounded by `[floor, ceiling]`, capped step; mint `r x total_supply` into the reward pool.
- **Why time-free is correct.** The loop targets the ratio (a dimensionless outcome), not a precise inflation number, so it self-compensates for the network's pace: faster epochs mint more for a given `r`, yield rises, more stake comes in, the ratio overshoots, the loop lowers `r`. It converges regardless of pace.
- **Pace manipulation is bounded and subsumed.** Closing epochs faster to mint more requires accelerating round production, which requires a two-thirds-stake quorum (a minority cannot advance rounds), i.e. the catastrophic threshold at which the attacker already controls consensus and can do far worse. It is also bounded by the ceiling and self-destructive (the attacker dilutes its own stake). So it is not a standalone hole. The oracle later re-denominates issuance in real time, closing the residual and restoring a clean per-year bound.
- **Why not the clock or a VDF.** Tying issuance to a median-of-timestamps clock creates a collective bias (validators profit from over-stating elapsed time, and the median cannot stop a colluding majority). A VDF measures hardware-dependent compute, keeps a fast-hardware bias, and does not fit a leaderless DAG (no canonical runner; aggregating per-validator VDFs reintroduces a median and burns continuous compute).
- **Auto-restake.** A fraction of each reward is auto-restaked by default (opt-out), to relieve the treadmill where liquid rewards are spent, raising supply and lowering the ratio, forcing the loop to mint more.
- **Why the thermostat ships now while the dynamic fee does not.** Issuance is needed from genesis to pay and attract validators (the genesis rate is the bootstrap incentive); the dynamic fee only matters under congestion that a pre-traffic network cannot exhibit. The control loop over a now-real condition (paying validators) is justified; the one over an unobservable condition (congestion) is premature.

**Starting parameters.** Enforced and applied per-epoch, expressed here as the annual figures they are calibrated to approximate against an assumed epoch pace: floor ~1 percent (a low perpetual security floor, not zero); ceiling ~20 percent (emergency/bootstrap capacity, rarely reached); genesis ~8 to 10 percent; plus a small per-epoch step cap. Because the loop targets the ratio, realized annual inflation self-corrects regardless of pace; the annual reading is approximate until the oracle supplies true time. All governed.

---

## 4. No scarcity burn

A burn creates scarcity (upward price pressure coupled to usage), the opposite of the stability goal. The token is left mildly inflationary, appropriate for a cloud's medium of payment.

- **The scarcity fee burn is removed.** One hundred percent of consumed fees go to validators (the off-chain design's 70/30 split is superseded; see Doc impact).
- **The anti-gaming role the burn used to play is now covered by the reward gate (section 6), not by a burn.** A burn deterred validators from recycling fees to farm rewards; the gate removes the farming incentive at the source (you cannot earn more by manufacturing attestations), so the burn is not needed for that purpose.
- **The 5 percent deletion burn on the storage deposit is kept** (anti-spam on create/delete churn, orthogonal to monetary policy).
- **Real price stability** is delivered later by the cycles/peg model (a stable internal credit minted by burning the token, ICP style), unblocked by the planned oracle. Removing the burn only removes one source of instability.

---

## 5. Fees

### 5.1 Structure (already implemented)
Proportional to the fraction of nodes a transaction touches (compute and storage scale with the replication ratio; a singleton caps at the full base fee and never grows with network size). Kept.

### 5.2 Distribution
Consumed fees (compute, transit, domain) accumulate in the global epoch reward pool; there is no per-object storage fund, because Rendezvous Hashing spreads storage load uniformly, so the common pool is approximately equivalent to paying each validator for what it stores. The storage component is a refundable deposit, not a consumed fee: locked in the object's `fees` field, refunded 95 percent to the owner on deletion, 5 percent burned. It is held, never distributed as reward.

### 5.3 Level: fixed for launch, dynamic deferred
`gas_price` stays a fixed, governed constant. A congestion-responsive dynamic fee is deferred: it closes none of this chantier's gaps, is not the acute-DDoS tool, and cannot be calibrated without live traffic. Simulations decide later whether it is needed.

### 5.4 DDoS defense in layers
The acute defense already exists and does not need the dynamic fee:

| Layer | Timescale | Mechanism | Status |
|---|---|---|---|
| Ingress hardening | microseconds | QUIC Retry, per-IP caps, rate limiting | exists |
| Minimum fee floor | instant, per tx | `min_gas` | exists |
| Dynamic fee | minutes | prices sustained congestion | deferred |

---

## 6. Reward distribution (gated, not multiplied)

This finishes the `TODO: credit to validator's reward_coin` in `epoch.go`, and fixes the freeloading and self-dealing holes both review iterations found.

- **The pool** for an epoch is `epochFees + thermostat issuance` (consumed fees; storage deposits are held, not pooled).
- **Weight = `effective_stake x liveness participation`, GATED by attestation serving.** Liveness participation is `epochRoundsProduced / epochTotalRounds`, which is not farmable (a validator produces at most one vertex per round; two is equivocation), so it rewards uptime. Reward is NOT proportional to attestation count, which is farmable (a validator can manufacture demand by transacting against its own held objects). Making attestation a gate, not a multiplier, kills self-dealing (serving more earns nothing extra) and the original freeloading (a live-but-non-serving validator fails the gate).
- **The gate.** A validator must serve the attestations it is asked for, for the objects it holds, when others transact them. It is measured from the signer bitmaps of committed, proof-verified ATXs (only appearances from ATXs whose proofs passed are counted). A validator consistently absent from the bitmaps of objects it holds, while those objects are being transacted, fails the gate (jail / no reward).
- **Crediting.** Each validator's reward is credited to a liquid `reward_coin` it designates, using the consensus `creditCoin` primitive on `d.coinStore` (not `state.creditGasCoin`, which is unreachable from the consensus package). The validator's reward is split with its delegators through the accumulator (fixed commission to the validator, the rest to delegators); a fraction is auto-restaked by default.
- **Cold objects are not drained** (reward depends on stake and liveness, not on appearance count, so a cold-object holder earns the same as a hot-object holder at equal stake and uptime). A holder could still drop a permanently-cold object to save storage, but the gate is a probabilistic deterrent (if the object is ever transacted, the holder fails to serve and fails the gate) and replication means one holder dropping does not lose the object. Full verification of cold storage (active storage challenges) is deferred (section 10).

---

## 7. Supply and bonded tracking

- **`total_supply`** is a maintained protocol counter (a dedicated key): increased by mint and issuance, decreased by the deletion burn and future slashing. Maintained in the commit path, persisted, included in snapshots. It makes the burn real and feeds the thermostat denominator. Net-new concept (no whitepaper home today; see Doc impact). A useful invariant to property-test: the sum of all coin balances plus `total_bonded` equals `total_supply` after every commit.
- **`total_bonded`** is derived: sum of (`self-stake` + `delegated_total`) over the active set at the epoch boundary, O(validators), the set already snapshotted.

---

## 8. The pod clock (deterministic time, decoupled from money)

A deterministic on-chain clock is a core primitive for a cloud (subscriptions, time-locks, expirations). It is kept, but controls no money.

- **Source:** the median of each committed round's vertex timestamps, made monotonic. Deterministic in value, approximate in accuracy (bounded by the honest-clock spread, seconds); a Byzantine minority cannot move it outside the honest range.
- **New, consensus-breaking infrastructure:** vertices have no timestamp field today, so one is added to `vertex.fbs` and included in the signed/hashed body with a validity bound. Acceptable because there is no live mainnet; it must land before any consumer (no in-scope money consumer precedes it).
- **Exposed to pods** as a deterministic execution-context input (like `block.timestamp`), with documented caveats: coarse, not for randomness or value-bearing sub-second logic.
- **It touches no money** (issuance is per-epoch, section 3), so the over-state-time-to-mint bias does not exist. The oracle later anchors it to a non-falsifiable wall-clock.

---

## 9. Sponsored transactions

A zero-balance new user cannot make a first transaction (every user tx needs a funded gas coin owned by the sender, section 11). The proper fix, and the resolution of the off-chain design's open relayer/sponsor tier, is native sponsored transactions, done the Sui way (a protocol-level fee payer), not the Base way (layered ERC-4337, a workaround for Ethereum's frozen base layer that BluePods does not need).

- **Schema.** `transaction.fbs` gains `fee_payer`, `sponsor_signature`, and `valid_until` (an epoch); a schema change of the same character as the `vertex.fbs` timestamp.
- **Mutual binding.** Both the sender and the sponsor sign the SAME canonical body hash, covering everything (sender, content, refs, `gas_coin`, `fee_payer`, `max_gas`, `valid_until`) except the two signature fields. The sender thus commits to who pays, and the sponsor to the exact content, gas coin, and cap; neither field can be swapped after the other signs. An empty `fee_payer` yields a byte-identical body to today, so existing single-sender hashes and signatures still verify. The sponsor signature is verified in the validation layer, gated on `fee_payer` being present; the gas-coin ownership check becomes `owner == fee_payer`.
- **Replay** is prevented by the existing commit-once guard (keyed on the tx hash, which now covers the sponsor binding), so a sponsored tx commits once and the sponsor pays once.
- **Aggregate exposure.** `valid_until` (in epochs, not the clock) kills stale sponsorships; the sponsor manages its budget off-chain (it signs only what it wants; the chain cannot overdraw its coin) and is the natural submitter (controlling the rate).
- **Minor:** on a sponsored create then delete, the 95 percent storage refund goes to the owner-deleter, not the sponsor who paid the deposit. Accepted.
- **Coordination is off-chain;** the chain sees only the final doubly-signed tx. For a sponsored tx that also needs attestation, the submitter (sponsor) collects it last, just before submitting, so the handoff does not widen the version-race window.

---

## 10. Slashing (deferred)

In line with newer L1s, this chantier ships without principal slashing.

- **Now:** reward-withholding (fail the gate, no reward) and removal/jail. No principal slash.
- **Stake is slashing-ready** (`Stake` reducible, `total_supply` decrementable); slashed stake is burned.
- **The dispute / fraud-proof / relay-resistant storage-challenge system** is a dedicated future branch (an open problem). BluePods shards execution, so a Byzantine holder has local shard authority; the full deterrent requires this system. In the interim the sharded layer has capability-based security (honest majority per object) plus the reward gate, not yet economic punishment.

---

## 11. Genesis and fee integrity

- **Genesis is initial state, not transactions.** The initial coin allocation, founding validator set, and their initial bonded stake are the ledger's starting state, set at chain creation. A real bootstrap rewrite (today the genesis mint and `register_validator` are real transactions), not a relabel.
- **Every user transaction requires a funded gas coin** (or a sponsor, section 9) or it is rejected. The missing-gas-coin exemption is removed.
- **The faucet** becomes a transfer from a genesis-allocated reserve (a normal fee-paying tx). Production has no faucet reserve.
- **Protocol actions** (issuance, reward crediting, fee deduction, future slashing) are consensus-driven state mutations, not transactions, so they need no gas coin.

---

## Parameters (starting values, calibration placeholders)

| Parameter | Starting value | Role |
|---|---|---|
| Staking-ratio target band | ~25 to 35% of total_supply (dead-band) | thermostat set point (err low, reachable) |
| Inflation floor / ceiling | ~1% / ~20% annual (per-epoch, approximate until oracle) | thermostat bounds |
| Genesis inflation | ~8 to 10% annual | bootstrap lever |
| Inflation per-epoch step cap | small | anti-lurch limit |
| Auto-restake fraction | a fraction, opt-out | relieves the treadmill |
| Scarcity fee burn | 0 | removed for stability |
| Deletion burn | 5% | anti-spam |
| gas_price | fixed, governed | dynamic deferred |
| Delegation commission | fixed (e.g. ~10%), governed | validator compensation, no rug-pull |
| Per-validator voting-power cap | loose then ~10%, calibrated to set size | anti-concentration |
| Minimum stake | placeholder | registration bar |
| Unbonding period | short, parameterized | tied to future slashing |

---

## Out of scope (deferred, with triggers)

- **Liquid staking.** Trigger: demand for stake liquidity; widens the willing-to-lock base.
- **Per-validator commission market.** Trigger: demand for differentiation; needs anti-rug-pull change limits.
- **Dynamic fee.** Trigger: live traffic, decided by simulation.
- **Relay-resistant storage challenges, slashing, fraud proofs.** Trigger: a dedicated branch; structurally required because execution is sharded.
- **Cycles / USD-peg model and the data oracle.** Trigger: the planned oracle. Delivers real price stability, sponsored/reverse gas at scale, and real-time re-denomination of issuance for a clean per-year bound.

---

## Rejected alternatives

| Decision | Rejected | Why |
|---|---|---|
| Token identity | hybrid utility + value | a cloud wants a stable medium of payment |
| Issuance | fixed calendar / fee-indexed loop / clock or VDF denomination | guessing the trajectory / fragile feedback / clock-on-money bias |
| Reward weight | proportional to attestation count | farmable by self-dealing; use a gate |
| Reward weight | stake only (drop participation) | loses the uptime incentive; keep liveness participation as a non-farmable multiplier |
| Scarcity | a fee burn | appreciation pressure against stability; the gate covers anti-gaming |
| Consensus weight | stake-age | attackable, rewards inactivity |
| Consensus weight | equal weight (defer stake-weighting until slashing) | Sybil-able, strictly worse than capped stake-weighting |
| Anti-concentration | defer the cap | delegation creates the concentration regime now |
| Commission | per-validator | rug-pull risk and change-mechanics; fixed is simpler and safer |
| Sponsored tx | Base-style ERC-4337 layer | a workaround for a constraint BluePods does not have |
| Onboarding | defer all sponsorship to the cycles model | native sponsored tx is lighter and standalone |
| Fee-less transactions | genesis-authority exemption | eliminate the path at the root (genesis as state) |
| Stake representation | an `is_staked` flag on coins | record on `ValidatorInfo` |
| Storage payment | per-object storage fund | rendezvous uniformity makes the global pool equivalent |

---

## Doc impact

To update in place once implemented:

- **WHITEPAPER section 9 (Fees):** 70/30 to 100/0, drop the burn-as-anti-gaming argument, and the reward-distribution prose (now gated stake x liveness, not consensus participation).
- **WHITEPAPER section 10 (Reward Distribution formula):** `stake x (rounds/total)` becomes `effective_stake x liveness, gated by serving`, split with delegators.
- **WHITEPAPER sections 10 / 5 (Validators, Consensus):** stake-weighted capped quorum and the dual security model replace the equal-weight framing and the "stake is equal (1) for now" note; genesis-as-state changes the genesis-epoch story.
- **WHITEPAPER section 7 / 9 (Transaction Lifecycle, Gas Coin):** sponsored transactions (a `fee_payer`, a sponsor signature, gas coin owned by the fee payer not the sender) and the new daemon trust note.
- **WHITEPAPER (new):** a short `total_supply` accounting subsection (net-new concept).
- **`types/vertex.fbs`:** `FeeSummary.total_burned` becomes vestigial (zero); a new signed `Vertex.timestamp` field is added.
- **`types/transaction.fbs`:** `fee_payer`, `sponsor_signature`, `valid_until` added.
- **The off-chain aggregation spec** is superseded on the 70/30 split and its open relayer/sponsor item is resolved here.

---

## Relationship to the off-chain aggregation design

This chantier is the deferred tokenomics study; it resolves that design's open relayer/sponsor tier (section 9) and burn question (section 4); it reuses its machinery (committed signer bitmaps gate the reward in section 6; the global-pool reasoning in 5.2; the holder snapshot and commit-epoch selection for the stake-weighted quorum in section 1); it supersedes its 70/30 split (now 100/0) and refines its reward basis (consensus participation to gated stake-and-liveness); it keeps its deferrals (slashing, fraud proofs, storage challenges).

---

## Implementation decomposition

Ordered plans, one design of record:

1. Genesis as initial state; require a gas coin (or sponsor) on every user transaction (close the fee-less hole).
2. `total_supply` counter; remove the scarcity fee burn (100/0).
3. Stake on `ValidatorInfo` (accessors, snapshot encoder, minimum stake, unbonding); `total_bonded` derivation.
4. Delegation: delegation-position objects, `delegated_total`, fixed commission, the F1 accumulator, delegator unbonding.
5. Stake-weighted capped quorum (all counting sites, epoch-pinned) and the dual security model.
6. The thermostat (per-epoch issuance, pre-mint ratio, band with dead-band) at the epoch boundary.
7. Reward distribution: pool, `effective_stake x liveness` gated by attestation serving (the verified-bitmap serving counter), crediting via `creditCoin`, delegator split, auto-restake.
8. Sponsored transactions (`fee_payer`, `sponsor_signature`, `valid_until`, dual signature over the shared body hash).
9. The pod clock (signed `Vertex.timestamp`, median, pod exposure), decoupled from money.

The whitepaper, VISION, and the FlatBuffers schemas are updated to match (see Doc impact) once the implementation lands.
