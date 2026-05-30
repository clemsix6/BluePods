# Off-chain aggregation design

Scope: how attestation aggregation moves from the validators to the client. This covers the client daemon, the QUIC-only transport, the denial-of-service defense, the fee adjustment, and the module structure. It does not redesign the consensus, the object model, or the monetary policy, which stay as they are in the whitepaper. The existing transaction verification (owner, version, hashes) stays as the core of consensus safety and is updated, not replaced.

## Context

Today a validator that receives a transaction becomes its aggregator. It contacts the holders of the referenced objects over QUIC, collects their BLS signatures on the object hashes, assembles the quorum proof, and includes the attested transaction in a vertex. This couples two unrelated jobs in the consensus-critical code: participating in consensus, and running an errand on behalf of a user.

That coupling causes four problems. The aggregation work (opening streams, awaiting responses, verifying and aggregating BLS) is network and CPU cost that has nothing to do with ordering. The aggregator reward (a 20 percent fee share) creates an incentive that tracks who receives transactions rather than who secures the network, with its own distribution-fairness subproblem. (In the current code that reward is in fact a no-op stub: the 20 percent is split off but never credited to anyone, so removing it also cleans up an unfinished path.) The orchestration code (parallel collection, timeouts, fail-fast) is complexity that must live in and be maintained inside the node. And it contradicts the project's guiding principle that validators do only the strict minimum: the DAG is leaderless, state and execution are sharded by holders, fees are computed outside the VM, and on-chain aggregation is the last piece that breaks the pattern.

The redesign moves aggregation to the client. A validator no longer receives a raw transaction to aggregate. It receives an already-attested transaction (an ATX) whose quorum proof it verifies on top of the verification it already does. This is a trade, not a pure simplification: it lifts the aggregation work off the consensus path, and in exchange it puts a new untrusted-client ingress on the node that has to be defended. The trade is favorable because the new defense is off the consensus-critical path and mostly static and cheap.

## Goals and audience

The maintainer works solo. This document serves him returning to the work, and an AI agent that needs the design in context. It records every decision and the reasoning behind it, so the implementation plan can be written from it without rediscovering the tradeoffs.

The design is one coherent change. The five moving parts (client-side aggregation, QUIC-only transport, the new denial-of-service defense, the fee adjustment, and the protocol simplifications) are coupled: moving aggregation to the client forces the client to reach holders directly (transport), removes the aggregator role (fees), and lets several protocol rules simplify. They are specified together because they ship together.

## The client daemon

Clients interact with the network through a local daemon. In this iteration the daemon is a Go library, not a separate process. An application imports it and calls it directly. The library encapsulates validator-set synchronization, rendezvous hashing, object retrieval, attestation collection, local verification, ATX construction, and submission, behind a high-level `SubmitTransaction` entry point.

The library was chosen over a sidecar process for two reasons. The daemon is tiny (a list of several thousand validators is a few megabytes, and the only thing it synchronizes, the validator set, changes only at epoch boundaries), so the duplication cost of each application embedding its own copy is negligible, which removes the sidecar's only real advantage of sharing one instance across applications. And a library is far simpler to distribute (an import) than a process to deploy and supervise on every platform. The tradeoff is that the library binds consumers to Go. When a real non-Go consumer appears, the same library can be wrapped in a process that exposes a local socket, at low cost.

Placement. The daemon lives in the same repository and the same Go module as the node, as a top-level `daemon/` package. This keeps a single module to manage, which suits a solo project, and is acceptable because every consumer (the client SDK, the integration tests, a future CLI) is in-repo. The cost of this choice is that an external application importing the daemon would pull the node's full dependency graph. The discipline that keeps a future split cheap: the daemon imports only the pure, light shared code (rendezvous hashing, BLS aggregation, the wire types, the attestation protocol) and never `storage`, `podvm`, or the consensus packages.

