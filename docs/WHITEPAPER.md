# BluePods: a sharded object-oriented blockchain with DAG consensus

## Abstract

BluePods is a Layer 1 blockchain designed around three core ideas: an object-oriented state model where data is split into independent, versioned objects rather than stored in global account trees; a leaderless DAG-based consensus that achieves finality in two rounds without requiring a designated block proposer; and horizontal execution sharding where each transaction is processed only by the validators that hold the objects it touches.

Together, these properties spread storage and execution across the validator set, so adding validators spreads that work rather than duplicating it on every node. Every transaction still passes through one global order, so throughput is bounded by what a single node can ingest and validate, not by the size of the validator set. The programming model stays simple: a transaction declares its inputs, calls a function, and produces outputs, much like a function call with explicit parameters.

This document describes the architecture, the reasoning behind key design decisions, and the tradeoffs involved.

---

## 1. Design principles

These are the assumptions that drive every design decision in BluePods. Some are obvious in retrospect, others were only clear after trying the alternatives.

**Objects over accounts.** Traditional blockchains store state in account-based trees where each address maps to a balance and storage. This couples unrelated data: two users transacting have to reason about the same global state root. BluePods decouples state into independent objects, each versioned and owned separately. Two transactions touching different objects never conflict, enabling natural parallelism without requiring dependency analysis.

**Explicit inputs, deterministic outputs.** A transaction explicitly declares every object it reads or modifies. This makes conflict detection trivial (compare declared versions against current versions) and eliminates the need for speculative execution or optimistic rollbacks. Any validator can determine whether a transaction will conflict without executing it.

**Compute follows data.** Instead of routing data to a fixed set of executors, BluePods routes execution to the validators that already hold the data. If an object is replicated on 10 holders, those 10 holders execute transactions involving that object. This eliminates the need to transfer large state between validators before execution.

**Protocol-level simplicity, pod-level flexibility.** Core operations like fee deduction, version tracking, and ownership checks are handled at the protocol level, outside the smart contract runtime. Smart contracts (called pods) focus on business logic. This separation keeps the protocol predictable and auditable while allowing arbitrary logic in the execution layer.

**Dual security: honest two-thirds of holders for attestation, honest two-thirds of stake for ordering.** Security in BluePods is scoped along two axes. Each object's attestation is safe under a two-thirds-of-its-holders (by count) honest majority: for a replication factor of 50, an attacker needs to corrupt 34 holders of that specific object, not 34% of the network. The global DAG order is secured separately, by stake: it is safe under a two-thirds-of-stake honest majority, because voting weight in consensus is proportional to a validator's bonded stake (Section 10). The two compose: count-weighting secures per-object attestation, stake-weighting secures ordering. In a network with thousands of validators, scoping attestation to an object's holders matters a lot.

---

## 2. Data model

### Objects

The global state of the network is composed of discrete objects. Each object is an independent unit of data with the following fields:

- **ID** (32 bytes): a unique identifier computed via BLAKE3, immutable for the lifetime of the object.
- **Version** (uint64): an integer that increments on every mutation, used for conflict detection.
- **Parent** (32 bytes plus a one-byte kind tag): the object's attachment point. Under kind `KeyRoot` the 32 bytes are an Ed25519 public key and the object is a tree root, which is what a plain owned object is. Under kind `ObjectParent` they are another object's ID and the object is nested inside that one. The 32 bytes are carried in the object's `owner` field, which the kind tag reinterprets, so a root object and an owned object are the same encoding.
- **Replication** (uint16): the number of validators that store this object, immutable after creation.
- **Content** (up to 4 KB): the application-specific payload, serialized by the pod.
- **Fees** (uint64): the storage deposit locked at creation, read-only for pods.

This model is conceptually similar to Sui's object model but diverges in how replication and execution are distributed. In Sui, all validators store all objects and execute all transactions. In BluePods, objects are stored only by their designated holders, and execution is scoped to those holders.

Why 4 KB? Because almost everything fits. A wallet with metadata is a few hundred bytes, a small record with its attributes is 1-2 KB, a DeFi position is under 1 KB. The limit also means objects can be included directly in transactions without blowing up message sizes. For larger payloads, an off-chain storage layer with erasure coding is planned, where only metadata and availability certificates would live on-chain.

### Hierarchy and cascade control

Objects form a forest. Every object hangs off a parent, and following the parent chain upward always ends at a key, because only a `KeyRoot` edge terminates a walk. That terminal key is the object's **controller**, and controlling it means controlling everything beneath it: if you control an object, you control its children, recursively. This is the Sui object-owns-object model, and it is what makes "my objects" a structure rather than a list a client has to keep for itself.

Creation is permissioned, asymmetrically. A `KeyRoot` parent may be any key: the creator pays the deposit, so rooting a new object at someone else's key is a gift, the same consent-free attach a transfer already performs, and one-transaction payments depend on it. An `ObjectParent` is restricted, because without a rule anyone could hang junk inside another controller's subtree: the parent object must be one the sender controls, one created earlier in the same transaction, or one this transaction reaches through a domain reference (the existing shared-access exemption). The check runs at commit, where every node executes it. The rule covers whichever parent a created object declares, though the Rust SDK exposes only the key form, so a pod creates roots and an object becomes nested through a reparent.

Resolving control is a walk over global metadata, not a sharded traversal. Every node holds every object's parent reference in the object tracker, exactly as it already holds every object's version, so any node answers "who controls X" from local metadata without the bodies of the ancestors, which live on their own holders. Permission resolution touches metadata; execution touches the body and stays on the holders. The walk is bounded by a depth cap, a denial-of-service guard against an adversarially deep chain rather than a functional limit, and it fails closed: a chain that cannot be resolved to a terminal key confers no control on anyone.

The edge set is authenticated. Both directions of it, child-to-parent and parent-to-children, are maintained as Merkle trees whose root is anchored in consensus (Section 5), so a client asking one node to enumerate a parent's children or to walk an object's ancestry gets an answer it can verify rather than one it has to trust.

Moving an object to another parent and destroying it are not application logic. They are declared operations carried in the transaction and applied by every node without pod execution (Section 7); a pod may choose a created object's parent, but never change an existing object's.

### Standard objects and singletons

Objects come in two flavors, determined by their replication factor:

**Standard objects** have a replication factor between 10 and several hundred. They are stored by a subset of validators (holders) determined by Rendezvous Hashing. The minimum replication of 10 guarantees that a 67% quorum is reachable with 7 holders and tolerates up to 3 simultaneous failures. Higher replication increases availability at the cost of higher storage fees.

**Singletons** have a replication factor of 0, meaning they are stored by every validator in the network. Singletons are used for data that must be universally accessible: pod code (smart contracts), system parameters, the active validator list, and gas coins used for fee payment. Developers can also create their own singletons for use cases requiring maximum availability guarantees, though the creation and modification fees are significantly higher than for standard objects since the cost scales with the entire validator set.

Singletons benefit from a key optimization: since every validator already has them locally, they are excluded from the transaction body and do not require attestation. Only their ID and expected version appear in the transaction header. This saves bandwidth and eliminates an entire collection round for singleton-only transactions.

### Versioning and conflict detection

Every time an object appears in the MutableRefs list of a successful transaction, its version increments by 1, regardless of whether the pod actually changed its content. This ensures that version tracking is deterministic and deducible from the transaction header alone, without executing the pod.

Conflict detection is straightforward: a transaction declares the expected version of each object it touches. If the current version (computed from the committed DAG history) does not match, the transaction is rejected. No locks, no two-phase commits, just an optimistic version check.

In practice, this means two transactions modifying the same object will conflict if submitted concurrently: the first to commit wins, the second gets a version mismatch and the client retries. Sui does the same thing. It is not ideal (eventually some form of batching or sequencing would be better), but for an MVP it works.

### Object references in transactions

A transaction declares its object dependencies through two lists:

- **ReadRefs**: objects read but not modified. Their version is checked but not incremented.
- **MutableRefs**: objects that may be modified. Their version is incremented upon success.

A transaction may reference up to 40 objects total across both lists. References can use either a 32-byte object ID or a human-readable domain name (see Section 3). Domain references are resolved at execution time and are exempt from ownership validation on MutableRefs, enabling shared-access patterns.

---

## 3. Domain name system

The network provides a protocol-level naming system that maps human-readable identifiers to object IDs. Instead of referencing `0x7a3f8b2c...`, developers and users can use names like `system.validators` or `myapp.config`.

### Architecture

The registry is derived state: every node computes it from the same committed transactions in the same DAG-determined order, so it rides on consensus rather than on any node's honesty. The raw name-to-leaf mapping is stored in Pebble on each validator, and a Merkle tree over the same leaves gives every answer a proof (Section 5). The registry is not an object and not a singleton. Making it a singleton was the alternative, but then every registration would read and rewrite the whole registry, with gas costs growing linearly in the number of registered names. Not worth it.

A leaf holds the name, the object it resolves to, the owner, and the lease's expiry epoch. The name rides inside the leaf rather than only in the tree position, so a proof is self-describing: a verifier who knows only the claimed name checks it against the leaf it decoded instead of trusting the server's framing. Resolution is a point lookup, answerable with an inclusion proof when the name exists and an absence proof when it does not. The registry is included in state snapshots for new validator synchronization.

### Namespaces and registration

Names are dotted, and the namespace of a dotted name is its **suffix**: the parent of `x.y` is `y`. Registering `x.y` therefore requires holding a current lease on `y`, which recursively makes the first-come-first-served rule real. Bare roots are first come, first served. The `system` namespace is reserved and closed in both directions: the bare root `system` is unregistrable, which under the suffix rule leaves every `*.system` name permanently unclaimable because no key can ever own the parent that would authorize one, and names beginning with the literal label `system.` are rejected outright. An expired namespace mints no sub-names: grace reserves the right to renew, never continued authority.

