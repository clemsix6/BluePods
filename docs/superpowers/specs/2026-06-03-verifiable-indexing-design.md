# Verifiable Indexing Design

**Goal:** Give BluePods two protocol-level indexes that clients can read with cryptographic proofs: a domain registry (human-readable name to object) and an object hierarchy (each object hangs off a parent, where a parent is either an owner key or another object). Both are maintained as authenticated structures whose root is anchored in consensus, so a single node's answer is self-verifying and completeness is provable. The hierarchy generalizes ownership: controlling an object means controlling its whole subtree. No new trust assumption is introduced and the execution-sharding model is untouched.

**Why now:** The domain machinery is half-built and dead. The write path exists (a pod can emit `registered_domains`, the commit path handles `MaxCreateDomains`, `state.applyRegisteredDomains` writes a Pebble entry) and the read path exists (`State.ResolveDomain`, the `DomainResolve` QUIC message), but no pod ever registers a domain, no client command exposes it, and there is orphaned generated code (`domain_generated.rs`, a `TrieNode`/`DomainRegistry` from an abandoned on-chain-trie idea). Separately, there is no way to enumerate a user's objects: objects are keyed by raw ID with no owner index, so "my objects" lives only in the client wallet file and is lost on key re-import. This design replaces the throwaway registry with a real one and answers enumeration through the same mechanism.

**Guiding principle:** The index is derived state, not user state. It is computed deterministically by every node from the committed transaction stream, so its correctness rides on consensus, not on a separate honesty assumption. Verifiability replaces policing: a node cannot lie about an index answer because the answer carries a Merkle proof against a quorum-attested root, so there is no index-specific slashing to build. The execution model stays a pure function over a statically declared, pre-attested input set; nothing here lets a pod traverse the index at runtime.

This is one design of record, implemented as one chantier on the `verifiable-indexing` branch, decomposed into ordered plans.

---

## 1. Scope and non-goals

In scope:

- An object model change: the `owner` field generalizes to a `parent` reference (an owner key or another object), and ownership cascades down the tree.
- A domain registry index: `name -> objectID`.
- A hierarchy index: `parent -> [childID]`, where `parent` is an owner key or an object ID.
- Both indexes authenticated by a Sparse Merkle Tree whose root is anchored in every vertex.
- Domain economics: a recurring rental (consumed fee) with an expiry lifecycle.
- The sync, client/API, and documentation changes these require.

Non-goals, deliberately excluded and justified:

- **No dynamic on-chain traversal.** A pod cannot read the index or load objects it discovers at runtime. The access list stays static and pre-attested; this is the direct consequence of execution sharding (a holder physically lacks objects it does not hold, and their attestations). The index is read by whoever builds the transaction (the client), not by the pod mid-execution.
- **No protocol-managed multi-round transactions.** Chains of dependent transactions are orchestrated off-chain by the client, where the attestation-gathering layer already lives. Each transaction is atomic; a chain is a saga (the client retries a step on a version conflict). The atomic boundary is the transaction, never split across a chain.
- **No rent for the hierarchy index.** A name is a scarce, squattable namespace and pays rent. The hierarchy index is automatic per-object derived state, not a namespace; it pays only the storage it consumes, folded into the object's existing creation deposit.
- **No new state root beyond the index.** Object content stays verified by the existing per-holder `ObjectSig` attestations. The anchored root commits to the index (domain registry plus parent graph), not to a full object-state tree. The mechanism is extensible to a full state root later, but that is out of scope here.

---

## 2. Object model: parent and cascade ownership

Today an object has an `owner` field holding a 32-byte public key, and the protocol checks `owner == sender` before allowing a mutation or deletion. This generalizes.

