# BluePods — Acceptance Test Plan (ATP)

Exhaustive list of tests covering all features, security mechanisms, edge cases and attack vectors.
Source of truth: the code and `WHITEPAPER.md`.

---

## 1. Transaction Validation (ingress layer)

`internal/validation/validate.go`, `cmd/node/clienthandlers.go`

The node is QUIC only. A client submits a raw transaction or a full attested
transaction over the `MsgSubmitTx` message, and the operational queries (status,
validators, object, domain, health, faucet) are QUIC messages on the same
listener. There is no HTTP. A submission the node accepts maps to the former 202;
one it rejects at ingress maps to the former 400.

| # | Test | Detail |
|---|------|--------|
| 1.1 | Field sizes — hash must be 32 bytes | Reject if hash != 32B |
| 1.2 | Field sizes — sender must be 32 bytes | Reject if sender != 32B |
| 1.3 | Field sizes — signature must be 64 bytes | Reject if sig != 64B |
| 1.4 | Field sizes — pod must be 32 bytes | Reject if pod != 32B |
| 1.5 | Field sizes — function_name must be non-empty | Reject empty string |
| 1.6 | Field sizes — gas_coin must be 0 or 32 bytes | Reject other sizes (e.g. 16B) |
| 1.7 | Hash recomputation — blake3 of unsigned tx | Reject on mismatch |
| 1.8 | Hash recomputation — canonical field order | sender, pod, func, args, cor, max_create_domains, max_gas, gas_coin, mutable_refs, read_refs |
| 1.9 | Ed25519 signature verification | Reject invalid sig |
| 1.10 | Ed25519 — wrong sender key | Valid sig but wrong pubkey |
| 1.11 | Object refs — max 40 total (8 standard + 32 singletons) | Reject 41+ refs |
| 1.12 | Object refs — duplicate object ID in read_refs | Reject |
| 1.13 | Object refs — duplicate object ID in mutable_refs | Reject |
| 1.14 | Object refs — same ID in read_refs AND mutable_refs | Reject (cross-list duplicate) |
| 1.15 | Object refs — duplicate domain name | Reject |
| 1.16 | Object refs — invalid ID size (not 32 bytes) | Reject |
| 1.17 | Object refs — domain ref with no ID (valid) | Accept, skip ID check |
| 1.18 | FlatBuffers panic recovery — malformed data | defer/recover catches panic |
| 1.19 | Transaction data too short (< 8 bytes) | Reject |
| 1.20 | submit-tx — body over 1MB | Reject |
| 1.21 | submit-tx — empty body | Reject |
| 1.22 | submit-tx — valid tx accepted, returns hash | Success path |
| 1.23 | health message | Reports ok |
| 1.24 | status message — fields present | round, lastCommitted, validators, epoch, epochHolders, systemPod |
| 1.25 | validators message | Per validator: pubkey, BLS key, QUIC address; plus current epoch |
| 1.26 | get-object — found | Returns object fields |
| 1.28 | get-object — not found | Returns not-found |
| 1.29 | get-object — local-only flag | Skip remote routing, answer from local state only |
| 1.30 | domain-resolve — found | Returns object ID |
| 1.31 | domain-resolve — not found | Returns not-found |
| 1.32 | faucet — valid | Accepted, returns hash and coinID |
| 1.33 | faucet — amount=0 | Reject |
| 1.34 | faucet — invalid pubkey | Reject |
| 1.36 | status — systemPod field | BLAKE3 of the system pod WASM |

---

## 2. Consensus DAG

`internal/consensus/dag.go`, `internal/consensus/validate.go`

| # | Test | Detail |
|---|------|--------|
| 2.1 | Vertex from unknown producer rejected | validateProducer checks ValidatorSet |
| 2.2 | Vertex with invalid Ed25519 signature rejected | validateSignature |
| 2.3 | Vertex with wrong epoch rejected | validateEpoch |
| 2.4 | Vertex with missing parents rejected | validateParents |
| 2.5 | Vertex with parent from wrong round rejected | Parent must be round-1 |
| 2.6 | Vertex with insufficient parent quorum rejected | Need 67% unique validator parents |
| 2.7 | Duplicate vertex rejected (same hash) | store.has() check |
| 2.8 | Round progression — quorum advances round | N+2 quorum rule |
| 2.9 | Commit order — sequential rounds, no skipping | Stops at first uncommitted round |
| 2.10 | Pending vertex buffer — out-of-order arrival | Buffer then retry when parents arrive |
| 2.11 | Listener mode — no vertex production | listenerMode=true disables production |
| 2.12 | Bootstrap mode — produces alone before minValidators | isBootstrap=true, quorum=1 |
| 2.13 | Transition grace period — quorum=1 for 20 rounds after minValidators | transitionGraceRounds=20 |
| 2.14 | Transition buffer — 10 extra rounds after grace | transitionBufferRounds=10 |
| 2.15 | Full quorum achieved flag — set after first BFT quorum post-buffer | fullQuorumAchieved atomic bool |
| 2.16 | Sync mode — only reference trusted producers after snapshot | syncModeActive + trustedProducers |
| 2.17 | Sync mode disabled after first vertex production | disableSyncMode() |
| 2.18 | Sequential production — nextProductionRound prevents gaps | lastProducedRound + 1 |
| 2.19 | Liveness loop — produces vertex every 500ms when idle | livenessTimeout ticker |
| 2.20 | Vertex gossip — production fanout=40, relay fanout=10 | defaultGossipFanout=40 (dag.go), relayVertex uses Gossip(data, 10) (handlers.go) |

---

## 3. Version Tracking & Conflicts

`internal/consensus/tracker.go`

