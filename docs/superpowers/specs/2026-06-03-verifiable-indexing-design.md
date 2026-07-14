# Verifiable indexing design

**Goal:** Give BluePods two protocol-level indexes that clients can read with cryptographic proofs: a domain registry (human-readable name to object) and an object hierarchy (each object hangs off a parent, where a parent is either an owner key or another object). Both are maintained as authenticated structures whose root is anchored in consensus, so a single node's answer is self-verifying and completeness is provable. The hierarchy generalizes ownership: controlling an object means controlling its whole subtree. No new trust assumption is introduced and the execution-sharding model is untouched.

**Why now:** The domain machinery is half-built and dead. The write path exists (a pod can emit `registered_domains`, the commit path handles `MaxCreateDomains`, `state.applyRegisteredDomains` writes a Pebble entry) and the read path exists (`State.ResolveDomain`, the `DomainResolve` QUIC message), but no pod ever registers a domain, no client command exposes it, and there is orphaned generated code (`domain_generated.rs`, a `TrieNode`/`DomainRegistry` from an abandoned on-chain-trie idea). Separately, there is no way to enumerate a user's objects: objects are keyed by raw ID with no owner index, so "my objects" lives only in the client wallet file and is lost on key re-import. This design replaces the throwaway registry with a real one and answers enumeration through the same mechanism.

**Guiding principle:** The index is derived state, not user state. It is computed deterministically by every node from the committed transaction stream, so its correctness rides on consensus, not on a separate honesty assumption. Verifiability replaces policing: a node cannot lie about an index answer because the answer carries a Merkle proof against a quorum-attested root, so there is no index-specific slashing to build. The execution model stays a pure function over a statically declared, pre-attested input set; nothing here lets a pod traverse the index at runtime.

This is one design of record, implemented as one chantier on the `verifiable-indexing` branch, decomposed into ordered plans. It has one prerequisite that ships separately first (section 2).

---

## 1. Scope and non-goals

In scope:

- An object model change: the `owner` field generalizes to a `parent` reference (an owner key or another object), and ownership cascades down the tree.
- Ownership mutation and object deletion become protocol-declared operations carried in the transaction; pods lose write authority over both.
- A domain registry index: `name -> objectID`, with an owner, a rental lifecycle, and enforced namespace ownership.
- A hierarchy index: parent-to-children and child-to-parent, both authenticated.
- A validator index: the epoch's validator set and capped stakes, authenticated, so light clients can verify quorums without trusting the serving node.
- All indexes authenticated by Sparse Merkle Trees whose combined root is anchored in every vertex through a detached provable header.
- The sync, client/API, and documentation changes these require.

Non-goals, deliberately excluded and justified:

- **No dynamic on-chain traversal.** A pod cannot read the index or load objects it discovers at runtime. The access list stays static and pre-attested; this is the direct consequence of execution sharding (a holder physically lacks objects it does not hold, and their attestations). The index is read by whoever builds the transaction (the client), not by the pod mid-execution.
- **No protocol-managed multi-round transactions.** Chains of dependent transactions are orchestrated off-chain by the client, where the attestation-gathering layer already lives. Each transaction is atomic; a chain is a saga (the client retries a step on a version conflict). The atomic boundary is the transaction, never split across a chain.
- **No rent for the hierarchy index.** A name is a scarce, squattable namespace and pays rent. The hierarchy index is automatic per-object derived state, not a namespace; it pays only the storage it consumes, as a flat term folded into the object's existing creation deposit.
- **No new state root beyond the index.** Object content stays verified by the existing per-holder `ObjectSig` attestations. The anchored root commits to the index (domain registry, parent graph, validator set), not to a full object-state tree. The mechanism is extensible to a full state root later, but that is out of scope here.

---

## 2. Prerequisite: deterministic commit (separate fix, lands first)

The whole design assumes every node derives the index from the same committed transactions in the same order. The code today does not guarantee that: `commitRound` applies whatever vertices the node happens to hold when the commit rule fires, in arrival order, and a vertex arriving after its round committed locally is stored but never applied on that node. This nondeterminism is latent today (version increments commute, but fee deductions already diverge silently); a 32-byte root anchored in every vertex would convert any divergence into permanent, consensus-visible disagreement.

