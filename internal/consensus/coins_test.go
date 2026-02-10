package consensus

import (
	"encoding/binary"
	"math"
	"testing"

	flatbuffers "github.com/google/flatbuffers/go"

	"BluePods/internal/types"
)

// mockCoinStore is an in-memory CoinStore for tests.
type mockCoinStore struct {
	objects map[[32]byte][]byte
}

func newMockCoinStore() *mockCoinStore {
	return &mockCoinStore{objects: make(map[[32]byte][]byte)}
}

func (m *mockCoinStore) GetObject(id [32]byte) []byte {
	return m.objects[id]
}

func (m *mockCoinStore) SetObject(data []byte) {
	obj := types.GetRootAsObject(data, 0)
	idBytes := obj.IdBytes()
	if len(idBytes) != 32 {
		return
	}

	var id [32]byte
	copy(id[:], idBytes)
	m.objects[id] = append([]byte(nil), data...)
}

// buildTestCoinObject creates a serialized Coin object with given properties.
func buildTestCoinObject(id [32]byte, balance uint64, owner [32]byte, replication uint16) []byte {
	builder := flatbuffers.NewBuilder(256)

	// Coin content: 8-byte LE balance
	content := make([]byte, 8)
	binary.LittleEndian.PutUint64(content, balance)

	idVec := builder.CreateByteVector(id[:])
	ownerVec := builder.CreateByteVector(owner[:])
	contentVec := builder.CreateByteVector(content)

	types.ObjectStart(builder)
	types.ObjectAddId(builder, idVec)
	types.ObjectAddVersion(builder, 1)
	types.ObjectAddOwner(builder, ownerVec)
	types.ObjectAddReplication(builder, replication)
	types.ObjectAddContent(builder, contentVec)
	types.ObjectAddFees(builder, 0)

	offset := types.ObjectEnd(builder)
	builder.Finish(offset)

	return builder.FinishedBytes()
}

func TestReadCoinBalance(t *testing.T) {
	id := [32]byte{1}
	owner := [32]byte{2}
	data := buildTestCoinObject(id, 12345, owner, 0)

	balance, err := readCoinBalance(data)
	if err != nil {
		t.Fatal(err)
	}

	if balance != 12345 {
		t.Errorf("got %d, want 12345", balance)
	}
}

func TestReadCoinOwner(t *testing.T) {
	id := [32]byte{1}
	owner := [32]byte{2, 3, 4}
	data := buildTestCoinObject(id, 100, owner, 0)

	got, err := readCoinOwner(data)
	if err != nil {
		t.Fatal(err)
	}

	if got != owner {
		t.Errorf("owner mismatch")
	}
}

func TestWriteCoinBalance(t *testing.T) {
	id := [32]byte{1}
	owner := [32]byte{2}
	data := buildTestCoinObject(id, 1000, owner, 0)

	// Write new balance
	newData := writeCoinBalance(data, 500)

	balance, err := readCoinBalance(newData)
	if err != nil {
		t.Fatal(err)
	}

	if balance != 500 {
		t.Errorf("got %d, want 500", balance)
	}

	// Owner should be preserved
	newOwner, err := readCoinOwner(newData)
	if err != nil {
		t.Fatal(err)
	}

	if newOwner != owner {
		t.Error("owner should be preserved")
	}

	// Replication should be preserved
	if rep := readCoinReplication(newData); rep != 0 {
		t.Errorf("replication should be 0, got %d", rep)
	}
}

func TestDeductCoinFee_SufficientBalance(t *testing.T) {
	store := newMockCoinStore()
	id := [32]byte{1}
	owner := [32]byte{2}
	store.SetObject(buildTestCoinObject(id, 1000, owner, 0))

	deducted, covered, err := deductCoinFee(store, id, 300)
	if err != nil {
		t.Fatal(err)
	}

	if deducted != 300 || !covered {
		t.Errorf("got deducted=%d covered=%v, want 300/true", deducted, covered)
	}

	// Balance should be 700
	balance, _ := readCoinBalance(store.GetObject(id))
	if balance != 700 {
		t.Errorf("balance: got %d, want 700", balance)
	}
}

func TestDeductCoinFee_ExactBalance(t *testing.T) {
	store := newMockCoinStore()
	id := [32]byte{1}
	owner := [32]byte{2}
	store.SetObject(buildTestCoinObject(id, 500, owner, 0))

	deducted, covered, err := deductCoinFee(store, id, 500)
	if err != nil {
		t.Fatal(err)
	}

	if deducted != 500 || !covered {
		t.Errorf("got deducted=%d covered=%v, want 500/true", deducted, covered)
	}

	balance, _ := readCoinBalance(store.GetObject(id))
	if balance != 0 {
		t.Errorf("balance: got %d, want 0", balance)
	}
}