| # | Test | Detail |
|---|------|--------|
| 3.1 | Version match — tx passes check | read + mutable versions all match tracker |
| 3.2 | Version mismatch on read_refs — tx rejected | One stale read ref |
| 3.3 | Version mismatch on mutable_refs — tx rejected | One stale mutable ref |
| 3.4 | Sequential increments — v0 → v1 → v2 → ... | Multiple successful txs on same object |
| 3.5 | Two txs modify same object in same vertex | Second tx sees version conflict |
| 3.6 | Two txs read same object in same vertex | Both pass (reads don't increment) |
| 3.7 | Tx A reads obj, tx B modifies obj (same vertex, A before B) | A passes, B passes (A saw old version) |
| 3.8 | Tx A modifies obj, tx B reads obj (same vertex, A before B) | A passes, B fails (B expects old version, tracker has new) |
| 3.9 | Malformed standard ref (ID != 32 bytes, no domain) | checkRefList returns false |
| 3.10 | Domain ref in mutable_refs — skipped by tracker | No version check for domain refs |
| 3.11 | Untracked object (version=0) — tx with version=0 passes | Go zero-value behavior |
| 3.12 | incrementMutableObjects preserves replication and fees | Only version changes |
| 3.13 | Tracker persistence — Pebble roundtrip | Export then Import, values match |
| 3.14 | Tracker key format — "t:" + 32-byte objectID | 34 bytes total |
| 3.15 | Tracker value format — 18 bytes (version:8 + replication:2 + fees:8) | Encoding/decoding |

---

## 4. Fee System

`internal/consensus/fees.go`

| # | Test | Detail |
|---|------|--------|
| 4.1 | CalculateFee — gas component: max_gas * gas_price * rep_ratio | Basic computation |
| 4.2 | CalculateFee — transit component: nb_standard_objects * transit_fee | Singletons excluded |
| 4.3 | CalculateFee — storage component: sum(effRep/totalValidators * storage_fee) | Per created object |
| 4.4 | CalculateFee — domain component: max_create_domains * domain_fee | Per domain |
| 4.5 | CalculateFee — all four components summed | Total = gas + transit + storage + domain |
| 4.6 | SplitFee — 70% epoch, 30% burned | Basis points: 7000/3000, no aggregator share |
| 4.7 | SplitFee — epoch is the remainder | epoch = total - burned |
| 4.8 | SplitFee — invariant: burned + epoch == total | EpochBPS + BurnBPS == 10000 |
| 4.9 | ReplicationRatio — creating objects → 1/1 | All validators execute |
| 4.10 | ReplicationRatio — creating domains → 1/1 | All validators execute |
| 4.11 | ReplicationRatio — mutable singleton → 1/1 | Singleton = all validators |
| 4.12 | ReplicationRatio — no mutable refs → 0/1 | Read-only tx |
| 4.13 | ReplicationRatio — standard mutable refs | Union of holders / totalValidators |
| 4.14 | ReplicationRatio — 0 validators → 0/1 | Division by zero protection |
| 4.15 | effectiveRep — replication=0 → totalValidators | Singleton normalization |
| 4.16 | effectiveRep — replication>0 → replication | Standard behavior |
| 4.17 | CountStandardObjects — excludes singletons (replication=0) | Only count rep>0 |
| 4.18 | safeMul — overflow caps at MaxUint64 | bits.Mul64 hi check |
| 4.19 | safeAdd — overflow caps at MaxUint64 | sum < a check |
| 4.20 | StorageDeposit — singleton uses totalValidators as effRep | effectiveRep(0, N) = N |
| 4.21 | StorageDeposit — 0 validators returns 0 | Division by zero protection |
| 4.22 | StorageRefund — 95% refund, 5% burned | storageRefundBPS=9500 |
| 4.23 | DefaultFeeParams values | GasPrice=1, MinGas=100, Transit=10, Storage=1000, Domain=10000 |

---

## 5. Fee Deduction Flow

`internal/consensus/commit.go`

| # | Test | Detail |
|---|------|--------|
| 5.1 | Fee deduction — sufficient balance → full deduction | balance >= fee, proceed=true |
| 5.2 | Fee deduction — insufficient balance → partial deduction, tx rejected | Take all balance, proceed=false |
| 5.3 | Fee deduction — no gas_coin (len != 32) → skip fees | Bootstrap/genesis tx |
| 5.4 | Fee deduction — feeParams=nil → skip fees | Fees disabled |
| 5.5 | Fee deduction — coinStore=nil → skip fees | No coin store |
| 5.6 | Fee deduction — fee=0 → proceed without deduction | No-op |
| 5.7 | min_gas check — max_gas < MinGas → reject | Anti-spam check |
| 5.8 | min_gas check — max_gas = MinGas → accept | Boundary |
| 5.9 | min_gas check — max_gas > MinGas → accept | Normal case |

---

## 6. Gas Coin Validation

`internal/consensus/commit.go`, `internal/consensus/coins.go`

| # | Test | Detail |
|---|------|--------|
| 6.1 | Gas coin not found → reject | coinStore.GetObject returns nil |
| 6.2 | Gas coin not a singleton (replication != 0) → reject | Must be replication=0 |
| 6.3 | Gas coin owner != sender → reject | Ownership mismatch |
| 6.4 | Gas coin valid → proceed | All checks pass |
| 6.5 | readCoinBalance — first 8 bytes LE | Parse uint64 |
| 6.6 | readCoinBalance — content too short (< 8 bytes) | Error |
| 6.7 | readCoinOwner — extracts 32-byte owner | From Object.OwnerBytes() |
| 6.8 | writeCoinBalance — preserves all fields except content[0:8] | Version NOT incremented |
| 6.9 | deductCoinFee — balance >= fee → deducted=fee, fullyCovered=true | Normal deduction |
| 6.10 | deductCoinFee — balance < fee → deducted=balance, fullyCovered=false | Partial deduction |
| 6.11 | creditCoin — overflow check (balance + amount wraps) → error | Prevents uint64 overflow |
| 6.12 | creditCoin — amount=0 → no-op | Early return |

---

## 7. Vertex Fee Summary Validation

`internal/consensus/validate.go`

| # | Test | Detail |
|---|------|--------|
| 7.1 | Fee summary matches recalculation — all 3 fields | total_fees, total_burned, total_epoch |
| 7.2 | Fee summary mismatch — total_fees wrong | Reject vertex |
| 7.4 | Fee summary mismatch — total_burned wrong | Reject vertex |
| 7.5 | Fee summary mismatch — total_epoch wrong | Reject vertex |
| 7.6 | No fee summary + 0 transactions → accept | No summary needed |
| 7.7 | No fee summary + >0 transactions → reject | Missing fee_summary |
| 7.8 | feeParams=nil → skip validation entirely | Fees disabled |
| 7.9 | Tx without gas_coin skipped in fee summary recalc | len(gas_coin) != 32 |

---

## 8. Execution Sharding

`internal/consensus/commit.go`

| # | Test | Detail |
|---|------|--------|
| 8.1 | shouldExecute — holder of mutable object → execute | isHolder returns true |
| 8.2 | shouldExecute — not holder of any mutable → skip | isHolder returns false for all |
| 8.3 | shouldExecute — creating objects (CreatedObjectsReplicationLength > 0) → ALL execute | Bypasses shouldExecute |
| 8.4 | shouldExecute — creating domains (MaxCreateDomains > 0) → ALL execute | Bypasses shouldExecute |
| 8.5 | shouldExecute — mutable singleton (replication=0 in ATX) → execute | Default missing=0 in replicationMap |
| 8.6 | shouldExecute — no mutable refs → execute | tx.MutableRefsLength() == 0 returns true |
| 8.7 | shouldExecute — isHolder=nil → always execute | Fallback |
| 8.8 | Singleton in read_refs only, no mutable singletons → NOT all validators execute | Only holders of mutable refs execute |
| 8.9 | buildReplicationMap from ATX objects | objectID → replication mapping |

---

## 9. Epoch System

`internal/consensus/epoch.go`

| # | Test | Detail |
|---|------|--------|
| 9.1 | isEpochBoundary — round % epochLength == 0 | True at boundaries |
| 9.2 | isEpochBoundary — round=0 → false | Never epoch at genesis |
| 9.3 | isEpochBoundary — epochLength=0 → false | Epochs disabled |
| 9.4 | transitionEpoch — epoch counter increments | currentEpoch++ |
| 9.5 | transitionEpoch — rewards distributed BEFORE removals | Outgoing validators get share |
| 9.6 | transitionEpoch — pending removals applied | Validators removed from set |
| 9.7 | transitionEpoch — epochHolders snapshot taken | Frozen copy of validator set |
| 9.8 | transitionEpoch — epoch state cleared | epochFees=0, epochRoundsProduced reset, epochAdditions=nil |
| 9.9 | transitionEpoch — callback fired | onEpochTransition called with new epoch |
| 9.10 | Churn limiting — removals sorted by pubkey | Deterministic across validators |
| 9.11 | Churn limiting — excess removals deferred | maxChurnPerEpoch cap |
| 9.12 | Churn limiting — additions sorted and capped | sortedAdditions with limit |
| 9.13 | Churn limiting — unlimited (maxChurnPerEpoch=0) | All changes applied |
| 9.14 | Reward distribution — proportional to rounds_produced | share = rewardTotal * rounds / totalWeight |
| 9.15 | Reward distribution — 0 fees → no distribution | Early return |
| 9.16 | Reward distribution — 0 totalRounds → no distribution | Early return |
| 9.17 | Register validator — added to ValidatorSet + epochAdditions | isNew tracked |
| 9.18 | Deregister validator — added to pendingRemovals | Active until next epoch |
| 9.19 | InitEpochHolders — called at startup | Sets initial epochHolders |

---

## 10. State Management

`internal/state/state.go`

| # | Test | Detail |
|---|------|--------|
| 10.1 | Execute — full pipeline: extract → resolve → serialize → execute → process | Happy path |
| 10.2 | validateOutput — created objects > max → reject | createdCount > CreatedObjectsReplicationLength |
| 10.3 | validateOutput — registered domains > max → reject | domainCount > MaxCreateDomains |
| 10.4 | validateOutput — empty domain name → reject | len(name) == 0 |
| 10.5 | validateOutput — domain name > 253 bytes → reject | Max DNS length |
| 10.6 | validateOutput — duplicate domain in output → reject | seen map |
| 10.7 | validateOutput — domain collision (already exists) → reject | domains.exists() |
| 10.8 | Rollback semantics — validation before mutations | If validation fails, no state changes |
| 10.9 | applyCreatedObjects — deterministic ID: blake3(txHash \|\| index_u32_LE) | computeObjectID |
| 10.10 | applyCreatedObjects — storage deposit set in fees field | computeStorageDeposit overrides pod value |
| 10.11 | applyCreatedObjects — isHolder sharding | Only stores objects this node holds |
| 10.12 | applyCreatedObjects — onObjectCreated callback fired for ALL objects | Even non-held objects tracked |
| 10.13 | applyUpdatedObjects — version incremented (version+1) | rebuildObjectIncrementVersion |
| 10.14 | applyDeletedObjects — ownership check (owner == sender) | Non-owner deletion blocked |
| 10.15 | applyDeletedObjects — refund 95% of fees to gas_coin | storageRefundBPS=9500 |
| 10.16 | applyDeletedObjects — object not in local storage → skip | Sharding: may not have it |
| 10.17 | ensureMutableVersions — bumps version even if pod didn't return object | Gap closer between tracker and state |
| 10.18 | resolveMutableObjects — loads singletons from local storage | Not in ATX body |
| 10.19 | Pod execution error (errCode != 0) → error returned | Pod-level failure |
| 10.20 | Gas limit hardcoded at 10,000,000 | defaultGasLimit constant |

---

## 11. Domain Registry

`internal/state/domain.go`

| # | Test | Detail |
|---|------|--------|
| 11.1 | Set and get domain | name → objectID mapping |
| 11.2 | Domain exists check | Returns true/false |
| 11.3 | Domain delete | Removes mapping |
| 11.4 | Domain overwrite | New objectID replaces old |
| 11.5 | Domain export/import | Snapshot roundtrip |
| 11.6 | resolveDomainObjectID — prefers object_id over object_index | FlatBuffers default=0 issue |
| 11.7 | Pebble key prefix "d:" | Storage isolation |

---

## 12. Off-chain aggregation & BLS

Aggregation is off-chain. The client daemon (`daemon/`) fetches each replicated
object, computes its holders by rendezvous hashing (`internal/attest`), requests
attestations, aggregates them, and submits the attested transaction. A node
holder (`internal/aggregation/handler.go`) serves the deterministic signature it
stored at execution and answers a negative request with a static error and no
signature. There is no node-side collector, no designated top-1 holder, and no
`WantFull` flag.

| # | Test | Detail |
|---|------|--------|
| 12.1 | BLS sign and verify | Basic roundtrip |
| 12.2 | BLS wrong message → verify fails | Signature mismatch |
| 12.3 | BLS wrong key → verify fails | Key mismatch |
| 12.4 | BLS deterministic key derivation from Ed25519 seed | blake3("bluepods-bls-keygen" \|\| seed) |
| 12.5 | BLS aggregation — N signers | AggregateSignatures + VerifyAggregated |
| 12.6 | BLS subset verification (3 of 5) | Partial signer set |
| 12.7 | BLS empty signature → reject | No signers |
| 12.8 | BLS invalid input sizes → reject | Wrong sig/pubkey lengths |
| 12.9 | Signer bitmap — build and parse | Bit i = validator i signed |
| 12.10 | Signer bitmap — out-of-range index | Bounds check |
| 12.11 | attest.QuorumSize — (replication * 67 + 99) / 100 | 67% per-object threshold |
| 12.12 | Daemon — singleton (replication=0) excluded from collection | Never attested, submitted raw |
| 12.13 | Daemon — fetch object, then rendezvous holders, then request attestations | Any holder serves the object |
| 12.14 | Daemon — quorum reached → early exit | Enough positive attestations |
| 12.15 | Daemon — fail-fast impossible quorum → typed error | Too many negatives |
| 12.16 | Daemon — version race → bounded randomized backoff and resync | Eventual success or ErrQuorumImpossible |
| 12.17 | Daemon — local verification: every signature on the same H, H matches the fetched object | Reject otherwise |
| 12.18 | ATX build — singletons excluded from body and proofs | Only replicated objects |
| 12.19 | Handler — held object at current version → stored signature | Served from the objsig store |
| 12.20 | Handler — not held → negative, no signature | Static error |
| 12.21 | Handler — non-current version → negative, no signature | Static error |
| 12.22 | Rendezvous hashing — deterministic holders | blake3(objectID \|\| pubkey) scoring |
| 12.23 | Rendezvous hashing — stability on validator change | Minimal reshuffling |
| 12.24 | Rendezvous hashing — different objects get different holders | Distribution check |
| 12.25 | Eager signing — holder signs at execution and stores the current-version signature | objsig store, evicted on version advance |
| 12.26 | Bounded fallback — store miss for a held current version → sign and store once | Never for unheld or non-current versions |
| 12.27 | Snapshot warmth — a node that applies a snapshot can serve attestations without signing | Stored signatures travel in snapshots |
| 12.28 | Attestation hash — H = BLAKE3(content \|\| version_u64_BE) over content bytes | Signer and verifier agree |

---

## 13. Snapshot & Sync

`internal/sync/snapshot.go`

| # | Test | Detail |
|---|------|--------|
| 13.1 | Snapshot creation — all components included | Objects, validators, vertices, tracker, domains |
| 13.2 | Snapshot checksum — blake3 over canonical sorted data | Deterministic |
| 13.3 | Snapshot checksum verification — valid | Computed matches stored |
| 13.4 | Snapshot checksum verification — corrupted → reject | Mismatch detected |
| 13.5 | Snapshot version = 4 | snapshotVersion constant |
| 13.6 | Snapshot roundtrip — create then apply | All data preserved |
| 13.7 | zstd compression/decompression | Roundtrip |
| 13.8 | Validator encoding — pubkey + u16 quic + 48B BLS | Format (no HTTP address) |
| 13.10 | Snapshot carries objsig signatures (format version 6) | New holder is warm after sync |
| 13.9 | Validator decoding — truncated data | Graceful stop |
| 13.10 | Object sorting by ID for determinism | sortObjects |
| 13.11 | Consensus keys excluded from objects | v:, r:, m:, t:, d: prefixes skipped |
| 13.12 | Tracker entries in checksum | Replication affects checksum |
| 13.13 | Domain entries in checksum | Domain data affects checksum |
| 13.14 | ApplySnapshot — atomic write via SetBatch | All objects written together |

---

## 14. Network & Gossip

`internal/network/`

| # | Test | Detail |
|---|------|--------|
| 14.1 | Node start and stop | Lifecycle |
| 14.2 | Two-node connection | QUIC handshake |
| 14.3 | Direct P2P messaging | SendMessage |
| 14.4 | Broadcast to all peers | Broadcast |
| 14.5 | Disconnect handling | Clean disconnect |
| 14.6 | Reconnection with address updates | Auto-reconnect |
| 14.7 | Large message (16MB) | maxMessageSize = 16<<20 in protocol.go |
| 14.8 | 100 concurrent messages | Thread safety |
| 14.9 | Dedup — basic | Same message ID ignored |
| 14.10 | Dedup — concurrent | Thread-safe dedup |
| 14.11 | Dedup — TTL expiry | Old entries cleaned |
| 14.12 | Gossip — subset selection (fanout < peers) | Random N of M peers |
| 14.13 | Gossip — fallback to broadcast (fanout >= peers) | All peers |
| 14.14 | Request-response protocol | Bidirectional |
| 14.15 | Request timeout | Deadline exceeded |

---

## 15. Client Operations

`client/client.go`

| # | Test | Detail |
|---|------|--------|
| 15.1 | Faucet request | Mint coins |
| 15.2 | Split operation | One coin → two |
| 15.3 | Transfer operation | Change owner |
| 15.4 | CreateObject | New object with content |
| 15.5 | TransferObject | Change object owner |
| 15.6 | DeregisterValidator | System pod call |
| 15.7 | ObjectRefData — ID mode vs domain mode | Dual-mode references |
| 15.8 | Wallet coin tracking | Balance management |

---

## 16. Pod VM

`internal/podvm/`

| # | Test | Detail |
|---|------|--------|
| 16.1 | Mint — success | Creates coin object |
| 16.2 | Mint — zero amount | Accepted (edge case) |
| 16.3 | Mint — large amount (1 trillion) | No overflow |
| 16.4 | Mint — invalid args | Rejected |
| 16.5 | Transfer — success | Ownership change |
| 16.6 | Transfer — missing coin | Error |
| 16.7 | Split — success | Balance divided |
| 16.8 | Split — exact balance | Entire balance moved |
| 16.9 | Split — insufficient balance | Error |
| 16.10 | Merge — success | Balances combined |
| 16.11 | Merge — two coins | Minimum merge |
| 16.12 | Merge — single coin → reject | Need >= 2 |
| 16.13 | Register validator — creates singleton | replication=0 |
| 16.14 | Unknown function → error | Invalid function name |
| 16.15 | Gas exhaustion — hostGas panics "gas exhausted" | Limit enforcement |
| 16.16 | Gas limit hardcoded 10M (NOT tx.MaxGas) | defaultGasLimit = 10,000,000 |
| 16.17 | Module not found → error | Unknown pod ID |

---

## 17. Creation Limits Enforcement

| # | Test | Detail |
|---|------|--------|
| 17.1 | Pod creates exactly max objects → accept | createdCount == maxCreate |
| 17.2 | Pod creates fewer than max objects → accept | createdCount < maxCreate |
| 17.3 | Pod creates more than max objects → reject | createdCount > maxCreate |
| 17.4 | Pod registers exactly max domains → accept | domainCount == maxDomains |
| 17.5 | Pod registers more than max domains → reject | domainCount > maxDomains |
| 17.6 | max_create_domains > 0 forces ALL validators to execute | Sharding bypass |
| 17.7 | created_objects_replication > 0 forces ALL validators to execute | Sharding bypass |
| 17.8 | Replication of created objects from tx.CreatedObjectsReplication vector | Per-object replication |

---

## 18. Gas Metering

| # | Test | Detail |
|---|------|--------|
| 18.1 | Tx consuming exactly max_gas → succeeds | Boundary |
| 18.2 | Tx consuming max_gas + 1 → gas exhausted panic | Over limit |
| 18.3 | Gas limit = 10M regardless of tx.MaxGas | Hardcoded defaultGasLimit |
| 18.4 | Gas host function called with cost=0 | No-op? |
| 18.5 | Gas metering via WASM instrumentation | env.gas(cost) host function |

---

## 19. Version Conflict Scenarios

| # | Test | Detail |
|---|------|--------|
| 19.1 | Two txs in same vertex, both modify object A | Second gets version conflict |
| 19.2 | Two txs in same vertex, tx1 reads A + tx2 modifies A | Depends on order: if read first → OK, if modify first → read fails |
| 19.3 | Tx reads object A at version 5, but A was just modified to v6 by prior tx | Read fails |
| 19.4 | Tx modifies A (v5→v6), next tx modifies A (expects v6) | Second tx passes (sees new version) |
| 19.5 | Object not yet tracked (new object just created) | Version=0, first tx at v0 passes |
| 19.6 | Multiple objects in same tx — one conflicts, one doesn't | Entire tx rejected (all-or-nothing check) |
| 19.7 | Declared version matches tracker, but ATX content is from older version | Version match but stale content (Scenario D) |

---

## 20. Fee Edge Cases

| # | Test | Detail |
|---|------|--------|
| 20.1 | Tx with 0 standard objects → transit fee = 0 | No transit cost |
| 20.2 | Tx with only singletons → transit fee = 0 | Singletons not counted |
| 20.3 | Tx creating singleton (rep=0) → storage deposit = storageFee * 1 | effRep = totalValidators |
| 20.4 | Tx with gas_coin balance = fee exactly → fullyCovered=true, balance=0 | Boundary |
| 20.5 | Tx with gas_coin balance = fee - 1 → partial deduction, rejected | Insufficient |
| 20.6 | Multiple fee components overflow → capped at MaxUint64 | safeMul/safeAdd |
| 20.7 | SplitFee with very large total → no overflow in BPS calc | total * 2000 / 10000 |
| 20.8 | Fee summary with mixed gas_coin / no gas_coin txs | Only gas_coin txs counted |
| 20.9 | Delete refund credited to gas_coin without version increment | Implicit protocol modification |
| 20.10 | Delete refund with fees=0 on object → no refund | 0 * 9500 / 10000 = 0 |

---

## 21. Object Types & Behavior

### Standard Objects (replication > 0)

| # | Test | Detail |
|---|------|--------|
| 21.1 | Standard object included in ATX body | Aggregator fetches + BLS proofs |
| 21.2 | Standard object has transit data in memory | Stored in ATX objects vector |
| 21.3 | Standard object stored only by holders | isHolder sharding |
| 21.4 | Standard object has QuorumProof in ATX | BLS aggregate sig + bitmap |
| 21.5 | Standard object holders computed via Rendezvous | blake3(objID \|\| pubkey) |
| 21.6 | Standard object read_ref → no execution trigger | shouldExecute only checks mutable |

### Singleton Objects (replication = 0)

| # | Test | Detail |
|---|------|--------|
| 21.7 | Singleton NOT in ATX body | buildAttestedTransaction skips rep=0 |
| 21.8 | Singleton has NO transit data | No object bytes in ATX |
| 21.9 | Singleton resolved from local storage | resolveMutableObjects |
| 21.10 | Singleton stored by ALL validators | No sharding |
| 21.11 | Singleton in mutable_refs → ALL validators execute | shouldExecute: rep=0 → isHolder returns true |
| 21.12 | Singleton in read_refs only → NOT all validators execute | Read refs don't trigger execution |
| 21.13 | Tx with ONLY singletons → 0 bytes transit data | No objects in ATX |
| 21.14 | Singleton no BLS attestation needed | Collector: immediate return |
| 21.15 | Gas coin must be singleton (replication=0) | validateGasCoin check |

---

## 22. Concurrent Object Modification

| # | Test | Detail |
|---|------|--------|
| 22.1 | Object modified while in transit (being attested) | ATX has version V, but object is now V+1 — version conflict at commit |
| 22.2 | Two clients aggregate and submit txs modifying the same object | Second to commit gets a version conflict |
| 22.3 | Tx A creates object, Tx B (same vertex, later) tries to use it | B doesn't know the ID yet (deterministic from A's hash) |
| 22.4 | Object deleted by Tx A, Tx B (later in vertex) reads it | B has version conflict (tracker deleted entry?) |
| 22.5 | Singleton modified between attestation and commit | Local state may differ — ensureMutableVersions handles gap |

