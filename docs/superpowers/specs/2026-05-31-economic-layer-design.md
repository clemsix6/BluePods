# Economic Layer Design

**Goal:** Make the BluePods economy close the loop. Today fees are computed and deducted, but rewards are never paid, the burn destroys nothing, there is no issuance, no supply tracking, and no staking. This design fills those gaps and adds the missing primitives (adaptive issuance, real supply accounting, staking with delegation, attestation-based rewards, sponsored transactions, and a deterministic pod-facing clock) around the existing epoch, consensus, and off-chain-attestation machinery.

This chantier is the dedicated tokenomics study that the off-chain aggregation design explicitly deferred (it left the burn question and "a relayer or sponsor tier" open for exactly this work).

**Token identity:** BluePods is a decentralized cloud. The native token is utility-first and stability-oriented: a means to pay for compute and storage and to secure the network, not a speculative asset. The more stable its value, the better the product. This identity drives every decision below.

**Guiding principle:** Design the mechanism, parameterize the numbers. Concrete values (inflation rate, stake minimum, fee level) are governed, calibrated parameters with sane starting values tuned before genesis. Control loops are bounded and rate-limited. Crucially, nothing that controls money depends on a manipulable clock: issuance is denominated in epoch events, not time.

This is one design of record. The implementation decomposes into ordered plans (see "Implementation decomposition").

---

## 1. Stake-weighted consensus

Voting weight in the DAG becomes proportional to a validator's bonded stake, not one vertex per node. This is the only mechanism that resists Sybil attacks: splitting stake across many identities yields the same total weight.

- **Global DAG quorum** is stake-weighted. The change is broader than one site: equal-weight counting lives in `ValidatorSet.QuorumSize()`, in `isRoundCommitted` (which counts distinct producers), and in `validateParentsQuorum`, plus the relaxed bootstrap literals (`requiredQuorum = 1` below `minValidators`). All move from counting distinct validators to summing their stake against a two-thirds threshold, in exact integer arithmetic (`3 * sum >= 2 * total`), never floating point. Per-mille is display-only.
- **Stake must be read from the epoch holder snapshot**, not the live set. The stake total used for a vertex committed at round R is taken from the holder snapshot selected by the deterministic commit epoch (reusing `HoldersForEpoch` / `commitEpochForRound`), exactly as the off-chain attestation layer already does, so commit determinism and the attestation grace window agree on "the total."
- **Per-object attestation stays equal-weight** (one holder, one vote). Holders are assigned by Rendezvous Hashing over the validator set, not by stake, so weighting a small per-object set by stake would be both complex and worse. Stake weighs the global order, never the local attestation. (This separation is consistent with the off-chain aggregation design and does not disturb it.)

