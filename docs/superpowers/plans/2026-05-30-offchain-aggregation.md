# Off-chain aggregation implementation plan

> **For agentic workers:** This plan is executed by **one subagent per batch** (not per task), via superpowers:subagent-driven-development adapted to batch granularity. Each batch is a self-contained unit a single subagent implements end to end, then a review gate runs before the next batch. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Move attestation aggregation from validators to a client-side Go library, make the node QUIC-only (no HTTP anywhere), add a structural denial-of-service defense (eager per-object signatures), adjust fees to 70/30, and simplify the attestation protocol, without weakening the existing consensus verification.

**Architecture:** Batches 1 to 5 are additive: they build the new path (validators/attest packages, eager signatures, QUIC client surface, daemon library) alongside the existing on-chain aggregation and HTTP, so the tree stays green throughout. Batches 6 and 7 are the flip: 6 switches submission to the daemon and removes node-side aggregation; 7 purges all remaining HTTP (client API, inter-node routing, the on-chain `HTTPAddr`). Batch 8 migrates the integration tests and the docs. The design of record is `docs/superpowers/specs/2026-05-30-offchain-aggregation-design.md`; read it before starting.

**Tech stack:** Go 1.26 node, Rust/WASM pods (untouched), QUIC (quic-go v0.59), BLS12-381 (blst), BLAKE3, FlatBuffers (regenerate with `bash types/generate.sh`, which also regenerates the Rust types), Pebble.

**Branch:** All work happens on a feature branch created at execution time (not `main`).

---

## Shared contracts

Every batch must use these exact names and formats.

**Package layout.**
- `internal/validators/` holds the leaf types `Hash [32]byte`, `ValidatorInfo`, `ValidatorSet`, extracted from `internal/consensus`. Imports nothing from `consensus`. `consensus` keeps the types via aliases (`type Hash = validators.Hash`, etc.) so consensus code is not churned.
- `internal/attest/` holds the pure attestation helpers: rendezvous hashing (`ComputeHolders`, `QuorumSize`), BLS aggregation/verification, and `ComputeObjectHash`. It imports only blake3, blst, and `internal/validators`. The daemon imports `internal/attest` and `internal/validators` and nothing else from the node.
- `internal/aggregation/` keeps the holder `Handler`, the relocated ATX verifier (`verify.go`), and the signature store (`sigstore.go`). It may import `state`/`storage`/`network`; the daemon never imports it.
- `daemon/` is the new client library (top-level package, same module). `client/quic.go` is the SDK QUIC transport.

**Attestation hash (canonical, fixes a latent bug).** `ComputeObjectHash(content, version)` returns `BLAKE3(content || version_u64_BE)` where `content` is the object's content bytes (`obj.ContentBytes()`), not the full FlatBuffer object bytes. Today the handler signs over full object bytes (`handler.go:51`) while the verifier recomputes over `ContentBytes()` (`cmd/node/aggregation.go:138`); these disagree. Batch 1 makes the handler, the collector's check, and the verifier all call the one canonical `attest.ComputeObjectHash(content, version)`, so signer and verifier always agree.

**Durable signature store.** Pebble key `"objsig:" || objectID[32]` maps to `version_u64_BE(8) || bls_signature(96)`. Keying by object ID (not version) means a version advance overwrites the entry, so only the current-version signature is kept. Reads must copy out of the Pebble-owned buffer. The store handle is the node's `*storage.Storage`.

**Eager signing.** A holder signs at execution. When `internal/state` persists a held object at a new version, it invokes an injected signer callback (same pattern as the existing `SetOnObjectCreated`), and the node's callback computes `attest.ComputeObjectHash` + BLS sign + writes the `objsig:` entry, atomically with the object via `storage.SetBatch`. State holds only a `func`, so `state` does not import `aggregation` (no cycle). A bounded fallback covers crash or reshuffle gaps: on a store miss the handler signs and stores **only** for an object it actually holds at its **current** version (a cheap reject otherwise), so the work is bounded by held-object count and never attacker-enumerable beyond it.

**Wire changes (`types/vertex.fbs`, regenerate with `bash types/generate.sh`).**
- Remove `total_aggregator:uint64` from the `FeeSummary` table.
- Add `attestation_epoch:uint64` to the `AttestedTransaction` table. `serializeAttestedTx` (`commit.go:730`) and any other ATX builder must carry it.
- Remove the `http_address` field from the `Validator` table.

**Epoch rule.** Add `func commitEpochForRound(round uint64) uint64` (`round / epochLength`, deterministic) in `epoch.go`. Retain the previous epoch's holder snapshot (`prevEpochHolders`) in `transitionEpoch`, and expose `HoldersForEpoch(epoch uint64) (*validators.ValidatorSet, bool)`. `const EpochGraceRounds = 50`. The ATX verifier accepts if `attestation_epoch == commitEpoch`, or `attestation_epoch == commitEpoch-1` and the commit round is within `EpochGraceRounds` of the boundary, verifying the quorum against `HoldersForEpoch(attestation_epoch)`. The verifier callback type changes to carry the commit round (see Batch 5).

