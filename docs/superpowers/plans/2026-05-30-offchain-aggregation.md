# Off-chain aggregation implementation plan

> **For agentic workers:** This plan is executed by **one subagent per batch** (not per task), via superpowers:subagent-driven-development adapted to batch granularity. Each batch is a self-contained unit a single subagent implements end to end, then a review gate runs before the next batch. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Move attestation aggregation from validators to a client-side Go library, go QUIC-only, add a structural denial-of-service defense (eager per-object signatures), adjust fees to 70/30, and simplify the attestation protocol, without weakening the existing consensus verification.

**Architecture:** Batches 1 to 5 are additive: they build the new path (validators package, eager signatures, QUIC client surface, daemon library) alongside the existing on-chain aggregation and HTTP API, so the tree stays green throughout. Batch 6 is the flip: it removes node-side aggregation and the HTTP API and switches submission to the daemon. Batch 7 migrates the integration tests. The design of record is `docs/superpowers/specs/2026-05-30-offchain-aggregation-design.md`; read it before starting.

**Tech stack:** Go 1.26 node, Rust/WASM pods (untouched here), QUIC (quic-go), BLS12-381 (blst), BLAKE3, FlatBuffers, Pebble.

**Branch:** All work happens on a feature branch created at execution time (not `main`).

---

## Shared contracts

Every batch must use these exact names and formats so the pieces fit together.

**Package layout.**
- `internal/validators/` holds the leaf types `Hash [32]byte`, `ValidatorInfo`, `ValidatorSet`, extracted from `internal/consensus`. It imports nothing from `consensus`.
- `internal/consensus` keeps using those types through aliases (`type Hash = validators.Hash`, etc.), so consensus code is not churned.
- `internal/aggregation/verify.go` holds the ATX verifier (moved from `cmd/node/aggregation.go`).
- `internal/aggregation/sigstore.go` holds the durable signature store helpers.
- `internal/network/clientconn.go` holds the ephemeral client connection tier and the DoS gates.
- `daemon/` is the new client library (top-level package, same module).
- `client/quic.go` is the SDK QUIC transport.

**Attestation signature.** `H = BLAKE3(content || version_u64_BE)`, signed with the holder's BLS key, exactly as `ComputeObjectHash` does today in `internal/aggregation/handler.go`. Deterministic per (object, version).

**Durable signature store.** Pebble key `"objsig:" || objectID[32]` maps to value `version_u64_BE(8) || bls_signature(96)`. Keying by object ID (not by version) means a version advance overwrites the entry, so only the current-version signature is ever stored (automatic eviction). The store lives next to object state and is written in the same batch as the object.

**Wire changes (`types/vertex.fbs`).**
- Remove `total_aggregator:uint64` from the `FeeSummary` table.
- Add `attestation_epoch:uint64` to the `AttestedTransaction` table (the epoch whose holder snapshot the proofs were collected against).

**Daemon public API (`daemon/`).**
- `func New(nodeAddrs []string) (*Daemon, error)` syncs the validator set and current epoch.
- `func (d *Daemon) SubmitTransaction(ctx context.Context, nodeAddr string, txBytes []byte) ([32]byte, error)` runs the full flow and returns the transaction hash.

**Epoch rule.** The ATX carries `attestation_epoch`. A validator computes `commitEpoch` deterministically from the committing round (existing epoch logic). It accepts the ATX if `attestation_epoch == commitEpoch`, or if `attestation_epoch == commitEpoch - 1` and the commit round is within `EpochGraceRounds` of the boundary. The quorum is verified against the retained holder snapshot for `attestation_epoch`. Otherwise the ATX is rejected and the daemon recollects. `EpochGraceRounds` is a named constant (start at 50).

**Singletons.** Singletons (replication 0, including the gas coin) are never carried in an ATX and never attested. A transaction touching only singletons is submitted raw and wrapped by the receiving validator into a trivial ATX (empty objects, empty proofs). A mixed transaction carries only its replicated objects in the ATX; singletons are version-checked locally by each validator from the header, as today.

