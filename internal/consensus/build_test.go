package consensus

import (
	"bytes"
	"crypto/ed25519"
	"errors"
	"testing"

	flatbuffers "github.com/google/flatbuffers/go"
	"github.com/zeebo/blake3"

	"BluePods/internal/types"
)

// TestTryRebuildAttestedTx_MalformedNoPanic verifies that garbage bytes
// do not crash the node — tryRebuildAttestedTx returns (0, false).
func TestTryRebuildAttestedTx_MalformedNoPanic(t *testing.T) {
	db := newTestStorage(t)
	validators, vs := newTestValidatorSet(1)

	dag := New(db, vs, nil, testSystemPod, 1, validators[0].privKey, nil)
	defer dag.Close()

	builder := flatbuffers.NewBuilder(256)

	// Garbage bytes long enough to pass the len<8 check
	garbage := []byte{0xFF, 0xFE, 0xFD, 0xFC, 0xFB, 0xFA, 0xF9, 0xF8, 0xF7, 0xF6}

	offset, ok := dag.tryRebuildAttestedTx(builder, garbage)
	if ok {
		t.Fatal("expected ok=false for garbage data")
	}

	if offset != 0 {
		t.Fatalf("expected offset=0, got %d", offset)
	}
}

// TestTryRebuildAttestedTx_TruncatedNoPanic verifies that a valid ATX header
// with a truncated body does not crash — returns (0, false).
func TestTryRebuildAttestedTx_TruncatedNoPanic(t *testing.T) {
	db := newTestStorage(t)
	validators, vs := newTestValidatorSet(1)

	dag := New(db, vs, nil, testSystemPod, 1, validators[0].privKey, nil)
	defer dag.Close()

	builder := flatbuffers.NewBuilder(256)

	// Build a valid ATX, then truncate it
	validATX := buildTestATX(t, "test_func", nil, nil, 0)
	truncated := validATX[:len(validATX)/2]

	offset, ok := dag.tryRebuildAttestedTx(builder, truncated)
	if ok {
		t.Fatal("expected ok=false for truncated data")
	}

	if offset != 0 {
		t.Fatalf("expected offset=0, got %d", offset)
	}
}

// TestTryRebuildAttestedTx_ValidRoundtrip verifies that a valid ATX is rebuilt correctly.
func TestTryRebuildAttestedTx_ValidRoundtrip(t *testing.T) {
	db := newTestStorage(t)
	validators, vs := newTestValidatorSet(1)

	dag := New(db, vs, nil, testSystemPod, 1, validators[0].privKey, nil)
	defer dag.Close()

	builder := flatbuffers.NewBuilder(1024)

	validATX := buildTestATX(t, "test_func", nil, nil, 0)

	offset, ok := dag.tryRebuildAttestedTx(builder, validATX)
	if !ok {
		t.Fatal("expected ok=true for valid ATX data")
	}

	if offset == 0 {
		t.Fatal("expected non-zero offset for valid ATX")
	}

	// Verify the rebuilt ATX is readable
	builder.Finish(offset)
	rebuilt := builder.FinishedBytes()
	atx := types.GetRootAsAttestedTransaction(rebuilt, 0)
	tx := atx.Transaction(nil)

	if tx == nil {
		t.Fatal("rebuilt ATX has nil transaction")
	}

	if string(tx.FunctionName()) != "test_func" {
		t.Fatalf("expected function 'test_func', got '%s'", string(tx.FunctionName()))
	}
}

// TestBuildVertex_TimestampSignedAndHashed verifies a produced vertex carries a
// non-zero timestamp, that the timestamp is covered by the signed hash, and that
// the signature verifies against the producer over that hash.
func TestBuildVertex_TimestampSignedAndHashed(t *testing.T) {
	db := newTestStorage(t)
	validators, vs := newTestValidatorSet(1)

	dag := New(db, vs, nil, testSystemPod, 1, validators[0].privKey, nil)
	defer dag.Close()

	data := dag.buildVertex(0, nil, nil)
	vertex := types.GetRootAsVertex(data, 0)

	if vertex.Timestamp() == 0 {
		t.Fatal("expected non-zero vertex timestamp")
	}

	pubkey := vertex.ProducerBytes()
	if !ed25519.Verify(pubkey, vertex.HashBytes(), vertex.SignatureBytes()) {
		t.Fatal("signature does not verify against the signed hash")
	}
}

// TestBuildVertex_TimestampInHash verifies that the timestamp feeds the vertex
// hash: two unsigned bodies differing only in timestamp must hash differently.
func TestBuildVertex_TimestampInHash(t *testing.T) {
	db := newTestStorage(t)
	validators, vs := newTestValidatorSet(1)

	dag := New(db, vs, nil, testSystemPod, 1, validators[0].privKey, nil)
	defer dag.Close()

	hashAt := func(ts uint64) Hash {
		builder := flatbuffers.NewBuilder(1024)
		unsigned := dag.buildUnsignedVertex(builder, vertexParts{timestamp: ts})
		identity, _ := vertexIdentity(types.GetRootAsVertex(unsigned, 0))
		return identity
	}

	if hashAt(1000) == hashAt(2000) {
		t.Fatal("vertices differing only in timestamp must hash differently")
	}
}