**Validator identity.** Validators register only a QUIC address. `HTTPAddr` is removed from `ValidatorInfo`, `ValidatorSet.Add`, the register-validator borsh args, the `Validator` FlatBuffer, the snapshots, and the genesis config (Batch 7, consensus-breaking).

**Singletons.** Singletons (replication 0, including the gas coin) are never in an ATX and never attested. Singleton-only transactions are submitted raw and wrapped by the validator into a trivial ATX. A mixed transaction carries only its replicated objects; singletons are version-checked locally from the header.

**Verification layering.** ATX verification (recompute `H`, aggregated signature, bitmap, quorum against the epoch snapshot) runs in addition to the existing commit-path lifecycle checks (version vs declared and vs DAG, owner on mutable refs, gas coin, replay). The lifecycle checks stay where they are in `commit.go` and are not duplicated at ingress.

**Per-batch gate.** Each batch ends green. Green means `go build ./...`, `go vet ./...`, and `go test ./internal/... ./client/... ./daemon/...`. Integration tests (`./test/integration/`) migrate in Batch 8; Batches 6 and 7 may leave them failing, which is expected and called out.

---

## Batch 1: Foundations (validators + attest packages, canonical hash, verifier relocation)

One subagent. Behavior-preserving except the hash standardization, which is made consistent across signer and verifier in this batch so nothing breaks.

**Files:** Create `internal/validators/validators.go` (+test); create `internal/attest/{rendezvous.go,bls.go,hash.go}` (+test); modify `internal/consensus/types.go` (aliases), `internal/aggregation/{rendezvous.go,bls.go,handler.go,collector.go}` (use `attest`/`validators`), `cmd/node/aggregation.go` (use relocated verifier); create `internal/aggregation/verify.go`.

- [ ] **Step 1: Read.** `internal/consensus/types.go` (`Hash`, `ValidatorInfo`, `ValidatorSet`, `NewValidatorSet(pubkeys []Hash)` at line 41, `Add(pubkey Hash, httpAddr, quicAddr string, blsPubkey [48]byte)` at 69); `internal/aggregation/{rendezvous.go,bls.go}`; `handler.go:51,86-98` (`ComputeObjectHash`, signs `objectData`); `cmd/node/aggregation.go:106-202` (`buildATXVerifier`/`verifySingleProof`, uses `obj.ContentBytes()` at 138 and `n.rendezvous`); `collector.go:402` (verifies against `ComputeObjectHash(result.ObjectData, ...)`).

- [ ] **Step 2: Extract validator types.** Move `Hash`, `ValidatorInfo`, `ValidatorSet` and their methods verbatim into `internal/validators/validators.go` (package `validators`, imports only `sync`/`bytes`). In `internal/consensus/types.go` replace the definitions with aliases:

```go
import "BluePods/internal/validators"

type Hash = validators.Hash
type ValidatorInfo = validators.ValidatorInfo
type ValidatorSet = validators.ValidatorSet

func NewValidatorSet(pubkeys []validators.Hash) *validators.ValidatorSet { return validators.NewValidatorSet(pubkeys) }
```

- [ ] **Step 3: Validators test.** `internal/validators/validators_test.go`:

```go
package validators

import "testing"

func TestValidatorSetAddAndLen(t *testing.T) {
	vs := NewValidatorSet([]Hash{{1}, {2}, {3}})
	if vs.Len() != 3 {
		t.Fatalf("want 3, got %d", vs.Len())
	}
	vs.Add(Hash{4}, "", "addr:9000", [48]byte{0xAB})
	if vs.Len() != 4 {
		t.Fatalf("want 4 after Add, got %d", vs.Len())
	}
}
```

Run `go test ./internal/validators/`: PASS.

- [ ] **Step 4: Extract pure attest helpers.** Move rendezvous (`ComputeHolders`, `QuorumSize`), the BLS aggregate/verify functions, and `ComputeObjectHash` from `internal/aggregation` into `internal/attest/` (package `attest`, imports only `github.com/zeebo/blake3`, blst, and `internal/validators`). `ComputeObjectHash(content []byte, version uint64) [32]byte` hashes `content || version_u64_BE` (the content-only form). Leave the holder `Handler` and the protocol encoding in `internal/aggregation`, now importing `internal/attest`.

- [ ] **Step 5: Standardize the hash at the signer and the verifier.** In `internal/aggregation/handler.go`, change the sign path (line ~51) to `hash := attest.ComputeObjectHash(obj.ContentBytes(), req.Version)` (content, not full bytes). In `collector.go:402` change the verification to the same `attest.ComputeObjectHash(contentBytes, version)`. This keeps the old path internally consistent (handler signs content, collector verifies content) while matching the spec and the verifier.