**Verification layering.** ATX verification (recompute `H`, aggregated signature, bitmap, quorum against the epoch snapshot) runs in addition to the existing lifecycle checks (version vs declared and vs DAG, owner on mutable refs, gas coin, replay). The lifecycle checks are not removed.

**Per-batch gate.** Each batch ends green. Unless stated otherwise, green means `go build ./...`, `go vet ./...`, and `go test ./internal/... ./client/... ./daemon/...` all pass. Integration tests (`./test/integration/`) are migrated only in batch 7; batches 6 may leave them failing, which is called out explicitly.

---

## Batch 1: Foundations (behavior-preserving)

One subagent. Extract the leaf validator types and relocate the ATX verifier. No behavior changes; all existing tests keep passing.

**Files:**
- Create: `internal/validators/validators.go`, `internal/validators/validators_test.go`
- Modify: `internal/consensus/types.go` (replace type definitions with aliases)
- Create: `internal/aggregation/verify.go`
- Modify: `cmd/node/aggregation.go` (remove the verifier, call the relocated one)

- [ ] **Step 1: Read the current types.** Read `internal/consensus/types.go` and note the exact definitions of `Hash`, `ValidatorInfo`, `ValidatorSet` (fields, methods). Read `internal/aggregation/rendezvous.go` to see how it consumes them, and `cmd/node/aggregation.go:106-202` for `buildATXVerifier`/`verifySingleProof`.

- [ ] **Step 2: Write the failing test** in `internal/validators/validators_test.go`:

```go
package validators

import "testing"

func TestValidatorSetHoldersDeterministic(t *testing.T) {
	vs := NewValidatorSet([]ValidatorInfo{
		{Pubkey: Hash{1}, BLSPubkey: make([]byte, 48)},
		{Pubkey: Hash{2}, BLSPubkey: make([]byte, 48)},
		{Pubkey: Hash{3}, BLSPubkey: make([]byte, 48)},
	})
	if vs.Len() != 3 {
		t.Fatalf("expected 3 validators, got %d", vs.Len())
	}
}
```

- [ ] **Step 3: Run it, expect failure** (`go test ./internal/validators/`): FAIL, package/types undefined.

- [ ] **Step 4: Create `internal/validators/validators.go`** by moving the canonical definitions of `Hash`, `ValidatorInfo`, `ValidatorSet` (and their methods) out of `internal/consensus/types.go` into this package verbatim, changing only the package clause to `package validators`. This package must not import `internal/consensus`.

- [ ] **Step 5: Replace the moved definitions in `internal/consensus/types.go` with aliases** so consensus code stays unchanged:

```go
import "BluePods/internal/validators"

type Hash = validators.Hash
type ValidatorInfo = validators.ValidatorInfo
type ValidatorSet = validators.ValidatorSet

// keep any consensus-only constructors as thin forwarders if needed, e.g.:
func NewValidatorSet(v []validators.ValidatorInfo) *validators.ValidatorSet { return validators.NewValidatorSet(v) }
```

Resolve any method-set issues: if methods were defined on these types, they move with the type into `validators`. Aliases share the method set, so consensus callers are unaffected.

- [ ] **Step 6: Point `internal/aggregation/rendezvous.go` at `internal/validators`** instead of `internal/consensus` (change the import and the `consensus.` qualifiers to `validators.`). Do the same for any other file in `internal/aggregation` that referenced `consensus.Hash`/`consensus.ValidatorSet` purely for these types (`bls.go` if applicable).

- [ ] **Step 7: Build and test** (`go build ./... && go test ./internal/...`): PASS. Fix import cycles if any (the only legal direction is `consensus -> validators` and `aggregation -> validators`, never the reverse).

- [ ] **Step 8: Relocate the ATX verifier.** Create `internal/aggregation/verify.go` and move `buildATXVerifier`/`verifySingleProof` (and helpers) from `cmd/node/aggregation.go` into it as exported functions, e.g. `func NewATXVerifier(...) *ATXVerifier` and `func (v *ATXVerifier) Verify(atx *types.AttestedTransaction) error`. Keep the logic identical for now. Update `cmd/node/aggregation.go` to call the relocated verifier.