func TestDeductCoinFee_InsufficientBalance(t *testing.T) {
	store := newMockCoinStore()
	id := [32]byte{1}
	owner := [32]byte{2}
	store.SetObject(buildTestCoinObject(id, 200, owner, 0))

	deducted, covered, err := deductCoinFee(store, id, 500)
	if err != nil {
		t.Fatal(err)
	}

	if deducted != 200 || covered {
		t.Errorf("got deducted=%d covered=%v, want 200/false", deducted, covered)
	}

	// Balance should be 0
	balance, _ := readCoinBalance(store.GetObject(id))
	if balance != 0 {
		t.Errorf("balance: got %d, want 0", balance)
	}
}

func TestDeductCoinFee_ZeroBalance(t *testing.T) {
	store := newMockCoinStore()
	id := [32]byte{1}
	owner := [32]byte{2}
	store.SetObject(buildTestCoinObject(id, 0, owner, 0))

	deducted, covered, err := deductCoinFee(store, id, 100)
	if err != nil {
		t.Fatal(err)
	}

	if deducted != 0 || covered {
		t.Errorf("got deducted=%d covered=%v, want 0/false", deducted, covered)
	}
}

func TestDeductCoinFee_NotFound(t *testing.T) {
	store := newMockCoinStore()
	id := [32]byte{99}

	_, _, err := deductCoinFee(store, id, 100)
	if err == nil {
		t.Fatal("expected error for missing coin")
	}
}

func TestCreditCoin(t *testing.T) {
	store := newMockCoinStore()
	id := [32]byte{1}
	owner := [32]byte{2}
	store.SetObject(buildTestCoinObject(id, 500, owner, 0))

	err := creditCoin(store, id, 200)
	if err != nil {
		t.Fatal(err)
	}

	balance, _ := readCoinBalance(store.GetObject(id))
	if balance != 700 {
		t.Errorf("balance: got %d, want 700", balance)
	}
}

func TestCreditCoin_Zero(t *testing.T) {
	store := newMockCoinStore()
	id := [32]byte{1}
	owner := [32]byte{2}
	store.SetObject(buildTestCoinObject(id, 500, owner, 0))

	err := creditCoin(store, id, 0)
	if err != nil {
		t.Fatal(err)
	}

	balance, _ := readCoinBalance(store.GetObject(id))
	if balance != 500 {
		t.Errorf("balance should be unchanged: got %d", balance)
	}
}

func TestCreditCoin_NotFound(t *testing.T) {
	store := newMockCoinStore()

	err := creditCoin(store, [32]byte{99}, 100)
	if err == nil {
		t.Fatal("expected error for missing coin")
	}
}

func TestCreditCoin_Overflow(t *testing.T) {
	store := newMockCoinStore()
	id := [32]byte{1}
	owner := [32]byte{2}
	store.SetObject(buildTestCoinObject(id, math.MaxUint64-10, owner, 0))

	// Adding 100 to MaxUint64-10 would overflow
	err := creditCoin(store, id, 100)
	if err == nil {
		t.Fatal("expected overflow error")
	}

	// Balance should be unchanged
	balance, _ := readCoinBalance(store.GetObject(id))
	if balance != math.MaxUint64-10 {
		t.Errorf("balance should be unchanged: got %d", balance)
	}
}

func TestCreditCoin_ExactMax(t *testing.T) {
	store := newMockCoinStore()
	id := [32]byte{1}
	owner := [32]byte{2}
	store.SetObject(buildTestCoinObject(id, math.MaxUint64-100, owner, 0))

	// Adding 100 to MaxUint64-100 = MaxUint64 (no overflow)
	err := creditCoin(store, id, 100)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	balance, _ := readCoinBalance(store.GetObject(id))
	if balance != math.MaxUint64 {
		t.Errorf("expected MaxUint64, got %d", balance)
	}
}

func TestMultipleDeductions(t *testing.T) {
	store := newMockCoinStore()
	id := [32]byte{1}
	owner := [32]byte{2}
	store.SetObject(buildTestCoinObject(id, 350, owner, 0))

	// 5 txs each costing 100, balance=350
	for i := 0; i < 5; i++ {
		deducted, covered, err := deductCoinFee(store, id, 100)
		if err != nil {
			t.Fatalf("tx %d: %v", i, err)
		}

		switch i {
		case 0, 1, 2:
			// First 3 should succeed fully
			if deducted != 100 || !covered {
				t.Errorf("tx %d: got %d/%v, want 100/true", i, deducted, covered)
			}
		case 3:
			// 4th: 50 remaining
			if deducted != 50 || covered {
				t.Errorf("tx %d: got %d/%v, want 50/false", i, deducted, covered)
			}
		case 4:
			// 5th: nothing left
			if deducted != 0 || covered {
				t.Errorf("tx %d: got %d/%v, want 0/false", i, deducted, covered)
			}
		}
	}

	balance, _ := readCoinBalance(store.GetObject(id))
	if balance != 0 {
		t.Errorf("final balance: got %d, want 0", balance)
	}
}