// TestVertexHeader_EndToEnd verifies a produced vertex carries a complete detached
// header — a 32-byte body hash covering the body it ships — and that its identity
// is the hash of that header, signed by the producer.
func TestVertexHeader_EndToEnd(t *testing.T) {
	db := newTestStorage(t)
	validators, vs := newTestValidatorSet(1)

	dag := New(db, vs, nil, testSystemPod, 1, validators[0].privKey, nil)
	defer dag.Close()

	data := dag.buildVertex(0, nil, [][]byte{buildTestATX(t, "anchor_fn", nil, nil, 0)})
	v := types.GetRootAsVertex(data, 0)

	if len(v.BodyHashBytes()) != 32 {
		t.Fatalf("body_hash size = %d, want 32", len(v.BodyHashBytes()))
	}

	if err := dag.validateSignature(v); err != nil {
		t.Fatalf("produced vertex does not verify: %v", err)
	}

	if got := blake3.Sum256(append([]byte{headerDomainTag}, headerBytes(v)...)); !bytes.Equal(got[:], v.HashBytes()) {
		t.Fatal("vertex identity is not the hash of its declared header")
	}

	if n := len(headerBytes(v)); n != headerSize {
		t.Fatalf("header encoding = %d bytes, want %d", n, headerSize)
	}
}

// TestVertexHeader_TamperedTransactionRejected verifies the body hash binds the
// body to the signed header: flipping one byte inside a committed transaction
// breaks validation even though the signature bytes are untouched.
func TestVertexHeader_TamperedTransactionRejected(t *testing.T) {
	db := newTestStorage(t)
	validators, vs := newTestValidatorSet(1)

	dag := New(db, vs, nil, testSystemPod, 1, validators[0].privKey, nil)
	defer dag.Close()

	data := dag.buildVertex(0, nil, [][]byte{buildTestATX(t, "anchor_fn", nil, nil, 0)})
	v := types.GetRootAsVertex(data, 0)

	if err := dag.validateSignature(v); err != nil {
		t.Fatalf("untampered vertex must verify: %v", err)
	}

	var atx types.AttestedTransaction
	if !v.Transactions(&atx, 0) {
		t.Fatal("produced vertex carries no transaction")
	}

	if !atx.Transaction(nil).MutateHash(0, 0xFF) {
		t.Fatal("could not tamper with the transaction")
	}

	if err := dag.validateSignature(v); err == nil {
		t.Fatal("a tampered transaction must break validation")
	} else if !errors.Is(err, errBadSignature) {
		t.Fatalf("tampering must be a bad_signature rejection, got: %v", err)
	}
}

// TestVertexHeader_TamperedIndexRootRejected verifies the anchor is inside the
// signed header: flipping one byte of index_root breaks validation.
func TestVertexHeader_TamperedIndexRootRejected(t *testing.T) {
	db := newTestStorage(t)
	validators, vs := newTestValidatorSet(1)

	dag := New(db, vs, nil, testSystemPod, 1, validators[0].privKey, nil)
	defer dag.Close()

	data := dag.buildVertex(0, nil, nil)
	v := types.GetRootAsVertex(data, 0)

	if !v.MutateIndexRoot(0, 0xFF) {
		t.Fatal("could not tamper with index_root")
	}

	if err := dag.validateSignature(v); err == nil {
		t.Fatal("a tampered index_root must break validation")
	} else if !errors.Is(err, errBadSignature) {
		t.Fatalf("tampering must be a bad_signature rejection, got: %v", err)
	}
}

// TestVertexHeader_LightVerificationWithoutBody verifies the header is detached:
// {producer, round, epoch, frontier_round, index_root, body_hash, signature} — a
// couple of hundred bytes carrying no parents, no transactions, no fee summary —
// is enough to check the producer's signature over the vertex identity.
func TestVertexHeader_LightVerificationWithoutBody(t *testing.T) {
	db := newTestStorage(t)
	validators, vs := newTestValidatorSet(1)

	dag := New(db, vs, nil, testSystemPod, 1, validators[0].privKey, nil)
	defer dag.Close()

	data := dag.buildVertex(0, nil, [][]byte{buildTestATX(t, "anchor_fn", nil, nil, 0)})
	full := types.GetRootAsVertex(data, 0)

	headerOnly := detachHeader(full)
	if len(headerOnly) > 256 {
		t.Fatalf("detached header is %d bytes, expected a couple of hundred", len(headerOnly))
	}

	light := types.GetRootAsVertex(headerOnly, 0)

	if light.TransactionsLength() != 0 || light.ParentsLength() != 0 {
		t.Fatal("the detached header must carry no body")
	}

	if !bytes.Equal(headerBytes(light), headerBytes(full)) {
		t.Fatal("detached header does not encode to the full vertex's header")
	}

	identity := blake3.Sum256(append([]byte{headerDomainTag}, headerBytes(light)...))
	if !bytes.Equal(identity[:], full.HashBytes()) {
		t.Fatal("header hash does not reproduce the vertex identity")
	}

	if !ed25519.Verify(light.ProducerBytes(), identity[:], light.SignatureBytes()) {
		t.Fatal("signature does not verify against the detached header alone")
	}
}