- [ ] **Step 9: Build, vet, test** (`go build ./... && go vet ./... && go test ./internal/... ./client/...`): PASS.

- [ ] **Step 10: Commit.**

```bash
git add internal/validators/ internal/consensus/types.go internal/aggregation/ cmd/node/aggregation.go
git commit -F - <<'EOF'
[&] Extract validator leaf types, relocate ATX verifier
	- internal/validators holds Hash, ValidatorInfo, ValidatorSet
	- consensus keeps them via type aliases (no churn)
	- ATX verifier moved to internal/aggregation/verify.go
EOF
```

---

## Batch 2: Fee model (70/30, remove aggregator)

One subagent. Remove the aggregator share everywhere it is computed, written, and validated. Consensus-affecting but self-contained.

**Files:**
- Modify: `types/vertex.fbs`, regenerate `internal/types/FeeSummary.go`
- Modify: `internal/consensus/fees.go`, `internal/consensus/commit.go`, `internal/consensus/build.go`, `internal/consensus/validate.go`
- Modify: `internal/consensus/fees_test.go`, `internal/consensus/validate_test.go`

- [ ] **Step 1: Update the fee constants.** In `internal/consensus/fees.go`, remove the `AggregatorBPS` field and constant (currently `AggregatorBPS: 2000` at lines ~16-41), set `EpochBPS: 7000` and keep `BurnBPS: 3000`. Remove the `aggregator := total * params.AggregatorBPS / bpsMax` computation (line ~180) and the `Aggregator` field from the fee-split struct. The split is now `burned = total*BurnBPS/bpsMax`, `epoch = total - burned`.

- [ ] **Step 2: Update the fee test first to lock the new split.** In `internal/consensus/fees_test.go`, change the expected split (the test around line ~262 that sets `AggregatorBPS: 2000, BurnBPS: 3000`) to assert 70/30 with no aggregator field. Run `go test ./internal/consensus/ -run Fee`: expect FAIL until the code compiles, then PASS.