- [ ] **Step 6: Hash-agreement test** `internal/aggregation/handler_hash_test.go`: build an object, have the handler produce a signature, and assert it verifies under `attest`-based verification using `ContentBytes()`. Run it: PASS.

- [ ] **Step 7: Relocate the ATX verifier.** Create `internal/aggregation/verify.go` with `type ATXVerifier struct { holders *attest.HolderSource }` (or carry a `*validators.ValidatorSet` + a rendezvous accessor) and `func (v *ATXVerifier) Verify(atx *types.AttestedTransaction) error`, moving `buildATXVerifier`/`verifySingleProof`/`findATXObjectIndex`/`extractSignerBLSKeys` here, using `attest.ComputeObjectHash(obj.ContentBytes(), obj.Version())`. Update `cmd/node/aggregation.go` to construct and call it. Keep the (non-epoch) signature for now; Batch 5 makes it epoch-aware.

- [ ] **Step 8: Build, vet, test** (`go build ./... && go vet ./... && go test ./internal/... ./client/...`): PASS. Fix any import cycle (legal directions only: `consensus -> validators`, `aggregation -> attest -> validators`).

- [ ] **Step 9: Commit.**

```bash
git add internal/validators/ internal/attest/ internal/consensus/types.go internal/aggregation/ cmd/node/aggregation.go
git commit -F - <<'EOF'
[&] Extract validators and attest packages, fix attestation hash
	- internal/validators (types via alias), internal/attest (rendezvous, BLS, hash)
	- Canonical ComputeObjectHash over content bytes; handler and verifier agree
	- Relocate ATX verifier to internal/aggregation/verify.go
EOF
```

---

## Batch 2: Fee model (70/30, remove aggregator)

One subagent. Remove the aggregator share everywhere it is computed, written, validated, and logged.

**Files:** `types/vertex.fbs` (+ regen), `internal/consensus/{fees.go,commit.go,build.go,validate.go,fees_test.go,validate_test.go}`.

- [ ] **Step 1: Constants.** `fees.go`: remove the `AggregatorBPS` field and `AggregatorBPS:2000` (lines ~16,39), set `EpochBPS:7000`, keep `BurnBPS:3000`. Remove `FeeSplit.Aggregator` (field ~25) and the `aggregator := total*AggregatorBPS/bpsMax` line (~180); `epoch = total - burned`.

- [ ] **Step 2: Fee test first.** `fees_test.go`: update the split assertions (~262) to 70/30 with no aggregator, and the invariant at ~539 (`AggregatorBPS+BurnBPS+EpochBPS==bpsMax`) to `BurnBPS+EpochBPS==bpsMax`. Run `go test ./internal/consensus/ -run Fee`: expect FAIL until code compiles, then PASS after the next steps.

- [ ] **Step 3: Wire field.** `types/vertex.fbs`: delete `total_aggregator:uint64;` from `FeeSummary` (line ~48). Run `bash types/generate.sh`. Confirm pods still build (`cd pods/pod-system && make release`).

- [ ] **Step 4: Build/commit paths.** `build.go:76-98`: remove `FeeSummaryAddTotalAggregator` (~93) and the `totalAgg` accumulation (~85). `commit.go`: remove the `vertexFees.Aggregator` accumulation (~169), the `creditAggregator(producer, split.Aggregator)` call (~302), the `creditAggregator` function (438-453), and the `"aggregator", fees.Aggregator` log field (~147).

- [ ] **Step 5: Validation.** `validate.go:246-261`: remove the `declared.TotalAggregator() != totalAgg` recompute and rejection. `validate_test.go`: remove `totalAggregator` from the `feeSummaryValues` struct (~458), fix the ~6 positional literals (lines ~182,216,253,290,327,436), and delete `TestValidateFeeSummary_WrongAggregator` (~232).

- [ ] **Step 6: Build, vet, test** (`go build ./... && go vet ./... && go test ./internal/...`): PASS.

- [ ] **Step 7: Commit.**

```bash
git add types/ internal/types/ internal/consensus/ pods/
git commit -F - <<'EOF'
[&] Fee model: 70 epoch / 30 burn, remove aggregator share
	- Drop AggregatorBPS, EpochBPS now 7000
	- Remove total_aggregator from FeeSummary wire, validation, build, log
	- Remove creditAggregator stub
EOF
```

---

## Batch 3: Durable eager signatures (additive)

One subagent. Add the signature store, the eager signing callback, the bounded fallback, the serve-from-store handler path, and snapshot inclusion. `WantFull` and negative-signing are untouched here (the collector still needs them; removed in Batch 6).