Registration and every later change to a name are declared operations carried in the transaction (Section 7), not pod output. Every node reads them from the transaction and applies them at commit without executing anything, which is what the fee architecture requires: a name's fee has to be derivable from the header alone. There are five: register a name for a term, renew it, repoint it at another object, transfer it to another owner, delete it. All checks are point lookups in the registry, so they are deterministic on every node. Register and repoint additionally require the sender to control the object being named; without that rule anyone could alias a victim's object and reach it mutably through the shared-access exemption domain references enjoy.

The leaf's owner is the registrant, or whoever a transfer handed the name to. Renewal, repointing, transfer, and deletion require the sender to be that owner. Naming a just-created object takes two transactions, since a transaction carries either declared operations or a pod call; the client library orchestrates the pair.

### Leases and expiry

A name is leased, not owned. Registration and renewal pay a rental rate multiplied by the declared term in epochs, consumed immediately into the epoch reward pool. A refundable deposit was the alternative and deters squatting not at all, since the squatter gets it back. Escrowing the rent and recognizing it epoch by epoch was the other alternative, and it would touch every leaf every epoch, rewriting the tree wholesale.

The term is capped: an expiry beyond the current epoch plus the governed maximum reverts the operation rather than being clamped to the cap. The fee is the rate times the term the header declares, so a silently clamped term would charge for epochs the lease does not get. Without the cap, one prepayment would hold a name effectively forever, which is exactly what recurring rent exists to prevent. Prepaying ahead and renewing after expiry are the same operation: a renewal runs from the later of the current expiry and the current epoch.

A name stops resolving the moment it is past its expiry epoch, in execution and in queries alike. It stays in the registry through a grace window, during which the owner alone may renew it, and the epoch boundary then sweeps out every lease past expiry plus grace. The sweep reads the committed registry and removes names in sorted order, so the set swept and the sequence of writes are identical on every node, including one that restarted mid-epoch. After the sweep the name is registrable by anyone. Deleting a name refunds nothing: a lease buys epochs, not an asset. The object a name pointed to is never affected. Applications must treat a name reference as a lease.

---

## 4. Storage distribution

### Rendezvous hashing

For standard objects, holders are determined by Rendezvous Hashing (also known as Highest Random Weight hashing). For each object, every validator's score is computed as `BLAKE3(objectID || validatorPubkey)`. Validators are sorted by score in descending order, and the top N become the holders, where N is the object's replication factor.

This approach has a critical property: **minimal disruption on validator changes**. When a validator joins or leaves the network, only a fraction of objects need to be reassigned. If a validator disappears, only the (N+1)th-ranked validator for each affected object becomes a new holder. The other N-1 holders remain unchanged. This minimizes reshuffling and synchronization traffic during epoch transitions.

Any participant can independently compute the holder list for any object using only the object ID, its replication factor, and the current validator set. The computation is purely deterministic and requires no network communication.

### Replication factor tradeoffs

The replication factor creates a direct tradeoff between availability and cost:

| Replication | Fault Tolerance | Quorum Size (67%) | Storage Cost Multiplier |
|---|---|---|---|
| 10 (minimum) | 3 failures | 7 | 1x |
| 50 | 16 failures | 34 | 5x |
| 100 | 33 failures | 67 | 10x |
| 0 (singleton) | all validators | 67% of network | Full network |

Applications choose the appropriate factor based on their availability requirements. A rarely-accessed configuration object might use the minimum of 10, while a high-value DeFi vault might use 50 or more.

---

## 5. Consensus

### The DAG structure

BluePods uses a leaderless DAG-based consensus inspired by Mysticeti. The fundamental unit is a **vertex**, a data structure produced by a single validator, split into a compact signed header and a body:

- The header: the producer, the round, the epoch, the anchored index root with the committed round it describes, and the hash of the body.
- The body: transactions with their attested objects and quorum proofs, parent links to vertices from the previous round, a fee summary, and the producer's timestamp.
- The producer's Ed25519 signature, taken over the vertex identity, which is the BLAKE3 hash of the header.

Unlike traditional blockchains where a single leader proposes a block, every validator produces vertices in parallel. This eliminates the leader bottleneck and allows the network to utilize the aggregate bandwidth of all validators simultaneously.

### Rounds and quorum

The DAG progresses through sequential rounds. A vertex at round N must include parent links to vertices from round N-1, and the round advances only once its producers carry a BFT supermajority. Quorum is stake-weighted, not a head count: the protocol sums the capped effective stake of the round's producers, taken from the epoch holder snapshot, and applies the exact integer test `3 × capped_sum >= 2 × total`. Commit weighs against that snapshot, and production weighs against the same one, so they cannot diverge across an epoch boundary. (Production's quorum check falls back to the live validator set only when no snapshot resolves for the round; that is a local gate on whether this node produces, never a decision on the committed log, so it cannot fork it. Commit stays the sole authority.) This requirement prevents the DAG from fragmenting: a validator cannot advance without acknowledging a supermajority-by-stake of the previous round's output. (A receiving node still requires at least one known-validator parent during convergence; the authoritative stake quorum is enforced where every node agrees, at production and commit.)

Voting weight is capped per validator at a fraction of the total stake, with an equal-share floor so a small set keeps a reachable two-thirds quorum; the cap keeps the order decentralized when delegation concentrates stake. Reward weight is uncapped. Stake-weighting is the only Sybil defense (Section 10), replacing the earlier equal-weight model where every validator counted as one.

Each validator produces at most one vertex per round. A liveness timer (500ms) triggers vertex production when the network is idle, ensuring continuous progress even without incoming transactions.

### Commit rule and finality

Each round has a designated anchor producer, chosen deterministically by hashing the round number over the epoch holder snapshot. Designation rotates over the committed members that held at least one vertex in committed history as observed at the snapshot's freeze, so eligibility is frozen with the holder snapshot rather than evaluated live: a validator that registered but had not produced by the freeze stays ineligible for the whole epoch that snapshot serves, and becomes designatable only at the next freeze that observes its first committed vertex. While no member has produced, at genesis, the full snapshot is eligible. The anchor is not a production leader: every validator still produces every round, and the anchor is only the pivot the commit rule keys on.

The designated producer's vertex is certified by the votes of the next round: a round-N+1 vertex supports a specific round-N vertex by listing it among its direct parents, and the vertex whose supporters carry a two-thirds-of-stake quorum of round N+1 becomes the anchor. The votes select the vertex, never a local choice, so an equivocating producer cannot make two nodes certify different vertices. The round is skipped when a quorum of round-N+1 producers cites no vertex of the designated producer at all; a vertex citing a different vertex of the same producer abstains. Both quorums weigh over the same stake snapshot, so they intersect and cannot both form. A round with neither verdict is decided indirectly by the first later certified anchor: committed if it sits in that anchor's causal history, skipped otherwise.

Committing an anchor applies its entire not-yet-committed causal history, across any number of rounds, ordered by round and then by vertex hash. The batch is a pure function of the DAG's hash links, so its membership and order are identical on every node regardless of arrival order. A vertex that arrives late is applied in the batch where committed history first references it. Admission is late, never retroactive: the committed log is append-only and a committed transaction is final and irreversible. A vertex never referenced by any later anchor is dead permanently.

The commit check runs every 50ms. Rounds are decided sequentially: the protocol stops at the first undecided round and waits for the evidence to arrive rather than skipping ahead, because every validator must process commits in the same order.

### Conflict resolution through ordering

When two transactions declare the same object in their MutableRefs with the same expected version, the DAG determines which one succeeds. The committed ordering is deterministic: if the conflicting transactions are in different vertices, commit order decides priority. If they are in the same vertex, lexicographic ordering of transaction hashes provides a deterministic tiebreaker.

The key insight is that **conflict detection requires no execution**. Every validator can compute the current version of any object by scanning the committed DAG history: for each committed transaction, objects in its MutableRefs have their version incremented by 1. This tracking is lightweight: an entry is 55 bytes in persistent storage, holding the version, the replication, the locked deposit, the parent reference with its kind tag, and the count of nested children. Everything the protocol must decide without execution, version conflicts and cascade control alike, is decided from those bytes.

### The detached vertex header

Anchoring a root in every vertex is worth nothing if verifying the anchor means downloading the vertex. The header is therefore detached from the body, and it is what the producer signs.

The header is a fixed-width 120-byte string: the producer's public key, the round, the epoch, the committed round the anchor describes, the 32-byte index root, and the 32-byte hash of the body. The vertex identity is the domain-separated BLAKE3 of those 120 bytes, and the signature is taken over that identity. Parent links and store keys point at the identity, which commits to the body through the body hash, so a full node recomputes the body hash and binds the body it received back to the signed header, while a verifier that only wants the anchor needs the header and the signature and nothing else. That is 184 bytes per validator, which is what makes a quorum of them cheap enough to hand to a light client.

The layout is normative rather than incidental. It carries no lengths and no separators, and the two hashes are domain-separated by a one-byte tag each, so that a byte string read as a header and the same byte string read as a body cannot produce the same digest. Any external verifier reproducing it must match it byte for byte: a reordered field or a dropped tag does not fail loudly, it silently disagrees with every node on the network.

The epoch field carries the producer's own epoch, which tells a verifier which validator set the header belongs to. It is the producer's own word, so it is not taken at face value: ingress accepts it only within one epoch either side of the epoch the anchored frontier itself commits under, and a serving node assembling a bundle derives the epoch from that frontier rather than reading it off a header, since a producer must not be able to misname the tree its own quorum is weighed by.

### Anchoring the index root

Each producer embeds in its header the root of the verifiable index at its own committed frontier, together with that frontier's round. Four Merkle trees stand behind that one value: the domain registry keyed by name, the parent edge of every object keyed by child, a two-level map of each parent's children, and the epoch's validator set with its capped stakes. Each is a sparse Merkle tree over BLAKE3 key positions with empty-subtree compression, so its root is a pure function of its key-value set with no insertion order to agree on and no rebalancing, inclusion and absence cost the same, and a client reading one node's answer verifies it against the root instead of trusting the node. The four sub-roots fold into one combined root in a fixed order, which leaves room for further indexes behind the same anchor.

