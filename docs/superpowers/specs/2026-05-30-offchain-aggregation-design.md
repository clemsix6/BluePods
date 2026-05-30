# Off-chain aggregation design

Scope: how attestation aggregation moves from the validators to the client. This covers the client daemon, the QUIC-only transport, the denial-of-service defense, the fee adjustment, and the module structure. It does not redesign the consensus, the object model, or the monetary policy, which stay as they are in the whitepaper.

## Context

Today a validator that receives a transaction becomes its aggregator. It contacts the holders of the referenced objects over QUIC, collects their BLS signatures on the object hashes, assembles the quorum proof, and includes the attested transaction in a vertex. This couples two unrelated jobs in the consensus-critical code: participating in consensus, and running an errand on behalf of a user.

That coupling causes four problems. The aggregation work (opening streams, awaiting responses, verifying and aggregating BLS) is network and CPU cost that has nothing to do with ordering. The aggregator reward (a 20 percent fee share) creates an incentive that tracks who receives transactions rather than who secures the network, with its own distribution-fairness subproblem. The orchestration code (parallel collection, timeouts, fail-fast) is complexity that must live in and be maintained inside the node. And it contradicts the project's guiding principle that validators do only the strict minimum: the DAG is leaderless, state and execution are sharded by holders, fees are computed outside the VM, and on-chain aggregation is the last piece that breaks the pattern.

The redesign moves aggregation to the client. A validator no longer receives a raw transaction to aggregate. It receives an already-attested transaction (an ATX) that it only has to verify and order.

## Goals and audience

The maintainer works solo. This document serves him returning to the work, and an AI agent that needs the design in context. It records every decision and the reasoning behind it, so the implementation plan can be written from it without rediscovering the tradeoffs.

The design is one coherent change. The five moving parts (client-side aggregation, QUIC-only transport, the new denial-of-service defense, the fee adjustment, and the protocol simplifications) are coupled: moving aggregation to the client forces the client to reach holders directly (transport), removes the aggregator role (fees), and lets several protocol rules simplify. They are specified together because they ship together.

## The client daemon

Clients interact with the network through a local daemon. In this iteration the daemon is a Go library, not a separate process. An application imports it and calls it directly. The library encapsulates validator-set synchronization, rendezvous hashing, object retrieval, attestation collection, local verification, ATX construction, and submission, behind a high-level `SubmitTransaction` entry point.

The library was chosen over a sidecar process for two reasons. The daemon is tiny (a list of several thousand validators is a few megabytes, and the only thing it synchronizes, the validator set, changes only at epoch boundaries), so the duplication cost of each application embedding its own copy is negligible, which removes the sidecar's only real advantage of sharing one instance across applications. And a library is far simpler to distribute (an import) than a process to deploy and supervise on every platform. The tradeoff is that the library binds consumers to Go. When a real non-Go consumer appears, the same library can be wrapped in a process that exposes a local socket, at low cost.

Placement. The daemon lives in the same repository and the same Go module as the node, as a top-level `daemon/` package. This keeps a single module to manage, which suits a solo project, and is acceptable because every consumer (the client SDK, the integration tests, a future CLI) is in-repo. The cost of this choice is that an external application importing the daemon would pull the node's full dependency graph. The discipline that keeps a future split cheap: the daemon imports only the pure, light shared code (rendezvous hashing, BLS aggregation, the wire types, the attestation protocol) and never `storage`, `podvm`, or the consensus packages. When the first external non-monorepo consumer appears, the daemon and that shared code move into their own module. At that point the shared code must leave `internal/`, because `internal/` packages are not importable across modules; until then it stays under `internal/`.

## Denial-of-service defense

Moving aggregation to the client opens one new attack surface: any client can open QUIC connections to an object's holders and demand attestations without ever submitting a paid transaction. Each attestation is a storage lookup plus a BLS signature. The danger is amplification: a tiny request triggers real signing work the holder cannot recover the cost of, and cannot evict.

The primary defense is structural rather than economic or computational. An attestation signature is deterministic: `H = BLAKE3(content || version)` and the BLS signature over `H` is identical for every requester for as long as the object stays at that version. So a holder signs at most once per (object, version) pair. It computes the signature lazily, on the first request for a (object, version) it has not signed, stores it durably next to the object, and serves it as static data on every later request. To force a fresh signature, an attacker must request a (object, version) the holder has not stored, which only happens when an object actually advances to a new version, which requires a real, paid transaction. A holder's signing rate is therefore bounded by the real, paid rate of state change, not by the request rate. The amplification disappears: a repeated request costs a lookup and a send, not a pairing.

