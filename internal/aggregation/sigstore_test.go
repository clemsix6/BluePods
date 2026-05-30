package aggregation

import (
	"testing"

	"BluePods/internal/storage"
)

// newSigTestStorage creates a temporary storage for signature-store tests.
func newSigTestStorage(t *testing.T) *storage.Storage {
	t.Helper()

	db, err := storage.New(t.TempDir())
	if err != nil {
		t.Fatalf("create storage: %v", err)
	}

	t.Cleanup(func() { db.Close() })

	return db
}

// TestSigStoreCurrentVersionAndCopy verifies version overwrite semantics and
// that GetObjectSig returns a copy that cannot mutate the stored value.
func TestSigStoreCurrentVersionAndCopy(t *testing.T) {
	db := newSigTestStorage(t)

	var id [32]byte
	id[0] = 0xAB

	sig := make([]byte, BLSSignatureSize)
	sig[0] = 0x01

	if err := PutObjectSig(db, id, 7, sig); err != nil {
		t.Fatal(err)
	}

	v, got, ok := GetObjectSig(db, id)
	if !ok || v != 7 || got[0] != 0x01 {
		t.Fatalf("ok=%v v=%d got[0]=0x%02x", ok, v, got[0])
	}

	// Mutate the returned slice; the store must be unaffected (copy-on-read).
	got[0] = 0xFF

	_, again, _ := GetObjectSig(db, id)
	if again[0] != 0x01 {
		t.Fatal("GetObjectSig returned an aliased buffer")
	}

	sig2 := make([]byte, BLSSignatureSize)
	sig2[0] = 0x02

	if err := PutObjectSig(db, id, 8, sig2); err != nil {
		t.Fatal(err)
	}

	v, _, _ = GetObjectSig(db, id)
	if v != 8 {
		t.Fatalf("want v8 after advance, got %d", v)
	}
}

// TestSigStoreMiss verifies a miss for an absent object.
func TestSigStoreMiss(t *testing.T) {
	db := newSigTestStorage(t)

	var id [32]byte
	id[0] = 0xCD

	if _, _, ok := GetObjectSig(db, id); ok {
		t.Fatal("expected miss for absent object")
	}
}