The validator tree is rebuilt at each epoch boundary from the same snapshot that freezes the holder set, so the membership a light client weighs a quorum against is exactly the membership consensus itself uses.

The root covers committed state only, because a producer cannot know the committed order of its own in-flight round. That is a structural lag of about two rounds, not a tunable delay. The root is computed incrementally: a committed batch rehashes only the paths its transactions touched.

The marginal cost is 32 bytes per vertex plus that rehash, and one honest cost besides. Deriving a vertex's identity re-serializes the whole body canonically, so it scales with the vertex rather than being a constant: on a loaded vertex, a full round of parent links and a batch of attested transactions, the re-derivation measures a small multiple of the Ed25519 verification the same vertex already pays, not a rounding error next to it. It is accepted, because binding the signed header to the body is not an optimization on top of the anchor, it is the correctness property the anchor rests on.

The whole index is derived state. No node is trusted to compute it honestly, because no node is asked to: it is a deterministic function of the committed transaction stream, which is exactly why anchoring it in consensus requires no new signing step and no new trust assumption.

### Enforcing the anchor

A producer that lies about the root would fork the index. Three stages make the lie worthless, following the same doctrine as the rest of the protocol: ingress validates what a receiver can reliably check at that moment, and convergence-sensitive checks are enforced where every node agrees, at production and at commit.

**Ingress.** A vertex anchoring a frontier at or below the receiver's own committed frontier is checked against retained history, a 32-byte compare. A mismatch is quarantined, not rejected: the vertex is stored and served on request, and withheld only from relay and from reference. Terminal rejection would be a partition lever, because a vertex some nodes refuse to store, smuggled into committed causal history through references from nodes that could check nothing, leaves the refusing nodes unable ever to complete that batch. Any node's causal batch must stay completable from its own store, so storage is not where a lie is punished. A vertex anchoring a frontier the receiver has not reached yet is accepted as it stands; blocking the door on it would couple vertex acceptance to commit lag and stall the DAG under ordinary skew. A zero root is tolerated only during the genesis epoch, where no index exists yet.

**Production.** A producer never references a parent whose anchor it has proved wrong. The filter is a denylist of proven liars rather than an allowlist of verified anchors, and the difference is liveness: a peer that commits a round ahead anchors a frontier above this node's own, so its perfectly honest vertex is unverifiable here, and an allowlist would silence whichever node happened to lag. A wrong root is therefore never referenced by an honest node able to check it, and reaches committed history only through nodes that could not.

**Commit.** As the commit cursor passes a vertex, its anchor is re-checked against a history that has grown since the vertex arrived, so a lie the first two stages could not see is caught here, and caught on every node, since committed history is identical everywhere. It convicts, it does not correct: the vertex stays committed, because rolling it back would make the committed log depend on each node's index retention, which is the divergence the anchor exists to detect. What comes out is self-contained evidence, the producer's signed header next to the deterministic recomputation, which a third party checks with no access to the node that recorded it. It is logged today and becomes slashable material when slashing lands.

There is consequently no index-specific slashing to build. A lie is never referenced by a node able to check it, it cannot fool a client who requires a stake quorum a minority cannot forge, and if it reaches committed history it convicts its author with his own signature everywhere. Root history retention is a bounded window of recent rounds plus one checkpoint per epoch boundary, enough to judge lagging producers and to serve clients. Quarantine marks and fault records are node-local by construction and deliberately outside every convergence-checked structure: whether a node could disprove an anchor depends on how far its own commit had advanced, so two honest nodes legitimately hold different sets and must still agree on state.

### Verified reads

A client reads the index from one node and verifies the answer itself. It asks that node to resolve a name, list a parent's children, or walk an ancestry; it gets the value, a Merkle proof, and an anchor bundle, which is a set of producer-signed headers all reporting the same committed round and index root and together carrying a stake quorum. It then checks locally that the headers are signed by validators of the right epoch, that they reach the quorum, and that the proof folds to the root they attest. The serving node assembles the bundle once per frontier from vertices it holds anyway and caches it, so one bundle serves every query at that frontier.

The validator tree closes the circularity of "which validators weigh the quorum". An epoch's attested root commits to that epoch's validator set, so a client that trusts one checkpoint walks forward across boundaries by reading each new set out of the index with a proof, the standard light-client pattern. At a boundary the sets overlap by construction, since churn is capped, and the client only re-pins to a new epoch when that epoch's own headers carry its committee's quorum, never on the strength of a minority: one byzantine member must not be able to drag a client's trust forward by a whole epoch.

Bootstrap trust is a checkpoint obtained out of band, which is standard weak subjectivity, stated here rather than assumed silently. A light client pins an epoch, an index root, and the hash of that epoch's validator set. The load-bearing component is the set hash: it authenticates the committee the first quorum is weighed by, and every later committee comes out of an attested root. The pinned index root serves first contact, when the serving node still holds that root; once the chain has moved past it, which is the ordinary case, the client falls back to the set hash and checks proofs against a fresh attested root, never against the stale pin. Persisting the walked-forward checkpoint between runs is what saves a returning client from needing a fresh out-of-band pin, and a client that has fallen more than one boundary behind the handoff window needs one, which is the weak-subjectivity boundary rather than a defect to paper over.

Freshness is the client's explicit choice. The anchored root covers committed state and trails the live tip by the structural lag, so a client that just saw its transaction finalize either waits for a bundle at or past that frontier, which takes the couple of rounds producers need to anchor it, or takes the node's unproven live answer. The client library exposes exactly those two, and nothing in between.

A proved read and an anchor bundle rarely name the same round, since the bundle serves the highest frontier a quorum has been observed for and a query answers at the serving node's own. What has to match is the root, not the round: most committed rounds change no index entry, so the two roots are usually the same 32 bytes, and the client waits only when the index has genuinely moved.

### Bootstrap and convergence

When the network starts or new validators join, a grace period relaxes the quorum requirements:

- **Bootstrap mode**: the first validator produces vertices alone with a relaxed quorum of one producer. Its self-stake is seeded at genesis, so its weight is non-zero.
- **Transition grace**: after reaching the minimum validator count, 20 rounds of relaxed quorum allow the network to converge.
- **Transition buffer**: 10 additional rounds with relaxed quorum provide a safety margin.
- **Strict regime**: the switch is deterministic, never a function of a node's local join timing. The committed round that crosses the minimum validator count fixes a strict-start round (that round plus the grace and buffer windows), persisted as a monotone latch and carried in sync snapshots, so every node relaxes and tightens at the same committed position. At the latch, the committed registration set is frozen with its stakes as the genesis holder snapshot that weighs anchor designation and quorums until the first epoch boundary. During relaxed rounds the anchor candidate is still selected by the votes, and an absent producer is never blamed; the strict blame rule applies from the strict-start round on.

---

## 6. Attestation and aggregation

### The problem with broadcast voting

In traditional BFT consensus, validators broadcast their votes to the entire network. With N validators, each vote generates N messages, and each round generates N² total messages. At 200 validators this is manageable (40,000 messages). At 2,000 validators it becomes untenable (4 million messages per round).

### Direct collection, off-chain

BluePods replaces broadcast voting with **direct collection**, and the collection runs off-chain in a client daemon rather than on a validator. The daemon contacts only the holders of the referenced objects, not the entire network, over QUIC. With a typical replication factor of 50, this means 50 direct messages instead of thousands of broadcasts.

The daemon's role is purely coordinative: it cannot forge attestations because it does not possess the holders' private keys. A validator never trusts the daemon and reverifies every attested transaction on receipt. If a holder is unreachable, the daemon collects from the others; if the quorum becomes impossible, it fails fast and the client retries.

### BLS signatures and stored attestations

Each holder attests to an object's state by signing `H = BLAKE3(content || version_u64_BE)` over the object's content bytes with its BLS key. BLS keys are derived deterministically from the validator's Ed25519 seed: `BLAKE3("bluepods-bls-keygen" || ed25519_seed)`.

The signature is deterministic: it is identical for every requester for as long as the object stays at that version. A holder therefore computes it once, eagerly, at execution time, and stores it next to the object. An attestation request is then a pure read of stored bytes. There is no designated top-ranked holder and no full-object response: every holder answers with the same hash and its stored signature (32 + 96 bytes), and a holder that does not hold the object, or holds it at a different version, answers with a static error and signs nothing. Getting the object content is a separate request the daemon makes to any holder it chooses.

Once the quorum is reached (67% of holders signing the same hash), the daemon combines the individual BLS signatures into a single **aggregated signature** of 96 bytes, accompanied by a bitmap indicating which holders signed. This aggregated proof is compact and verifiable by any validator.

### Quorum validation and fail-fast

The per-object attestation quorum threshold is **(replication × 67 + 99) / 100**, implementing a 67% requirement with integer arithmetic, counted by head: one holder, one vote. (This is distinct from the consensus quorum used to commit vertices, which is two-thirds of stake, not of head count.)

The daemon implements fail-fast: as soon as enough negative responses accumulate to make the quorum mathematically impossible, it abandons the collection immediately. For example, with 50 holders and a quorum of 34, receiving 17 negatives means only 33 positives are possible, so the collection is abandoned without waiting for the rest.

### Singleton optimization

Transactions involving only singletons skip the entire attestation phase. Since every validator already stores every singleton, there is nothing to collect or attest. The transaction is included directly in a vertex and propagated via gossip. This optimization benefits system transactions (staking, parameter updates, validator registration) which primarily interact with singletons.

---

## 7. Transaction lifecycle

A complete transaction follows these stages from submission to finality:

### Submission

A transaction that touches only singletons is submitted raw to any validator over QUIC, which wraps it into a trivial attested transaction with no objects and no proofs. A transaction that touches replicated objects is collected off-chain by the client daemon and submitted as a full attested transaction. The transaction is serialized in FlatBuffers for compact binary encoding with zero-copy field access. Maximum transaction size is 1 MB. Each transaction is uniquely identified by its hash, computed as `BLAKE3` over the canonical unsigned content.