**Files:** create `internal/aggregation/sigstore.go` (+test); modify `internal/state/store.go` (signer callback + apply hooks), `internal/aggregation/handler.go` (serve-from-store + bounded fallback), `cmd/node/` (wire the signer), `internal/sync/snapshot.go` + `types/snapshot.fbs` (carry signatures); modify `internal/consensus/commit.go` if the apply path runs there.

- [ ] **Step 1: Read.** `internal/storage/storage.go:56-103` (`Get`/`Set`/`SetBatch`); `internal/state/store.go` (db `*storage.Storage`; `SetIsHolder`:63, `SetOnObjectCreated`:69; apply at `applyUpdatedObjects`:383, `applyCreatedObjects`:438; exclude `creditGasCoin`:619 and `SetObject`:193); `internal/sync/snapshot.go` (`collectObjects`:50-82 keeps only 32-byte keys; `computeChecksumWithInfo`:303; `ApplySnapshot`:398; `snapshotVersion`).

- [ ] **Step 2: sigstore test** `internal/aggregation/sigstore_test.go`:

```go
func TestSigStoreCurrentVersionAndCopy(t *testing.T) {
	db := newTestStorage(t) // *storage.Storage on a temp dir
	var id [32]byte; id[0] = 0xAB
	sig := make([]byte, 96); sig[0] = 0x01
	if err := PutObjectSig(db, id, 7, sig); err != nil { t.Fatal(err) }
	v, got, ok := GetObjectSig(db, id)
	if !ok || v != 7 || got[0] != 0x01 { t.Fatalf("ok=%v v=%d", ok, v) }
	got[0] = 0xFF // mutate the returned slice; store must be unaffected (copy-on-read)
	_, again, _ := GetObjectSig(db, id)
	if again[0] != 0x01 { t.Fatal("GetObjectSig returned an aliased buffer") }
	sig2 := make([]byte, 96); sig2[0] = 0x02
	PutObjectSig(db, id, 8, sig2)
	v, _, _ = GetObjectSig(db, id)
	if v != 8 { t.Fatalf("want v8 after advance, got %d", v) }
}
```

- [ ] **Step 3: Implement `sigstore.go`** (copy on read):

```go
package aggregation

import (
	"encoding/binary"
	"BluePods/internal/storage"
)

const sigKeyPrefix = "objsig:"

func sigKey(id [32]byte) []byte {
	k := make([]byte, len(sigKeyPrefix)+32)
	copy(k, sigKeyPrefix); copy(k[len(sigKeyPrefix):], id[:])
	return k
}

func PutObjectSig(db *storage.Storage, id [32]byte, version uint64, sig []byte) error {
	val := make([]byte, 8+len(sig))
	binary.BigEndian.PutUint64(val[:8], version)
	copy(val[8:], sig)
	return db.Set(sigKey(id), val)
}

func GetObjectSig(db *storage.Storage, id [32]byte) (uint64, []byte, bool) {
	val, err := db.Get(sigKey(id))
	if err != nil || len(val) != 8+96 {
		return 0, nil, false
	}
	out := make([]byte, 96)
	copy(out, val[8:])
	return binary.BigEndian.Uint64(val[:8]), out, true
}
```

Run `go test ./internal/aggregation/ -run SigStore`: PASS.

- [ ] **Step 4: Signer callback in State.** In `internal/state/store.go`, add a field `signObject func(id [32]byte, content []byte, version uint64, replication uint16)` and `SetObjectSigner(fn ...)` (mirroring `SetOnObjectCreated`:69). In `applyUpdatedObjects` (383) and `applyCreatedObjects` (438), after the object is persisted, call `s.signObject(id, content, version, replication)` when set and `replication > 0` and the node holds the object (reuse the `isHolder` predicate from `SetIsHolder`:63). Do NOT call it from `creditGasCoin` (619) or `SetObject` (193). `state` imports no `aggregation`.

- [ ] **Step 5: Wire the signer in the node.** In `cmd/node/` (where state is constructed), call `state.SetObjectSigner(func(id, content, version, rep){ h := attest.ComputeObjectHash(content, version); sig := blsKey.Sign(h[:]); _ = aggregation.PutObjectSig(db, id, version, sig) })`. Prefer writing the object and the signature in one `storage.SetBatch` if the apply path exposes a batch; otherwise two `Set` calls plus the Step 6 fallback cover the crash window.

- [ ] **Step 6: Serve-from-store + bounded fallback in the handler.** In `handler.go`, on a positive request: if `GetObjectSig` returns a signature whose version equals the requested current version, return it. On a store miss, sign-and-store **only if** the node holds the object at the requested version and that version is current (otherwise the existing negative path); never sign for an unheld or non-current version. Keep the existing `WantFull` behavior intact for the collector.