---

## 23. Object Size & Limits

| # | Test | Detail |
|---|------|--------|
| 23.1 | Object content max size | Spec says 4KB but NOT enforced in code |
| 23.2 | Min replication enforcement | Spec says 10 but NOT enforced in code |
| 23.3 | Max 40 object refs per tx | Enforced at API level |
| 23.4 | Replication=0 semantics | Singleton, all validators |
| 23.5 | Very large object content in ATX | Memory pressure |

---

## 24. Security & Attack Vectors

| # | Test | Detail |
|---|------|--------|
| 24.1 | Replay attack — same tx submitted twice | Version conflict on second attempt |
| 24.2 | Hash tampering — modified field after signing | Hash mismatch detected |
| 24.3 | Signature forgery — invalid Ed25519 sig | validateSignature rejects |
| 24.4 | Malicious FlatBuffers — crafted to trigger panic | defer/recover in validateTx |
| 24.5 | Sybil attack — fake validators | Only registered validators accepted |
| 24.6 | Double-spend — same gas_coin in two txs | Version conflict (coin modified) |
| 24.7 | Fee overflow attack — craft max_gas * gas_price to wrap to 0 | safeMul prevents |
| 24.8 | Transit data DoS — tx with 40 large objects | Memory pressure on the client daemon, not the node |
| 24.14 | Attestation flood DoS — client requests attestations without paying | Holder serves a stored signature (a read), signs nothing new; negatives are static |
| 24.15 | Cold-holder DoS — request current versions of many held objects | Eager signing leaves no cold window; bounded sign-on-miss only for held current versions |
| 24.16 | Certless connection flood | Ephemeral client tier: per-IP caps, rate limiting, QUIC Retry source validation |
| 24.17 | Duplicate inclusion — same gossiped tx in several producers' vertices | Commit-once guard by tx hash: first occurrence executes, repeats skipped (no double create/fee/validator change) |
| 24.9 | Pending vertex buffer DoS — unlimited buffering | No cap on pendingVertices map |
| 24.10 | Non-owner deletion attempt | Owner check blocks it |
| 24.11 | Gas coin owned by different sender | Ownership check blocks it |
| 24.12 | Domain squatting — register existing domain | Collision check blocks it |
| 24.13 | creditGasCoin overflow | balance + amount wraps → silently returns (no credit) |