The transaction header declares: sender public key, pod ID, function name, Borsh-serialized arguments, ReadRefs, MutableRefs, created object replications, max gas budget, gas coin ID, declared operations, and an Ed25519 signature. A sponsored transaction additionally carries a `fee_payer`, a `sponsor_signature`, and a `valid_until` epoch (see Section 9). These three fields are absent-when-empty: a non-sponsored transaction omits them and serializes byte-identically to one built before sponsorship existed, so its single-sender hash and signature verify unchanged. Declared operations follow the same rule.

### Object collection

The daemon refreshes the live epoch and holder set before collecting, so the attested transaction it builds carries the current attestation epoch rather than a stale one cached at startup. It then identifies holders for each standard object via Rendezvous Hashing and contacts them in parallel over QUIC. Singletons are skipped, since every validator has them locally and they are never attested. Upon reaching quorum for all replicated objects, the daemon assembles the attested transaction (ATX) with the collected objects, aggregated BLS signatures, signer bitmaps, and the epoch the attestation was collected in, and submits it to any validator.

### Vertex production and gossip

The receiving validator reverifies the ATX, adds it to its pending set, and forwards it to mesh peers over the one-way gossip stream. The forwarded transaction is tagged so a peer tells it apart from a vertex (both are FlatBuffers on the same stream) and adds it to its own pending set rather than misparsing it; without the tag the transaction would be read as a malformed vertex and dropped, stranding any submission that did not land directly on a producer. The validator then includes the pending transaction in its next vertex. The vertex is gossipped with a production fanout of 40 peers. Relaying validators forward with a reduced fanout of 10 to prevent amplification. With these parameters, a vertex reaches the entire network in approximately 3 hops: first hop reaches 40 validators, second hop reaches ~400, third hop covers the rest.

### Consensus and commit

The vertex enters the DAG. When referenced by vertices two rounds later by a two-thirds-of-stake quorum, it is committed. All transactions in the vertex become final.

Before a committed transaction takes effect, each node re-verifies its authenticity in the commit path, deterministically and on every node: it recomputes the canonical body hash, checks it against the declared hash, and verifies the sender's Ed25519 signature (and, for a sponsored transaction, the sponsor's signature) against that hash. This matters because a transaction can reach commit through a gossiped vertex without ever passing the local ingress check of the node that commits it. Validating only at ingress would let a relaying node inject a forged transaction inside an otherwise valid (producer-signed) vertex; enforcing authenticity at commit, where every node agrees, closes that gap. The check runs after the attestation-proof verdict is consumed and before the duplicate guard, so it neither desyncs proof verification nor lets a forged hash censor a legitimate transaction.

Because forwarding places a transaction in several validators' pending sets, the same transaction can appear in more than one committed vertex. The commit path is therefore idempotent per transaction: each validator records the hashes it has already processed and skips any repeat, so object creation, fee deduction, and validator-set changes apply exactly once. The hash covers the mutable references and their versions, so a genuine retry against a newer version is a distinct transaction and is not mistaken for a duplicate. Replay of a sponsored transaction is prevented by the same duplicate guard, now keyed on a commit-verified hash, bounded further by `valid_until`.

### Attestation epoch validity

Holders change at epoch boundaries, so an attested transaction carries the epoch its attestations were collected in. At commit, each validator recomputes the quorum against the holder snapshot of that epoch, but only after validating the epoch against the round the transaction commits at. The deterministic commit epoch is always accepted; the immediately preceding epoch is accepted only when the commit round still falls within a fixed grace window after the boundary, so an attestation collected late in an epoch and committed shortly into the next still verifies. An attestation that lands outside this window is rejected, and the client recollects against the current epoch and resubmits. The grace window is sized so that, at production epoch lengths, an attestation always commits well within it; only artificially short epochs make the window tight.

### Fee deduction

After commit, the protocol computes fees from the transaction header and deducts them from the sender's gas coin. This deduction is implicit: it does not increment the gas coin's version, allowing multiple in-flight transactions from the same sender without version conflicts on the gas coin. Fees are always deducted, even if the transaction subsequently fails.

### Version check and ownership validation

Each validator checks that the declared versions match the versions computed from the DAG history. If any version mismatches, the transaction is marked as failed without execution.

A transaction carrying declared operations is routed out here, before the ownership check and instead of it: those operations answer to the cascade control model, resolved by walking parent metadata, not to the object's parent bytes read directly.

For a pod call the protocol verifies that the sender owns every object in MutableRefs, comparing the sender against the object's parent bytes, which reach it from the source every node reads identically: a replicated object's from the attested copy carried in the transaction, whose BLS quorum proof binds it, a singleton's from local content every node holds. A pod call therefore reaches only objects rooted directly at the sender's key; a nested object is mutated by first reparenting it out, which is a declared operation. Domain references, identified by name rather than ID, are exempt from the check, enabling shared-access patterns through the naming system.

### Declared operations

Some effects must be seen by every node without execution, and pod output cannot carry them: only holders run a pod, so most of the network would never observe them. Reassigning an object, destroying it, and the whole domain lifecycle are therefore **declared operations**, listed in the transaction itself and applied here, in place of execution.

The list is covered by the canonical body hash, so the sender's signature, and the sponsor's when there is one, both bind it: neither party signs a list the other can change afterward. Every node reads the list at commit and applies it deterministically, exactly like version increments. There are seven kinds: reparent an object, where a transfer is a reparent to a key, delete an object, and register, renew, repoint, transfer, or delete a name.

**A transaction carries either declared operations or a pod call, never both.** This is what keeps each transaction's semantics atomic without asking a non-holder to observe a pod revert: a reverted pod call cannot strand a half-applied reparent, because the two cannot share a transaction. A mixed transaction is rejected at ingress, and rejected again at commit, so a submission that slipped past ingress fails deterministically everywhere rather than half-applying.

The list is all or nothing. Each operation validates against the state the previous ones in the same list would produce, so a transaction can register a namespace and a name under it in one shot, and the first failure rejects the whole list with no effect. Nothing is written until the entire list is proven valid, because a settled deletion, with its supply burn and deposit release, cannot be undone. Fees are paid regardless, as everywhere else.

The protocol enforces, from tracker metadata alone: the sender controls the object, through the cascade walk of Section 2; a reparent onto another object requires that object to be sender-controlled too, while a reparent onto a key may target any key, which is precisely what a transfer is; the new edge closes no cycle; and a deleted object has no children, so a subtree is dismantled from the leaves or its children are reparented away first. An object-targeted operation must name its object in MutableRefs, so it increments the object's version and fails cleanly on a version mismatch like any other mutation. Domain operations reference no object and bump no version: the name's leaf is their only state.

Leases are measured against the epoch the transaction's **commit round** belongs to, the same deterministic mapping attestation-proof verification uses, never a node's live epoch counter, which would date a lease by when that node happened to reach the round.

### Execution

If versions and ownership are valid, each holder of at least one MutableRef object calls `execute()` on the target pod. Since all holders receive the same inputs (objects attested by quorum), they compute the same output deterministically. The pod returns updated objects, created objects, deleted objects, and logs.

The output is bounded by what only holders can see. A pod may set the parent of an object it creates, subject to the creation rule of Section 2, since creating objects forces every validator to execute the transaction anyway; it may never change the parent of an existing object, and an updated object whose parent bytes differ from the attested input's is rejected on a purely local compare. Deletion survives in pod output only where execution is global, meaning the transaction creates objects or all its mutable references are singletons, so every node observes the deletion. That is what keeps the system pod's `merge` working. A sharded object is deleted only through the declared operation.

### Post-execution

Object versions in MutableRefs are incremented. Created objects receive deterministic IDs computed as `BLAKE3(tx_hash || index_u32_LE)` and are stored by their respective holders (computed via Rendezvous Hashing). This eliminates the need for a separate object creation transaction, so finality is achieved in 2 rounds instead of 4. A deletion, declared or from the pod-output carve-out, settles the same way: the storage deposit is released, 95% refunded to the gas coin of the deleting transaction and 5% burned, and the object leaves the tracker and both hierarchy trees.

When a validator becomes a new holder of an existing object (due to an epoch change or another validator departing), it recovers the object from the remaining holders via the routing mechanism. The DAG contains the trace of the transaction that created the object, allowing verification of authenticity.

---

## 8. Execution model: pods

### WASM runtime

Smart contracts in BluePods are called **pods**. Each pod is a WebAssembly module executed via the **wazero** runtime (a pure-Go, zero-dependency WASM implementation). Modules are compiled once and cached in a pool. Each execution creates an isolated instance with fresh memory, ensuring no state leaks between transactions.

Pods are singletons: their code is replicated on every validator, ensuring any validator can execute any transaction without fetching code from peers.

### Host interface

The WASM sandbox exposes four host functions:

| Function | Signature | Purpose |
|---|---|---|
| `gas` | `(cost: u32)` | Declares gas consumption. Aborts execution if the budget is exceeded. |
| `input_len` | `() → u32` | Returns the size of the input buffer. |
| `read_input` | `(ptr: u32)` | Copies input data into WASM memory. |
| `write_output` | `(ptr: u32, len: u32)` | Copies output from WASM memory to the host. |

Four functions. That is the entire attack surface. The pod cannot touch the filesystem, the network, or the system clock. It reads input, runs logic, writes output. Nothing else.

### Input and output

The input is a `PodExecuteInput` serialized in FlatBuffers, containing the sender's public key, function name, Borsh-serialized arguments, and all referenced objects (both mutable and read-only, resolved by the protocol before execution).

The output is a `PodExecuteOutput` containing:

1. **Updated objects**: modified versions of MutableRef objects, with their parent reference unchanged.
2. **Created objects**: new objects, each with a replication factor declared in the transaction header via the `created_objects_replication` vector, and a parent the creation rule authorizes.
3. **Deleted objects**: objects to remove from the state, accepted only in a globally executed transaction (Section 7).
4. **Logs**: debug messages emitted by the pod.
5. **Error code**: 0 for success, non-zero causes a revert (fees still deducted).

There is no domain output. Binding a name is a declared operation, so a pod output that declares one reverts the transaction.