The fix is full Mysticeti alignment, and it is a consensus bugfix independent of this design (the whitepaper section 5 already describes the intended behavior):

- A **deterministic anchor vertex per round**, designated by hash. Not a production leader: every validator still produces; the anchor is only the pivot the commit rule keys on. Designation rotates over committed members that have at least one vertex in committed history, so a registered-but-not-yet-producing member can never pin a round while the relaxed regime cannot blame it; eligibility changes only at committed events, never with the evaluating node's cursor, and falls back to the full snapshot while no member has produced yet (bootstrap).
- The commit batch for a round is the anchor's **entire not-yet-committed causal history**, any round, ordered by (round, vertex hash). The batch is a pure function of the DAG's hash links, so its membership and order are identical on every node.
- A late vertex is appended in the batch where committed history first references it. Admission is late, never retroactive: the committed log is append-only and zero rollback is preserved. A vertex never referenced by the following round is dead permanently.
- The anchor is decided by evidence over one stake set, so the verdicts cannot fork: *support* is a round-N+1 vertex listing a specific vertex of the designated producer among its direct parents. The certified vertex is selected by the citations themselves: the producer's vertex reaching the stake quorum of round N+1 is the anchor, so an equivocating producer cannot make two nodes certify different vertices; a vertex citing a different vertex of the same producer abstains, and the round is **skipped** only when a quorum cites no vertex of the producer at all. Both quorums are over the same round's stake, so they intersect and are mutually exclusive. An anchor with split evidence is decided **indirectly** by the first later decided anchor: committed if it sits in that anchor's causal history, skipped otherwise, so an undecided round can never wedge the pipeline. The relaxed bootstrap regime is bounded deterministically, never by a node's local join timing: relaxed until `strictStartRound`, a latch derived from committed history (the round whose commit crosses the minimum validator count, plus the transition grace and buffer windows), persisted and carried in sync snapshots. During relaxed rounds the candidate is still selected by citations under the existing relaxed certificate, never by a locally-chosen lowest-arrived-hash rule, which is view-dependent and can fork two honest nodes on a delivery gap; at the latch the registered set is frozen from committed registrations as the genesis holder snapshot, so anchor designation and stake quorums never read a live, mutating validator set. Total latency stays in the N+2 envelope the current commit rule already has.

This ships on its own branch with divergence tests (two nodes fed the same vertices in different orders must produce identical committed logs) before the indexing work begins.

---

## 3. Object model: parent and cascade ownership

Today an object has an `owner` field holding a 32-byte public key, and the protocol checks `owner == sender` before allowing a mutation or deletion. This generalizes.