---

## 25. Spec-Code Mismatches (Potential Bugs)

| # | Issue | Detail |
|---|-------|--------|
| 25.1 | maxTxSize: spec=48KB, code=1MB | 20x mismatch |
| 25.2 | Object 4KB limit NOT enforced | No content size check anywhere |
| 25.3 | Min replication=10 NOT enforced | Any uint16 accepted |
| 25.4 | Vertex size unbounded | No limit on tx count or total size |
| 25.5 | Gas limit hardcoded, ignores tx.MaxGas | defaultGasLimit = 10M always |
| 25.6 | Aggregator credits TODO — never distributed | creditAggregator is a no-op |
| 25.7 | tracker.checkRefList — no FlatBuffer panic recovery | Unlike api/validate.go |

---

## 26. BLS Proof Verification

`internal/consensus/commit.go`

| # | Test | Detail |
|---|------|--------|
| 26.1 | ATX with valid BLS proofs → accepted | verifyATXProofs passes |
| 26.2 | ATX with invalid BLS proof → rejected | Proof verification fails |
| 26.3 | ATX with 0 proofs → verification skipped | ProofsLength() == 0 |
| 26.4 | verifyATXProofs=nil → verification skipped | No verifier configured |
| 26.5 | Proof verification failure → tx emitted as failed | emitTransaction(tx, false) |