### SDK and developer experience

A Rust SDK (`pod-sdk`) provides abstractions over the raw host interface:

- `dispatcher!` macro for routing to handlers based on function name.
- `Context` type giving access to sender, deserialized arguments, and local objects.
- `ExecuteResult` with chainable builders: `ok()`, `err(code)`, `with_updated()`, `with_created()`, `log()`.

If you have written Solidity or Move, the model will feel familiar: a function receives inputs, performs logic, and returns state changes. The difference is that objects are explicit parameters: you declare what you are going to touch upfront, rather than reaching into a global state tree.

### Gas metering

Gas metering is implemented through WASM instrumentation: a separate tool (`wasm-gas`) injects calls to the `gas()` host function into the pod's bytecode before deployment. This ensures metering is transparent to the developer and cannot be bypassed.

The default gas budget is 10,000,000 units per transaction. If execution exceeds this budget, it is immediately aborted and the transaction reverts. Fees are deducted regardless. Otherwise, an attacker could spam intentionally-failing transactions and consume validator resources for free.

### System pod

The system pod is the foundational smart contract of the network. It exposes functions covering basic financial operations, object management, staking, and validator management:

| Function | Description |
|---|---|
| `split` | Divides a Coin into two (original balance reduced, new Coin created) |
| `merge` | Combines two Coins into one |
| `create_object` | Creates a replicated, owned object holding arbitrary content |
| `set_object` | Overwrites the content of an owned object |
| `register_validator` | Registers a new validator on the network |
| `deregister_validator` | Schedules a validator for removal |
| `bond` | Locks a validator's coin as self-stake |
| `unbond` | Releases self-stake back to a coin |
| `delegate` | Stakes a delegator's coin behind a validator (creates a stake position) |
| `undelegate` | Releases a delegation, returning principal plus compounded reward |

There is no transfer entrypoint either, for coins or for objects. Handing an object to someone else is a reparent onto their key, which is a declared operation the client builds directly (Section 7), so a pure transfer executes no WASM at all and is faster and cheaper for it.

There is no `mint`. Creating balance from nothing would be an unbacked supply printer; the only token creation is genesis seeding and protocol issuance (Section 10). The faucet on a test network is a `split` from a genesis-allocated reserve coin, not a mint.

Coins follow a minimal structure: a single `balance` field of type uint64, serialized in Borsh (8 bytes). There is no reason to put complex financial logic in the system pod. That is what application-level pods are for.

---

## 9. Fee system

### Design rationale

The fee system looks simple on the surface, but two decisions behind it took a while to get right:

**Fees are at the protocol level, not inside pods.** If fees were deducted by the system pod, the gas coin (a singleton) would need to be executed by every validator for every transaction. This would destroy execution sharding: the system pod would become a bottleneck on 100% of transactions. By moving fee deduction to the protocol layer, it becomes a simple arithmetic operation computed from the transaction header, with no pod execution required.

**Fees are based on declared max_gas, not actual gas_used.** The gas coin is a singleton. All validators store it and must agree on the amount deducted. But only holders of the mutable objects execute the transaction, and non-holders do not know the actual gas consumed. If fees depended on gas_used, either all validators would have to execute (killing sharding) or non-holders could not update the gas coin correctly. Using the declared max_gas makes the fee deterministic from the header alone. Pods that consume less gas simply declare a lower max_gas and pay less.

### Fee components

Each transaction pays four types of fees:

```
total = max_gas × gas_price × replication_ratio
      + standard_objects_in_ATX × transit_fee
      + Σ(storage_fee × effective_rep(replication_i) / total_validators + index_entry_fee)
      + Σ declared_operation_fee(op)
```

Where:

- **Compute**: proportional to the declared gas budget and the fraction of validators that execute. A transaction that carries only declared operations runs no metered code, so it pays a flat `min_gas × gas_price` instead of its declared budget, and its operations are priced individually below.
- **Transit**: a flat fee per standard object included in the transaction body (singletons excluded).
- **Storage**: per created object, a share of the flat 4 KB rate weighted by replication, plus the flat `index_entry_fee` its hierarchy-index entry costs every node.
- **Operations**: per declared operation. Reparent and delete pay a flat fee, since each grows or shrinks global state by a fixed amount. Register and renew pay `rental_rate × term_epochs`, the rent for the lease. The remaining domain kinds rewrite a leaf that already exists and pay nothing beyond the compute floor.

`effective_rep(r)` equals `total_validators` for singletons (r=0) and `r` for standard objects. `replication_ratio` is the fraction of validators that execute the transaction, computed as the union of holders across all mutable objects.

Every term is derivable from the header alone, which is what lets ingress, commit, summary production, and summary validation all reach the same number without consulting state. Operation fees are consequently charged whether or not the operation applies: the fee is fixed at ingress, long before commit decides validity, exactly as a reverted pod call still pays the gas it declared. Rent is read from the declared term, never from the expiry the operation would produce, which is why a term past the cap reverts instead of being clamped.

### Current constants

| Constant | Value | Description |
|---|---|---|
| `gas_price` | 1 | Price per gas unit |
| `min_gas` | 100 | Minimum gas per transaction (anti-spam), and the flat compute of a declared-operation transaction |
| `transit_fee` | 10 | Per standard object in the transaction body |
| `storage_fee` | 1,000 | Per created object (flat 4 KB rate) |
| `index_entry_fee` | 25 | Per created object, for its hierarchy-index entry |
| `reparent_fee` | 100 | Per declared reparent |
| `delete_fee` | 100 | Per declared delete |
| `rental_rate` | 100 | Per epoch of a domain lease |
| `max_term_epochs` | 256 | Cap on how far past the current epoch a lease may run |
| `grace_epochs` | 8 | Window past expiry in which only the owner may renew |

These numbers are placeholders. The real values will come from mainnet observation. There is no way to get fee constants right without live traffic. The lease cap and the grace window are governed alongside the fee constants deliberately: the cap that reverts an operation and the price of the term it allows come from the same frozen parameters, so a parameter change cannot move them apart.

### Distribution

A fee has two parts that are accounted differently. The consumed part (compute, transit, and the declared-operation fees, domain rent included) goes entirely to the epoch reward pool. The storage part is not a fee at all but a refundable deposit: it is locked in the created object's `fees` field and never pooled (see Storage Deposits and Refunds below).

| Destination | Share of consumed fee | Timing |
|---|---|---|
| Epoch rewards | 100% | Accumulated and distributed at epoch boundary |