This last point is a prerequisite refactor, not a property the code has today. The rendezvous and BLS helpers currently depend on `internal/consensus` for `ValidatorSet` and `Hash`, which live in the same package as the entire DAG. Before the daemon can import them without dragging in the DAG, those light types (`ValidatorSet`, `Hash`, `ValidatorInfo`) must be extracted into a small shared package, for example `internal/validators/`. When the first external non-monorepo consumer appears, the daemon and that shared package move into their own module, at which point the shared package must leave `internal/` because `internal/` packages are not importable across modules; until then it stays under `internal/`.

## Denial-of-service defense

Moving aggregation to the client opens one new attack surface: any client can open QUIC connections to an object's holders and demand attestations without ever submitting a paid transaction. Each attestation is a storage lookup plus a BLS signature. The danger is amplification: a tiny request triggers real signing work the holder cannot recover the cost of, and cannot evict.

The primary defense is structural rather than economic or computational. An attestation signature is deterministic: `H = BLAKE3(content || version)` and the BLS signature over `H` is identical for every requester for as long as the object stays at that version. The holder computes that signature eagerly, at execution time, as a side effect of the work it already does. A holder executes every committed transaction that touches an object it holds; at the moment it applies the operation that advances the object to version V, it also signs `H` and stores the signature durably next to the object. This is a pure, local, sub-millisecond computation, not a lock and not a change to concurrency control (the version mechanism remains the only conflict detector). It is "eager" only in the sense of when the signature is computed.

The consequence is that the signature for an object's current version always exists by the time anyone could request it, because a version only becomes current after a holder has executed the transaction that produced it. There is no cold window. A holder's signing work is therefore bounded by the real, paid rate of state change, because signatures are produced exactly once per version advance and version advances require paid transactions. An attestation request becomes a pure read of stored bytes.

Eager signing was chosen over lazy signing (signing on the first attestation request and caching the result). Lazy signing is lighter in the absence of an attack, but it leaves a cold-miss hole: the current version of any object that has not yet been requested is unsigned, so an attacker who enumerates object IDs (which are derivable from the public DAG) can request the current version of many distinct held objects, each a free, error-free, fresh signature, bounded only by how many objects a holder holds and re-armed on every restart or epoch reshuffle. Eager signing closes that hole by construction, at the cost of signing for versions that may never be attested. That cost is bounded by paid throughput and piggybacked on execution, which is the right trade for a denial-of-service property.

For this to hold across a holder-set change, stored signatures travel in the state snapshots used to sync a validator that becomes a new holder of an object, so the new holder is warm immediately rather than cold. Only the current-version signature is retained per object; the prior version's signature is evicted when the version advances.

Two supporting rules. Negative and error responses must not sign. A request for an object the holder does not hold, or for a non-current version, is answered with a static error after a cheap lookup, never a BLS signature; today the code signs negative responses, and that must change, or invalid requests would not be cheap. And the rejection path must sit before the (now precomputed) read, so that malformed or unauthorized requests cost a lookup, not a signature.

What remains after the structural defense is a generic network flood (saturating bandwidth and connection slots), which is the same problem every networked service has, and which no decentralized network fully solves. Standard ingress hardening handles it, layered from cheapest to most expensive:

- QUIC Retry address validation. Before doing any work, the node forces the client through a Retry round-trip that proves it owns a routable IP. This is one stateless RTT and it eliminates spoofed-source amplification. The QUIC handshake is itself a measured amplification vector, so address validation must precede any work, and memory must be accounted from the first packet.
- Per-peer resource scopes. Nested caps on concurrent connections, in-flight streams, and pending memory per peer, modeled on the libp2p resource manager. An ephemeral connection that never submits stays in a cheap transient tier and is rejected before any read.
- Ordinary rate limiting per IP and per connection.

