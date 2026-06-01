# Economic Layer Design

**Goal:** Make the BluePods economy close the loop. Today fees are computed and deducted, but rewards are never paid, the burn destroys nothing, there is no issuance, no supply tracking, and no staking. This design fills those gaps and adds the missing primitives (a monetary thermostat, a dynamic fee, real supply accounting, staking, and a deterministic on-chain clock) around the existing epoch and consensus machinery.

**Token identity:** BluePods is a decentralized cloud. The native token is utility-first and stability-oriented: a means to pay for compute and storage and to secure the network, not a speculative asset. The more stable its value, the better the product. This identity drives every decision below.

**Guiding principle:** Design the mechanism, parameterize the numbers. Concrete values (inflation rates, stake minimum, fee responsiveness) are not truths; they are governed, calibrated parameters, with sane starting values tuned before genesis. Every control loop in this design is bounded, rate-limited, deterministic (a pure function of committed on-chain state), and denominated in real time.

This is one design of record. The implementation will be decomposed into several ordered plans (see "Implementation decomposition").

---

## 1. Stake-weighted consensus

Voting weight in the DAG becomes proportional to a validator's bonded stake, not one vertex per node. This is the only mechanism that resists Sybil attacks: splitting stake across many identities yields the same total weight, so creating identities buys nothing.

- **Global DAG quorum** is stake-weighted. The commit and parent-link checks change from "2n/3+1 distinct validators" to "validators whose combined stake reaches two thirds of the total." The comparison is done in exact integer arithmetic (`3 * sum >= 2 * total`), never floating point, to preserve determinism. Per-mille (‰) is a display-only convenience for showing a validator's share.
- **Per-object attestation stays equal-weight** (one holder, one vote). Holders are assigned by Rendezvous Hashing over the validator set, not by stake, so weighting a small per-object holder set by stake would be both complex and worse (a single large holder could dominate a set of ten). The separation is deliberate: stake weighs the global order, never the local attestation.

**Rejected:** stake-age (coin-age) weighting. It is Peercoin's deprecated model: it enables an accumulation attack (bank age while offline, strike in a burst), rewards inactivity over work, blurs the BFT threshold, and entrenches early holders. The legitimate concern it targets (a new whale should not capture the network instantly) is handled by the unbonding delay and activation, and duration-weighting belongs to governance (vote-escrow models), not consensus.