**The `parent` reference.** An object's `owner` field becomes a `parent` reference: a 32-byte value plus a one-byte kind tag, where kind is either `KeyRoot` (the 32 bytes are an owner public key, the object is a tree root, identical to today's top-level objects) or `ObjectParent` (the 32 bytes are another object's ID, the object is nested). A pod sets the parent when it creates an object.

**Effective controller.** The key that controls an object is the terminal public key reached by walking the parent chain to its `KeyRoot`. Controlling a root key therefore controls the entire subtree beneath it: if you own an object, you own its children, recursively. This is the Sui object-owns-object model.

**The permission check is a local walk over global metadata, not a sharded traversal.** This is the crux that keeps cascade ownership compatible with execution sharding. The parent of every object lives in the global object tracker (section 5), which every node holds in full, exactly as it already holds every object's version. So to check "may sender mutate object X", any node walks X's parent chain to its root key entirely in local metadata, without needing the bodies of the ancestor objects (which are sharded elsewhere). Permission resolution touches metadata; execution touches the body and stays on the holders. The two concerns are independent, which is precisely why co-location is not required.

This also retires the "declared ancestor path" idea: because the parent graph is global, no transaction needs to carry its ancestor chain as read-refs. The walk is free local metadata lookups at commit time.

**Subtree transfer is cheap; effective owner is derived, never denormalized.** Because the controller is derived by walking to the root, transferring a whole subtree is a single mutation of the root object's parent (its `KeyRoot` changes to the new owner key). No descendant is touched, so there is no cascade write and no contention near the root. The effective owner is never stored on each object; storing it would force a cascade rewrite on every transfer, which sharding cannot do in one transaction.

**Reparenting.** An object's parent may change after creation (a reparent operation). The protocol requires that the sender controls both the object and the target parent, and that the new edge does not create a cycle. Cycle detection is a local ancestor walk over global metadata (cheap, same as the permission walk): reject if the target parent is the object itself or any of its descendants. A reparent (and a transfer, which is a reparent to a `KeyRoot`) declares the new parent in the transaction, so it is applied deterministically by every node from the committed transaction without pod execution (section 5).

**Deletion.** An object with children cannot be deleted (rmdir semantics): the children would dangle. Delete the leaves first, or reparent them. This keeps the forest invariant with no orphan-handling special case.

**Depth bound.** Tree depth is capped at a parameter (default 256) so the permission and cycle walks have a bounded worst case. This is a denial-of-service guard, not a functional limit; 256 is far beyond any realistic hierarchy.

---

## 3. The index structure: a two-level Sparse Merkle Tree

Two indexes share one primitive.

**Primitive: binary Sparse Merkle Tree (SMT), BLAKE3, empty-subtree compression.** A key's position is `BLAKE3(key)`, so the structure is a pure function of its key-value set with no insertion-order dependence and no rebalancing. This determinism is mandatory: every node must compute the identical root. Empty subtrees collapse to a precomputed default hash per level, and only non-empty paths are materialized, so effective depth is about `log2(n)` for `n` entries. Inclusion and absence proofs cost the same (an absence proof shows the leaf at the key's position is the default). The Jellyfish Merkle Tree (Diem, Aptos) is the production-hardened form of exactly this primitive and is the reference implementation to follow. Merkle Patricia tries (heavy, Ethereum is moving away) and Verkle trees (exotic vector-commitment crypto, premature) are rejected.

**Domain index: a flat SMT.** Key `BLAKE3(name)`, value the leaf record `{name, objectID, expiry_epoch}` (the name is stored in the leaf so a proof is self-describing). Resolution is a point lookup with an inclusion or absence proof.

**Hierarchy index: a two-level SMT (a map of sets).** Hashing scatters keys, which destroys the locality needed to enumerate one parent's children and prove the enumeration complete. So the hierarchy nests:

- Top tree: key `BLAKE3(parentID)` (where `parentID` is an owner key or an object ID), value the root of that parent's children subtree.
- Children subtree (one per parent): key `childID`, value `present`.

Enumerating a parent's children is a walk of its subtree. Completeness is provable because the subtree root commits to exactly that child set (an absence proof inside the subtree shows "nothing else"). Adding or removing a child is an `O(log k)` update in the parent's subtree plus one leaf update in the top tree; no list is ever rewritten wholesale.

**Combined index root.** The anchored value is `indexRoot = BLAKE3(domainRoot || hierarchyRoot)`, leaving room to fold in further indexes later behind one anchor.