- [ ] **Step 7: Signatures in snapshots.** In `types/snapshot.fbs` add a `signatures:[ObjectSig]` vector (`ObjectSig { id:[ubyte]; version:ulong; sig:[ubyte]; }`); run `bash types/generate.sh`. In `snapshot.go`: collect `objsig:` entries (they are 39-byte keys, not caught by `collectObjects`), add them to `buildSnapshot`, include them in `computeChecksumWithInfo` (deterministic order), bump `snapshotVersion`, and apply them in `ApplySnapshot`. Add a round-trip test: a node that applies a snapshot can serve an attestation for a held object without signing.

- [ ] **Step 8: Build, vet, test** (`go build ./... && go vet ./... && go test ./internal/...`): PASS.

- [ ] **Step 9: Commit.**

```bash
git add internal/aggregation/sigstore.go internal/aggregation/sigstore_test.go internal/aggregation/handler.go internal/state/ cmd/node/ types/snapshot.fbs internal/types/ internal/sync/
git commit -F - <<'EOF'
[+] Eager per-object BLS signatures with durable store
	- Sign at execution via an injected State callback (no import cycle)
	- objsig: store, copy-on-read, current version only, in snapshots
	- Handler serves stored sig; bounded sign-on-miss for held current versions
EOF
```

---

## Batch 4: QUIC client surface and DoS gates (additive)

One subagent. Add client-facing QUIC handlers, an ephemeral untrusted client tier, the ingress gates, and the inter-node `GetObject` message, alongside the existing HTTP and mesh.

**Files:** modify `internal/network/node.go` (TLS, accept path), create `internal/network/clientconn.go` (+test) and `internal/network/messages.go`, modify `cmd/node/handlers.go` (route new messages).

- [ ] **Step 1: Read.** `internal/network/node.go:91` (`tls.RequireAnyClientCert`), `node.go:114` (`quic.Transport`), `handleIncoming`:348, `setupPeer`:358-394, `handlePeerDisconnect`:397; `cmd/node/handlers.go:57-77` (`OnRequest` first-byte dispatch); `aggregation/protocol.go:10-12,54` (message-type byte).

- [ ] **Step 2: Message types** in `internal/network/messages.go` with round-trip tests: `MsgSubmitTx` (raw tx or full ATX, distinguished by structure), `MsgGetObject`/`GetObjectResp`, `MsgGetValidators`/`Resp` (per-validator pubkey, BLS, QUICAddr, plus the current epoch), `MsgStatus`/`Resp` (current round, `epochLength`, epoch), `MsgHealth`, `MsgFaucet`, `MsgDomainResolve`. Length-prefixed framing, distinct first-byte type tags not colliding with attestation/snapshot tags.

- [ ] **Step 3: Allow certless clients.** Change `node.go:91` `ClientAuth` from `RequireAnyClientCert` to `tls.RequestClientCert`. In `handleIncoming` (348), classify post-handshake: if the peer presents a cert whose derived pubkey is in the validator set, route to `setupPeer` (mesh, unchanged); otherwise treat it as an ephemeral client connection.

- [ ] **Step 4: Ephemeral client tier** in `internal/network/clientconn.go`: client connections are not added to the `peers` map, not reconnected, and closed on idle. Add QUIC Retry address validation via `quic.Transport.VerifySourceAddress` (a func returning true to force a Retry round-trip before work). Add per-IP caps (concurrent connections, in-flight streams, pending bytes) returning a typed "resource limit exceeded" before any handler work, and a per-IP token-bucket rate limiter. Account memory from the first packet.

- [ ] **Step 5: clientconn test** asserting the per-IP connection cap rejects excess and the limiter throttles. Run `go test ./internal/network/`: PASS.

- [ ] **Step 6: Route messages** in `cmd/node/handlers.go`: `MsgGetObject` returns the object if held, else (inter-node path) fetches from the computed holder over QUIC `MsgGetObject` using `info.QUICAddr` and returns it; `MsgGetValidators` returns the set plus epoch; `MsgStatus` returns round/`epochLength`/epoch; `MsgHealth`; `MsgFaucet`/`MsgDomainResolve` mirror the current HTTP handlers; `MsgSubmitTx` returns "not yet enabled" (wired in Batch 6).

- [ ] **Step 7: Build, vet, test** (`go build ./... && go vet ./... && go test ./internal/... ./client/...`): PASS. HTTP still works.

- [ ] **Step 8: Commit.**

```bash
git add internal/network/ cmd/node/handlers.go
git commit -F - <<'EOF'
[+] QUIC client surface, ingress DoS gates, inter-node GetObject
	- Message types: submit, object, validators+epoch, status, faucet, domain
	- RequestClientCert + post-handshake classify; ephemeral untrusted tier
	- QUIC Retry (VerifySourceAddress), per-IP scopes and rate limiting
EOF
```

---

## Batch 5: Daemon library and epoch-aware ATX verification (additive)

One subagent. Build the client daemon and the SDK transport, and wire the epoch-boundary rule end to end.