- [ ] **Step 3: Remove the wire field.** In `types/vertex.fbs`, delete `total_aggregator:uint64;` from the `FeeSummary` table. Regenerate the Go type (the repo's flatc invocation; if a Makefile target exists use it, otherwise hand-edit `internal/types/FeeSummary.go` to remove the `TotalAggregator` accessor and `FeeSummaryAddTotalAggregator`, matching the regenerated layout).

- [ ] **Step 4: Remove the build path.** In `internal/consensus/build.go` (`buildFeeSummary`, lines ~78-97), remove the `FeeSummaryAddTotalAggregator` call and the aggregator accumulation.

- [ ] **Step 5: Remove the accumulation and crediting.** In `internal/consensus/commit.go`, remove `vertexFees.Aggregator` accumulation (lines ~156-175), the `creditAggregator(producer, split.Aggregator)` call (lines ~299-302), and the `creditAggregator` function itself (lines ~438-453, currently a no-op stub).

- [ ] **Step 6: Remove the consensus check.** In `internal/consensus/validate.go` (lines ~246-261), remove the `declared.TotalAggregator() != totalAgg` recomputation and mismatch rejection. Update `internal/consensus/validate_test.go` cases that assert on `total_aggregator`.

- [ ] **Step 7: Build, vet, test** (`go build ./... && go vet ./... && go test ./internal/...`): PASS.

- [ ] **Step 8: Commit.**

```bash
git add types/vertex.fbs internal/types/FeeSummary.go internal/consensus/
git commit -F - <<'EOF'
[&] Fee model: 70 epoch / 30 burn, remove aggregator share
	- Drop AggregatorBPS, EpochBPS now 7000
	- Remove total_aggregator from FeeSummary wire + validation
	- Remove creditAggregator stub and accumulation
EOF
```

---

## Batch 3: Durable eager signatures (additive)

One subagent. Add the signature store and the eager signing hook at execution, plus a handler path that serves stored signatures. Do not touch `WantFull` or the negative-response path yet (the existing collector still needs them); those are removed in batch 6. Old path stays green.

**Files:**
- Create: `internal/aggregation/sigstore.go`, `internal/aggregation/sigstore_test.go`
- Modify: the object-apply path in `internal/state/` (where a held object is persisted during execution)
- Modify: `internal/aggregation/handler.go` (add a serve-from-store path)
- Modify: `internal/sync/` (include signatures in snapshots)

- [ ] **Step 1: Read the apply path.** Read `internal/state/store.go` (object persistence, e.g. `Set(id, data)` around line 31) and the commit/execution path in `internal/consensus/commit.go` that applies object writes for held objects, to find the exact point a holder persists a new object version. Read `internal/aggregation/handler.go` and `internal/aggregation/bls.go` for the signing key access.

- [ ] **Step 2: Write the failing test** `internal/aggregation/sigstore_test.go`:

```go
func TestSigStorePutGetCurrentVersion(t *testing.T) {
	st := newTestStore(t) // wraps an in-memory Pebble
	var id [32]byte
	id[0] = 0xAB
	sig := make([]byte, 96)
	sig[0] = 0x01
	PutObjectSig(st, id, 7, sig)

	gotVer, gotSig, ok := GetObjectSig(st, id)
	if !ok || gotVer != 7 || gotSig[0] != 0x01 {
		t.Fatalf("want v7 sig, got ok=%v ver=%d", ok, gotVer)
	}
	// version advance overwrites (eviction)
	sig2 := make([]byte, 96)
	sig2[0] = 0x02
	PutObjectSig(st, id, 8, sig2)
	gotVer, gotSig, _ = GetObjectSig(st, id)
	if gotVer != 8 || gotSig[0] != 0x02 {
		t.Fatalf("want v8 sig after advance, got ver=%d", gotVer)
	}
}
```

- [ ] **Step 3: Run it, expect failure.** `go test ./internal/aggregation/ -run SigStore`: FAIL, undefined.

- [ ] **Step 4: Implement `internal/aggregation/sigstore.go`:**

```go
package aggregation

import (
	"encoding/binary"
	"BluePods/internal/storage"
)

const sigKeyPrefix = "objsig:"

func sigKey(objectID [32]byte) []byte {
	k := make([]byte, len(sigKeyPrefix)+32)
	copy(k, sigKeyPrefix)
	copy(k[len(sigKeyPrefix):], objectID[:])
	return k
}

// PutObjectSig stores the current-version BLS signature for an object,
// overwriting any prior version (automatic eviction).
func PutObjectSig(db storage.KV, objectID [32]byte, version uint64, sig []byte) error {
	val := make([]byte, 8+len(sig))
	binary.BigEndian.PutUint64(val[:8], version)
	copy(val[8:], sig)
	return db.Set(sigKey(objectID), val)
}

// GetObjectSig returns the stored (version, signature) for an object.
func GetObjectSig(db storage.KV, objectID [32]byte) (uint64, []byte, bool) {
	val, err := db.Get(sigKey(objectID))
	if err != nil || len(val) < 8+96 {
		return 0, nil, false
	}
	return binary.BigEndian.Uint64(val[:8]), val[8:], true
}
```

Match `storage.KV` to the real `internal/storage` interface (it exposes `Get`/`Set`); adjust the signature if the method set differs.

- [ ] **Step 5: Run it, expect PASS.** `go test ./internal/aggregation/ -run SigStore`.

- [ ] **Step 6: Add the eager signing hook.** At the point a holder persists a new object version (found in step 1), after the object `Set`, compute and store the signature in the same write batch:

```go
// holder eagerly signs the new version it just persisted
hash := ComputeObjectHash(objectContent, version)
sig := blsKey.Sign(hash[:]) // same key path as handler.go
_ = PutObjectSig(db, objectID, version, sig)
```

Guard it so it runs only when this node is a holder of the object (the apply path already only writes objects this node holds, so persistence implies holding; confirm in step 1). Thread the node's BLS key into the apply path if not already available.

- [ ] **Step 7: Add a serve-from-store path in the handler.** In `internal/aggregation/handler.go`, before the eager-sign-on-request logic, add: if `GetObjectSig` returns a signature whose version equals the requested version, return it directly without re-signing. Keep the existing sign-on-request path as a fallback for now (it stays until batch 6 to avoid changing the collector's expectations).

- [ ] **Step 8: Include signatures in snapshots.** In `internal/sync/`, find the snapshot encode/decode of object state and add the `objsig:` entries so a node syncing as a new holder receives current-version signatures. Write a round-trip test asserting a synced node can serve an attestation for a held object without re-signing.

- [ ] **Step 9: Build, vet, test** (`go build ./... && go vet ./... && go test ./internal/...`): PASS.

- [ ] **Step 10: Commit.**

```bash
git add internal/aggregation/sigstore.go internal/aggregation/sigstore_test.go internal/aggregation/handler.go internal/state/ internal/sync/ internal/consensus/commit.go
git commit -F - <<'EOF'
[+] Eager per-object BLS signatures with durable store
	- Sign at execution when a holder persists a new version
	- objsig: store keyed by object ID, current version only
	- Handler serves stored signature; signatures travel in snapshots
EOF
```

---

## Batch 4: QUIC client surface and DoS gates (additive)

One subagent. Add the client-facing QUIC handlers, an ephemeral untrusted client connection tier, and the ingress gates, alongside the existing HTTP and validator mesh. Nothing is removed.

**Files:**
- Modify: `internal/network/node.go`, `internal/network/peer.go`, `internal/network/protocol.go`
- Create: `internal/network/clientconn.go`, `internal/network/messages.go`, `internal/network/clientconn_test.go`
- Modify: `cmd/node/handlers.go` (route new client message types)

- [ ] **Step 1: Read the network layer.** Read `internal/network/node.go` (the QUIC server setup, `ClientAuth`, `setupPeer`, `handlePeerDisconnect`) and `cmd/node/handlers.go` (the `OnRequest` first-byte dispatch). Note how attestation/snapshot requests are routed by message type.

- [ ] **Step 2: Define client message types** in `internal/network/messages.go`: encode/decode for `MsgSubmitTx` (carries a raw tx or an ATX, distinguished by structure), `MsgGetObject`, `MsgGetValidators` (response includes the current epoch and per-validator pubkey/BLS/QUIC addr), `MsgStatus`, `MsgHealth`, `MsgFaucet`, `MsgDomainResolve`. Use the existing length-prefixed framing. Write encode/decode round-trip tests.

- [ ] **Step 3: Add the ephemeral client tier.** In `internal/network/clientconn.go`, accept client QUIC connections that are NOT added to the persistent `peers` map and NOT scheduled for reconnect. Distinguish a client connection from a validator connection (validators present a known cert / are in the validator set; clients are not). Close client connections on idle.

- [ ] **Step 4: Enable QUIC Retry address validation** for the listener so an Initial packet must echo a Retry token before the server does work. Set it in the `quic.Config`/transport used for incoming connections. Add a test or a documented manual check that a connection completes only after the Retry round-trip.

- [ ] **Step 5: Add per-peer resource scopes and rate limiting.** In `clientconn.go`, cap concurrent client connections per remote IP, in-flight streams per connection, and total pending bytes, returning a typed "resource limit exceeded" error before any handler work. Add a token-bucket rate limiter per IP. Write `clientconn_test.go` asserting that exceeding the per-IP connection cap is rejected and that the limiter throttles. (The frequency/error blocklist is deferred per the spec; do not build it.)

- [ ] **Step 6: Route the new messages.** In `cmd/node/handlers.go`, extend the first-byte dispatch to handle the new client message types: `MsgGetObject` reads object state; `MsgGetValidators` returns the set plus epoch; `MsgStatus`/`MsgHealth` return node status; `MsgFaucet`/`MsgDomainResolve` mirror the existing HTTP handlers; `MsgSubmitTx` is wired in batch 6 (for now return "not yet enabled" so the surface exists without changing submission).

- [ ] **Step 7: Build, vet, test** (`go build ./... && go vet ./... && go test ./internal/... ./client/...`): PASS. HTTP still works.

- [ ] **Step 8: Commit.**

```bash
git add internal/network/ cmd/node/handlers.go
git commit -F - <<'EOF'
[+] QUIC client surface and ingress DoS gates
	- Client message types: object, validators+epoch, status, faucet, domain
	- Ephemeral untrusted client tier (no mesh, no reconnect)
	- QUIC Retry address validation, per-IP scopes, rate limiting
EOF
```

---

## Batch 5: Daemon library and ATX ingest with epoch rule (additive)

One subagent. Build the client daemon, the SDK QUIC transport, and the full ATX ingest verification with the epoch-boundary rule. The daemon is new code; the old path still works, so the tree stays green.

**Files:**
- Create: `daemon/daemon.go`, `daemon/aggregation.go`, `daemon/aggregation_test.go`
- Create: `client/quic.go`
- Modify: `types/vertex.fbs` (add `attestation_epoch` to `AttestedTransaction`), regenerate `internal/types/AttestedTransaction.go`
- Modify: `internal/aggregation/verify.go` (epoch-aware quorum check)
- Modify: `internal/consensus/epoch.go` (retain previous epoch holder snapshot, add `EpochGraceRounds`)

- [ ] **Step 1: Add the wire field.** In `types/vertex.fbs`, add `attestation_epoch:uint64;` to the `AttestedTransaction` table, regenerate `internal/types/AttestedTransaction.go` (add the accessor and `AttestedTransactionAddAttestationEpoch`).

- [ ] **Step 2: Retain the previous epoch snapshot.** In `internal/consensus/epoch.go`, where `snapshotEpochHolders` runs (around line 144), keep the prior epoch's snapshot accessible (e.g. `d.prevEpochHolders`) and add `const EpochGraceRounds = 50`. Expose a lookup `HoldersForEpoch(epoch uint64) (*validators.ValidatorSet, bool)` that returns the current or previous snapshot.

- [ ] **Step 3: Make the verifier epoch-aware.** In `internal/aggregation/verify.go`, change `Verify` to take the ATX's `attestation_epoch` and the deterministic `commitEpoch`, accept only if `attestation_epoch == commitEpoch` or (`attestation_epoch == commitEpoch-1` and the commit round is within `EpochGraceRounds` of the boundary), and recompute holders against `HoldersForEpoch(attestation_epoch)`. Write a unit test covering: same-epoch accept, previous-epoch-within-grace accept, previous-epoch-past-grace reject, future-epoch reject.

- [ ] **Step 4: Write the daemon failing test** `daemon/aggregation_test.go` with mocked holders (reuse the pattern from the deleted branch if useful): assert `CollectAttestations` returns one `ObjectResult` per replicated ref with an aggregated signature and a bitmap, fails fast when negatives make quorum impossible, and that `computeHolders` matches `internal/aggregation` rendezvous for the same set and replication.

- [ ] **Step 5: Implement `daemon/daemon.go`** (validator+epoch sync, QUIC round-trip helper, `GetObject`, `Validators`): the daemon dials a node over QUIC, sends `MsgGetValidators`, stores the set and epoch; `GetObject` sends `MsgGetObject`. Use `internal/validators` for holder math, never import `internal/consensus`.

- [ ] **Step 6: Implement `daemon/aggregation.go`:** `CollectAttestations` (fetch object to learn content/version/replication, THEN `computeHolders`, THEN request attestations from holders in parallel with fail-fast and bounded randomized backoff), local verification (all signatures on the same `H`, and `H` matches the fetched object), `BuildATX` (assemble tx + replicated objects + quorum proofs + `attestation_epoch` from the synced epoch), `SubmitTransaction` (collect, build, submit; singleton-only intent skips collection and submits the raw tx). Exclude singletons from collection and from the ATX object list.

- [ ] **Step 7: Implement `client/quic.go`** as the SDK transport (dial, length-prefixed request/response, the message encoders from batch 4). Do not yet remove `client/http.go` (that is batch 6).

- [ ] **Step 8: Run daemon tests, expect PASS.** `go test ./daemon/`.

- [ ] **Step 9: Build, vet, test** (`go build ./... && go vet ./... && go test ./internal/... ./client/... ./daemon/...`): PASS.

- [ ] **Step 10: Commit.**

```bash
git add daemon/ client/quic.go types/vertex.fbs internal/types/AttestedTransaction.go internal/aggregation/verify.go internal/consensus/epoch.go
git commit -F - <<'EOF'
[+] Client daemon, QUIC SDK transport, epoch-aware ATX verify
	- daemon: sync, rendezvous, collect, build ATX with attestation_epoch
	- verifier accepts same-epoch or previous-epoch within grace window
	- consensus retains previous epoch holder snapshot
EOF
```

---

## Batch 6: The flip (remove the old path)

One subagent. Switch submission to the daemon path and tear out node-side aggregation, the HTTP API, and the obsolete attestation features. After this batch the system is QUIC-only.

**Green for this batch is `go build ./...`, `go vet ./...`, and unit tests `go test ./internal/... ./client/... ./daemon/...`. Integration tests under `./test/integration/` will fail until batch 7; that is expected.**

**Files:**
- Delete: `internal/aggregation/aggregator.go`, `internal/aggregation/collector.go` (+ their tests), `internal/api/server.go` (+ test), `client/http.go`
- Modify: `internal/aggregation/handler.go`, `internal/aggregation/protocol.go`, `internal/aggregation/types.go` (remove `WantFull`, make negatives static)
- Modify: `client/client.go`, `client/transactions.go` (port to QUIC)
- Modify: `cmd/node/handlers.go` (enable `MsgSubmitTx`), `cmd/node/` wiring, `internal/validation/` (move from `internal/api/validate.go`)

- [ ] **Step 1: Move structural validation.** Create `internal/validation/validate.go` from `internal/api/validate.go` (change package, keep the `genesis` dependency), with a test file moved alongside. Update callers.

- [ ] **Step 2: Enable ATX submission.** In `cmd/node/handlers.go`, wire `MsgSubmitTx`: if the message is a raw transaction (no objects/proofs), validate it references only singletons and wrap it in a trivial ATX; if it is a full ATX, run `internal/validation` then the epoch-aware `ATXVerifier.Verify`, then the existing lifecycle checks, then include it in a vertex. Reuse the verifier from batch 5.

- [ ] **Step 3: Remove `WantFull` and static-negative the handler.** In `internal/aggregation/protocol.go`, `handler.go`, `types.go`: delete the `WantFull` flag, the positive-with-data response suffix, and make `buildNegativeResponse` return a static error with no BLS signature. The handler now only serves stored signatures (the batch-3 fallback sign-on-request can be removed since eager signing guarantees presence; keep a one-line sign-and-store on a store miss for safety, but never sign on negatives).

- [ ] **Step 4: Delete node-side aggregation.** Remove `internal/aggregation/aggregator.go`, `collector.go`, and their test files (this also removes the top-1 holder rule in `collector.go`). Remove any references in `cmd/node/`.

- [ ] **Step 5: Remove the HTTP API.** Delete `internal/api/server.go` and its test. Remove the HTTP listener startup from `cmd/node/`. Remove the `-http` flag.

- [ ] **Step 6: Port the client SDK.** Rewrite `client/client.go` and `client/transactions.go` to use `client/quic.go` (the batch-4/5 transport and the daemon for replicated-object submissions): `NewClient` syncs via `MsgGetValidators`/`MsgStatus`; the submit methods route singleton-only transactions raw and replicated-object transactions through `daemon.SubmitTransaction`. Delete `client/http.go`. Update `client/client_test.go`.

- [ ] **Step 7: Build, vet, unit test** (`go build ./... && go vet ./... && go test ./internal/... ./client/... ./daemon/...`): PASS. Do not run `./test/integration/` yet.

- [ ] **Step 8: Commit.**

```bash
git add -A
git commit -F - <<'EOF'
[&] Flip to off-chain aggregation, QUIC only
	- Submission goes through the daemon; node verifies and orders
	- Remove node-side aggregator/collector, top-1 holder, WantFull
	- Remove HTTP API; port client SDK to QUIC
	- Structural validation moved to internal/validation
EOF
```

---

## Batch 7: Integration tests and ATP

One subagent. Migrate the multi-node simulations to QUIC and client-side aggregation, add the new scenarios, and update the acceptance test plan. Full suite green.

**Files:**
- Modify: `test/integration/helpers.go`, `test/integration/helpers_tx.go`, rename `helpers_http.go` to `helpers_quic.go`
- Modify: all `test/integration/sim_*_test.go`
- Modify: `test/integration/TESTING.md`, `docs/ATP.md`

- [ ] **Step 1: Port the helpers.** Rename `test/integration/helpers_http.go` to `helpers_quic.go` and rewrite its client calls to use the QUIC SDK and the daemon. Update `helpers.go` (node startup: drop `-http`, keep `-quic`) and `helpers_tx.go` (submit via daemon for replicated objects, raw for singletons).

- [ ] **Step 2: Migrate each simulation.** Update `sim_bootstrap_test.go`, `sim_consensus_test.go`, `sim_fees_test.go` (assert 70/30, no aggregator), `sim_epochs_test.go`, `sim_objects_test.go`, `sim_stress_test.go`, `sim_progressive_test.go` to the QUIC/daemon path. Run each (`go test ./test/integration/ -run TestSim<Name> -count=1 -timeout 10m`) and fix until green.

- [ ] **Step 3: Add new scenarios.** Add tests for: end-to-end client aggregation with all nodes converging on the same state; the singleton fast path (raw tx wrapped into a trivial ATX); a version race during collection (concurrent writers, bounded-backoff retry, eventual success or typed failure); an epoch boundary crossed during an attestation's validity (grace-window accept, and recollect-after-grace); a cold-holder scenario confirming a freshly synced holder serves attestations without an exploitable cold window.

- [ ] **Step 4: Run the full suite** (`go test ./... -count=1 -timeout 30m`): PASS.

- [ ] **Step 5: Update docs.** Rewrite `test/integration/TESTING.md` for the QUIC/daemon architecture and the new scenarios. Update `docs/ATP.md`: remove items on the HTTP API, the aggregator role, and the 20/30/50 split; add items for off-chain aggregation, QUIC transport, the DoS gates, eager signing, the complete validator reverification, and the epoch-boundary rule.

- [ ] **Step 6: Commit.**

```bash
git add test/ docs/ATP.md
git commit -F - <<'EOF'
[&] Migrate integration tests to QUIC and client aggregation
	- Port helpers and all 7 simulations to the daemon path
	- New scenarios: fast path, version race, epoch boundary, cold holder
	- Update TESTING.md and ATP.md
EOF
```

---

## Self-review (run before execution)

- **Spec coverage:** daemon library (B5), QUIC-only transport + ops messages (B4, B6), eager-signature DoS defense + Retry/scopes/rate-limit (B3, B4), fee 70/30 across all sites (B2), singleton fast path + WantFull/top-1 removal + TX/ATX-by-structure (B6), complete validator reverification + lifecycle layering (B6 step 2, B5 verifier), epoch determinism + grace + attestation_epoch on wire (B5), daemon/consensus decoupling (B1), client SDK port (B6), tests + ATP (B7). All spec sections map to a batch.
- **Green-at-each-step:** B1-B5 additive; B6 unit-green with integration deferred (called out); B7 full-green. The one intentional red window (integration tests during B6) is explicit.
- **Type consistency:** `internal/validators` types, the `objsig:` key format, `attestation_epoch`, `EpochGraceRounds`, and the daemon API are defined once in Shared contracts and referenced identically across batches.
