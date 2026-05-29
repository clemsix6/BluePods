# BluePods vision

Last updated: 2026-05-29
Scope: why this project exists, what it refuses to compromise, and how it positions against other systems. For how it works, see WHITEPAPER.md. For current state, see STATUS.md.

## What this is

BluePods is a decentralized cloud. The goal is to host arbitrary backends, written as WASM modules called pods, on a subset of nodes rather than on every node in the network. The native token exists, but it is a means, not the point. The point is to be a place where someone can deploy a backend and its state, and have the network run it, store it, and keep it consistent, without that backend living on all machines.

The closest existing system in ambition is the Internet Computer. BluePods shares the vision and makes the opposite architectural choice at the center, which is the subject of the positioning section below.

## The problem

Most Layer 1 blockchains force a choice between throughput, decentralization, and state management. Ethereum keeps strong decentralization but every validator stores and processes the whole state, which caps throughput. Solana reaches high throughput through a leader-based pipeline, but the leader is a bottleneck and the hardware bar limits who can participate. Sui introduces an object model that allows parallel execution, but still has every validator process every transaction.

The common thread is that scaling usually means either trusting fewer validators or pushing work to a Layer 2. BluePods refuses both. Instead of having every validator do everything, it splits the work across the validator set: each object has a defined set of holder validators that store it, attest to it, and execute the transactions that touch it. A transfer involves the ten to fifty holders of the objects in play, not the entire network. State and execution are sharded by holders; the consensus that orders everything is not.

## Cardinal properties

Two properties are non-negotiable, because they are what make a decentralized cloud usable rather than just possible.

Zero rollback. A finalized operation is never reverted. Finality is deterministic and absolute, not probabilistic. A backend can treat a finalized write as permanent and build on it without defensive logic.

Global synchronous atomic composability. Any pod can call any other pod atomically and consistently, in a single finalized step, regardless of which holders own the objects involved. There is no asynchronous boundary between applications.

No centralized cloud offers the second property (AWS services are eventually consistent across regions and services), and no fragmented blockchain offers both at once. That combination is the wedge.

## The tradeoff we accept

Holding both properties requires a single global DAG with one total order. Every operation, from every app, passes through that order and is seen by every validator, even though each validator only stores and executes a fraction of the state.

The price is explicit. There is no horizontal scaling of throughput: total capacity is bounded by what one node can ingest and validate, not by the sum of all nodes, so adding validators does not raise the ceiling. And under a network partition, the system is CP: it stops rather than risk a rollback, so a global partition can halt the cloud until it heals. These are accepted, not overlooked.

The bet that makes this viable is bandwidth. Per-node bandwidth grows over time (the same wager Solana makes), so the cost of a global DAG becomes affordable, and a future data-availability split that keeps only hashes on the consensus path makes the bet easier to win without giving up the total order. The choice is atomicity and simplicity now, at the cost of horizontal scaling.

## Non-goals

No state or subnet sharding. Splitting the network into independent shards or subnets would make cross-shard finality conditional on every shard involved, which breaks zero rollback and the synchronous composability above. This is the deliberate divergence from the Internet Computer. Note that this is distinct from execution sharding, which BluePods does do: work is routed to an object's holders. The two uses of the word are kept separate as "state/subnet sharding" (a non-goal) and "execution sharding" (a core mechanism).

No separate consensus-less fast path. A fast path for single-owner objects, in the style of Sui, would finalize some transactions without the global order and so without uniform atomic composability. BluePods accepts a slightly higher latency on simple transfers in exchange for one uniform guarantee across every operation.

## Positioning

Versus the Internet Computer. Same vision, opposite center. ICP hosts WASM backends on subnets of a few dozen nodes each, which scales by adding subnets but makes cross-subnet calls asynchronous and non-atomic, with application-level rollback left to the developer. BluePods keeps one global DAG, so composition is synchronous and atomic and nothing rolls back, at the cost of the scaling that subnets buy.

Versus Sui. The closest cousin: the object model, version-based conflict detection, and the DAG consensus lineage all come from this family. Sui keeps a consensus-less fast path for single-owner objects, which is faster on simple transfers but is not globally atomically composable on that path, and it puts large storage in a separate network (Walrus). BluePods folds storage and execution into one protocol and keeps everything in one atomic order.

Versus Solana. Solana makes the same bandwidth bet, but it is leader-based and its roadmap moves toward isolating execution into domains that cannot touch each other's state, which is the opposite of global composability. BluePods is leaderless and keeps composition global.

Versus Ethereum. Ethereum's account model with a global state trie has every validator process every transaction, and it scales through Layer 2. BluePods decouples unrelated state into objects. The honest downside is that cross-object composability is bounded by the per-transaction reference cap, so the deep, many-contract compositions Ethereum's global state allows are harder here.