One hundred percent of consumed fees go to validators. There is no scarcity burn. The native token is utility-first and stability-oriented (Section 10), and a burn creates appreciation pressure that works against that goal; the token is left mildly inflationary instead. The earlier 70/30 split (and the off-chain aggregation design's 70/30) is superseded.

The burn once carried a second, anti-gaming role: making it harder for validators to profit from manufacturing fee traffic. That role is now covered structurally rather than by a burn. Reward weight is `effective_stake x liveness` (Section 10), and neither term scales with manufacturable activity: stake requires bonded capital, and liveness is capped at one vertex per round. Self-dealing fee traffic earns nothing extra, so the burn is no longer needed to deter it.

Aggregation moved off-chain to the client, so there is no aggregator role and no aggregator share; the work that used to earn it is no longer done by a validator. Integer rounding remainders go to the epoch pool.

### Fee summary verification

Each vertex contains a pre-computed `FeeSummary` with fields `total_fees`, `total_burned`, and `total_epoch`. Every receiving validator recalculates this summary from the transaction headers and rejects the vertex if it does not match. The summary is computed over the consumed portion only; the storage deposit is locked in the object, not summarized. This summary serves as a cache for epoch reward distribution: instead of re-scanning all transactions at epoch boundary, validators sum the `total_epoch` fields across committed vertices.

Since the scarcity burn was removed, `total_burned` is always zero. The field is vestigial and soft-deprecated: it is kept in the schema so non-sponsored transactions and existing vertices serialize byte-identically, but it carries no value.

### Storage deposits and refunds

Every created object locks a storage deposit in its `fees` field, computed as `storage_fee × effective_rep(replication) / total_validators + index_entry_fee`. The deposit is debited from the gas coin at creation but is never pooled: it stays locked in the object as a deposit, not a fee. The debit at creation and the amount stamped on the object go through the same shared function reading the same live validator count, so the debited storage equals the stamped deposit and total supply is unchanged at creation.

The index term is there because a hierarchy entry is genuine global state, held by every node, so it cannot be free. Pricing it as a full 4 KB object would overcharge it by an order of magnitude, hence a flat constant sized for what the entry actually costs at full replication.

A deposit is locked only against a coin that was actually debited. A fee-exempt transaction is the exception: `register_validator` and `deregister_validator` carry no gas coin (Section 10), so nothing is debited and the objects they create, such as the Validator singleton a registration produces, lock a zero deposit. This keeps the locked deposit equal to what was paid, so a registration leaves total supply and the supply identity (Section 10) exact rather than inflating the deposits term by an unpaid amount.

A deposit is also locked only when the object is actually created. A created-object transaction pays its storage portion up front, before execution, and fees are always deducted even when a transaction later fails (Section 7). When such a transaction's pod execution fails, no object is created, so nothing locks the already-debited storage portion; it is pooled into the epoch reward pool like the consumed part instead. The full declared fee is still charged and fully accounted, so the supply identity (Section 10) stays exact rather than leaking the storage component.

On deletion, 95% of the deposit is refunded and 5% is burned, index term included. The burn prevents spam through rapid creation/deletion cycles, and it applies to index entries for exactly the same reason it applies to bodies, since a creation and deletion cycle churns the trees too; the burned remainder leaves total supply (Section 10). The refund follows the gas coin of the delete transaction: a self-paid delete refunds the owner, and a sponsored delete refunds that delete's sponsor. The 5% deletion burn is the only burn in the protocol.

### Gas coin mechanics

The gas coin is a Coin singleton (replication=0) owned by whoever pays the fee. It is referenced in a dedicated `gas_coin` field in the transaction header, separate from the business-logic inputs. This separation ensures that fee deduction does not interfere with the transaction's object references. At commit the protocol checks that the gas coin's owner is the sender, or the `fee_payer` for a sponsored transaction.

Gas coin modifications are **implicit protocol operations**: they change the balance but do not increment the version. This is critical: it allows a sender to have multiple transactions in flight simultaneously without version conflicts on their gas coin. Fee deductions are applied sequentially in DAG-committed order.

### Sponsored transactions

A zero-balance new user cannot make a first transaction, because every value-bearing transaction needs a funded gas coin owned by the payer. The fix is native sponsored transactions, done the protocol-level way (a fee payer baked into the transaction) rather than layered on top in a separate account-abstraction contract.

A sponsored transaction carries a `fee_payer` (the sponsor's public key), a `sponsor_signature`, and a `valid_until` epoch. The sender and the sponsor sign the same canonical body hash, which covers everything except the two signature fields, so neither field can be swapped after the other signs. At commit, the protocol verifies the sponsor signature against `fee_payer` and requires the gas coin to be owned by `fee_payer` instead of the sender. The sponsor's exposure is bounded: it signs only what it agrees to pay, the chain cannot overdraw its coin, and `valid_until` (checked against the commit epoch, denominated in epochs and so clock-free) kills a stale sponsorship. A sponsored transaction must carry a non-zero `valid_until`. The honest caveat is that a sponsored transaction that fails on a version conflict still charges the sponsor's gas, and a holder of the signed artifact can submit it at an inconvenient moment within the window; the sponsor prices this and bounds it with `valid_until`.

---

## 10. Validator management

### Epochs

The network operates in epochs of configurable length (measured in consensus rounds). At each epoch boundary, the active validator set is frozen into a snapshot called `epochHolders`, which determines storage distribution and Rendezvous Hashing for the entire epoch. Changes to the validator set (additions, removals) only take effect at the next epoch boundary.

Genesis is initial state, not transactions. The initial coin allocation, the founding validator set, and the founder's bonded self-stake are the ledger's starting state, seeded directly at chain creation rather than injected as fee-less transactions. The founder's self-stake is locked out of its genesis coin so its bonded weight has real backing, which the stake-weighted quorum needs to reach quorum from the very first round.

The genesis epoch is the exception: it holds no frozen snapshot. Its set is still forming as the founding validators register, so freezing it at process startup would capture a partial, per-node-divergent membership, and attestation verification would then recompute holders against a set the client daemon never collected from. The genesis epoch therefore tracks the live validator set, which converges to the same set the daemon syncs. The first frozen snapshot is taken at the first epoch boundary, where the set is already stable.

### Epoch transitions

At each epoch boundary, the protocol executes the following steps in order:

1. **Issuance and reward distribution**: the thermostat (when enabled) adjusts the per-epoch issuance rate and mints into the reward pool, then the pool (accumulated consumed fees plus issuance) is distributed to validators by `effective_stake x liveness`. Distribution runs before removals so an outgoing validator still receives its share.
2. **Removal application**: pending validator removals are applied, subject to churn limiting.
3. **Validator set snapshot**: the current set is frozen as `epochHolders` for the new epoch.
4. **Counter reset**: epoch fees, round production counters, and pending additions are cleared.
5. **Epoch increment**: the epoch counter advances.

### Registration and deregistration

Validators join by submitting a `register_validator` transaction containing their Ed25519 public key, QUIC address, BLS public key (48 bytes), and a proof of possession of that BLS key (96 bytes). The proof is a BLS signature over the Ed25519 public key followed by the BLS public key, made under the proof-of-possession domain tag of the ciphersuite, which is distinct from the tag attestation signatures use so neither can be replayed as the other. It is verified in the commit path on every node: a registration claiming a BLS key without a valid proof fails there and joins nothing (Section 12). A validator publishes only a QUIC address; there is no on-chain HTTP address. They are added to the active set and tracked in the epoch's additions for churn limiting.

`register_validator` and `deregister_validator` are the one narrow exception to the rule that every value-bearing transaction needs a funded gas coin. They move no value and carry zero quorum weight until the validator bonds, and a node joining at genesis holds no coin yet, so requiring gas would be a chicken-and-egg. They are still authenticated (signed by the sender, verified at commit). The exemption is revisited once bonding lands: a registrant that must bond already needs a funded coin.

Deregistration is a two-phase process: a `deregister_validator` transaction places the validator in a pending removal list, but the validator remains active until the next epoch boundary. This ensures no disruption mid-epoch.

### Churn limiting

To maintain network stability, the number of validators added or removed per epoch is capped. When pending removals exceed the limit, they are sorted by public key (for deterministic ordering across all validators) and only the first N are processed. Excess removals are deferred to the following epoch. The same logic applies to additions.

### Staking and bonding

Stake is the network's only Sybil defense: it gives a validator its voting weight in consensus and its weight in the reward, and (once slashing lands) it is the slashable collateral. Stake is a field on the validator's record, not a flag on coins. Bonding debits a coin the validator owns and credits its self-stake; unbonding reverses it. A validator's `effective_stake` is its self-stake plus the stake delegated to it.

A minimum self-stake is required to register and remain a validator. It is a governed parameter, calibrated against the validator-set size (which in turn sets per-object holder-set sizes), and it serves as both anti-spam and skin in the game. An unbonding period, during which withdrawn stake stays locked, is tied to fraud-detection latency and is finalized together with slashing; for now unbonding returns the stake without delay.

### Delegation

Most of the supply on an adopted cloud is user working capital, not validator stake. Without a way for that broad base to stake idle balances, the staking ratio is structurally capped and the monetary policy below saturates. Delegation lets any holder stake behind a validator they trust.

- **Positions.** Each delegation is a stake-position object owned by the delegator, recording the chosen validator and the amount. A validator maintains a `delegated_total` aggregate (used for `effective_stake`, the voting cap, and `total_bonded`).
- **Fixed commission.** The validator's cut of delegated rewards is a single governed parameter (10% to start), not a per-validator rate. This avoids commission-change mechanics and the rug-pull risk of a validator raising commission to 100%. Delegators choose validators on reliability. A per-validator commission market is a later refinement.
- **Epoch-boundary proportional split.** A delegation takes effect at the next epoch boundary, mirroring validator-set churn deferral. At the boundary, where the validator's reward is already computed, the validator keeps its self-stake share plus the fixed commission on the delegated portion, and the rest is split among its delegations pro-rata to their amounts. There is no reward-per-share accumulator; the simple per-epoch iteration suffices at launch scale and the accumulator is deferred until delegator count makes it costly.
- **Rewards compound into the position.** A delegator's reward is added back into its stake position (and into the validator's `delegated_total`), not credited to a separate liquid coin. It therefore compounds and is returned together with the principal when the delegator undelegates. This is because the protocol tracks a delegator only by its position, not by a payout coin.
- **Unbonding.** Undelegating destroys the position and returns principal plus accrued reward (subject to the future unbonding delay, as for self-stake).
- **Jailing.** A jailed validator's effective stake stops counting toward the quorum and its delegators stop accruing reward, without anyone needing to act; delegators can redelegate. The jail mechanism (zeroing weight, stopping accrual) ships now. The automatic fault trigger that decides when to jail (liveness faults, equivocation) is part of the dispute and fault-proof system that is deferred together with slashing, so at launch the live weight-removal path is deregistration. Jailing is not presented as an active automatic defense yet.

### Reward distribution

At each epoch boundary, the reward pool (accumulated consumed fees plus any issuance) is distributed to validators by a weighted formula:

```
weight_i = effective_stake_i × liveness_i
share_i  = (weight_i / Σ weights) × pool
```

Liveness is the validator's rounds produced this epoch (`rounds_produced_i / total_rounds_in_epoch`; the common denominator cancels in the share). Neither term is a farmable multiplier: stake requires bonded capital, and liveness is capped at one vertex per round (a second is equivocation, not extra liveness). Reward is deliberately not proportional to attestation count, which would be farmable by self-dealing against one's own held objects. A validator that produces nothing during an epoch earns nothing, a soft inactivity penalty that needs no explicit slashing.

The whole pool is distributed: each validator's share is split with its delegators as described above, the validator's liquid portion is credited to a reward coin it designated at registration, a configured fraction is auto-restaked into self-stake by default, and the integer-division remainder is credited to the highest-weight validator (ties broken by public key) so nothing is left undistributed.

**Serving and storage enforcement is deferred, honestly.** Reward is `effective_stake x liveness` and nothing more. An earlier design gated reward on a holder appearing in committed attestation bitmaps, but that signal is unsound: the bitmap is assembled by the submitting daemon, which can omit an honest holder or include a relay that stores nothing. A sound serving check needs a protocol-issued, relay-resistant storage challenge, which is the deferred enforcement branch (Section 14). So at launch the network does not economically reward serving or punish under-serving. What protects the sharded layer in the interim is the attestation quorum tolerating a minority of non-serving holders, replication so one holder dropping does not lose an object, and bonded capital at stake once slashing lands. A cold object's holder earns the same as a hot object's holder at equal stake and uptime, so cold objects are not under-rewarded; their durability rests on replication alone until storage challenges exist.

### Stake-weighted consensus

Voting weight in the DAG is proportional to a validator's effective stake, replacing the earlier equal-weight (one-vote-per-validator) model. This is the Sybil defense: splitting stake across many keys yields the same total weight. The commit and production quorums both sum capped effective stake over the round's producers, read from the epoch holder snapshot selected by the deterministic commit epoch, and apply the exact integer test `3 × capped_sum >= 2 × total` (never floating point). The founding validator's self-stake is seeded at genesis so the very first quorum is reachable.

Voting power (not reward) is capped per validator at a fraction of the total, so the global order stays decentralized even when delegation concentrates stake on the most reliable validators. The cap is calibrated to set size: loose while the set is small (with an equal-share floor so a small set keeps a reachable two-thirds quorum) and tightened toward ~10% as the set grows. Reward weight is uncapped: bigger stake earns proportionally more, as in standard proof of stake. The cap bounds a single advertised identity; a determined entity can split across keys to reconstitute weight, at the cost of running that many independent nodes.

Per-object attestation stays equal-weight: one holder, one vote, with holders assigned by Rendezvous Hashing (Section 6). This deliberately insulates the storage layer from stake concentration. The two combine into a dual security model: the global order is safe under a two-thirds-of-stake honest majority, and each object's attestation is safe under a two-thirds-of-its-holders (by count) honest majority. Stake-weighting secures ordering; count-weighting secures per-object attestation.

Stake-weighting is kept even though slashing is deferred, because equal weight is Sybil-able and strictly worse. An attacker buying two-thirds of the stake pays an enormous capital cost and destroys its own holdings by attacking; the cap bounds concentration; reward-withholding and removal handle misbehavior. This is the same posture as Sui, Aptos, and Solana. Economic punishment (slashing) is added later (Section 14).

### Monetary policy: adaptive issuance

Issuance is governed by an adaptive control loop, a thermostat, evaluated at each epoch boundary and denominated in epoch events rather than time, so it depends on no clock. Money never reads a clock: tying issuance to wall-clock time would invite a collective bias to over-state elapsed time that a median of timestamps cannot stop from a majority.

The thermostat targets a band around the staking ratio (`total_bonded / total_supply`), roughly 25% to 35% of total supply, with a dead-band inside which the rate holds so it does not oscillate. The band errs low on purpose: targeting too low merely rests at low inflation and is easily raised, while targeting too high saturates at the ceiling and dilutes forever. Each epoch the loop reads the ratio on pre-mint supply (so issuance cannot lower its own denominator), steps the per-epoch rate toward the band bounded by a floor and ceiling and a per-epoch step cap, and mints `rate × supply` into the reward pool. A configured fraction of each reward is auto-restaked by default to relieve the treadmill where spent rewards lower the ratio. The starting parameters approximate a ~1% floor, ~20% ceiling, and ~8% to 10% genesis rate annually against an assumed epoch pace; all are governed and recalibrated once a data oracle supplies true time.

The thermostat is the implemented mechanism but it is opt-in: a node leaves it off unless it is enabled by configuration, so issuance is not active by default. It is meant to be turned on together with reward crediting, so that every minted token is always backed by a credit and the supply invariant holds. Issuance is the bootstrap incentive that pays and attracts validators from genesis, which is why the mechanism ships now while the congestion-sensitive dynamic fee, which only matters under traffic a pre-launch network cannot exhibit, does not.

### Supply accounting

`total_supply` is a maintained protocol counter, not a derived sum. It is set at genesis, increased only by protocol issuance, and decreased only by the 5% deletion burn (and future slashing). There is no user-callable mint; the only ways tokens come into existence are genesis seeding and issuance. The counter is maintained in the commit path, persisted, and carried in state snapshots under the snapshot checksum. It feeds the thermostat's denominator.

The supply invariant the protocol holds is:

```
sum(coin balances) + total_bonded + sum(locked storage deposits) + fees_in_flight == total_supply
```

`total_bonded` is the sum of effective stake over the active set. Locked storage deposits are the `fees` fields of live objects. `fees_in_flight` is the epoch's accumulated consumed fees not yet credited to validators; it is zero immediately after an epoch boundary, so the invariant is asserted as exact equality at the boundary. Because the storage component of a fee is locked in the object rather than pooled, it stays accounted as a deposit until 95% is refunded and 5% is burned on deletion, and the counter only ever moves by issuance (up) and the deletion burn (down).

---

## 11. Network architecture

### QUIC mesh

Every validator maintains a persistent QUIC connection to every other validator. With 5,000 validators, this represents roughly 5,000 connections per node. QUIC provides stream multiplexing (parallel requests without head-of-line blocking), persistent connections (no repeated handshakes), and integrated TLS 1.3 encryption.

On machines with 64 GB of RAM, the memory overhead of 5,000 connections (approximately 250 MB) is negligible. This is not a "run a validator on your laptop" design. It targets machines with serious hardware and good network connectivity.

### Gossip protocol

Vertices are propagated through gossip rather than direct broadcast to avoid the O(n²) message complexity of full broadcast. The gossip parameters are:

- **Production fanout**: 40 peers (when a validator produces a new vertex).
- **Relay fanout**: 10 peers (when forwarding a received vertex).

With a production fanout of 40, a vertex reaches the entire network in approximately 3 hops: 40 → 400 → 4,000+. A deduplication cache with TTL prevents message amplification loops.

### QUIC client surface

There is no HTTP. Clients interact with the network over the same QUIC transport the validators use among themselves, through a small set of length-prefixed messages on the node's single listener:

| Message | Description |
|---|---|
| Submit | Submit a raw transaction or a full attested transaction (returns the tx hash) |
| Health | Node liveness probe |
| Status | Consensus state (round, last commit, validators, epoch, system pod) |
| Faucet | Mint test tokens (returns the tx hash and predicted coin ID) |
| Validators | Active validator set with QUIC addresses and BLS keys, plus the current epoch |
| GetObject | Retrieve an object by ID (with automatic holder routing) |
| DomainResolve | Resolve a domain name, with the registry proof for the answer |
| GetIndexAnchor | The cached quorum bundle every other proof is verified against |
| ListChildren | The children of an owner key or an object, with the proof of the parent's subtree root |
| GetAncestors | An object's ancestry walk, one proved edge per hop |
| GetValidatorTree | An epoch's validator leaves, the input for weighing a quorum |

The index messages are what makes a single node's answer worth anything: a proved read carries the Merkle proof for its value, and the anchor bundle carries the quorum-attested root that proof is folded against (Section 5). Enumeration is complete rather than merely plausible, because the subtree root commits to exactly that child set; for a large set the node streams the raw leaves and the client rebuilds the subtree and checks it against the proven root, which keeps completeness without a proof per chunk. An ancestry walk provably terminates, because the parent kind lives inside the authenticated leaf, so a server cannot present an intermediate object as a root by withholding the edge above it.

A connection that presents a validator certificate joins the trusted mesh; every other connection is served in an ephemeral, rate-limited client tier with per-IP caps and a QUIC Retry source-address check, separate from the mesh. Object retrieval includes transparent routing: if the queried validator is not a holder, it forwards the request to a computed holder over the QUIC mesh via Rendezvous Hashing. A local-only flag disables routing to prevent cascading queries. The operational messages replace the former REST API; HTTP liveness probes and metrics scraping are covered by a small CLI built on the client library, which also builds the declared-operation transactions for object and name management directly, with no pod involved.

### Snapshot and synchronization

New validators synchronize through state snapshots:

1. The new validator buffers incoming vertices for a configurable period (default 12 seconds).
2. It requests a snapshot from the bootstrap node, containing: all locally stored objects, the current validator set with addresses and BLS keys, the object tracker with each entry's parent reference, the domain registry with each name's owner and expiry, the frozen holder snapshots, and the last 100 rounds of committed vertices.
3. The snapshot is compressed with zstd and verified via a BLAKE3 checksum over canonically sorted data.
4. The new validator applies the snapshot atomically, rebuilds the index, replays buffered vertices, and goes live only once the state it rebuilt is attested.

Snapshots are created every 10 seconds. A 2-second delay after genesis prevents premature snapshot creation before the network has bootstrapped.

**Nothing Merkle-shaped travels on the wire.** All four trees are derived from raw mappings the snapshot already carries, so the joining node rebuilds them locally and computes the root itself. Each replayed batch keeps them current, so at the tip its root matches the live attested one.

**A snapshot never supplies both the state and its judge.** That is the failure the join gate exists to catch, and it is why a non-genesis join must pin a checkpoint out of band: an epoch and that epoch's validator-set root, which every node publishes whenever it freezes a validator tree. The joiner rebuilds the checkpointed epoch's validator tree from the holder snapshot it just imported and requires its root to equal the pinned one, so a source that invented a committee to bless its own snapshot is refused. Only then does that authenticated committee weigh anything: the node goes live only once a stake quorum of it, each signature verified over the header identity, attests the root the node computed for itself, at or beyond the frontier its rebuilt state describes. If no quorum forms, the join fails. The node does not go live on unverified state.

The residual trust is stated rather than assumed. It is the same weak subjectivity a light client lives with (Section 5), narrowed here to what a joiner can actually recompute: the validator-set hash, not the index root, since a snapshot pinned at the source's last committed round carries no versioned history to recompute a past frontier's root from. An operator who wants none of this can pass an explicit `--insecure-bootstrap` and take the bootstrap's word for both the state and the authority over it; it is a deliberate, loudly logged choice, never a default. Freshness is not closed either: every link verifies authenticity, none verifies recency, so a source able to eclipse a joiner can serve a genuine but old snapshot with its genuine attestations.

A restart is not a join. A node that already owns the committed history in its data directory, because it originated it or proved it before going live, is its own witness: it boots from local state and catches up through gossip with every vertex fully validated, pinning nothing. Only the adopted-state marker settles this, never the mere presence of committed state, since a join the gate refused leaves committed state behind too and resuming that would hand a rejected snapshot the acceptance it was denied. Routing a restart through the join gate would also be unsatisfiable: after a full-cluster outage the first node back can never see a live quorum, because there is none.

---

## 12. Security analysis

### Object attestation security

Can a minority of malicious holders corrupt an attested object? No. Each holder computes the object hash independently from its local copy. A holder sending a falsified object or hash produces an attestation that does not match the honest majority. It is effectively isolated from the quorum.

With a replication factor of 50, an attacker would need to control 34 holders of a specific object to forge a quorum, a targeted attack that becomes exponentially harder as the replication factor increases. The attacker cannot choose which objects they hold: holder assignment is determined by Rendezvous Hashing using the validator's permanent public key.

Can one validator forge a quorum by choosing its BLS key? No, because a key must be proven before it counts. Holder signatures are verified as one aggregated signature over one message, against the aggregated public keys of the declared signers. That is the setting a rogue key exploits: a registrant free to pick any 48 bytes could register `pk_attacker - sum(pk_honest)`, which cancels the honest holders' keys inside the aggregate and leaves a signature it alone can produce verifying as though the whole quorum had signed. Registration therefore carries a proof of possession of the BLS key (Section 10), which only the holder of the matching secret can produce; a key derived from other members' keys has no known secret and cannot be registered at all. Genesis carries no transaction and so no proof: it derives the founding validator's BLS key from that validator's own private key instead, which gives the same guarantee by construction.

### Double-spend prevention

Double-spending is prevented by version tracking. If a user submits two transactions spending the same coin, both declare the same version. The first transaction to be committed increments the version; the second encounters a version mismatch and is rejected. This holds even if the transactions are collected by different clients and included in different vertices. The DAG's deterministic ordering ensures consistent conflict resolution.

### Client daemon trust model

The client daemon that collects attestations has no trust requirements at all: the validator reverifies every attested transaction on receipt and trusts nothing the daemon sends. The daemon cannot forge holder signatures (it lacks their private keys), cannot exclude valid attestations (any validator recomputes the quorum independently), and cannot alter transaction content (the user's Ed25519 signature covers the transaction hash). Because the attestation binds only the object hash, a daemon could in principle staple valid attestations to a different transaction touching the same objects at the same versions; the lifecycle layer (the owner and version checks) is what rejects that, which is why it runs in full on every attested transaction and is not optional.

A misbehaving daemon can only cause a liveness failure for its own submission, which the client resolves by recollecting and resubmitting. The system remains safe regardless of what the daemon does.

### Transaction authenticity at commit

A transaction's authenticity, its sender signature, its sponsor signature when sponsored, and its hash, is verified in the commit path on every node, not only at the ingress of the node that first received it. A transaction can reach commit inside a gossiped, producer-signed vertex without ever passing the local ingress check of the node that commits it. If authenticity were checked only at ingress, a relaying node could embed a forged transaction in an otherwise valid vertex and have it commit. The commit-path check recomputes the canonical body hash from the same shared primitive the builder and ingress use (so the three sites cannot drift), checks it against the declared hash, and verifies the sender's signature; for a sponsored transaction it also verifies the sponsor's signature against `fee_payer`. Because the sponsor signs the same body hash, a forged sponsor signature naming a victim as fee payer cannot drain that victim's coin, and neither signature can be swapped after the other is made.

### Ownership enforcement

Ownership is enforced at the protocol level, not inside pods, so a buggy or malicious pod cannot bypass it. Two checks do it, and both read data every node holds identically.

For a pod call, every object in MutableRefs must carry the sender's key as its parent, read from the attested copy for a replicated object and from local content for a singleton. Those attested bytes are authenticated by the object's BLS quorum proof, which covers content, version, and parent together, so a submitter cannot rewrite them to steal the object. Reading them from a source every node holds identically is what makes the verdict identical too: a replicated object's body lives only on its holders, so validating from local content would have non-holders reject, for want of the bytes, the very transaction holders accept.

For a declared operation, control is resolved by walking the object's parent chain in the tracker to its terminal key and comparing that key to the sender. The walk is bounded and fails closed, so an unresolvable chain confers control on nobody, and an all-zero sender is rejected before it can reach the walk at all and seize an object frozen under the zero key.

Domain references are exempt from the mutable-ref check, enabling controlled shared-access patterns. That exemption is exactly why binding a name requires controlling the object it points at: without the rule, a name could alias a victim's object and reach it mutably through the exemption.

### Malicious vote detection

If a holder signs a hash H but provides data D where `BLAKE3(D) ≠ H`, this constitutes provable on-chain misbehavior. The signed hash and the incompatible data serve as a fraud proof, leading to stake slashing.

If the inconsistency is due to network corruption rather than malice, the client simply requests the data from another holder that signed the same hash. No fraud proof is generated, and no penalty is applied.

### Fee deduction safety

Fees are always deducted, even on failed transactions. This prevents griefing attacks where an attacker submits transactions designed to fail after consuming validator resources. The `min_gas` requirement (currently 100 units) prevents dust transactions that would cost nearly nothing to submit but still consume processing resources.

As a safety note: arithmetic overflow in fee computation is handled through `safeMul` and `safeAdd` functions that cap at `MaxUint64` instead of silently wrapping around.

### Attestation flood defense

Moving collection to the client opens one new surface: any client can ask an object's holders for attestations without ever submitting a paid transaction. The primary defense is structural. An attestation signature is deterministic, so a holder computes it once, eagerly, at execution time and stores it next to the object; an attestation request is then a pure read of stored bytes, not fresh signing work. The current-version signature always exists by the time anyone could request it, because a version only becomes current after a holder has executed the transaction that produced it, so there is no cold window an attacker can exploit by enumerating object IDs. Negative and non-current requests are answered with a static error and never a signature, and that rejection sits before the read. A bounded fallback covers a crash or a holder-set reshuffle: on a store miss a holder signs and stores only for an object it actually holds at its current version, so the work stays bounded by the real, paid rate of state change.

What remains is a generic network flood, handled by standard ingress hardening layered cheapest first: a QUIC Retry round-trip that validates the source address before any work, per-IP caps on connections, in-flight streams, and pending memory, and per-IP rate limiting. A cert-presenting validator joins the mesh; every certless client stays in this ephemeral tier. This is a filter, not a wall: IP-based limiting does not stop botnets or address rotation, and on-protocol punishment for a Byzantine holder awaits the slashing and fraud-proof systems.

---

## 13. Scaling characteristics

Fair warning: nothing in this section is benchmarked. These are back-of-the-envelope estimates based on the protocol design. Real performance will depend on network conditions, hardware, geography, and workload. Proper benchmarking on a distributed testnet is future work. For now, this is just reasoning about what the architecture should allow.

### Latency factors

Finality latency is determined by the critical path: one round-trip to collect attestations from holders, gossip propagation across ~3 hops, and 2 DAG rounds for the commit rule. Each of these steps depends on inter-validator latency, which varies with network topology and geographic distribution. BLS aggregation itself is negligible (sub-millisecond). The architecture is designed to minimize the number of sequential network round-trips, but actual finality time is an open question until measured on a real distributed deployment.

### Bandwidth scaling

Network load scales linearly with throughput because each transaction is a fixed-size message propagated through gossip. With an average transaction size of ~1.5 KB for common operations (transfers, splits), the bandwidth cost per validator is roughly proportional to the total TPS. The gossip mechanism (fanout 40 for production, 10 for relay) adds a constant multiplier. The exact bandwidth requirements at scale depend on vertex batching behavior and the proportion of singleton-only transactions (which skip the attestation phase entirely).

### Storage costs

Storage costs are deterministic from the protocol parameters:

Object tracking requires 55 bytes per object in Pebble: version, replication, locked deposit, parent kind and parent reference, and child count. With the 34-byte key, this is roughly 90 bytes per tracked object. At 1 million objects, the tracker consumes ~90 MB. At 100 million objects, ~9 GB. Every validator tracks every object regardless of whether it holds it. This is the cost of global metadata: version tracking for conflict detection, and the parent graph for cascade control and for the hierarchy index.

The index trees themselves are derived state, held in memory and rebuilt from the tracker, the domain registry, and the epoch's validator snapshot rather than persisted, so they cost no disk and a memory footprint that scales with the same entry count. A joining node downloads the whole of that global metadata, unlike sharded bodies, which is the honest cost of the design. Deletion actually removing entries, and the deposit refund rewarding cleanup, temper the growth.

Object storage depends on holder assignments. A validator's share of stored objects is roughly `replication / total_validators` for each object it holds. Singletons (replication=0) are stored by every validator. At 100 bytes per Coin singleton, 10 million coins consume roughly 1 GB per validator, the main storage cost at scale.

---

## 14. Open problems

These are the problems I know about and have not solved yet:

**Fraud proofs for incorrect execution.** Multiple holders execute the same transaction and should produce identical results for shared objects. A mechanism to detect and penalize incorrect execution, where a holder produces a different state than its peers, is not yet defined. Potential approaches include periodic cross-holder verification or challenge-based fraud proofs.

**Storage challenge system.** Holders can be challenged to prove they still store the objects they are responsible for. The exact proof format, challenge frequency, and spam prevention for challenges remain to be specified.

**Contended-object collection.** Client-side collection over several round-trips widens the version-race window compared to the old in-node collection, so on a hot, contended object a client can lose repeatedly. The daemon retries with bounded randomized backoff and a validator-set resync, then surfaces a typed error. Genuinely hot objects need an application-level batching or sequencing pattern, which remains an open problem.

**Inactivity detection and progressive penalties.** Right now, if a validator stops producing vertices, nothing happens besides it missing out on rewards. There is no automatic detection, no graduated penalties, no forced removal. The system relies on voluntary deregistration, which is obviously not enough for a real network. Graduated penalties (reduced rewards → stake reduction → removal) are planned.

**Cross-shard composability.** This is probably the hardest open problem. Complex transactions touching many objects with different holder sets will face latency challenges in the attestation phase, since the client has to collect from the union of all holder sets. Patterns for efficient cross-shard composition are not obvious, and this might end up being the ceiling on what kinds of applications BluePods can support.

---

## 15. Conclusion

BluePods is built on a simple bet: that a Layer 1 blockchain does not need every validator to do everything. Split the state into independent objects, let a leaderless DAG handle ordering, and route execution to the validators that hold the data. Adding validators spreads the storage and execution work across the set; it does not raise the throughput ceiling of the global order, which stays bounded by what a single node can ingest and validate.

Does it actually work at scale? The prototype says yes for the core protocol: consensus, attestation, execution, fee deduction, storage sharding, domain naming, and validator management all function. But there is a long list of things not built yet (Section 14), and real performance numbers only come from a distributed testnet that does not exist yet.

The tradeoffs are real. Distributed state is harder to manage than global state. The 40-reference limit constrains what transactions can do. Fees are based on declared gas, not actual consumption. These are choices made for specific reasons explained in this document, but they have costs, and some of them might turn out to be wrong.

This is a working prototype, not a finished network. But the architecture is taking shape.