**Deferred:** a per-validator voting-power cap (Sui's 10%). A per-key cap is circumvented by splitting across keys, so it is not the Sybil defense (stake-weighting is). Its real value is limiting honest concentration, which matters mainly once delegation exists. Revisit with delegation.

---

## 2. Bonding

Bonding locks tokens as stake. The stake determines voting weight, reward share, and (later) slashable collateral.

- **Representation.** The stake is a field on `ValidatorInfo` (consensus level), durable in the validator's committed record. It is not a flag on coins. Bonding is a transaction that references a coin the validator owns; the protocol debits that coin and credits `ValidatorInfo.Stake`. The coin model stays untouched; bonded value simply leaves the liquid balance and is recorded against the validator. This keeps stake-weighting trivial (the consensus already iterates `ValidatorInfo`), slashing trivial (reduce the field), and the thermostat trivial (read the aggregate).
- **Minimum stake** to register: yes, a parameter. It gives every validator slashable skin in the game and filters registration spam.
- **Unbonding period.** When stake is withdrawn it stays locked and slashable for a delay before becoming liquid, tracked as a pending amount plus an available-at timestamp (using the median clock of section 6). The length is a security parameter coupled to fraud-detection latency, so it is finalized alongside the future slashing/dispute system.
- **Self-stake only.** Delegation is deferred. It is a large subsystem (delegator accounts, reward sharing, commission, delegated slashing, undelegation queues) and is not needed to launch.

**Consequence for the thermostat.** With self-stake only, the achievable staking ratio is bounded by what validators themselves bond; tokens held by cloud users to pay for hosting cannot be staked. The thermostat's target ratio is therefore set modestly (around 25 to 30 percent), and revisited upward when delegation lets the broad token base contribute.

---

## 3. Monetary policy: the thermostat

Issuance is an adaptive control loop, not a fixed schedule. At each epoch boundary the protocol reads the current staking ratio and nudges the inflation rate toward a target, the way a thermostat tracks a set temperature.

- **Target:** a healthy staking ratio (`total_bonded / total_supply`). Under target, inflation rises to attract stake; over target, it falls (no point over-paying for security).
- **Bounded:** the inflation rate stays within `[floor, ceiling]`.
- **Rate-limited:** the rate of change is capped (a maximum percentage-points-per-year), so inflation drifts and never lurches. The speed of change is proportional to the distance from target and capped at the maximum, so a large shortfall (a crisis) moves at full speed while near-target moves slowly. Inflation can rise (accelerate upward) freely within the band; only the speed of that rise is capped.
- **Deterministic:** the loop is a pure function of committed on-chain state (the staking ratio), so every validator computes the identical next rate. "Dynamic" does not mean non-deterministic.
- **Real-time denomination:** the amount minted at a boundary is pro-rated by the epoch's real duration (section 6), so it is correct in per-year terms regardless of how many rounds the epoch took.

The rate-limit is consistent with the threat it tracks: the staking ratio cannot crash instantly because exits are bounded by the unbonding delay and churn limit, so a rate-limited response matches a rate-limited change.

**Why a control loop and not a calendar.** A fixed schedule (Solana, Celestia) forces guessing the bootstrap duration and the trajectory. The thermostat removes that guess: the network discovers the inflation rate that sustains the target ratio. You set a target plus guardrails, not a curve.

**Rejected:** a naive fee-indexed issuance loop. Indexing on fee revenue is fragile (a closed loop that can oscillate or death-spiral), introduces a manipulable sensor, and couples two levers that should stay independent. The proven, robust way to make activity reduce net inflation is the burn side, not feedback-controlled gross issuance; here the staking-ratio target is the proven control variable (Cosmos), not a market signal (Solana's rejected SIMD-0228).

**Starting parameters:** target ratio 25 to 30 percent, floor 1 percent, ceiling 20 percent, genesis rate 8 to 10 percent, maximum change 5 percentage points per year. The high ceiling is emergency/bootstrap capacity, rarely reached in healthy operation; fast bootstrap comes from the high genesis starting rate, not the ceiling.

---

## 4. No scarcity burn

A burn creates scarcity, which is upward price pressure coupled to usage. That is the opposite of the stability goal. The token is therefore left mildly inflationary (the thermostat only adds supply), which is appropriate for a cloud's medium of payment.

- **The fee burn is removed.** One hundred percent of fees go to validators (previously 70/30). This also gives validators more income, which the thermostat would otherwise have to supply through inflation.
- **The deletion burn (5 percent of an object's storage deposit, on deletion) is kept.** It is anti-spam against rapid create/delete churn and is orthogonal to monetary policy.
- **Real price stability** is not delivered by removing the burn; any free-floating token is volatile. It is delivered later by the cycles/peg model (a stable USD-denominated credit minted by burning the token, ICP-style), which is on the roadmap and unblocked by the project's planned data oracle. Removing the burn only removes a source of instability (appreciation).

---

## 5. Fees

### 5.1 Structure (already implemented)

The fee is proportional to the fraction of nodes a transaction touches: compute scales with the replication ratio of mutated objects, storage scales with each created object's replication ratio. A singleton's storage component caps at the full base fee and never grows with network size, because the ratio is normalized by the validator count. This is the existing model in `fees.go` and is kept.

### 5.2 Distribution

The consumed fees (compute, transit, domain) accumulate in the global epoch reward pool; there is no per-object storage fund. Rendezvous Hashing spreads storage load uniformly across validators (law of large numbers), so paying everyone from the common pool is approximately equivalent to paying each validator for what it stores, which is why uniformity makes a dedicated storage fund unnecessary.

The storage component is a refundable deposit, not a consumed fee. It is locked against the created object (in the object's `fees` field) and refunded 95 percent to the owner on deletion, with 5 percent burned. This resolves the double count flagged in the audit, where the storage amount was both split as a consumed fee and recorded as a refundable deposit: it is a deposit, held, never distributed as reward.

### 5.3 Dynamic level (EIP-1559 / Base style)

`gas_price` adjusts toward a target throughput, rising under congestion and falling when idle, instead of being a fixed constant.

- **You set capacity, the market sets price.** The operator sets a target throughput (gas per second), benchmarked from a node's real processing capacity. The price is then discovered: it rises until demand fits capacity, falls to a floor when there is room. The absolute "cost of a gas" is never chosen; the dynamic mechanism converts that abstract question into a concrete capacity question.
- **Target via per-round gas limit and fullness ratio.** A per-round gas limit (like a block gas limit) defines capacity per round; the fee adjusts on the fullness ratio (`gas_used / limit`), which is rate-independent for the target.
- **Responsiveness denominated in real time.** The adjustment magnitude is scaled by real elapsed time (section 6), not by round count, so a variable round rate does not change the behavior. Starting responsiveness copies Base's proven configuration (roughly a 10x rise over about 4 minutes of sustained maximum congestion, elasticity 6), expressed per real second and scaled to the node's round time.
- **The floor** is an anti-spam baseline (the minimum cost to do anything). Its fiat meaning becomes concrete only with the cycles/peg model.
- **No burn:** the full fee goes to validators.
- An asymmetric response (rise faster than it falls) is an optional calibration refinement for smoother user experience.

### 5.4 DDoS defense in layers

The dynamic fee is the economic backstop for sustained congestion, not the acute-DDoS tool. Acute attacks are handled by faster, cheaper layers that already exist:

| Layer | Timescale | Mechanism | Status |
|---|---|---|---|
| Ingress hardening | microseconds | QUIC Retry (address validation before work), per-IP connection/stream/memory caps, per-IP rate limiting | exists |
| Minimum fee floor | instant, per tx | `min_gas` makes every transaction cost something | exists |
| Dynamic fee | minutes | prices sustained economic congestion | new |

Because layers 1 and 2 absorb volumetric floods, the dynamic fee does not need to react in seconds; cranking it that fast would cause whipsaw (punishing legitimate transient bursts) without being the right tool.

---

## 6. Real-time via median-quorum timestamps

The DAG advances by causal rounds, not by time, and its round rate is emergent and variable. Denominating any rate "per round" or "per epoch" is therefore fragile (at 20 rounds per second instead of 1, a per-round loop moves 20x too fast). The fix is to denominate every loop in real time, using a deterministic clock derived from data the consensus already commits.

- **The round clock.** Each committed round gets a canonical timestamp: the median of its committed vertices' timestamps. Each validator already stamps its vertex with its local clock; the median over a committed round's vertices is a single value every node computes identically from the same committed set. It is deterministic in value and approximate in accuracy (bounded by the spread of honest clocks, on the order of seconds); a Byzantine minority cannot move the median outside the honest range. It is the Bitcoin median-time-past / Tendermint BFT-time technique, and it is free because the timestamps ride inside vertices the consensus commits anyway, with no separate agreement step. The clock is made monotonic (a round's median never precedes the previous round's).
- **Usage.** The elapsed real time between two points is the difference of their round medians (`delta_t`). The fee loop measures throughput as `gas_committed / delta_t` against a per-second target and scales its adjustment by `delta_t`. The thermostat pro-rates issuance by the epoch's real duration: `mint = annual_rate * supply * (delta_t_epoch / seconds_per_year)`. Both become independent of the round/epoch rate.
- **Epoch boundaries stay round-based.** A boundary fires every `epochLength` rounds (a deterministic, immediate trigger for consensus events such as the validator-set snapshot), while the monetary amounts are time-denominated. Round-based trigger for the event, real-time amount for the money.

**Exposed to pods.** The committed round timestamp is passed into pod execution as a deterministic context input (like `block.timestamp`), identical across all holders so determinism holds. This enables time-locks, vesting, auctions, subscriptions, and recurring storage rent. Documented caveats: it is coarse (do not use for sub-second logic), manipulable within a bounded window (do not use as randomness or for sub-second security-critical logic), and updates per commit.

---

## 7. Reward distribution

This finishes the existing `TODO: credit to validator's reward_coin` in `epoch.go`.

- **The pool** for an epoch is `epochFees + thermostat issuance`, where `epochFees` is the consumed fees (compute, transit, domain); storage deposits are held against objects, not pooled (section 5.2).
- **The weight** is `stake * participation`. Participation is `epochRoundsProduced / epochTotalRounds`, already tracked: a per-validator counter incremented for each committed vertex (in the commit path, never on receipt, so it is deterministic), divided by the epoch's committed-round count. It measures consensus liveness. Verified useful work (did a holder actually store and serve and execute correctly) has no light measurement; it is the deferred fraud-proof concern, not part of participation.
- **Crediting.** Each validator's share is credited to a liquid `reward_coin` it designates at registration (a coin ObjectID stored on `ValidatorInfo`), reusing the existing `creditGasCoin` mechanism. Rewards are liquid so validators, who are infrastructure providers with real fiat costs, can realize income; compounding is available by manually bonding from the reward coin rather than forced.

---

## 8. Supply and bonded tracking

The thermostat needs both terms of the staking ratio, and the burn must become real.

- **`total_supply`** is a maintained protocol counter: increased by mint and inflation, decreased by the deletion burn and (later) slashing. It cannot be derived cheaply (summing all coins is O(coins)), so it is maintained incrementally in the commit path, persisted, and included in snapshots so a syncing node recovers it. This makes the burn real (it now actually reduces supply) and closes the "no supply tracking" gap.
- **`total_bonded`** is derived, not a counter: the sum of `ValidatorInfo.Stake` over the active set at the epoch boundary. It is O(validators) and the set is already snapshotted, so no separate counter is maintained.
- Both are dedicated protocol values (a known storage key, like the version tracker), not on-chain objects, because they are aggregate protocol state that mutates frequently.

---

## 9. Slashing (deferred)

In line with newer L1s (Sui, Aptos do not slash principal; Solana has no automatic slashing), this design ships without principal slashing.

- **Now:** reward-withholding (no participation, no reward, already in place) and removal/jail. No principal slash.
- **Stake is slashing-ready:** the `Stake` field is reducible and `total_supply` is decrementable (section 8), so slashing plugs in cleanly later. Slashed stake is burned (reduces supply), Cosmos-style.
- **Equivocation** (double-signing: two conflicting vertices at the same round) is the one tractable slash, with a self-contained proof (the two signed vertices), needing no dispute protocol. It is deferred with the rest in this chantier but is the natural first slash to add.
- **The dispute / fraud-proof system** for incorrect execution and bad attestation is a dedicated future chantier (an open problem). Unlike the no-slash chains, BluePods shards execution, so a Byzantine holder has real local authority over its shard; the deterrent against that genuinely requires this system. Slashing is therefore deferred, not abandoned.

---

## 10. Genesis and fee integrity

A transaction without a gas coin currently skips fees (`commit.go`), intended for genesis but exploitable by any user (the dry-run hole where a zero-balance key created objects for free). The fix eliminates the fee-less path at the root rather than guarding it.

- **Genesis is initial state, not transactions.** The initial coin allocation, the founding validator set, and their initial bonded stake are the ledger's starting state, set at chain creation, never transactions. This removes the chicken-and-egg that justified a fee-less branch.
- **Every user transaction requires a funded gas coin** or it is rejected (not executed). There is no exemption.
- **The faucet** (a development/testnet tool) becomes a transfer from a genesis-allocated reserve coin (a normal, fee-paying transaction), not a fee-less mint. In production there is no faucet reserve; the real initial distribution is genesis state. The unlimited mint must be disabled in production (a deployment concern).
- **Protocol actions** (inflation minting, reward crediting, fee deduction, future slashing) are consensus-driven state mutations, not transactions, so they require no gas coin and are not subject to this rule. The invariant is clean: user transactions always pay; the protocol mutates state directly.

---

## Parameters (starting values, calibration placeholders)

| Parameter | Starting value | Role |
|---|---|---|
| Staking-ratio target | 25 to 30 percent | thermostat set point (modest, self-stake only) |
| Inflation floor | 1 percent | thermostat lower bound (security insurance, not zero) |
| Inflation ceiling | 20 percent | thermostat upper bound (emergency/bootstrap capacity) |
| Genesis inflation | 8 to 10 percent | starting rate (the real bootstrap lever) |
| Inflation max change | 5 percentage points per year | thermostat rate limit |
| Scarcity fee burn | 0 | removed for stability |
| Deletion burn | 5 percent | anti-spam on create/delete churn |
| Fee responsiveness | ~10x over ~4 minutes (Base), elasticity 6 | dynamic fee, expressed per real second |
| Fee target | gas per second from node benchmark | dynamic fee capacity |
| Minimum stake | placeholder | registration bar, slashable skin in the game |
| Unbonding period | short, parameterized | coupled to future slashing latency |

---

## Out of scope (deferred, with triggers)

- **Delegation**, the per-validator voting cap, and liquid staking. Trigger: real demand from small holders; also raises the staking-ratio target.
- **Slashing and the dispute / fraud-proof system** for incorrect execution. Trigger: a dedicated chantier; structurally required because execution is sharded.
- **The cycles / USD-peg payment model** (stable credit, reverse gas). Trigger: the planned data oracle. This is the real path to price stability.
- **Correlated slashing** (Ethereum's formula) and an **adaptive burn**: refinements after the basics.

---

## Rejected alternatives

| Decision | Rejected | Why |
|---|---|---|
| Token identity | hybrid utility + value | a cloud wants a stable medium of payment, not a speculative asset |
| Payment model | cycles/peg now | needs the oracle; deferred to roadmap |
| Issuance | fixed calendar | forces guessing the bootstrap duration and trajectory |
| Issuance | fee-indexed loop | fragile feedback, manipulable sensor, couples independent levers |
| Scarcity | a fee burn | scarcity is appreciation pressure, against the stability goal |
| Consensus weight | stake-age (coin-age) | attackable, rewards inactivity, entrenches early holders |
| Fee denomination | per round/epoch | fragile against the variable round rate; use real time |
| Fee-less transactions | genesis-authority exemption | eliminate the path at the root (genesis as state) instead |
| Stake representation | an `is_staked` flag on coins | scattered and heavy; record stake on `ValidatorInfo` instead |

---

## Implementation decomposition

The implementation will be ordered into plans, each building on the prior:

1. Genesis as initial state; require a gas coin on every user transaction (close the hole).
2. Median-quorum timestamp clock; expose it to pods.
3. `total_supply` counter (make the burn real); remove the scarcity fee burn.
4. Bonding: stake on `ValidatorInfo`, minimum stake, unbonding, `total_bonded` derivation.
5. Stake-weighted consensus quorum.
6. The thermostat (issuance) at the epoch boundary, real-time pro-rated.
7. Reward crediting to the liquid `reward_coin`.
8. Dynamic `gas_price` (EIP-1559 style, real-time denominated).

The whitepaper and VISION are updated to match once the implementation lands.