**Rejected:** stake-age (coin-age) weighting (Peercoin's deprecated model: enables an accumulation attack, rewards inactivity, entrenches early holders; the "no instant whale" concern is handled by activation and the unbonding delay). **Deferred:** a per-validator voting-power cap (a per-key cap is circumvented by splitting; its real value is limiting honest concentration, which matters mainly once delegation exists).

---

## 2. Bonding and delegation

Bonding locks tokens as stake. Stake determines voting weight, reward weight, and (later) slashable collateral.

- **Representation.** Stake is a field on `ValidatorInfo`, not a flag on coins. Bonding is a transaction that references a coin the validator owns; the protocol debits that coin and credits `ValidatorInfo.Stake`. Adding this field is not "just a field": it must be carried in the copy-returning accessors (`ValidatorSet.Get` / `All`), set through a bonding mutation (extending `Add` or a new setter), rebuilt in the epoch snapshot helpers (`snapshotEpochHolders` / `snapshotOf`), and added to the persisted `Snapshot.validators` schema so `total_bonded` survives state sync.
- **Minimum stake** to register: a parameter (slashable skin in the game, anti-registration-spam).
- **Unbonding period.** Withdrawn stake stays locked and slashable for a delay before becoming liquid (a pending amount with an available-at marker). The length is a security parameter tied to fraud-detection latency, finalized with the future slashing/dispute system.

- **Delegation is in scope and is a precondition for the thermostat (section 3), not a deferred nicety.** A token holder who does not run a validator can delegate stake to a validator, sharing its rewards and (later) its slashing. Without delegation, only validators can bond, so on an adopted cloud, where most of the supply is held by users as working capital for hosting, the staking ratio is structurally capped far below any useful target and the thermostat saturates. Delegation lets the broad token base stake idle balances, which is what makes the staking-ratio target reachable and removes the saturation failure. Delegators share slashing risk (Cosmos/Polkadot style).
- **Liquid staking is deferred.** Basic delegation carries the unbonding-delay friction (a delegator who needs to spend must wait to unbond). A transferable staked-position derivative (stETH style) removes that friction and fits a utility token where holders need spend-ability, but it is a larger subsystem. Start with basic delegation; liquid staking is an evolution.

---

## 3. Monetary policy: the thermostat (time-free)

Issuance is an adaptive control loop, evaluated at each epoch boundary event. It is denominated in epoch events, not in real time, so it does not depend on any clock and is immune to clock manipulation.

- **Target:** a healthy staking ratio (`total_bonded / total_supply`), reachable because delegation (section 2) lets the whole token base stake.
- **Mechanism, per epoch boundary:** compute the ratio; if below target, raise the per-epoch mint rate `r`; if above, lower it (bounded by `[floor, ceiling]`, capped step per epoch). Mint `r * total_supply` into the reward pool. No time, no round counting.
- **Why time-free is correct.** The loop targets the staking ratio (a dimensionless outcome), not a precise inflation number. If the network runs fast (more epochs), it mints more for a given `r`, yield rises, more stake comes in, the ratio overshoots, and the loop lowers `r`. It converges to whatever per-epoch rate holds the target ratio at whatever pace the network runs. The unknown and variable real duration of rounds and epochs is absorbed by the feedback. The realized real-time inflation is an outcome, not an input.
- **Why not the clock.** Tying issuance to a median-of-timestamps clock creates a collective bias (every validator profits from over-stating elapsed time to mint more, and the median cannot stop a colluding majority). A VDF was considered and rejected: it measures hardware-dependent compute, keeps a fast-hardware bias, and fits leader-based chains (Solana PoH), not a leaderless DAG (no canonical runner; aggregating per-validator VDFs reintroduces a median and burns continuous compute network-wide). The clean answer is to keep money off the clock.
- **The accepted cost.** A "maximum X percentage-points per year" change bound is fuzzy, because the rate limit is per-epoch and epochs-per-year is unknown. This is tolerable: the per-epoch step still caps any single move, and with delegation the ratio rests near target so inflation rarely races toward a bound. The clean per-year bound returns later with the oracle (see section 8 and Out of scope), which gives a non-falsifiable wall-clock that can safely re-denominate issuance in real time.

**Starting parameters.** The rate is enforced and applied per-epoch, but expressed here as the annual figures it is calibrated to approximate (against an assumed epoch pace): floor around 1 percent (a low perpetual floor, not zero, a security backstop); ceiling around 20 percent (emergency/bootstrap capacity, rarely reached); genesis around 8 to 10 percent (the real bootstrap lever); plus a small per-epoch step cap (the anti-lurch limit). The target staking ratio is calibrated before genesis so it is reachable with delegation. Because the loop targets the ratio, the realized annual inflation self-corrects regardless of the true epoch pace, so the annual reading of these bounds is approximate until the oracle supplies true time. All governed.

**Rejected:** a fixed calendar schedule (forces guessing the bootstrap duration and trajectory); a fee-indexed loop (fragile feedback, manipulable sensor); a clock-denominated rate (reintroduces the manipulation bias). The staking-ratio target is the proven control variable (Cosmos), not a market signal (Solana's rejected SIMD-0228).

---

## 4. No scarcity burn

A burn creates scarcity, which is upward price pressure coupled to usage, the opposite of the stability goal. The token is left mildly inflationary (the thermostat only adds supply), appropriate for a cloud's medium of payment.

- **The scarcity fee burn is removed.** One hundred percent of consumed fees go to validators (the off-chain design's 70/30 split is superseded; see Doc impact). This also gives validators more income, which the thermostat would otherwise supply through inflation.
- **The 5 percent deletion burn on the storage deposit is kept** (anti-spam against create/delete churn, orthogonal to monetary policy).
- **Real price stability is not delivered by removing the burn**; any free-floating token is volatile. It is delivered later by the cycles/peg model (a stable internal credit minted by burning the token, ICP style), on the roadmap and unblocked by the planned data oracle. Removing the burn only removes one source of instability (appreciation).

---

## 5. Fees

### 5.1 Structure (already implemented)

The fee is proportional to the fraction of nodes a transaction touches: compute scales with the replication ratio of mutated objects, storage with each created object's replication ratio. A singleton's storage component caps at the full base fee and never grows with network size. Kept as in `fees.go`.

### 5.2 Distribution

The consumed fees (compute, transit, domain) accumulate in the global epoch reward pool; there is no per-object storage fund. Rendezvous Hashing spreads storage load uniformly across validators, so the common pool is approximately equivalent to paying each validator for what it stores, which is why uniformity makes a dedicated storage fund unnecessary. (This is the same reasoning the off-chain aggregation design used to send operational fees to the common pool rather than to the executing holders.)

The storage component is a refundable deposit, not a consumed fee. It is locked against the created object (in the object's `fees` field) and refunded 95 percent to the owner on deletion, with 5 percent burned. This resolves the double count flagged in the audit, where the storage amount was both split as a consumed fee and recorded as a refundable deposit: it is a deposit, held, never distributed as reward.

### 5.3 Level: fixed for launch, dynamic deferred

`gas_price` stays a fixed, governed constant for launch. A dynamic, congestion-responsive fee (EIP-1559 / Base style) is deferred. Reasons: it closes none of the gaps this chantier targets; by the layered DDoS analysis below it is not the acute-DDoS tool; it only handles sustained economic congestion, which a pre-mainnet network with no live traffic has never observed; and the whitepaper itself states fee constants cannot be calibrated without live traffic. Building a control loop for an unobservable condition is premature. When live traffic exists, simulations decide whether a dynamic fee is needed, and it can be added then (it would denominate its responsiveness in real time, which is another reason it waits for the oracle-grade clock). Deferring it also removes a second control loop from this chantier.

### 5.4 DDoS defense in layers

The dynamic fee would have been the economic backstop for sustained congestion, not the acute-DDoS tool. Acute attacks are handled by faster, cheaper layers that already exist:

| Layer | Timescale | Mechanism | Status |
|---|---|---|---|
| Ingress hardening | microseconds | QUIC Retry (address validation before work), per-IP connection/stream/memory caps, per-IP rate limiting | exists (off-chain design) |
| Minimum fee floor | instant, per tx | `min_gas` makes every transaction cost something | exists |
| Dynamic fee | minutes | prices sustained economic congestion | deferred |

Because layers 1 and 2 absorb volumetric floods, launch does not need the dynamic fee.

---

## 6. Reward distribution

This finishes the existing `TODO: credit to validator's reward_coin` in `epoch.go`, and refines the reward basis the off-chain design assumed.

- **The pool** for an epoch is `epochFees + thermostat issuance`, where `epochFees` is the consumed fees (compute, transit, domain); storage deposits are held against objects, not pooled (section 5.2).
- **The weight is `stake * attestation participation`**, not stake times consensus liveness. The off-chain design distributed by consensus participation (vertices produced), assuming holder load "averages out." The review found that assumption fails against freeloading: producing vertices is near-free (a liveness timer emits one every 500 ms, empty vertices count), while the real work (storing assigned objects and serving attestations) is unmeasured, so a validator can farm full rewards while storing and serving nothing, and with slashing deferred nothing deters it. The fix rewards the real work, using data already committed.
- **Attestation participation is measured from committed signer bitmaps.** Every committed ATX records which holders signed (the bitmap, from the off-chain design). A per-epoch counter `attestations served per validator` is incremented from those bitmaps in the commit path (deterministic, never on receipt), exactly like the existing `epochRoundsProduced`. A holder that stores and serves appears; a freeloader does not. This is the light, organic signal; it also resists the relay attack better than an explicit challenge because the daemon collects attestations in real time under a quorum deadline, so a slow relayer misses the window and the bitmap.
- **The cold-object gap is deferred.** Holders of rarely-transacted objects appear seldom in bitmaps even when faithful. Full coverage needs active storage challenges, which are the relay-resistant proof-of-storage problem (the off-chain design and the whitepaper both flag it as open). Deferred to a dedicated branch with slashing.
- **Crediting.** Each validator's share is credited to a liquid `reward_coin` it designates at registration (a coin ObjectID on `ValidatorInfo`), using the consensus credit primitive `creditCoin` on `d.coinStore` (not `state.creditGasCoin`, which is unexported in the `state` package and unreachable from `consensus`). Rewards are liquid so validators (infrastructure providers with real fiat costs) can realize income; compounding is opt-in by manually bonding from the reward coin.

---

## 7. Supply and bonded tracking

- **`total_supply`** is a maintained protocol counter (a dedicated storage key): increased by mint and issuance, decreased by the deletion burn and future slashing. It cannot be derived cheaply, so it is maintained incrementally in the commit path, persisted, and included in snapshots. This makes the burn real and feeds the thermostat denominator.
- **`total_bonded`** is derived (sum of `ValidatorInfo.Stake` plus delegated stake over the active set at the epoch boundary, O(validators), the set already snapshotted), not a separate counter.

---

## 8. The pod clock (deterministic time, decoupled from money)

A deterministic on-chain clock is a core primitive for a cloud hosting backends (subscriptions, time-locks, scheduled tasks, expirations). It is kept, but it controls no money.

- **Source: the median-quorum timestamp.** Each committed round gets a canonical timestamp: the median of its committed vertices' timestamps, made monotonic. It is deterministic in value (every node computes the identical value from the same committed set) and approximate in accuracy (bounded by the spread of honest clocks, on the order of seconds); a Byzantine minority cannot move the median outside the honest range. This is the Bitcoin median-time-past / Tendermint BFT-time technique.
- **It is new infrastructure, not free.** Vertices have no timestamp field today; one must be added to `vertex.fbs` and included in the signed/hashed body (a Byzantine producer must not be able to stamp garbage), with a validity bound. This is a consensus-breaking schema change, which is acceptable because there is no live mainnet to break; it must land before any consumer.
- **Exposed to pods** as a deterministic execution-context input (like `block.timestamp`), identical across all holders, with documented caveats: coarse (do not use for sub-second logic), manipulable within the honest range (do not use as randomness or for value-bearing sub-second logic).
- **It does not touch issuance.** Inflation is per-epoch (section 3), so the collective "over-state time to mint more" bias does not exist here: the clock controls no money. A minority cannot move the median, and for a dev clock that bound is sufficient (the same trust model as `block.timestamp`).
- **The oracle later upgrades it.** When the planned data oracle exists, anchoring the clock to it periodically gives a non-falsifiable wall-clock for everyone, and at that point issuance can be safely re-denominated in real time to recover a clean per-year inflation bound. No VDF.

---

## 9. Sponsored transactions

A new user with a zero balance cannot make a first transaction (every user tx requires a funded gas coin owned by the sender, section 11). Out-of-band funding (a transfer from an existing holder) covers launch, but the proper fix, and the resolution of the "relayer or sponsor tier" the off-chain design left open, is native sponsored transactions, done the Sui way (a protocol-level fee payer), not the Base way (a layered ERC-4337 / meta-transaction workaround that exists only because Ethereum cannot change its base layer; BluePods controls its own transaction format and has no such constraint).

- **Two signatures.** The sender signs the transaction content (authorizing the action); the sponsor signs to authorize paying gas from its coin (the consent that stops anyone from naming a victim as payer). Both are required.
- **Structure.** A `fee_payer` field names the sponsor; the `gas_coin` is owned by the sponsor; the protocol checks `gas_coin.owner == fee_payer`, both signatures valid, then deducts the fee from the sponsor's coin. This extends the existing fee-deduction check (`owner == sender` becomes `owner == fee_payer`) without changing the rest of the path.
- **The sponsor signs a specific transaction** (specific content and `max_gas`), so it commits to exactly what it pays for, not a blank check. A dApp sponsor applies its own policy (sponsor txs that call my pod, up to X gas) off-chain.
- **Coordination is off-chain.** The sender signs the content locally and hands it to the sponsor over any off-chain channel; the sponsor signs the gas and submits. The chain sees only the final doubly-signed transaction and verifies it. This matches the off-chain aggregation philosophy (coordinate client-side, submit the finished artifact). For a sponsored transaction that also needs holder attestation, the submitter's daemon (the sponsor) collects the attestation last, just before submitting, so the sender-sponsor handoff does not widen the version-race window.

This is a transaction-model change (who pays), adjacent to but distinct from monetary policy; it is in scope for this chantier.

---

## 10. Slashing (deferred)

In line with newer L1s (Sui, Aptos do not slash principal; Solana has no automatic slashing), this chantier ships without principal slashing.

- **Now:** reward-withholding (no attestation participation, no reward) and removal/jail. No principal slash.
- **Stake is slashing-ready:** the `Stake` field is reducible and `total_supply` is decrementable, so slashing plugs in cleanly later. Slashed stake is burned (reduces supply).
- **The dispute / fraud-proof / relay-resistant storage-challenge system** is a dedicated future branch (an open problem). Unlike the no-slash chains, BluePods shards execution, so a Byzantine holder has real local authority over its shard; the deterrent against that genuinely requires this system. Deferred, not abandoned; in the interim the sharded layer has capability-based security (honest majority per object) plus the reward-withholding incentive, not yet economic punishment.

---

## 11. Genesis and fee integrity

A transaction without a gas coin currently skips fees (`commit.go`), intended for genesis but exploitable by any user. The fix eliminates the fee-less path at the root.

- **Genesis is initial state, not transactions.** The initial coin allocation, the founding validator set, and their initial bonded stake are the ledger's starting state, set at chain creation, not transactions run through the commit path. This is a real bootstrap rewrite (today the genesis mint and `register_validator` are real transactions that execute pods), not a relabel: the initial state must replicate their effects (create the coin objects and the validator set, seed stake) directly.
- **Every user transaction requires a funded gas coin** (or a sponsor, section 9) or it is rejected. The exemption keyed on a missing gas coin is removed.
- **The faucet** (a dev/testnet tool) becomes a transfer from a genesis-allocated reserve (a normal fee-paying transaction), not a fee-less mint. Production has no faucet reserve; the real initial distribution is genesis state.
- **Protocol actions** (inflation minting, reward crediting, fee deduction, future slashing) are consensus-driven state mutations, not transactions, so they require no gas coin and are exempt by nature. The invariant: user transactions always pay; the protocol mutates state directly.

---

## Parameters (starting values, calibration placeholders)

| Parameter | Starting value | Role |
|---|---|---|
| Staking-ratio target | calibrate (reachable with delegation) | thermostat set point |
| Inflation floor / ceiling | ~1% / ~20% annual (enforced per-epoch, approximate until oracle) | thermostat bounds |
| Genesis inflation | ~8 to 10% annual | the bootstrap lever |
| Inflation per-epoch step cap | small | anti-lurch limit |
| Scarcity fee burn | 0 | removed for stability |
| Deletion burn | 5% | anti-spam on create/delete churn |
| gas_price | fixed, governed | flat fee for launch (dynamic deferred) |
| Minimum stake | placeholder | registration bar, slashable skin in the game |
| Unbonding period | short, parameterized | tied to future slashing latency |

---

## Out of scope (deferred, with triggers)

- **Liquid staking** (transferable staked-position derivative). Trigger: demand for stake liquidity from utility-token holders. Basic delegation ships now.
- **Dynamic fee** (EIP-1559 / Base style). Trigger: live traffic to calibrate, decided by simulation; also wants the oracle-grade clock.
- **Relay-resistant storage challenges, slashing, and fraud proofs** (the dispute system). Trigger: a dedicated branch; structurally required because execution is sharded.
- **Per-validator voting-power cap.** Trigger: meaningful honest concentration, which matters once delegation is mature.
- **Cycles / USD-peg payment model and the data oracle.** Trigger: the planned oracle. This is the real path to price stability, to sponsored gas at scale (reverse-gas), and to re-denominating inflation in real time for a clean per-year bound. Building sponsored/reverse gas at that scale also needs the fee-payer concept added in section 9.

---

## Rejected alternatives

| Decision | Rejected | Why |
|---|---|---|
| Token identity | hybrid utility + value | a cloud wants a stable medium of payment, not a speculative asset |
| Issuance | fixed calendar | forces guessing the bootstrap duration and trajectory |
| Issuance | fee-indexed loop | fragile feedback, manipulable sensor |
| Issuance timing | clock-denominated (median or VDF) | clock-on-money invites collective bias; VDF is hardware-dependent and unfit for a leaderless DAG |
| Scarcity | a fee burn | scarcity is appreciation pressure, against the stability goal |
| Consensus weight | stake-age (coin-age) | attackable, rewards inactivity, entrenches early holders |
| Reward weight | consensus liveness only | freeloading: pays the cheap work (vertices), ignores the real work (storage/attestation) |
| Sponsored tx | Base-style ERC-4337 layer | a workaround for Ethereum's frozen base layer; BluePods adds a native fee payer instead |
| Onboarding | defer all sponsorship to the cycles model | native sponsored tx is lighter and standalone, no oracle needed |
| Fee-less transactions | genesis-authority exemption | eliminate the path at the root (genesis as state) instead |
| Stake representation | an `is_staked` flag on coins | record stake on `ValidatorInfo` instead |
| Storage payment | per-object storage fund | rendezvous uniformity makes the global pool equivalent |

---

## Doc impact

These documents of record contradict this design once it lands and must be updated in place (per the doc conventions):

- **WHITEPAPER section 9 (Fees):** the 70/30 split, the burn-as-anti-gaming argument, and the distribution-by-consensus-participation prose are superseded (100/0, no scarcity burn, reward by attestation participation).
- **WHITEPAPER section 10 / 5 (Validators, Consensus):** stake-weighted quorum replaces the equal-weight "2n/3+1 distinct validators" framing and the "stake is equal (1) for now" note; the safety argument now rests on stake honesty. The genesis-epoch story changes (genesis as state, founding validators not registered via transactions).
- **`types/vertex.fbs`:** `FeeSummary.total_burned` becomes vestigial (zero) and its comments are wrong; a new signed `Vertex.timestamp` field is added.
- **The off-chain aggregation spec** set the 70/30 split and left the burn and the relayer/sponsor tier to this study; note it as superseded/resolved there or rely on this spec as the newer record.

---

## Relationship to the off-chain aggregation design

This chantier is consistent with and builds on the off-chain aggregation design:

- It is the dedicated tokenomics study that design deferred; it resolves that design's open relayer/sponsor tier (section 9) and its open burn question (section 4).
- It reuses that design's machinery: the committed signer bitmaps feed attestation-based rewards (section 6); the global-pool/no-storage-fund choice is the same reasoning (section 5.2); the holder snapshot and commit-epoch selection are reused for stake-weighted quorums (section 1).
- It supersedes that design's 70/30 split (now 100/0) and refines its reward basis (consensus participation to attestation participation).
- It keeps that design's deferrals: slashing, fraud proofs, and storage challenges remain unbuilt (section 10).

---

## Implementation decomposition

Ordered plans, each building on the prior:

1. Genesis as initial state; require a gas coin (or sponsor) on every user transaction (close the fee-less hole).
2. `total_supply` counter (make the burn real); remove the scarcity fee burn (100/0).
3. Stake on `ValidatorInfo` (with the accessor/snapshot/schema surface), minimum stake, unbonding, `total_bonded` derivation.
4. Delegation (delegators, reward sharing, shared-slashing hooks).
5. Stake-weighted consensus quorum (all counting sites, epoch-pinned).
6. The thermostat (per-epoch issuance) at the epoch boundary.
7. Reward crediting to the liquid `reward_coin`, weighted by stake and attestation participation (the new attestation-served counter).
8. Sponsored transactions (`fee_payer` + dual signature).
9. The pod clock (new signed `Vertex.timestamp`, median, pod exposure), decoupled from money.

The whitepaper, VISION, and `vertex.fbs` are updated to match (see Doc impact) once the implementation lands.