Lazy signing was chosen over eager signing at execution time. Eager signing would make every attestation a pure read with no signing ever in the request path, but it would sign every version of every held object unconditionally, including the many objects that are never attested. Lazy signing only ever produces signatures that are actually requested, and the one cost it carries, a sub-millisecond signature on the first request for a fresh version, is already bounded by the paid-churn argument above.

What remains after the structural defense is a generic network flood (saturating bandwidth and connection slots), which is the same problem every networked service has, and which no decentralized network fully solves. Four standard, cheap gates handle the residual, layered on the node's QUIC ingress from cheapest to most expensive:

- QUIC Retry address validation. Before doing any work, the node forces the client through a Retry round-trip that proves it owns a routable IP. This is one stateless RTT and it eliminates spoofed-source amplification, which is the cheapest form of the attack. The QUIC handshake is itself a measured amplification vector, so address validation must precede any cryptography.
- Per-peer resource scopes. Nested caps on concurrent connections, in-flight streams, and pending memory per peer, modeled on the libp2p resource manager. An ephemeral connection that never submits stays in a cheap transient tier and is rejected before any BLS work.
- Traffic control. A count-min sketch tracks per-client request frequency in bounded memory, and clients are blocklisted on their error rate as well as their request rate, modeled on Sui's TrafficController. A client requesting attestations for objects a holder does not hold, or for stale versions, produces a clean error signal that is cheap to ban on. Policies start in dry-run to observe before blocking.
- Ordinary rate limiting as the last line of defense for the bandwidth residual.