---

## 27. Register/Deregister Validator Handlers

`internal/consensus/commit.go`

| # | Test | Detail |
|---|------|--------|
| 27.1 | Register validator — no BLS key length validation | Code doesn't verify 48 bytes |
| 27.2 | Register validator — no address format validation | No check on sender format |
| 27.3 | Register validator — duplicate overwrites existing | registerValidator() replaces entry |
| 27.4 | Deregister validator — non-existent validator added to pendingRemovals | No existence check |
| 27.5 | Deregister validator — no minimum validator count | Network can shrink to 0 |
| 27.6 | Register during epoch transition | Race between add and epochHolders snapshot |
| 27.7 | Deregister then re-register before epoch boundary | Validator in pendingRemovals AND validatorSet |
| 27.8 | Register validator — wrong pod ID → not detected as register_validator | Pod must match systemPod |
| 27.9 | Register validator — wrong function name → not detected | Must be "register_validator" exactly |
| 27.10 | Register validator — triggers enterTransition when minValidators reached | Immediate transition |
| 27.11 | Register validator — tracked in epochAdditions for churn | isNew checked |

---

## 28. DAG Edge Cases

| # | Test | Detail |
|---|------|--------|
| 28.1 | Multiple vertices from same producer in same round | Store behavior |
| 28.2 | Round skipping — vertex at round N+5 without N+1..N+4 | Gap handling |
| 28.3 | Pending vertex buffer unbounded | No cap → memory DoS |
| 28.4 | Circular parents (A→B→A) | Cycle detection in DAG |
| 28.5 | Vertex with 0 transactions | Accepted (empty vertex) |
| 28.6 | Vertex with 0 parents after bootstrap | Rejected for round > 0 |
| 28.7 | Quorum relaxation timing — exact boundary rounds | Round 20 vs 21 vs 30 edge |
| 28.8 | Sync mode — snapshot round vs DAG round mismatch | Recovery gap |
| 28.9 | PurgePendingBeforeRound — removes orphan vertices | Vertices before snapshot round |
| 28.10 | isRoundInTransitionOrBuffer — extended convergence | Until fullQuorumAchieved |