**The `parent` reference.** An object's `owner` field becomes a `parent` reference: a 32-byte value plus a one-byte kind tag, where kind is either `KeyRoot` (the 32 bytes are an owner public key, the object is a tree root, identical to today's top-level objects) or `ObjectParent` (the 32 bytes are another object's ID, the object is nested). A pod sets the parent when it creates an object.

**Creation is permissioned.** A created object's declared parent must be under the sender's control: the sender's own key, an object whose parent walk terminates at the sender's key, or an object this transaction references through a domain reference (the existing shared-access exemption, extended coherently). Without this rule anyone could attach junk objects to any key or object, polluting a user-visible, provable enumeration. Enforcement runs at commit during creation, which every node already executes.

**Effective controller.** The key that controls an object is the terminal public key reached by walking the parent chain to its `KeyRoot`. Controlling a root key therefore controls the entire subtree beneath it: if you own an object, you own its children, recursively. This is the Sui object-owns-object model.

**The permission check is a local walk over global metadata, not a sharded traversal.** The parent of every object lives in the global object tracker (section 6), which every node holds in full, exactly as it already holds every object's version. To check "may sender mutate object X", any node walks X's parent chain to its root key entirely in local metadata, without the bodies of the ancestor objects (which are sharded elsewhere). Permission resolution touches metadata; execution touches the body and stays on the holders.

**Reassignment and deletion are protocol-declared operations, and pods lose authority over both.** Changing who controls an object, or removing it, is civil-registry work, not application logic. Today both ride pod execution output, which only holders run, so most of the network never observes them; that is the divergence channel this design closes. The mechanism:

- The transaction carries an explicit **declared operations** list, visible to every node without execution: `reparent{objectID, newParent}` (a transfer is a reparent to a `KeyRoot`), `delete{objectID}`, and the domain operations of section 8. Declared operations are signed by the sender, covered by the canonical body hash, and applied deterministically at commit, exactly like version increments.
- At commit the protocol enforces, from tracker metadata alone: the sender controls the object; a reparent target that is an `ObjectParent` must itself be sender-controlled, while a `KeyRoot` target may be any key (that is a transfer); the new edge creates no cycle (reject if the target is the object or any descendant; the walk is bounded by the depth cap); a deleted object has no children (rmdir semantics: delete leaves first or reparent them); the deposit refund of section 8 applies on delete. Operations in one transaction apply sequentially against the evolving state; the first failure aborts the whole list with no effect, fees kept.
- An object-targeted operation (reparent, delete) increments the object's version (it is a mutation like any other, same conflict detection) and fails cleanly on a version mismatch. Domain operations carry no object references and mutate no object version; the name leaf is their only state. Fees are paid regardless, as everywhere else.
- **A transaction is either declared operations or a pod call, never both.** This keeps each transaction's semantics atomic without asking non-holders to observe a pod revert: a reverted pod call cannot strand a half-applied reparent because the two cannot share a transaction. A pod may still set the parent of an object it creates (creation is globally executed); it may never change the parent of an existing object. `applyUpdatedObjects` rejects any updated object whose parent bytes differ from the ATX input object's, a purely local compare.
- **Pod-driven deletion survives in exactly one case: globally-executed transactions.** The reason pods lose deletion is observability (only holders run the pod), so where execution is global the capability stays: a transaction that creates objects, or whose mutable refs are all singletons, is executed by every node, so a `deleted_objects` entry there is observed by all and stays valid. This is what keeps the system pod's `merge` working: singleton-coin transactions execute everywhere, and every node sees the merged-away coin die. The carve-out path runs the same checks and effects as the declared operation: no children, index removal, and the 95/5 deposit settlement. A sharded object is deleted only through the declared operation.
- The system pod's `transfer` and `transfer_object` functions are retired; both become declared operations built directly by the client. A pure transfer no longer executes WASM at all, which is faster and cheaper.

The precedent is in the codebase already: staking operations are parsed and applied at the protocol level, and fee deduction is protocol-level for the same reason. Everything that must be seen by every node without execution lives in the transaction, not in the WASM.

**Depth bound.** Tree depth is capped at a parameter (default 256) so the permission and cycle walks have a bounded worst case. This is a denial-of-service guard, not a functional limit.

---

## 4. The index structure: Sparse Merkle Trees

One primitive, four trees, one root.

**Primitive: binary Sparse Merkle Tree (SMT), BLAKE3, empty-subtree compression.** A key's position is `BLAKE3(key)`, so the structure is a pure function of its key-value set with no insertion-order dependence and no rebalancing. This determinism is mandatory: every node must compute the identical root. Empty subtrees collapse to a precomputed default hash per level, and only non-empty paths are materialized, so effective depth is about `log2(n)` for `n` entries. Inclusion and absence proofs cost the same. The Jellyfish Merkle Tree (Diem, Aptos) is the production-hardened form of this primitive and is the reference implementation to follow. Merkle Patricia tries (heavy, Ethereum is moving away) and Verkle trees (exotic vector-commitment crypto, premature) are rejected.

**Domain tree: a flat SMT.** Key `BLAKE3(name)`, value the leaf record `{name, objectID, owner, expiry_epoch}`. The name is stored in the leaf so a proof is self-describing; the owner (a 32-byte public key, initially the registrant) is what renewal, update, transfer, and sub-namespace authority check against (section 8). Resolution is a point lookup with an inclusion or absence proof.

**Parent tree: a flat SMT keyed by child.** Key `BLAKE3(childID)`, value `{childID, parentKind, parentBytes}`. This answers "what is X's parent" with one inclusion proof per edge, and makes an ancestry walk provably terminating: the kind tag lives inside the authenticated leaf, so the client can verify the terminal edge really is a `KeyRoot` and not an object whose own parent edge the server withheld. The single-parent invariant (each object appears exactly once) is enforced by consensus and stated here so clients may rely on it.

**Children tree: a two-level SMT (a map of sets).** Top tree: key `BLAKE3(parentID)` (an owner key or an object ID), value the root of that parent's children subtree. Children subtree: key `childID`, value `present`. Enumerating a parent's children is a walk of its subtree, and completeness is provable because the subtree root commits to exactly that child set. Adding or removing a child is an `O(log k)` update in the parent's subtree plus one leaf update in the top tree. The parent tree and children tree are two views of the same edge set, both derived from the tracker; the double bookkeeping costs one extra `O(log n)` path per parent change and buys provable enumeration in one direction and provable ancestry in the other.

**Validator tree: a flat SMT, frozen per epoch.** Key `BLAKE3(validatorPubkey)`, value `{pubkey, cappedStake, blsKey, status}`, rebuilt at each epoch boundary from the same snapshot that freezes `epochHolders`. During the genesis epoch, which freezes no snapshot, the tree tracks the live registration set; the first frozen tree is taken at the first boundary. This is what lets a light client verify a stake quorum without trusting the serving node (section 5).

**Combined index root.** `indexRoot = BLAKE3(domainRoot || parentRoot || childrenRoot || validatorRoot)`, leaving room to fold in further indexes later behind one anchor.

---

## 5. Verifiability: proofs, not slashing

A client reads the index from a single node and verifies the answer itself:

1. It asks one node to resolve `name`, list a parent's children, or walk an ancestry.
2. The node returns the value, a Merkle proof, and the anchor bundle: a stake quorum of producer-signed vertex headers all reporting the same `(frontier_round, indexRoot)`.
3. The client verifies, locally, that the headers are signed by validators of the right epoch carrying a stake quorum, and that the proof links the value to that root.

**The detached provable header.** Verifying an anchor must not require downloading full vertices. The vertex signature therefore changes shape: the producer signs `BLAKE3(header)`, where the header is a compact struct `{producer, round, epoch, frontier_round, indexRoot, bodyHash}` and `bodyHash` is the BLAKE3 of everything else (parents, transactions, fee summary, timestamp). The header hash becomes the vertex's identity: parent links and the store key point at `BLAKE3(header)`, which commits to the body through `bodyHash`. Full vertex validation recomputes `bodyHash`; a light client needs only the header and the signature, about 200 bytes per validator. A quorum bundle at 100 validators is roughly 20 KB, assembled once per frontier by the serving node (which sees all vertices anyway) and cached; one bundle serves every query at that frontier. The header's `epoch` field is populated with the producer's current epoch (today's vertex epoch field is vestigially zero; this fixes it), so a client knows which validator tree weighs the quorum.