Two approaches were considered and rejected. Capital gating (requiring the requester's key to back a funded on-chain account) fails on a bootstrap circularity: obtaining the token to fund an account itself requires using the network. Computational gating (proof-of-work, or a verifiable delay function) fails on hardware asymmetry: a server out-produces a phone, and a VDF does not fix this because an attacker who needs many tokens parallelizes across puzzles, so aggregate throughput tracks total compute again. The empirical record supports the rejection: Nano's proof-of-work anti-spam visibly failed under the March 2021 flood, and Nano has since de-emphasized dynamic difficulty in favor of stake-weighted prioritization.

The honest residual: IP-based limiting is a filter, not a wall (botnets and address rotation defeat it), memory must be accounted from the first packet (a 2025 QUIC pre-handshake DoS bypasses stream-level caps otherwise), and the bootstrap question for fully anonymous clients (a relayer or sponsor tier, in the style of Solana's staked providers, versus accepting that anonymous clients live in a degrade-first best-effort lane) is left open and is not required for this iteration.

## Transport

The transport is QUIC only. The HTTP API is removed. Client transaction and ATX submission, object retrieval, attestation collection, and validator-set synchronization all pass over QUIC, the same transport the validators use among themselves. Inter-validator connections stay as they are: persistent, full mesh, mutually authenticated through TLS certificates derived from the Ed25519 keys. Client connections are ephemeral, untrusted, and rate-limited.

The operations endpoints that were HTTP (`health`, `status`, `validators`) become QUIC messages in the same client handler as the rest, which costs almost nothing because the handler already exists and keeps the node to a single listener. The one thing given up is out-of-the-box compatibility with HTTP liveness probes and Prometheus scraping, which is covered by a small CLI built on the daemon library (a Kubernetes exec probe can call it), and metrics are deferred to a separate concern when observability is actually needed.

## Fee model

The redesign forces exactly one change to the fees: the aggregator role disappears, so its 20 percent share has to go somewhere. It moves into the epoch reward pool. The split goes from 20 percent aggregator, 30 percent burn, 50 percent epoch, to 70 percent epoch, 30 percent burn. The `AggregatorBPS` parameter is removed.

Two judgment calls inside this. The operational fees (compute, transit, domain) go to the common epoch reward pool, distributed to validators by consensus participation, rather than being paid specifically to the holders that executed the work. Paying per work performed would reintroduce the same distribution-fairness subproblem that removing the aggregator just eliminated; rendezvous hashing spreads the holder role roughly evenly across the set, so it averages out, and the incentive stays attached to one thing, securing the network. And the operational burn is left unchanged at 30 percent for now: deciding whether to keep, lower, or remove it is a monetary-policy question the maintainer has not yet studied, deciding it under-informed in the middle of an aggregation redesign would be worse than leaving it, and it is explicitly deferred to a dedicated tokenomics study.

The storage deposit is unchanged: locked at object creation (`storage_fee × effective_replication / total_validators`), 95 percent refunded on deletion, 5 percent burned. That 5 percent burn stays because it serves a specific and still-valid purpose, discouraging create-and-delete object churn. It is an anti-spam mechanism, not a monetary lever.

## Protocol simplifications

Three rules simplify, all enabled by client-side aggregation.

The singleton fast path. Coins are singletons (replication 0), held by every validator, so they need no attestation. A transaction that touches only singletons is submitted raw to any validator, which wraps it in a trivial ATX with no objects and no proofs. A simple wallet never aggregates, never runs the daemon, and never contacts holders. The complexity of aggregation falls only on the developers and services that manipulate replicated objects.

The attestation response carries only the hash and the signature. The `WantFull` flag that let a holder return the full object in its attestation response is removed. Retrieving the object is a separate request, to any holder the client chooses. This decouples getting the data from getting the signature, so a client that already has the object locally (a future subscribing daemon) asks only for the signatures and does not re-download the object.

The designated top-1 holder rule is removed. No specific holder is required to serve the object. The daemon fetches it from whichever holder it likes, and the protocol requires only that the object included in the ATX matches the hash the quorum attested.

The validator distinguishes a raw transaction from a full ATX by structure. An ATX carries objects and quorum proofs, a raw transaction does not. There is no magic byte and no second endpoint.

## Architecture and module structure

After the redesign a node's job reduces to three verbs: verify, order, serve. The daemon's job is also three: collect, assemble, submit.

Node side, in `internal/`:

- Removed: `internal/aggregation/aggregator.go` and `collector.go` (the node no longer orchestrates collection), and `internal/api/server.go` (no HTTP).
- Kept and refocused in `internal/aggregation/` as the holder-and-verification module: `handler.go` (a holder answers attestation requests), `bls.go` (BLS aggregation and verification), `rendezvous.go` (holder selection), `protocol.go` (message encoding, in the simplified hash-and-signature form). Incoming-ATX verification is added here: recompute `H` from the included objects, check the aggregated signature, the bitmap, and the quorum.
- Added or modified: `internal/validation/` (structural transaction validation, extracted from the old `api/` package since it no longer belongs in an HTTP module); `internal/network/` (the client-facing QUIC surface: handlers for TX/ATX submission, object retrieval, validator-set, and the operations messages, plus the denial-of-service gates); the durable signature storage per (object, version) next to the object, in the object storage layer; `internal/consensus/fees.go` (remove `AggregatorBPS`, set 70 epoch and 30 burn); and `types/vertex.fbs` (the `AttestedTransaction` structure, distinguished from a raw transaction by its shape).

Client side:

- Removed: `client/http.go`.
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
2. The daemon computes the holders of each referenced replicated object by rendezvous hashing, locally, from its cached validator set.
3. The daemon fetches each object from a holder to get its content, version, and replication factor.
4. The daemon requests attestations from the holders. Each holder, past the Retry and the gates, looks up the object, checks that the requested version is the current one it holds, lazily signs `H = BLAKE3(content || version)` if it has not stored that signature (otherwise serves the stored one), and returns `H` and its BLS signature. Invalid requests (object not held, stale version) are cheap rejections that feed the error-rate blocklist.
5. The daemon verifies locally that every returned signature is on the same `H` and that this `H` matches the object it fetched, failing fast if quorum becomes impossible.
6. The daemon aggregates the BLS signatures, builds the signer bitmap, and assembles the ATX: original transaction, fetched objects, quorum proofs.
7. The daemon submits the ATX to any validator over QUIC.
8. The receiving validator reverifies everything, trusting nothing from the daemon: structure, `H` recomputed from the included objects, the aggregated signature against `H`, the bitmap, and the quorum (at least 67 percent of the object's holders, recomputed by rendezvous on the validator's side).
9. It includes the ATX in a vertex and gossips it. Consensus, commit, execution proceed as today.

## Error handling and edge cases

Recoverable errors, daemon side. If quorum becomes impossible (too many holders unreachable or negative), the daemon fails fast and returns a typed error to the application. The common case is not a failure but a version race: the daemon fetches an object at version V, but another transaction commits and the object advances to V+1 before collection finishes. The holders now hold V+1 and refuse to attest V, so the daemon does not reach quorum on `H(V)`, refetches the object at the new version, and retries a bounded number of times before surfacing to the application. This is the optimistic-concurrency the protocol already handles by version, moved into the collection path.

Safety rejections, validator side. The validator never trusts the daemon. An ATX whose included object does not match the attested `H` fails the recompute and is rejected. Insufficient quorum, an inconsistent bitmap, or an invalid aggregated signature are rejected at ingress with no state change. A malicious or buggy daemon forces nothing; it only wastes its own round-trip.

The epoch boundary, which needs an explicit rule. An object's holders depend on the validator set, which changes at epoch boundaries through the `epochHolders` snapshot. An attestation collected against epoch E's holder set is only verifiable against that set. The rule: an attestation is scoped to its epoch's holder snapshot, and an ATX must be committed within the epoch of its attestation, otherwise it is rejected and the daemon recollects in the new epoch. This bounds validity and removes the ambiguity of an ATX collected in E but ordered in E+1 after the holder set changed.

Denial-of-service false positives. A legitimate high-frequency service must not be blocklisted. Two protections: the error-rate blocklist mostly catches garbage, because an honest client requests the current version of objects it knows it holds and so produces few errors; and rate limiting stays generous and degrades first only under heavy load. Policies start in dry-run to observe before blocking for real.

## Testing

Three levels, aligned with what already exists.

Unit tests cover the `daemon` package (rendezvous, collection with mocked holders, fail-fast, local hash verification, ATX construction), the node's ATX verification, the denial-of-service gates (rate limiting, count-min sketch, blocklist, Retry validation), the durable signature storage (sign once per version, serve from store, invalidate on version change), and the new 70/30 fee split.

Integration tests migrate the existing simulations to the QUIC transport and client-side aggregation. New scenarios are added: end-to-end client aggregation with all nodes converging, the singleton fast path, a version race during collection, attestation spam that ends up blocklisted without disturbing legitimate traffic, and an epoch boundary crossed during an attestation's validity.

The acceptance test plan is updated: the items on the HTTP API, the aggregator role, and the old 20/30/50 split become obsolete and are replaced by items on off-chain aggregation, QUIC transport, the denial-of-service gates, and lazy signing.

## Alternatives considered

Daemon shape: a sidecar process was rejected for this iteration in favor of a library, because the daemon is too light for the sidecar's shared-instance advantage to matter and a library is simpler to distribute.

Daemon placement: a separate Go module or a separate repository was rejected for now in favor of a top-level package in the node's module, because every consumer is in-repo and multi-module tooling is friction without benefit until an external consumer exists.

Denial-of-service: capital gating was rejected for its bootstrap circularity, and proof-of-work and verifiable delay functions were rejected for hardware asymmetry (and for the VDF, cross-puzzle parallelism), with Nano's documented 2021 failure as empirical support. The structural durable-signature approach is the primary defense, with QUIC Retry validation, resource scopes, error-rate blocklisting, and rate limiting as the layered residual defense.

Signing time: eager signing at execution was rejected in favor of lazy signing, because eager signs unconditionally for objects that are never attested.

Transport: keeping a minimal read-only HTTP surface for operations was rejected in favor of operations-over-QUIC, because the QUIC handler already exists and a second listener is not worth it.

Fee burn: removing the operational burn entirely (a 100 percent epoch model) was rejected in favor of leaving it unchanged at 30 percent, because the burn-versus-issuance question is monetary policy that has not been studied and should not be decided inside this redesign.

## Out of scope and deferred

This design does not change the consensus, the object model, or the object storage beyond adding the durable signature. It does not build the sidecar process, the separate daemon module, or the metrics endpoint. It does not build the future subscribing daemon (a daemon that subscribes to objects and keeps them synchronized locally, which would let a client ask only for signatures), although the transport and the simplified attestation are chosen to be compatible with it. It does not settle the monetary policy (the burn rate, issuance, deflation), which is deferred to a dedicated study. And it does not resolve the bootstrap question for fully anonymous clients (a relayer or sponsor tier), which is left open.
