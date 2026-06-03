# BluePods

BluePods is a decentralized cloud, written from scratch in Go. The idea behind it is that not every validator should have to process every transaction. State is split into independent objects, each one held by a small subset of validators, and a transaction runs only on the holders of the objects it touches. Add more validators and the work spreads out instead of piling up, so capacity grows with the network rather than standing still. Two guarantees are non-negotiable, because they are what make a cloud worth building on. Nothing finalized is ever rolled back. And any backend can call any other atomically, in one finalized step, no matter who holds what. The price for both at once is a single global order that every validator sees, which caps throughput at what one machine can take rather than the whole fleet combined. The bet is that per-node bandwidth keeps climbing fast enough to make that a good trade.

A few choices do most of the work. There is no global state tree, only discrete objects, each with an ID, a version, an owner, and a replication factor. Two transactions on different objects can never collide, and when they hit the same one, catching it is a version check, not a lock. Ordering runs on a leaderless DAG in the Mysticeti lineage: validators produce vertices in parallel, with no proposer to bottleneck on, and a vertex is final once enough stake has acknowledged it two rounds later. Execution goes to where the data already lives, on an object's holders rather than the whole network, and the proofs of an object's state are gathered off-chain by the client, so their cost follows how widely the object is replicated, not how large the network has grown. The backends are called pods. They are Rust compiled to WebAssembly, sandboxed behind four host functions and nothing else.

This page is the short version. The rest lives in two documents of record: [VISION.md](docs/VISION.md) for why BluePods exists and how it sits against the Internet Computer, Sui, Solana, and Ethereum, and the [whitepaper](docs/WHITEPAPER.md) for how it actually works, from the object model through consensus, attestation, fees, the economic layer, and the security analysis. The acceptance tests are in [ATP.md](docs/ATP.md), and [TESTING.md](test/integration/TESTING.md) walks through the multi-node simulations.

## Build and run

You will need Go 1.26 or newer and Rust with the WebAssembly target (`rustup target add wasm32-unknown-unknown`). Building the pod with `make release` also compiles the `wasm-gas` instrumenter and stitches gas metering into the deployed `build/pod.wasm`.

```bash
cd pods/pod-system && make release && cd ../..
go build -o node ./cmd/node
go build -o bpctl ./cmd/cli
```

Bring up a local cluster across a few terminals, one bootstrap node and as many joiners as you want:

```bash
./node -bootstrap -quic 127.0.0.1:9000 -data ./data1
./node -bootstrap-addr 127.0.0.1:9000 -quic 127.0.0.1:9001 -data ./data2
./node -bootstrap-addr 127.0.0.1:9000 -quic 127.0.0.1:9002 -data ./data3
```

Run on a terminal, a node draws a live dashboard: round, transactions per second, connected peers, committed transactions. Add `--log` if you would rather have plain logs, which is also what you get automatically when the output is not a terminal.

The network speaks QUIC, not HTTP, so you reach it through `bpctl`. With no command it opens an interactive console, a live header above a prompt, where you type `faucet`, `transfer`, `split`, the `object` commands, `coins`, `objects`, and `pubkey` as you go, and balances and transaction statuses update in place:

```bash
./bpctl --node 127.0.0.1:9000
```

Give it a command instead and it becomes a one-shot tool for scripts:

```bash
./bpctl --node 127.0.0.1:9000 status
./bpctl --node 127.0.0.1:9000 coin faucet <pubkey-hex> <amount>
./bpctl --node 127.0.0.1:9000 object holders <id-hex>
```

A node can also join an existing network as a validator, or sit in listener mode and watch without taking part in consensus. Run `./node -help` for the full set of flags.
