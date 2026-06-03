# BluePods

BluePods is a decentralized cloud, built as a Layer 1 blockchain in Go. Its premise is that not every validator needs to process every transaction. The state is not a global tree but a collection of independent, versioned objects, each replicated on a small subset of validators called its holders, and a transaction is executed only by the holders of the objects it touches. Adding validators therefore spreads the work thinner instead of duplicating it, and the network's aggregate capacity grows rather than staying fixed. What BluePods refuses to trade away for that scale are the two properties that make a cloud usable rather than merely possible: zero rollback, so a finalized operation is never reverted, and global synchronous atomic composability, so any backend can call any other in a single finalized step regardless of which holders own the objects involved. Keeping both at once requires one global order that every validator sees, which bounds total throughput to a single node rather than the sum of all of them; the wager that makes this affordable is that per-node bandwidth keeps rising. Everything else in the system follows from that one choice.

Three ideas carry the design. The first is objects in place of accounts: there is no global state trie, only discrete units each carrying an ID, a version, an owner, and a replication factor, so two transactions that touch different objects can never conflict and detecting a conflict is a version comparison rather than a lock. The second is a leaderless DAG for ordering, in the lineage of Mysticeti: every validator produces vertices in parallel with no designated proposer, and a vertex is final once producers carrying a supermajority of stake acknowledge it two rounds later. The third is that execution follows the data, with each transaction run by the holders of the objects it mutates rather than by the whole network; the attestations that prove an object's state are collected off-chain by the client, so their cost tracks an object's replication factor and not the size of the validator set. The backends themselves, called pods, are Rust compiled to WebAssembly and run in a sandbox whose entire interface to the outside world is four host functions.

The reasoning behind each of these choices, and the parts this page deliberately leaves out, the economic layer and its monetary policy, the attestation and fee mechanics, validator management, and the security analysis, lives in two documents of record. [docs/VISION.md](docs/VISION.md) is the why: the goal of a decentralized cloud, the properties BluePods will not compromise, the tradeoff it accepts, and where it stands against the Internet Computer, Sui, Solana, and Ethereum. [docs/WHITEPAPER.md](docs/WHITEPAPER.md) is the how, in full. The acceptance criteria are in [docs/ATP.md](docs/ATP.md), and the multi-node integration tests are described in [test/integration/TESTING.md](test/integration/TESTING.md).

## Build and run

BluePods needs Go 1.26 or newer and a Rust toolchain with the WebAssembly target (`rustup target add wasm32-unknown-unknown`).

Build the system pod, the node, and the client. The pod's `make release` compiles it to WebAssembly, builds the `wasm-gas` instrumenter, and injects gas metering into the deployed `build/pod.wasm`.

```bash
cd pods/pod-system && make release && cd ../..
go build -o node ./cmd/node
go build -o bpctl ./cmd/cli
```

Start a local cluster in separate terminals, one bootstrap node and as many joiners as you like:

```bash
./node -bootstrap -quic 127.0.0.1:9000 -data ./data1
./node -bootstrap-addr 127.0.0.1:9000 -quic 127.0.0.1:9001 -data ./data2
./node -bootstrap-addr 127.0.0.1:9000 -quic 127.0.0.1:9002 -data ./data3
```

On a terminal a node shows a live dashboard with the round, transactions per second, connected peers, and total committed transactions. Pass `--log` to force line logs instead, which is also what happens automatically when the output is not a terminal, for example under a service manager.

The network is QUIC only, with no HTTP. Talk to it with `bpctl`. Run bare on a terminal it opens an interactive console with a live header and a command line, where `faucet`, `transfer`, `split`, the `object` operations, `coins`, `objects`, and `pubkey` are typed directly while each transaction's status and your balance update in place:

```bash
./bpctl --node 127.0.0.1:9000
```

With a subcommand it is a one-shot tool for scripts:

```bash
./bpctl --node 127.0.0.1:9000 status
./bpctl --node 127.0.0.1:9000 coin faucet <pubkey-hex> <amount>
./bpctl --node 127.0.0.1:9000 object holders <id-hex>
```

A node also runs in validator mode, joining an existing network, and in listener mode, observing without taking part in consensus. Run `./node -help` for every flag, including epoch length, churn limits, gossip fanout, and the sync buffer.