---

## 4. Verifiability: proofs, not slashing

A client reads the index from a single node and verifies the answer itself:

1. It asks one node to resolve `name` (or list a parent's children).
2. The node returns the value, a Merkle proof, and the `indexRoot` with the quorum attestation that backs it (section 6).
3. The client verifies, locally, that the root is attested by a stake quorum of the validator set and that the proof links the value to that root.

A node cannot lie: it cannot forge a proof for a false value, nor sign a false root in place of the quorum. A node cannot hide an entry: absence and completeness proofs make omission detectable ("here are all my objects, none withheld"). One honest node suffices; a client queries a second node only for availability (the first is down or refuses), never to vote on the answer. This is the same trust the client already places when reading an object via its holder attestation, applied to the index root.

There is therefore **no index-specific slashing**. Slashing is the tool when you cannot verify and must detect-and-punish; here a lie is worthless, caught for free by the proof. The remaining concern, a wrong root reaching commit, is covered by the consensus security that already exists: the root is deterministic, so every honest validator recomputes it and refuses to attest a vertex carrying a wrong one; forcing a bad root needs a two-thirds-of-stake collusion, which is the base assumption failing. A validator that signs a verifiably wrong root commits an attributable consensus fault under whatever general accountability the chain gains, not a bespoke index mechanism. The index sits downstream of execution integrity (the open fraud-proof problem already noted in the whitepaper) and neither solves nor worsens it.

---

## 5. Global tracking of the parent graph

The hierarchy index and the cascade permission check both need every node to know every object's parent. The object tracker, today 18 bytes per object (`version`, `replication`, `fees`), gains the `parent` reference (32 bytes plus the one-byte kind tag). This keeps body sharded, metadata global, exactly the split the version tracker already embodies.

Two write paths keep the global parent current, both without requiring a non-holder to execute a pod:

- **Creation.** A transaction that creates objects already forces every validator to execute it (the holder set of a created object is unknown until its ID is computed). So every node observes a created object's parent at creation and records it.
- **Reassignment (transfer or reparent).** This is a protocol-level operation: the new parent is declared in the transaction, so every node applies the change deterministically from the committed transaction, exactly as it applies version increments from the header, with no pod execution. The protocol enforces the control and acyclicity checks of section 2 at this point.

Because both paths are globally observable, the parent graph in the tracker is globally consistent, and the hierarchy SMT every node builds from it has the same root everywhere.

---

## 6. Anchoring the root in consensus

The root must be backed by a stake quorum so a client can trust it. The vertex already carries a single producer signature, and finality is structural (a round commits when round+2 producers carrying a stake quorum reference it); there is no separate per-round aggregate signature to extend. So the root rides the signatures that already exist.

- Each producer embeds, in the vertex it builds, the `indexRoot` of its committed frontier together with the round number of that frontier. The root covers already-committed state only, because a producer cannot know the committed order of its own in-flight round (it settles at round+2). This is a structural lag of about two rounds, not a tunable delay.
- The root is computed incrementally: each committed round rehashes only the SMT paths its transactions touched (a few thousand BLAKE3 hashes, sub-millisecond). The marginal cost on consensus is about 32 bytes per vertex plus this rehash. No new signing step.
- A client assembles a verified root by collecting producer-signed vertices that report the same `(frontier_round, indexRoot)` until they reach a stake quorum, and takes the highest such frontier. This is the same structural-quorum assembly the commit rule uses for finality; honest producers converge on the identical deterministic root for a given height, so the quorum forms.

A periodic checkpoint (a dedicated aggregate signature every N rounds) was rejected: it would add a signing subsystem and an epoch cadence for a benefit (instant provability) nobody needs, while per-vertex anchoring is fresher and reuses existing signatures. Per-round anchoring is a checkpoint with N=1 that costs no extra signature.

---

## 7. Economics

The index plugs into the existing two-bucket accounting unchanged: consumed fees feed the epoch reward pool, storage is a locked refundable deposit. Nothing new is invented.

**Domains pay rent, not a deposit.** A name is a scarce namespace, so it is leased, like a real domain. Registration and renewal pay a recurring rental that is a consumed fee to the reward pool. A refundable storage deposit on a name is rejected as over-engineering: the rent (deliberately far larger than the few bytes an entry occupies) covers the storage many times over, and a refundable deposit deters squatting not at all (the squatter gets it back). The rent is also the anti-squat lever; a one-time fee would let a squatter pay once and hold forever, so the cost recurs.

Domain lifecycle:

- Each domain entry carries an `expiry_epoch`.
- Registration sets `expiry_epoch` and pays rent for the initial term (in epochs). Renewal pays for further terms and pushes `expiry_epoch` forward; prepaying ahead and renewing late are the same operation.
- At each epoch boundary (existing machinery), the protocol deterministically sweeps entries past `expiry_epoch` plus a grace window. During grace, only the current owner may renew. After grace the name is removed from the SMT and is re-registrable by anyone.
- Dangling references: when a name expires it simply stops resolving (resolution is dynamic, at execution time). The object it pointed to is unaffected; it still belongs to its owner. After grace, someone else may claim the name and point it elsewhere. A name is a lease, not property, and applications must treat name references accordingly.

**The hierarchy index pays only storage, folded into object creation.** It is not a namespace, so no rent. But it is genuine global state: a `parent -> child` entry lives on every node, and for a small, lightly replicated object that full-replication entry can weigh as much as the sharded object body itself, so it cannot be unpriced (an unpriced global write is a state-bloat vector). The clean treatment, with no new line item: the object-creation storage deposit, today sized for the object's sharded footprint, also accounts for its global index entry (a full-replication term), and the whole deposit is refunded when the object is deleted (which removes the index entry). Using the hierarchy is free as a feature; the storage it induces is covered by the deposit object creation already pays, and recovered on cleanup.

**Default parameters** (mechanism fixed, numbers calibratable at review): rental rate per epoch quoted relative to the base compute fee; initial and renewal terms denominated in epochs; grace window a small number of epochs. These are proposed defaults, not load-bearing constants.

---

## 8. Sync

The existing flow already handles "rounds pass during sync" and the index fits it with one addition and one upgrade.

The flow (verified in `cmd/node/sync.go`): a joining node installs a vertex buffer and starts buffering gossiped vertices *before* requesting a snapshot; it buffers for `SyncBufferSec` (default 12s); it requests a snapshot pinned at `last_committed_round` (carrying objects, validators, tracker, domains, the last 100 rounds of vertices, supply, issuance); it applies the snapshot, switches the handler so live vertices go straight to the DAG, replays the buffer to bridge the gap to the live tip, and goes live. The 100-round vertex history overlaps the buffer so there is no gap. Replay is faster than live (committed history, no consensus wait), so it converges.

Index additions:

- **Tracker carries `parent`.** The snapshot's tracker entries gain the parent reference (the entry grows by 33 bytes). Same sync mechanism, slightly larger payload. Domains already ship in the snapshot.
- **The SMT is rebuilt, not shipped.** Both trees are derived from the raw mappings (domain entries and the tracker's parent fields), so the joining node rebuilds them locally and computes the root. Nothing Merkle-shaped travels on the wire.
- **Verifiable snapshot.** Today the snapshot has only a checksum, which catches corruption but not a dishonest bootstrap. After rebuilding the index, the joining node recomputes `indexRoot` and verifies it against the quorum-attested root assembled from the snapshot's vertices (section 6). A lying bootstrap is caught. This composes with the per-object `ObjectSig` attestations the snapshot already carries for bodies. The standard weak-subjectivity caveat applies: trustlessness assumes the validator set comes from a trusted anchor, a property of every proof-of-stake sync, not introduced here.
- **Replay keeps the index current.** Each replayed committed vertex updates the trees deterministically, so at the tip the root matches the live attested root with no special handling.

Honest cost: the global metadata (tracker plus domains) grows with the total object count, and a joining node downloads all of it, unlike sharded bodies of which it takes only its share. Adding `parent` increases this constant, a property the version tracker already has. As the snapshot grows, `SyncBufferSec` and the 100-round history may need to scale to preserve the overlap.

---

## 9. Client and API surface

QUIC messages and `bpctl` commands expose the indexes; the wallet uses them instead of its local-only object list. Minimal surface:

- `ResolveDomain(name) -> {objectID, proof, root, attestation}` (extend the existing `DomainResolve`, which today returns an unproven id, to carry the proof and attested root).
- `ListChildren(parentID) -> {childIDs, completenessProof, root, attestation}` where `parentID` is an owner key (a user's top-level objects) or an object ID (a subtree).
- `GetAncestors(objectID) -> {path, proofs}` for client-side ownership verification (walk to the root key with a proof per edge).
- `bpctl` verbs: `domain register|renew|resolve <name>`, `objects [owner|parentID]`, `object parent <id>`. The system pod gains a domain-registration entrypoint and a parent-assignment entrypoint so registration and reparenting are actually reachable (today no pod emits either).

Client verification is a library function: verify a proof against an attested root, given the validator set. It is shared by query verification and snapshot verification.

---

## 10. Code to remove and documents to update

Remove: the dead `pods/pod-sdk/src/domain_generated.rs` and its `TrieNode`/`DomainRegistry` types (an abandoned on-chain-trie registry, unreferenced by the SDK).

Whitepaper consequences (the whitepaper is the document of record for the how, edited in place):

- Section 3 (Domain Name System) is rewritten: the registry is no longer "local infrastructure, not an object or a singleton" with a one-time fee. It is an authenticated index with a consensus-anchored root, a recurring rental, and an expiry lifecycle.
- The object model section gains the `parent` reference, cascade ownership, and the local-metadata permission walk.
- The fees and economics section gains the domain rental and the hierarchy-index storage term; the existing two-bucket split is unchanged.
- The storage distribution / sync section gains the verifiable snapshot.
- The Section 11 transport table gains `ListChildren` and `GetAncestors` and notes the proof-and-root extension to `DomainResolve`.

VISION is unaffected: this strengthens the existing positioning (synchronous atomic composability within a transaction, off-chain orchestration across transactions) rather than changing the tradeoff.

---

## 11. Testing strategy

- **SMT determinism:** the same key-value set yields the same root regardless of insertion order; incremental updates match a from-scratch rebuild.
- **Proofs:** inclusion verifies; absence verifies for an unregistered name and for a non-child; completeness verifies that a parent's enumerated children are exactly its set; a tampered value or omitted entry fails verification.
- **Anchoring:** every honest producer computes the identical root for a given committed frontier; a client assembles a stake quorum of matching `(frontier_round, root)` and rejects a minority or mismatched root; a vertex carrying a wrong root is refused by honest validators.
- **Cascade and parent:** the permission walk resolves the controlling key through nested parents using only tracker metadata; subtree transfer changes only the root object; reparenting rejects cycles and non-controlled targets; deleting a parent with children is rejected; the depth cap is enforced.
- **Economics:** registration and renewal move `expiry_epoch`; the epoch-boundary sweep removes entries past expiry plus grace and refunds nothing for the name (rent, not deposit); an expired name stops resolving and becomes re-registrable; object deletion refunds the storage deposit including the index term.
- **Sync:** a joining node rebuilds both trees from a snapshot, verifies the root against the attested root, rejects a snapshot whose rebuilt root does not match, and converges to the live tip after replay.
- **Removal regression:** the build is clean after deleting the dead trie code.

---

## 12. Implementation order (for the plan)

The plan will batch this; the natural dependency order is: (1) `parent` in the object model and the global tracker, with the metadata permission walk and reassignment as a protocol operation; (2) the SMT primitive and the two indexes built from the tracker and domain entries; (3) root computation and per-vertex anchoring with client-side quorum assembly and proof verification; (4) domain rental, expiry sweep, and the system-pod registration and reparent entrypoints; (5) snapshot carries `parent`, rebuilds the trees, verifies the root; (6) QUIC and `bpctl` surface, wallet switchover; (7) remove dead code; (8) documentation. Each batch produces working, tested software.
