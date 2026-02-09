package consensus

import (
	"testing"

	flatbuffers "github.com/google/flatbuffers/go"

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