**Where the client's validator set comes from.** The validator tree closes the circularity: epoch N's quorum-attested root commits to epoch N+1's validator set, so a client that trusts one checkpoint can walk forward across epoch boundaries by reading each new set out of the index with a proof, exactly the light-client pattern of other proof-of-stake systems. The handoff at a boundary is explicit: headers whose epoch is N+1 but whose frontier falls within the boundary window are weighed by epoch N's tree (validator churn is capped per epoch, so the sets overlap by construction), and the first epoch-N+1-attested root then proves the new set for all later frontiers. Bootstrap trust is a configured checkpoint (a `(epoch, indexRoot, validator set hash)` obtained out-of-band); this is standard weak subjectivity, stated here explicitly rather than assumed silently.

**Enforcement inside the network** follows the codebase's existing two-tier doctrine (ingress validates what a receiver can reliably check at that moment; convergence-sensitive checks are enforced authoritatively at production and commit, the same pattern as the parents-quorum presence check and transaction authenticity at commit):

- **Ingress:** if a received vertex anchors a frontier at or below the receiver's own committed frontier (the common case), the receiver verifies the root against its retained history, a 32-byte compare, and rejects a mismatch exactly like a bad fee summary. A vertex anchoring a frontier the receiver has not reached yet is accepted without buffering; blocking the door on it would couple vertex acceptance to commit lag and slow the DAG under skew. A vertex anchoring a zero root is tolerated only during the genesis epoch, where no index exists yet; from the first epoch boundary on, an empty anchor is rejected like a wrong one.
- **Production (the teeth):** a producer never selects as parent a vertex whose anchor it has not verified. A producer at round N references round N-1 vertices whose anchors trail its own commit by the structural lag, so it nearly always can verify. Combined with the deterministic commit rule (a batch is the anchor's causal history), a wrong-root vertex is never referenced by honest producers and therefore never enters committed history: network-wide exclusion, deterministic, with zero receive-side liveness cost.
- **Commit (the record):** as its commit advances, every node re-verifies the anchor of each committed vertex. A mismatch produces signed, attributable fault evidence: the producer's signed header plus the deterministic recomputation prove the lie to anyone. It is logged today and becomes slashable material when slashing lands.

Root history retention: a bounded `round -> indexRoot` map (about 1,000 rounds) plus one checkpoint per epoch, enough to verify lagging producers and serve clients.

There is therefore **no index-specific slashing**. A lie is worthless: it cannot enter committed history, it cannot fool a client (who requires a stake quorum a minority cannot forge), and it convicts its author with his own signature. The index sits downstream of execution integrity (the open fraud-proof problem already noted in the whitepaper) and neither solves nor worsens it.

**Freshness.** The anchored root covers committed state only and trails the live tip by the structural lag of about two rounds. Responses carry `frontier_round`; a client that just saw its transaction finalize at round R either waits for a bundle at frontier >= R (sub-second) or accepts the live unproven read, and the client library exposes exactly that choice. Frontier skew across producers is normal; the client (or the serving node assembling the bundle) takes the highest frontier carrying a stake quorum within a small sliding window.

---

## 6. Global tracking of the parent graph

The hierarchy indexes and the cascade permission check both need every node to know every object's parent. The object tracker, today 18 bytes per object (`version`, `replication`, `fees`), gains the `parent` reference (32 bytes plus the one-byte kind tag). Body stays sharded, metadata goes global, exactly the split the version tracker already embodies.

Two write paths keep the global parent current, exhaustive up to one carve-out that shares their key property, global observability:

- **Creation.** A transaction that creates objects already forces every validator to execute it (the holder set of a created object is unknown until its ID is computed). Every node observes a created object's parent, validates the creation-permission rule of section 3, and records it.
- **Declared operations.** Reparent, transfer, and delete are read from the transaction and applied deterministically by every node at commit, with the control, acyclicity, and no-children checks of section 3. No pod execution is involved. Deletion finally exercises `tracker.deleteObject` (today dead code), removes both hierarchy entries, and releases the deposit. For pod output, `applyUpdatedObjects` rejects any updated object whose parent bytes differ from the ATX input object's (the attested bytes at the checked version, a purely local compare equivalent to a tracker check).

The pod output path is closed for parent changes, and pod deletion survives only inside globally-executed transactions (section 3), where every node observes it. No channel exists that only holders can see, so the parent graph in the tracker is globally consistent and the SMTs every node builds from it have the same root everywhere.

---

## 7. Anchoring the root in consensus

Finality is structural (a round commits when the anchor's certification quorum references it), and the root rides the signatures that already exist; no new signing step is added.

- Each producer embeds in its vertex header the `indexRoot` of its committed frontier together with that frontier's round number. The root covers already-committed state only, because a producer cannot know the committed order of its own in-flight round. This is a structural lag of about two rounds, not a tunable delay.
- The root is computed incrementally: each committed batch rehashes only the SMT paths its transactions touched (a few thousand BLAKE3 hashes, sub-millisecond). The marginal cost on consensus is about 32 bytes per vertex plus this rehash.
- A client assembles a verified root by collecting producer-signed headers that report the same `(frontier_round, indexRoot)` until they reach a stake quorum weighed by the validator tree the handoff rule of section 5 selects, taking the highest such frontier. Honest producers converge on the identical deterministic root for a given frontier, which the prerequisite of section 2 guarantees, so the quorum forms.

A periodic checkpoint (a dedicated aggregate signature every N rounds) stays rejected: per-vertex anchoring through the detached header is fresher, lighter for the verifier than full vertices, and reuses the one signature per vertex that already exists.

---

## 8. Economics

The index plugs into the existing two-bucket accounting unchanged: consumed fees feed the epoch reward pool, storage is a locked refundable deposit. Nothing new is invented, and the protocol's only burn (the 5% deletion burn) is untouched.

**Domain operations are declared operations.** `domain_register{name, objectID, term_epochs}`, `domain_renew{name, term_epochs}`, `domain_update{name, newObjectID}`, `domain_transfer{name, newOwner}`, `domain_delete{name}`. They live in the transaction, so the fee is derivable from the header alone by every node, which the fee architecture requires (`validateFeeSummary` recomputes per vertex); nothing about a domain touches pod execution, and the insert-only `registered_domains` pod output is removed along with `MaxCreateDomains`. The system pod gains no domain entrypoints; `bpctl` builds declared-operation transactions directly. Naming a just-created object takes two transactions (create, then register), per the either-ops-or-pod rule; the client saga layer handles the pair. `domain_register` and `domain_update` additionally require the sender to control the pointed object: without that rule, anyone could alias a victim's object and reach it mutably through the domain-reference ownership exemption.

**Domains pay rent, not a deposit.** A name is a scarce namespace, so it is leased. Registration and renewal pay `rental_rate x term_epochs`, a consumed fee to the reward pool. A refundable deposit on a name is rejected: it deters squatting not at all (the squatter gets it back). Rent is consumed immediately, in the paying transaction's epoch. The alternative, escrowing and recognizing rent epoch by epoch, would introduce a third accounting bucket and, worse, touch every domain leaf every epoch, rewriting the SMT wholesale; it is rejected. The distortion of immediate consumption (today's validators are paid for service rendered over the term) is bounded by the term cap below and by validator-set continuity across a capped term.

**Term cap.** `expiry_epoch` may never exceed `current_epoch + max_term_epochs` (a governed parameter). An operation whose term would push past the cap reverts: the fee is `rental_rate x term_epochs`, derivable from the header, so a clamped term would charge a fee that no longer matches the declared field. Without the cap, unbounded prepay would let a squatter pay once and hold a name effectively forever, the exact failure recurring rent exists to prevent. Prepaying ahead and renewing late remain the same operation, inside the cap.

**Ownership and namespaces.** The leaf's `owner` is the registrant, or whoever a `domain_transfer` handed the name to. Renewal, update, transfer, and deletion require `sender == owner`; during the grace window only the owner may renew. Registering a dotted name requires owning the immediate parent name (`x.y` requires owning `y`), which recursively makes the whitepaper's first-come-first-served namespace rule real for the first time; bare roots are first-come-first-served and `system.*` stays reserved. All checks are point lookups in the domain tree at commit, deterministic on every node.

**Lifecycle.** Registration sets `expiry_epoch`. At each epoch boundary (existing machinery, through a narrow consensus-to-state hook), the protocol deterministically sweeps entries past `expiry_epoch` plus a grace window. After grace the name leaves the SMT and is re-registrable by anyone. A name past `expiry_epoch` stops resolving immediately, at execution time and in queries (the resolver checks expiry against the current epoch), even during grace; grace only reserves the owner's exclusive right to renew. The leaf stays in the SMT until the sweep, so a client verifying a proof reads `expiry_epoch` from the leaf itself. The object a name pointed to is unaffected. A name is a lease, not property, and applications must treat name references accordingly.

**The hierarchy index pays a flat deposit term, folded into object creation.** A hierarchy entry is genuine global state (roughly 100 bytes on every node), so it cannot be unpriced, but pricing it as `storage_fee x 1` (the full-replication rate for a 4 KB object) would overcharge it by an order of magnitude. The creation deposit becomes `storage_fee x effective_rep / total_validators + index_entry_fee`, where `index_entry_fee` is a new flat constant sized for the entry's true full-replication footprint. The debit at creation and the deposit stamped on the object are computed by the same shared function, so supply stays exact at creation. On deletion the whole deposit, index term included, follows the existing rule: 95% refunded, 5% burned. The burn's anti-churn role applies to index entries exactly as to bodies (creation/deletion cycles churn the SMT), so no exception is made.

**Default parameters** (mechanism fixed, numbers calibratable at review): `rental_rate` per epoch quoted relative to the base compute fee; `max_term_epochs`; a grace window of a small number of epochs; `index_entry_fee`; flat per-operation fees for reparent and delete alongside the existing `min_gas`. These are proposed defaults, not load-bearing constants.

---

## 9. Sync

The existing flow already handles "rounds pass during sync" and the index fits it with additions.

The flow (verified in `cmd/node/sync.go`): a joining node installs a vertex buffer and starts buffering gossiped vertices before requesting a snapshot; it buffers for `SyncBufferSec` (default 12s); it requests a snapshot pinned at `last_committed_round` (carrying objects, validators with stakes, tracker, domains, the last 100 rounds of vertices, supply, issuance); it applies the snapshot, switches the handler so live vertices go straight to the DAG, replays the buffer to bridge the gap to the live tip, and goes live.

Index additions:

- **Tracker carries `parent`.** The snapshot's tracker entries gain the parent reference (33 bytes per entry). Same mechanism, slightly larger payload. Domain leaves now carry owner and expiry.
- **The SMTs are rebuilt, not shipped.** All four trees are derived from raw mappings (domain entries, the tracker's parent fields, the epoch validator snapshot), so the joining node rebuilds them locally and computes the root. Nothing Merkle-shaped travels on the wire.
- **Verifiable snapshot, fail-closed.** After rebuilding, the joining node recomputes `indexRoot` at the snapshot frontier F, replays the snapshot's vertex history, and goes live only once a stake quorum of matching anchors exists for some frontier at or beyond F reached during replay. If no quorum forms, sync fails; the node does not go live on an unverified state. A lying bootstrap is caught. The residual trust is the same configured checkpoint as for light clients (section 5): the validator set and stakes that weigh the quorum bootstrap from a trusted anchor, because a snapshot cannot self-certify. The sync path is also fixed to import stakes from the snapshot (today it drops them and rebuilds the set unstaked).
- **Replay keeps the index current.** Each replayed committed batch updates the trees deterministically, so at the tip the root matches the live attested root.

Honest cost: the global metadata (tracker plus domains) grows with total object count, and a joining node downloads all of it, unlike sharded bodies. Deletion now actually removing entries (section 3) and the deposit refund encouraging cleanup temper the growth; as the snapshot grows, `SyncBufferSec` and the vertex-history depth may need to scale to preserve the overlap, and the committed-tx-hash retention window remains an open scaling item outside this design's scope.

---

## 10. Client and API surface

QUIC messages and `bpctl` commands expose the indexes; the wallet uses them instead of its local-only object list. Minimal surface:

- `GetIndexAnchor() -> {frontier_round, indexRoot, headers[]}`: the cached quorum bundle for the highest quorum-carrying frontier (section 5). All other proofs verify against it.
- `ResolveDomain(name) -> {leaf, proof}`: extends the existing `DomainResolve` (which today returns an unproven id) with the domain-tree inclusion or absence proof.
- `ListChildren(parentID) -> {childIDs, proof}` where `parentID` is an owner key or an object ID. The proof is the top-tree inclusion of the parent's subtree root; for large child sets the node streams raw leaves and the client rebuilds the subtree and checks its root against the proven leaf, which preserves the completeness guarantee without per-chunk range proofs.
- `GetAncestors(objectID) -> {edges[], proofs[]}`: one parent-tree inclusion proof per edge; the walk provably terminates at a `KeyRoot` because the kind lives in the authenticated leaf.
- `GetValidatorTree(epoch) -> {leaves, proof}` for light-client epoch walking.
- `bpctl` verbs: `domain register|renew|update|transfer|delete|resolve <name>`, `objects [owner|parentID]`, `object parent <id>`, `object reparent|delete|transfer <id>`. All mutation verbs build declared-operation transactions; none goes through a pod.

"My objects" is `ListChildren(pubkey)` at the top level plus recursion into object-parented subtrees; the wallet file becomes a cache, and recovery from a bare key works by walking the index. The creation-permission rule of section 3 keeps this enumeration spam-free.

Client verification is a library function: verify a proof against an anchored root given a trusted checkpoint, walking validator trees across epochs. It is shared by query verification and snapshot verification.

---

## 11. Code to remove and documents to update

Remove: the dead `pods/pod-sdk/src/domain_generated.rs` and its `TrieNode`/`DomainRegistry` types; the `registered_domains` field of `PodExecuteOutput` and `MaxCreateDomains` (superseded by declared operations); the system pod's `transfer` and `transfer_object` functions (superseded by declared operations). `deleted_objects` stays, restricted to globally-executed transactions (section 3).

Whitepaper consequences (edited in place):

- Section 3 (Domain Name System) is rewritten: the registry is an authenticated index with a consensus-anchored root, an owner, enforced namespaces, a recurring rental with a term cap, and an expiry lifecycle.
- The object model section gains the `parent` reference, cascade ownership, the creation-permission rule, and the local-metadata permission walk.
- The transaction lifecycle section gains declared operations and the either-ops-or-pod rule; the consensus section (section 5) gains the deterministic anchor commit rule (from the prerequisite fix) and the detached provable header, and its "18 bytes per object" tracker figures (also in section 13) become the new entry size.
- Section 8's system pod table drops `transfer` and `transfer_object`; section 12's ownership-enforcement paragraph is rewritten around the tracker parent walk.
- The fees section gains the domain rental, the term cap, and the `index_entry_fee` deposit term; the 95/5 refund and the two-bucket split are unchanged.
- The sync section gains the verifiable fail-closed snapshot and the trusted-checkpoint caveat; the network section gains the new QUIC messages.

VISION is unaffected: this strengthens the existing positioning (synchronous atomic composability within a transaction, off-chain orchestration across transactions) rather than changing the tradeoff.

---

## 12. Testing strategy

- **Deterministic commit (prerequisite):** two nodes fed the same vertices in different arrival orders produce byte-identical committed logs; late vertices commit exactly once, in the same batch everywhere; a never-referenced vertex commits nowhere.
- **SMT determinism:** the same key-value set yields the same root regardless of insertion order; incremental updates match a from-scratch rebuild.
- **Proofs:** inclusion verifies; absence verifies for an unregistered name and a non-child; completeness verifies that an enumerated child set is exact; an ancestry walk verifies edge by edge and terminates at a proven `KeyRoot`; a tampered value, omitted entry, or truncated leaf stream fails verification.
- **Anchoring:** honest producers report the identical root per frontier; a client assembles a stake quorum of headers and rejects a minority or mismatched root; a wrong-root vertex is rejected at ingress when verifiable, is never referenced by an honest producer, never commits, and leaves signed fault evidence at the commit re-check.
- **Cascade and declared operations:** the permission walk resolves the controlling key through nested parents using tracker metadata only; subtree transfer changes only the root object; reparent rejects cycles, non-controlled targets, and version mismatches; deleting a parent with children is rejected; creation under a non-controlled parent is rejected; a pod output touching an existing parent is rejected; a pod output deleting a sharded object outside a globally-executed transaction is rejected while `merge` keeps working; the depth cap is enforced; ops-and-pod-call transactions are rejected.
- **Economics:** registration and renewal move `expiry_epoch` within the term cap and revert beyond it; namespace registration requires owning the parent name; non-owners cannot renew, update, transfer, or delete a name; the sweep removes expired entries after grace and refunds nothing for a name; object deletion refunds 95% of the deposit including the index term and burns 5%; the supply invariant holds across all of the above.
- **Sync:** a joining node rebuilds all four trees, verifies the root against a replay-assembled quorum, rejects a snapshot whose rebuilt root never reaches quorum, imports stakes, and converges to the live tip.
- **Removal regression:** the build is clean after deleting the dead trie code, the pod output fields, and the transfer entrypoints.

---

## 13. Implementation order (for the plan)

Batch 0 is the prerequisite fix of section 2, on its own branch, landed first. The chantier then proceeds: (1) `parent` in the object model and the global tracker, declared operations (reparent, transfer, delete) with the permission walk, creation-permission rule, and pod-output lockdown; (2) the SMT primitive and the four trees built from the tracker, domain entries, and epoch snapshot; (3) the detached provable header, root computation, per-vertex anchoring, the three-stage enforcement, and client-side quorum assembly; (4) domain declared operations, rental with term cap, ownership and namespace rules, expiry sweep; (5) snapshot carries `parent` and stakes, rebuild, fail-closed verification; (6) QUIC and `bpctl` surface, wallet switchover, light-client library; (7) remove dead code and retired entrypoints; (8) documentation. Each batch produces working, tested software.