---

## 29. Epoch Management Edge Cases

| # | Test | Detail |
|---|------|--------|
| 29.1 | Epoch with 0 fees collected | reward distribution when total=0 |
| 29.2 | Epoch transition with 1 validator | Churn limiting with single node |
| 29.3 | Epoch length = 1 round | Transition every round |
| 29.4 | Churn limit — exactly at limit (25%) | Boundary behavior |
| 29.5 | Pending removals > churn limit | Only first N (sorted by pubkey) removed |
| 29.6 | Epoch transition during sync mode | Node in sync receives epoch boundary |
| 29.7 | Multiple epoch transitions in same commit batch | Rounds [100, 200] if epoch_length=100 |
| 29.8 | Reward distribution — rounding with odd validator count | Remainder from integer division |
| 29.9 | Epoch additions > churn limit | Excess excluded from epochHolders |
| 29.10 | clearEpochState resets all counters | epochFees=0, epochRoundsProduced=new map, epochTotalRounds=0, epochAdditions=nil |

---

## 30. Snapshot Edge Cases

| # | Test | Detail |
|---|------|--------|
| 30.1 | Snapshot with corrupted checksum (1 bit flip) | Detection |
| 30.2 | Snapshot version mismatch (v3 vs v4) | Reject old format |
| 30.3 | Snapshot with malformed validator encoding | Truncated in validator list |
| 30.4 | Snapshot with duplicate object ID | Same objectID twice |
| 30.5 | Snapshot with orphan domain | Domain pointing to absent objectID |
| 30.6 | Snapshot with tracker entry without corresponding object | Tracker refs non-existent object |
| 30.7 | zstd decompression bomb | Small compressed → huge decompressed |
| 30.8 | Snapshot import interrupted mid-way | Atomicity (SetBatch) |
| 30.9 | Empty snapshot (0 objects, 0 validators) | Edge case |
| 30.10 | Snapshot with 0 vertices | Valid edge case |