// TestVertexHeader_EpochIsTheLiveEpoch verifies production stamps the LIVE epoch
// the commit path maintains, not the construction-time argument (which is 0 on
// every node and left every header claiming the genesis epoch).
func TestVertexHeader_EpochIsTheLiveEpoch(t *testing.T) {
	db := newTestStorage(t)
	validators, vs := newTestValidatorSet(1)

	dag := New(db, vs, nil, testSystemPod, 1, validators[0].privKey, nil, WithEpochLength(10))
	defer dag.Close()

	dag.commitMu.Lock()
	dag.setCurrentEpoch(3)
	dag.commitMu.Unlock()

	v := types.GetRootAsVertex(dag.buildVertex(30, nil, nil), 0)

	if v.Epoch() != 3 {
		t.Fatalf("vertex epoch = %d, want the live epoch 3", v.Epoch())
	}

	if err := dag.validateEpoch(v); err != nil {
		t.Fatalf("a vertex stamped with the live epoch must validate: %v", err)
	}
}

// TestValidateEpoch_BoundarySkew verifies a vertex produced in epoch N still
// validates on a receiver that has already transitioned to N+1 (and the other way
// round), while an epoch its own round cannot have reached is rejected. Producers
// cross a boundary at different moments; rejecting the in-flight ones would drop
// every vertex around every boundary.
func TestValidateEpoch_BoundarySkew(t *testing.T) {
	db := newTestStorage(t)
	validators, vs := newTestValidatorSet(4)

	dag := New(db, vs, nil, testSystemPod, 1, validators[0].privKey, nil, WithEpochLength(10))
	defer dag.Close()

	dag.commitMu.Lock()
	dag.setCurrentEpoch(2)
	dag.commitMu.Unlock()

	// Round 25 belongs to epoch 2: a producer still in epoch 1 (it had not
	// committed round 20 yet when it produced) and one already in epoch 2 both pass.
	for _, epoch := range []uint64{1, 2} {
		v := types.GetRootAsVertex(buildTestVertex(t, validators[1], 25, nil, epoch), 0)
		if err := dag.validateEpoch(v); err != nil {
			t.Fatalf("epoch %d at round 25 must validate on a receiver in epoch 2: %v", epoch, err)
		}
	}

	// A receiver still catching up must accept the epoch it has not reached yet,
	// or it can never buffer the tip vertices that let it catch up.
	dag.commitMu.Lock()
	dag.setCurrentEpoch(0)
	dag.commitMu.Unlock()

	v := types.GetRootAsVertex(buildTestVertex(t, validators[1], 25, nil, 2), 0)
	if err := dag.validateEpoch(v); err != nil {
		t.Fatalf("a vertex from an epoch ahead of the receiver must validate: %v", err)
	}

	// Epoch 3 is the top of the window at round 25 (a producer that committed
	// round 30 and resumed production below it), epoch 4 is beyond any honest skew.
	forged := types.GetRootAsVertex(buildTestVertex(t, validators[1], 25, nil, 4), 0)
	if err := dag.validateEpoch(forged); !errors.Is(err, errWrongEpoch) {
		t.Fatalf("an epoch two above the round's own epoch must be rejected, got: %v", err)
	}
}

// detachHeader re-serializes only the header fields of a vertex: what a serving
// node puts in an anchor bundle and a light verifier checks.
func detachHeader(v *types.Vertex) []byte {
	builder := flatbuffers.NewBuilder(256)

	producerVec := builder.CreateByteVector(v.ProducerBytes())
	sigVec := builder.CreateByteVector(v.SignatureBytes())
	indexRootVec := builder.CreateByteVector(v.IndexRootBytes())
	bodyHashVec := builder.CreateByteVector(v.BodyHashBytes())

	types.VertexStart(builder)
	types.VertexAddRound(builder, v.Round())
	types.VertexAddProducer(builder, producerVec)
	types.VertexAddSignature(builder, sigVec)
	types.VertexAddEpoch(builder, v.Epoch())
	types.VertexAddFrontierRound(builder, v.FrontierRound())
	types.VertexAddIndexRoot(builder, indexRootVec)
	types.VertexAddBodyHash(builder, bodyHashVec)
	builder.Finish(types.VertexEnd(builder))

	return builder.FinishedBytes()
}