**Files:** create `daemon/{daemon.go,aggregation.go}` (+test), `client/quic.go`; modify `types/vertex.fbs` (+ regen), `internal/consensus/{epoch.go,dag.go,commit.go,commit_test.go}`, `internal/aggregation/verify.go`, `cmd/node/aggregation.go`.

- [ ] **Step 1: Wire field + serializer.** `types/vertex.fbs`: add `attestation_epoch:uint64;` to `AttestedTransaction`; `bash types/generate.sh`. Update `serializeAttestedTx` (`commit.go:730`) to copy `attestation_epoch`. Confirm no other ATX builder drops it.

- [ ] **Step 2: Epoch helpers.** `epoch.go`: add `func (d *DAG) commitEpochForRound(round uint64) uint64 { if d.epochLength == 0 { return 0 }; return round / d.epochLength }`; in `transitionEpoch` (41) keep the prior snapshot (`d.prevEpochHolders = d.epochHolders` before overwriting at ~147); add `func (d *DAG) HoldersForEpoch(epoch uint64) (*validators.ValidatorSet, bool)` returning current or previous snapshot; add `const EpochGraceRounds = 50`. Resolve the `d.epoch` (48) vs `d.currentEpoch` (91) duplication: use `currentEpoch` for the snapshot/grace logic and note `d.epoch` if it is redundant.

- [ ] **Step 3: Epoch-aware verifier.** Change the verifier callback type from `func(*types.AttestedTransaction) error` to `func(atx *types.AttestedTransaction, commitRound uint64) error` at `dag.go:398-400` (`SetATXProofVerifier`) and the call site `commit.go:197-198` (pass the in-scope commit round, ~223). Update `commit_test.go:1419` and the wiring at `cmd/node/aggregation.go:58`. In `verify.go`, `Verify(atx, commitRound)` computes `commitEpoch = commitEpochForRound(commitRound)`, accepts `attestation_epoch == commitEpoch` or (`== commitEpoch-1` and `commitRound - commitEpoch*epochLength < EpochGraceRounds`), and recomputes holders from `HoldersForEpoch(attestation_epoch)`. Unit-test all four cases (same-epoch, prev-within-grace, prev-past-grace, future).

- [ ] **Step 4: Daemon test** `daemon/aggregation_test.go` with mocked holders: `CollectAttestations` returns one result per replicated ref with an aggregated signature and bitmap; fails fast when negatives make quorum impossible; `computeHolders` matches `attest.ComputeHolders` for the same set/replication.

- [ ] **Step 5: `daemon/daemon.go`** (imports only `internal/attest`, `internal/validators`, `internal/network` message codecs, quic-go): `New(nodeAddrs)` syncs validators + epoch via `MsgGetValidators`/`MsgStatus`; a QUIC round-trip helper; `GetObject`; `Validators`.