A frequency-based blocklist that bans clients on request rate and error rate (a count-min sketch with dry-run policies, in the style of Sui's TrafficController) is a known next step, deferred until load actually demands it rather than built up front, to keep the node lean. The honest residual: IP-based limiting is a filter, not a wall (botnets and address rotation defeat it). And enforcement against a misbehaving holder (one that refuses to serve, or serves wrong data) depends on the slashing and fraud-proof systems that are not implemented yet, so for this iteration there is no on-protocol punishment for a Byzantine holder beyond the existing quorum requirement masking a minority of them.

Two approaches were considered and rejected. Capital gating (requiring the requester's key to back a funded on-chain account) fails on a bootstrap circularity: obtaining the token to fund an account itself requires using the network. Computational gating (proof-of-work, or a verifiable delay function) fails on hardware asymmetry: a server out-produces a phone, and a VDF does not fix this because an attacker who needs many tokens parallelizes across puzzles, so aggregate throughput tracks total compute again. The empirical record supports the rejection: Nano's proof-of-work anti-spam visibly failed under the March 2021 flood, and Nano has since de-emphasized dynamic difficulty in favor of stake-weighted prioritization.

## Transport

The transport is QUIC only. The HTTP API is removed. Client transaction and ATX submission, object retrieval, attestation collection, validator-set synchronization, and the operations and functional endpoints (`health`, `status`, `validators`, faucet, domain resolution) all pass over QUIC, the same transport the validators use among themselves. Inter-validator connections stay as they are: persistent, full mesh, mutually authenticated through TLS certificates derived from the Ed25519 keys.

Client connections are different, and this is net-new work. Today the network layer treats every connection as a trusted validator: it requires a client certificate, adds every peer to a persistent mesh, and reconnects forever. The redesign adds a separate notion of an ephemeral, untrusted, rate-limited client connection that does not join the mesh, alongside the denial-of-service gates above. This is a new subsystem in the network layer, not a handler addition.

The operations endpoints that were HTTP become QUIC messages in the same client handler as the rest, which keeps the node to a single listener. The one thing given up is out-of-the-box compatibility with HTTP liveness probes and Prometheus scraping, which is covered by a small CLI built on the daemon library (a Kubernetes exec probe can call it), and metrics are deferred to a separate concern when observability is actually needed.

## Fee model

The redesign forces one change to the fees: the aggregator role disappears, so its 20 percent share has to go somewhere. It moves into the epoch reward pool. The split goes from 20 percent aggregator, 30 percent burn, 50 percent epoch, to 70 percent epoch, 30 percent burn.

This is not a localized edit to `fees.go`. The aggregator share is a consensus-critical wire field: every validator recomputes it and rejects a vertex on mismatch. Removing it touches `types/vertex.fbs` (drop `total_aggregator` from the `FeeSummary` table) and its regenerated type, the fee computation in `fees.go` (remove `AggregatorBPS`), the fee accumulation and crediting in `commit.go` (including the now-dead `creditAggregator` stub), the vertex assembly in `build.go`, and the `FeeSummary` validation in `validate.go`. It is a breaking schema and consensus change, which is acceptable pre-mainnet but must be done across all those sites together, not just in `fees.go`.

Two judgment calls inside this. The operational fees (compute, transit, domain) go to the common epoch reward pool, distributed to validators by consensus participation, rather than being paid specifically to the holders that executed the work. Paying per work performed would reintroduce the same distribution-fairness subproblem that removing the aggregator just eliminated; rendezvous hashing spreads the holder role roughly evenly across the set, so it averages out, and the incentive stays attached to one thing, securing the network. And the operational burn is left unchanged at 30 percent for now: deciding whether to keep, lower, or remove it is a monetary-policy question that has not been studied, deciding it under-informed in the middle of an aggregation redesign would be worse than leaving it, and it is explicitly deferred to a dedicated tokenomics study.

The storage deposit is unchanged: locked at object creation (`storage_fee × effective_replication / total_validators`), 95 percent refunded on deletion, 5 percent burned. That 5 percent burn stays because it discourages create-and-delete object churn. It is an anti-spam mechanism, not a monetary lever.

## Protocol simplifications

Three rules simplify, all enabled by client-side aggregation.

The singleton fast path. Coins, pod code, and other singletons (replication 0) are held by every validator, so they need no attestation. A transaction that touches only singletons is submitted raw to any validator, which wraps it in a trivial ATX with no objects and no proofs. A simple wallet never aggregates, never runs the daemon, and never contacts holders. This is not the consensus-less fast path the vision forbids: the singleton transaction still enters the one global DAG and is finalized by the same commit rule. Only the attestation round is skipped, never consensus. The wrapping into a trivial ATX is done internally by the receiving validator, so a trivial ATX (no objects, no proofs) being structurally identical to a raw transaction is not a problem: the structural distinction below applies to what a client submits, and a client submits either a raw transaction or a full ATX.

Singletons are never carried in an ATX and never attested, including the gas coin. A transaction that touches both replicated objects and singletons produces an ATX whose object list and proofs cover only the replicated objects; every singleton it references (the gas coin, or any read singleton) is checked locally by each validator from the transaction header, exactly as today. This preserves the singleton optimization and keeps a singleton reference from forcing a network-wide attestation.

The attestation response carries only the hash and the signature. The `WantFull` flag that lets a holder return the full object in its attestation response is removed. Retrieving the object is a separate request, to any holder the client chooses. This decouples getting the data from getting the signature, so a client that already has the object locally (a future subscribing daemon) asks only for the signatures and does not re-download the object.

The designated top-1 holder rule is removed. No specific holder is required to serve the object. The daemon fetches it from whichever holder it likes, and the protocol requires only that the object included in the ATX matches the hash the quorum attested.

The validator distinguishes a client-submitted raw transaction from a full ATX by structure. An ATX carries objects and quorum proofs, a raw transaction does not. There is no magic byte and no second endpoint.

## Architecture and module structure

After the redesign a node's job reduces to three verbs: verify, order, serve. The daemon's job is also three: collect, assemble, submit.

Node side, in `internal/`:

- Removed: `internal/aggregation/aggregator.go` and `collector.go` (the node no longer orchestrates collection), and `internal/api/server.go` (no HTTP).
- Kept and refocused in `internal/aggregation/` as the holder-and-verification module: `handler.go` (a holder answers attestation requests, now serving stored signatures and never signing on negative responses), `bls.go` (BLS aggregation and verification), `rendezvous.go` (holder selection), `protocol.go` (message encoding, with `WantFull` removed). The incoming-ATX verifier already exists in `cmd/node/aggregation.go`; it moves here and is updated, it is not written from scratch.
- New shared package `internal/validators/` holding `ValidatorSet`, `Hash`, and `ValidatorInfo`, extracted from `internal/consensus` so the rendezvous, BLS, ATX verifier, and daemon can use them without importing the DAG.
- New `internal/validation/`: structural transaction validation, extracted from `internal/api/validate.go`.
- `internal/network/`: the client-facing QUIC surface (handlers for TX/ATX submission, object retrieval, validator-set, operations, faucet, domain) plus the new ephemeral-untrusted-client connection tier and the denial-of-service gates. The existing mesh logic that assumes all peers are trusted validators stays for inter-validator links.
- Object storage: the durable per-object current-version signature, stored next to the object and included in state snapshots, evicted on version advance.
- The fee change across `internal/consensus/fees.go`, `commit.go`, `build.go`, `validate.go`, and `types/vertex.fbs` as described in the fee section.
- `types/vertex.fbs`: an epoch reference carried with the attestation (on the ATX or the `QuorumProof`) so a validator can select the correct holder snapshot, and the `FeeSummary` change.

The existing lifecycle verification stays unchanged and remains the core of consensus safety. Version tracking from the DAG, the owner check on mutable references, the gas-coin check, and replay protection are not replaced by ATX verification; ATX verification (the quorum proof) runs in addition to them.

Client side:

- Removed: `client/http.go`. Ported to QUIC: `client/client.go` and `client/transactions.go`, which are equally HTTP-coupled today and will not compile against a removed `http.go`.
- Added: `client/quic.go` (the SDK's QUIC transport), and the `daemon/` package described above.

The boundary is clean. The daemon shares with the node only pure, deterministic code: rendezvous hashing, the quorum proof format, BLS aggregation, the wire types. No consensus logic leaks into the client. The node, for its part, never trusts the daemon and reverifies everything on receipt of an ATX.

## Data flow

There are two paths, and the asymmetry between them is the point.

A transaction on singletons only (a coin transfer):

1. The wallet builds and signs the raw transaction.
2. It submits the raw transaction to any validator over QUIC, after the Retry address-validation round-trip.
3. The validator validates the structure and checks that no replicated object is referenced.
4. It wraps the transaction in a trivial ATX, with no objects and no proofs.
5. It includes the ATX in a vertex and gossips it. Consensus, commit, execution proceed as today.

A transaction touching replicated objects:

1. The application passes its transaction intent to the local daemon.
2. The daemon fetches each referenced replicated object from a holder to learn its content, version, and replication factor. (Replication is immutable after creation, so a daemon that has seen the object before may use a cached value.)
3. The daemon computes the holders of each object by rendezvous hashing, locally, from its cached validator set and the replication factor.
4. The daemon requests attestations from the holders. Each holder, past the Retry and the gates, looks up the object, checks that the requested version is the current one it holds, and returns `H` and its stored BLS signature for `H = BLAKE3(content || version)`. Invalid requests (object not held, non-current version) are cheap static rejections that feed the error-rate signal, never a signature.
5. The daemon verifies locally that every returned signature is on the same `H` and that this `H` matches the object it fetched, failing fast if quorum becomes impossible.
6. The daemon aggregates the BLS signatures, builds the signer bitmap, and assembles the ATX: original transaction, fetched replicated objects, quorum proofs, and the epoch the attestation was collected in.
7. The daemon submits the ATX to any validator over QUIC.
8. The receiving validator reverifies, trusting nothing from the daemon. This is two layers that both must pass. The ATX layer: structure; the included objects are standard (non-singleton); `H` recomputed from each included object; the aggregated signature, bitmap, and quorum (at least 67 percent of the object's holders) checked against the holder snapshot selected by the deterministic commit epoch, with the carried attestation epoch validated against it. The existing lifecycle layer, unchanged: the attested version equals the version the transaction declares and the version tracked from the DAG; the owner check on mutable references; the gas coin exists, is owned by the sender, and has sufficient balance; and the transaction is not a replay. The attestation certifies only the state of the input objects, so transaction legitimacy rests entirely on this second layer, which is why it must run in full.
9. It includes the ATX in a vertex and gossips it. Consensus, commit, execution proceed as today.

## Error handling and edge cases

Recoverable errors, daemon side. If quorum becomes impossible, the daemon fails fast and returns a typed error to the application. The common case is a version race: the daemon fetches an object at version V, but another transaction advances it to V+1 before collection finishes, so the holders refuse to attest V and the daemon refetches and retries. Client-side collection over several round-trips widens this race window compared to the old in-node aggregator, so on a hot, contended object a daemon can lose repeatedly. Retries use bounded randomized backoff and are bounded in number before surfacing to the application; genuinely hot objects need an application-level batching or sequencing pattern, which the whitepaper already flags as an open problem. This is not presented as a solved equivalent of the old path; it is a known regression in the contended case, traded for the off-chain move.

Safety rejections, validator side. The validator never trusts the daemon. An ATX whose included object does not match the attested `H`, whose quorum is insufficient, whose bitmap is inconsistent, or which fails any lifecycle check (version, owner, gas, replay) is rejected at ingress with no state change. A malicious or buggy daemon forces nothing; it only wastes its own round-trip. In particular, because the attestation binds the object hash and not the transaction, a daemon could staple valid object attestations to a different transaction touching the same objects at the same versions, and the lifecycle layer (owner and version checks) is what rejects it. That layer is therefore load-bearing and not optional.

The epoch boundary. An object's holders depend on the validator set, which changes at epoch boundaries through the `epochHolders` snapshot, so an attestation is only verifiable against the holder snapshot of the epoch it was collected in. Two rules. For safety, ATX validity is a function of deterministic committed state only: the commit epoch of a vertex is determined identically by every validator from the round at which it commits, and the holder snapshot used to recompute the quorum is selected by that commit epoch, never by a validator's local view. The attestation's epoch is carried on the wire and is part of the consensus-verified data, so validators cannot disagree on validity and fracture a finalized commit. For liveness, the previous epoch's holder snapshot is retained for a bounded grace window, so an ATX collected late in epoch E and committed shortly into E+1 still verifies rather than being rejected through no fault of the user; the daemon recollects only if the grace window is exceeded.

Daemon epoch awareness. The daemon must know the current epoch and the boundary schedule to scope its attestations and to refresh its validator set, since attesting against a stale set silently fails. It learns the epoch and the round-based boundary schedule from the `status` and `validators` QUIC messages, refreshes the validator set on an epoch change, and treats a quorum-impossible result as a trigger to re-sync the set before retrying, so a missed epoch transition surfaces as a diagnosable resync rather than a silent failure.

Denial-of-service false positives. A legitimate high-frequency service must not be blocklisted. The error-rate signal mostly catches garbage, because an honest client requests the current version of objects it knows it holds and so produces few errors, and rate limiting stays generous and degrades first only under heavy load. Any blocklist policy starts in dry-run to observe before blocking.

## Testing

Three levels, aligned with what already exists.

Unit tests cover the `daemon` package (rendezvous, collection with mocked holders, fail-fast, local hash verification, ATX construction including the epoch reference), the node's ATX verification together with the lifecycle checks (a daemon stapling valid attestations to a different transaction is rejected), eager signing at execution and warmth after snapshot sync, negative responses producing no signature, the denial-of-service gates (Retry validation, per-peer scopes, rate limiting), the durable signature storage (signed at execution, served from store, evicted on version change), and the new 70/30 fee split including the `FeeSummary` schema change.

Integration tests migrate the existing simulations to the QUIC transport and client-side aggregation. New scenarios: end-to-end client aggregation with all nodes converging, the singleton fast path, a version race during collection with bounded backoff, an epoch boundary crossed during an attestation's validity (both the grace-window accept and the recollect-after-grace cases), and a cold-holder scenario confirming eager signing leaves no exploitable cold window.

The acceptance test plan is updated: the items on the HTTP API, the aggregator role, and the old 20/30/50 split become obsolete and are replaced by items on off-chain aggregation, QUIC transport, the denial-of-service gates, eager signing, the complete validator reverification, and the epoch-boundary rule.

## Alternatives considered

Daemon shape: a sidecar process was rejected for this iteration in favor of a library, because the daemon is too light for the sidecar's shared-instance advantage to matter and a library is simpler to distribute.

Daemon placement: a separate Go module or repository was rejected for now in favor of a top-level package in the node's module, because every consumer is in-repo and multi-module tooling is friction without benefit until an external consumer exists.

Denial-of-service: capital gating was rejected for its bootstrap circularity, and proof-of-work and verifiable delay functions were rejected for hardware asymmetry (and for the VDF, cross-puzzle parallelism), with Nano's documented 2021 failure as empirical support. The structural deterministic-signature approach is the primary defense, with QUIC Retry validation, per-peer resource scopes, and rate limiting as the layered residual defense, and a frequency-and-error blocklist deferred until load demands it.

Signing time: lazy signing (sign on first request, cache) was rejected in favor of eager signing at execution, because lazy leaves a cold-miss hole that lets an attacker force fresh signatures for free across many distinct objects, whereas eager bounds signing to paid version churn and keeps every current version warm.

Transport: keeping a minimal read-only HTTP surface for operations was rejected in favor of operations-over-QUIC, because the QUIC handler already exists and a second listener is not worth it.

Fee burn: removing the operational burn entirely (a 100 percent epoch model) was rejected in favor of leaving it unchanged at 30 percent, because the burn-versus-issuance question is monetary policy that has not been studied and should not be decided inside this redesign.

## Out of scope and deferred

This design does not change the consensus, the object model, or the existing lifecycle verification beyond adding the durable signature and the ATX quorum check. Storing signatures in state snapshots is in scope, because eager signing needs new holders to be warm after a reshuffle. The frequency-and-error blocklist is deferred until load demands it. Slashing and fraud proofs are not built, so there is no on-protocol enforcement against a misbehaving holder beyond quorum, and the bootstrap question for fully anonymous clients (a relayer or sponsor tier) is left open. This design does not build the sidecar process, the separate daemon module, or the metrics endpoint. It does not build the future subscribing daemon, although the transport and the simplified attestation are chosen to be compatible with it. And it does not settle the monetary policy, which is deferred to a dedicated study.