---

## 31. BLS Signature Edge Cases

| # | Test | Detail |
|---|------|--------|
| 31.1 | BLS key all-zeros (point at infinity) | Trivially valid sig? |
| 31.2 | Duplicate signers in aggregation | Same validator signs 2x same object |
| 31.3 | Aggregated signature with 0 signers | Empty signer vector |
| 31.4 | Signature order independence | Same result regardless of aggregation order |
| 31.5 | QuorumProof with pubkey not in ValidatorSet | Valid sig but unknown signer |
| 31.6 | Subgroup check — key on wrong curve | BLS12-381 G1 vs G2 confusion |

---

## 32. Rendezvous Hashing Edge Cases

| # | Test | Detail |
|---|------|--------|
| 32.1 | Replication > number of validators | Top-N with N > len(validators) |
| 32.2 | 0 validators in epochHolders | Division by zero or panic |
| 32.3 | Score collision (same blake3 hash for 2 validators) | Deterministic tiebreaking |
| 32.4 | Holder computation with single validator | Always same holder |
| 32.5 | Stability — minimal reshuffling after adding validator | Rendezvous property |

---

## 33. Attestation Protocol Encoding

`internal/attest` (codec), `internal/aggregation/handler.go`

The response carries only the hash and the signature; there is no `WantFull`
flag and no full-object response. Getting the object is a separate request.

| # | Test | Detail |
|---|------|--------|
| 33.1 | Truncated request (< 42 bytes) | Partial read handling |
| 33.2 | Truncated response (header ok, body missing) | Partial body parse |
| 33.3 | Invalid type byte | Unknown message type |
| 33.4 | Positive response — hash + signature only | No object payload |
| 33.5 | Negative response — static error, no signature | Cheap rejection |
| 33.6 | Concurrent requests on same connection | Multiplexing / ordering |

---

## 34. Daemon Collection Edge Cases

`daemon/aggregation.go`, `daemon/submit.go`

| # | Test | Detail |
|---|------|--------|
| 34.1 | Quorum partial — exactly threshold-1 responses | Fail just below quorum |
| 34.2 | All holders timeout | No response received |
| 34.3 | Hash mismatch — a holder serves a different hash | Local verification rejects |
| 34.4 | Holder returns a negative for a non-current version | Refused, counts toward fail-fast |
| 34.5 | Fail-fast impossible quorum → typed error | (holders - negatives) < threshold |
| 34.6 | Singleton-only transaction → submitted raw | Wrapped into a trivial ATX by the validator |
| 34.7 | Version race → bounded backoff and one resync | Eventual success or ErrQuorumImpossible |

---

## 35. Network & Gossip Edge Cases

`internal/network/`

| # | Test | Detail |
|---|------|--------|
| 35.1 | Gossip fanout > number of peers | Fanout=40 with 5 validators |
| 35.2 | Duplicate gossip message (same vertex ID) | Dedup cache with TTL |
| 35.3 | QUIC reconnection stuck | Peer down → retry loop |
| 35.4 | Handler panic non-recovered | Crash in message handler |
| 35.5 | Message size > buffer limit | Very large vertex |
| 35.6 | Relay fanout (10) with < 10 peers | Not enough peers |
| 35.7 | Unknown message type received | Forward compatibility |

---

## 36. Sync Protocol

`internal/sync/protocol.go`, `internal/sync/buffer.go`, `internal/sync/manager.go`, `cmd/node/sync.go`