- [ ] **Step 6: `daemon/aggregation.go`:** `CollectAttestations` (fetch object to learn content/version/replication, THEN `attest.ComputeHolders`, THEN request attestations in parallel with fail-fast and bounded randomized backoff), local verification (all sigs on the same `H` and `H` matches the fetched object's content), `BuildATX` (tx + replicated objects + quorum proofs + `attestation_epoch` from the synced epoch), `SubmitTransaction` (singleton-only intent submits raw; otherwise collect, build, submit). On a quorum-impossible result, re-sync validators+epoch once and retry before surfacing a typed error. Exclude singletons from collection and the ATX.

- [ ] **Step 7: `client/quic.go`** SDK transport (dial, length-prefixed request/response, the Batch 4 codecs). Do not remove `client/http.go` yet.

- [ ] **Step 8: Build, vet, test** (`go build ./... && go vet ./... && go test ./internal/... ./client/... ./daemon/...`): PASS.

- [ ] **Step 9: Commit.**

```bash
git add daemon/ client/quic.go types/vertex.fbs internal/types/ internal/consensus/ internal/aggregation/verify.go cmd/node/aggregation.go
git commit -F - <<'EOF'
[+] Client daemon, QUIC SDK, epoch-aware ATX verification
	- daemon: sync, rendezvous, collect, build ATX with attestation_epoch
	- Verifier takes commit round; accepts same or previous epoch within grace
	- commitEpochForRound + retained previous holder snapshot
EOF
```

---

## Batch 6: Flip part 1 (switch submission, remove node-side aggregation)

One subagent. Route submission through the daemon path and remove on-chain aggregation. **Green for this batch is build + vet + unit tests (`go test ./internal/... ./client/... ./daemon/...`); integration tests are migrated in Batch 8.**

**Files:** create `internal/validation/validate.go` (+test); modify `cmd/node/{handlers.go,routing.go,aggregation.go,node.go,sync.go}`; delete `internal/aggregation/{aggregator.go,collector.go}` (+tests); modify `internal/aggregation/{handler.go,protocol.go}`.

- [ ] **Step 1: Move validation.** Create `internal/validation/validate.go` from `internal/api/validate.go` (package `validation`, keep the `genesis` import), with its test. Update the caller.

- [ ] **Step 2: Enable submission.** In `cmd/node/handlers.go`, wire `MsgSubmitTx`: a raw transaction (no objects/proofs) is validated (`internal/validation`), checked to reference only singletons, and wrapped into a trivial ATX; a full ATX is validated, then `ATXVerifier.Verify(atx, commitRoundAtIngest)` is deferred to the commit-time verifier already wired in Batch 5 (ingest only does structural + membership checks), then included in a vertex. The lifecycle checks (version, owner, gas, replay) remain in the commit path, not duplicated here.

- [ ] **Step 3: Port inter-node object routing to QUIC.** In `cmd/node/routing.go`, replace `fetchObjectFromHolder(info.HTTPAddr, id)` (HTTP) with a QUIC `MsgGetObject` to `info.QUICAddr`; delete `holderClient` and the `net/http` import. Update `scanObjectsForEpoch` (`cmd/node/aggregation.go:217-228`) and remove the `api.HolderRouter` usage / `newHolderRouter`.

- [ ] **Step 4: Remove node-side aggregation.** Delete `internal/aggregation/aggregator.go`, `collector.go`, and their tests (this removes the top-1 holder rule). Remove references in `cmd/node/` (`node.go` aggregator field, `sync.go`).

- [ ] **Step 5: Simplify the attestation protocol.** In `internal/aggregation/protocol.go` and `handler.go`: remove `WantFull`/`flagWantFull` (`protocol.go:17,30,41-42,60`, `handler.go:54`) and the positive-with-data response; make negative responses static (no BLS signature). The handler now serves stored signatures with the bounded fallback from Batch 3.

- [ ] **Step 6: Build, vet, unit test** (`go build ./... && go vet ./... && go test ./internal/... ./client/... ./daemon/...`): PASS. (Integration deferred.)

- [ ] **Step 7: Commit.**

```bash
git add -A
git commit -F - <<'EOF'
[&] Switch submission to the daemon, remove node-side aggregation
	- MsgSubmitTx: singleton wrap + ATX verify; lifecycle checks stay in commit
	- Inter-node object routing ported from HTTP to QUIC
	- Remove aggregator/collector/top-1/WantFull; static negative responses
	- Structural validation moved to internal/validation
EOF
```

---

## Batch 7: Flip part 2 (purge all HTTP, drop on-chain HTTPAddr, port SDK)

One subagent. Remove every remaining HTTP path and the legacy on-chain HTTP address. **Green is build + vet + unit tests; integration migrates in Batch 8.** This batch is consensus-breaking (register-validator args and the `Validator` type change).

**Files:** delete `internal/api/server.go` (+test), `client/http.go`; modify `cmd/node/{node.go,sync.go,config.go,main.go,init.go,registration.go}`; modify `client/{client.go,transactions.go}`; modify `internal/consensus/{types.go,commit.go,dag.go,epoch.go}`, `internal/genesis/{transaction.go,borsh.go,genesis.go}`, `internal/sync/snapshot.go`, `types/vertex.fbs` (`Validator` table) + regen.

- [ ] **Step 1: Remove the HTTP server and flag.** Delete `internal/api/server.go` and its test (and any remaining `internal/api` files now unused). Remove the `api.New`/`Start` calls (`node.go:128`, `sync.go:76`), the `-http` flag and `HTTPAddress` config (`config.go:16-17,82`), the startup log field (`main.go:48`), and the `init.go:140` use.

- [ ] **Step 2: Remove on-chain HTTPAddr.** `types/vertex.fbs`: remove `http_address` from the `Validator` table; `bash types/generate.sh`. `internal/genesis/borsh.go` + `transaction.go`: drop `httpAddr` from `encodeRegisterValidatorArgs`/`DecodeRegisterValidatorArgs`/`BuildRegisterValidatorTx` (validators register only quicAddr + blsPubkey). `internal/consensus/types.go`: remove `HTTPAddr` from `ValidatorInfo` (25) and from `ValidatorSet.Add` (69, 75-76, 90, 188, 206). `commit.go:622,629`: decode and `Add` without httpAddr. `dag.go:374-375` `AddValidator`; `epoch.go:154,175`. `genesis/genesis.go:16-17,51`. `cmd/node/registration.go`: remove `getRegistrationHTTPAddr`, the `http.Post`, and register with quicAddr only.

- [ ] **Step 3: Snapshots.** `internal/sync/snapshot.go:227,266`: remove the `HTTPAddr` serialize/deserialize for validators; bump `snapshotVersion`.

- [ ] **Step 4: Port the client SDK.** Rewrite `client/client.go` and `client/transactions.go` onto `client/quic.go` and the daemon: `NewClient` syncs via `MsgGetValidators`/`MsgStatus`; submit methods send singleton-only transactions raw and replicated-object transactions through `daemon.SubmitTransaction`. Delete `client/http.go`. Update `client/client_test.go`.

- [ ] **Step 5: Build, vet, unit test** (`go build ./... && go vet ./... && go test ./internal/... ./client/... ./daemon/...`): PASS. Confirm no `net/http` import remains outside tests (`grep -rn 'net/http' --include='*.go' cmd/ internal/ client/ | grep -v _test.go` is empty).

- [ ] **Step 6: Commit.**

```bash
git add -A
git commit -F - <<'EOF'
[&] Purge HTTP: remove API, flag, and on-chain HTTPAddr; port SDK to QUIC
	- Validators register only a QUIC address (consensus-breaking)
	- Remove HTTPAddr from Validator type, registration args, snapshots, genesis
	- Client SDK rewritten onto QUIC; delete client/http.go
EOF
```

---

## Batch 8: Integration tests and documentation

One subagent. Migrate the simulations, add the new scenarios, and update the docs of record. Full suite green.

**Files:** `test/integration/{helpers.go,helpers_tx.go}`, rename `helpers_http.go` to `helpers_quic.go`, all `sim_*_test.go`; `test/integration/TESTING.md`, `docs/ATP.md`, `docs/WHITEPAPER.md`.

- [ ] **Step 1: Port helpers.** Rename `helpers_http.go` to `helpers_quic.go`, rewrite client calls onto the QUIC SDK and daemon. Update `helpers.go` (node startup: drop `-http`, keep `-quic`) and `helpers_tx.go` (daemon for replicated objects, raw for singletons).

- [ ] **Step 2: Migrate sims.** Update `sim_bootstrap_test.go`, `sim_consensus_test.go`, `sim_fees_test.go` (assert 70/30, no aggregator), `sim_epochs_test.go`, `sim_objects_test.go`, `sim_stress_test.go`, `sim_progressive_test.go`. Run each (`go test ./test/integration/ -run TestSim<Name> -count=1 -timeout 10m`) green.

- [ ] **Step 3: New scenarios.** End-to-end client aggregation with all nodes converging; the singleton fast path; a version race during collection (bounded backoff, eventual success or typed failure); an epoch boundary during attestation validity (grace accept, and recollect-after-grace); a cold-holder scenario confirming eager signing plus the bounded fallback leaves no exploitable cold window.

- [ ] **Step 4: Full suite** (`go test ./... -count=1 -timeout 30m`): PASS.

- [ ] **Step 5: Docs of record.** `TESTING.md`: rewrite for the QUIC/daemon architecture and new scenarios. `docs/ATP.md`: remove HTTP-API, aggregator, and 20/30/50 items; add off-chain aggregation, QUIC transport, DoS gates, eager signing, complete reverification, epoch-boundary items. `docs/WHITEPAPER.md`: update the attestation section (off-chain, holder-serves-stored-signature, no top-1, no WantFull), the fee section (70/30, drop `total_aggregator`), the network section (QUIC client surface replaces the REST API, validators publish only a QUIC address).

- [ ] **Step 6: Commit.**

```bash
git add test/ docs/
git commit -F - <<'EOF'
[&] Migrate integration tests to QUIC/daemon, update docs of record
	- Port helpers and all 7 simulations; add fast-path/version-race/epoch/cold-holder
	- Update TESTING.md, ATP.md, and WHITEPAPER.md for the new design
EOF
```

---

## Self-review (run before execution)

- **Spec coverage:** daemon library (B5), QUIC-only incl. inter-node routing + on-chain HTTPAddr removal (B4 GetObject, B6 routing, B7 purge), eager-signature defense + bounded fallback + Retry/scopes/rate-limit (B3, B4), canonical content-hash fix (B1), fee 70/30 across all sites (B2), singleton fast path + singletons-never-in-ATX (B5/B6), WantFull/top-1 removal (B6), daemon/consensus/state decoupling via validators+attest leaf packages (B1), client SDK port (B7), complete reverification with lifecycle layering (B6 + B5 verifier), epoch determinism + grace + attestation_epoch on wire + serializeAttestedTx (B5), tests + ATP + whitepaper (B8). PoW/VDF/capital rejected, not reintroduced.
- **Green-at-each-step:** B1-B5 additive; B6 and B7 unit-green with integration deferred (called out); B8 full-green.
- **Type consistency:** `internal/validators`/`internal/attest`, the `objsig:` layout, the verifier callback signature `func(atx, commitRound)`, `attestation_epoch`, `EpochGraceRounds`, `HoldersForEpoch`, `commitEpochForRound`, and the daemon API are defined once in Shared contracts and referenced identically across batches.