| # | Test | Detail |
|---|------|--------|
| 36.1 | RequestSnapshot — 60s timeout | requestTimeout = 60s, context.WithTimeout |
| 36.2 | RequestSnapshot — request ID matching | Response request_id must match sent ID |
| 36.3 | RequestSnapshot — empty snapshot data → error | len(compressedData) == 0 rejected |
| 36.4 | HandleSnapshotRequest — returns latest compressed snapshot | manager.Latest() |
| 36.5 | HandleSnapshotRequest — no snapshot available → error | compressedData == nil |
| 36.6 | IsSnapshotRequest — data < 8 bytes → false | Length check |
| 36.7 | IsSnapshotRequest — valid request with request_id > 0 → true | FlatBuffers parse |
| 36.8 | VertexBuffer — add and dedup by hash | blake3 hash deduplication |
| 36.9 | VertexBuffer — GetSince filters by round | Only vertices >= minRound |
| 36.10 | VertexBuffer — GetAll returns sorted by round | GetSince(0) |
| 36.11 | VertexBuffer — MinRound/MaxRound | Boundary tracking |
| 36.12 | VertexBuffer — concurrent access safety | RWMutex, parallel Add/Get |
| 36.13 | VertexBuffer — Clear removes all | vertices = new map |
| 36.14 | SnapshotManager — periodic creation | defaultSnapshotInterval = 10s ticker |
| 36.15 | SnapshotManager — skip when no new rounds | currentRound == lastRound early return |
| 36.16 | SnapshotManager — 2s genesis delay | time.Sleep(2s) before first snapshot |
| 36.17 | SnapshotManager — vertex history limited to 100 rounds | vertexHistoryRounds = 100 |
| 36.18 | Sync flow — buffer → snapshot → replay → purge | performSync full pipeline |

---

## 37. Object Routing (QUIC)

`cmd/node/clienthandlers.go`

Inter-node object retrieval runs over the QUIC mesh as a `MsgGetObject` to a
computed holder's QUIC address; there is no HTTP path and no on-chain HTTP
address.

| # | Test | Detail |
|---|------|--------|
| 37.2 | fetchObjectFromHolder — probes holders with replication 10 | ComputeHolders(id, 10) |
| 37.3 | fetchObjectFromHolder — skips self | holder == own → continue |
| 37.5 | fetchObjectFromHolder — asks each holder locally until success | Remote answers local-only, no cascade |
| 37.6 | fetchObjectFromHolder — all holders fail → not-found | Returns nil |
| 37.7 | inter-node fetch — 5s QUIC timeout | interNodeFetchTimeout |
| 37.11 | local-only flag skips remote routing | A non-holder answers not-found |

---

## 38. ATX Verification at Commit (epoch-aware)

`internal/aggregation/verify.go`, wired at `cmd/node/aggregation.go`

| # | Test | Detail |
|---|------|--------|
| 38.1 | Verifier iterates all proofs | Loop over atx.ProofsLength() |
| 38.2 | Finds the matching object by ID | findATXObjectIndex linear search |
| 38.3 | Object not found in ATX → error | objIdx < 0 |
| 38.4 | Recomputes H = blake3(content \|\| version_BE) | attest.ComputeObjectHash |
| 38.5 | Quorum check (signerCount >= attest.QuorumSize) | 67% per-object threshold |
| 38.6 | Insufficient signers → error | signerCount < quorum |
| 38.7 | Invalid aggregated BLS → error | VerifyAggregated returns false |
| 38.8 | extractSignerBLSKeys — bitmap to BLS keys mapping | ParseSignerBitmap + validator lookup |
| 38.9 | extractSignerBLSKeys — out-of-range bitmap index → skipped | idx >= len(holders) |
| 38.10 | extractSignerBLSKeys — missing validator BLS key → skipped | info.BLSPubkey == zero |
| 38.15 | Epoch rule — attestation_epoch == commit epoch → accept | commitEpochForRound(commitRound) |
| 38.16 | Epoch rule — attestation_epoch == commit epoch - 1, within grace → accept | commitRound within EpochGraceRounds |
| 38.17 | Epoch rule — previous epoch past grace → reject | Daemon must recollect |
| 38.18 | Epoch rule — future epoch → reject | attestation_epoch > commit epoch |
| 38.19 | Holders recomputed from HoldersForEpoch(attestation_epoch) | Deterministic snapshot selection |
| 38.20 | Lifecycle layer still runs at commit | Version, owner, gas, replay checked independently of the ATX proof |
| 38.21 | Genesis epoch (0) → holders resolve to the live validator set | No frozen snapshot until the first boundary; matches the set the daemon syncs |

---

## Summary

| Section | Tests |
|---------|-------|
| 1. Transaction Validation (ingress) | 33 |
| 2. Consensus DAG | 20 |
| 3. Version Tracking | 15 |
| 4. Fee System | 23 |
| 5. Fee Deduction Flow | 9 |
| 6. Gas Coin Validation | 13 |
| 7. Vertex Fee Summary | 9 |
| 8. Execution Sharding | 9 |
| 9. Epoch System | 19 |
| 10. State Management | 20 |
| 11. Domain Registry | 7 |
| 12. Off-chain aggregation & BLS | 28 |
| 13. Snapshot & Sync | 14 |
| 14. Network & Gossip | 15 |
| 15. Client Operations | 8 |
| 16. Pod VM | 17 |
| 17. Creation Limits | 8 |
| 18. Gas Metering | 5 |
| 19. Version Conflicts | 7 |
| 20. Fee Edge Cases | 10 |
| 21. Object Types | 15 |
| 22. Concurrent Modification | 5 |
| 23. Object Size & Limits | 5 |
| 24. Security & Attack Vectors | 13 |
| 25. Spec-Code Mismatches | 7 |
| 26. BLS Proof Verification | 5 |
| 27. Register/Deregister Handlers | 11 |
| 28. DAG Edge Cases | 10 |
| 29. Epoch Edge Cases | 10 |
| 30. Snapshot Edge Cases | 10 |
| 31. BLS Signature Edge Cases | 6 |
| 32. Rendezvous Edge Cases | 5 |
| 33. Protocol Encoding | 6 |
| 34. Daemon Collection Edge Cases | 7 |
| 35. Network Edge Cases | 7 |
| 36. Sync Protocol | 18 |
| 37. Object Routing | 11 |
| 38. ATX Verification at Commit | 15 |
| **TOTAL** | **~418** |
