package storage

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// newTestStorage creates a temporary storage for testing.
func newTestStorage(t *testing.T) (*Storage, func()) {
	t.Helper()

	dir, err := os.MkdirTemp("", "storage-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}

	s, err := New(filepath.Join(dir, "db"))
	if err != nil {
		os.RemoveAll(dir)
		t.Fatalf("failed to create storage: %v", err)
	}

	cleanup := func() {
		s.Close()
		os.RemoveAll(dir)
	}

	return s, cleanup
}

func TestSetAndGet(t *testing.T) {
	s, cleanup := newTestStorage(t)
	defer cleanup()

	key := []byte("test-key")
	value := []byte("test-value")

	if err := s.Set(key, value); err != nil {
		t.Fatalf("Set failed: %v", err)
	}

	got, err := s.Get(key)
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}

	if !bytes.Equal(got, value) {
		t.Errorf("Get returned %q, want %q", got, value)
	}
}

func TestGetNonExistent(t *testing.T) {
	s, cleanup := newTestStorage(t)
	defer cleanup()

	got, err := s.Get([]byte("non-existent"))
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}

	if got != nil {
		t.Errorf("Get returned %q, want nil", got)
	}
}

func TestDelete(t *testing.T) {
	s, cleanup := newTestStorage(t)
	defer cleanup()

	key := []byte("to-delete")
	value := []byte("value")

	if err := s.Set(key, value); err != nil {
		t.Fatalf("Set failed: %v", err)
	}

	if err := s.Delete(key); err != nil {
		t.Fatalf("Delete failed: %v", err)
	}

	got, err := s.Get(key)
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}

	if got != nil {
		t.Errorf("Get after Delete returned %q, want nil", got)
	}
}

func TestSetBatch(t *testing.T) {
	s, cleanup := newTestStorage(t)
	defer cleanup()

	pairs := []KeyValue{
		{Key: []byte("batch-1"), Value: []byte("value-1")},
		{Key: []byte("batch-2"), Value: []byte("value-2")},
		{Key: []byte("batch-3"), Value: []byte("value-3")},
	}

	if err := s.SetBatch(pairs); err != nil {
		t.Fatalf("SetBatch failed: %v", err)
	}

	for _, kv := range pairs {
		got, err := s.Get(kv.Key)
		if err != nil {
			t.Fatalf("Get failed for %q: %v", kv.Key, err)
		}

		if !bytes.Equal(got, kv.Value) {
			t.Errorf("Get(%q) = %q, want %q", kv.Key, got, kv.Value)
		}
	}
}

func TestSetOverwrite(t *testing.T) {
	s, cleanup := newTestStorage(t)
	defer cleanup()

	key := []byte("overwrite-key")

	if err := s.Set(key, []byte("first")); err != nil {
		t.Fatalf("Set failed: %v", err)
	}

	if err := s.Set(key, []byte("second")); err != nil {
		t.Fatalf("Set failed: %v", err)
	}

	got, err := s.Get(key)
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}

	if !bytes.Equal(got, []byte("second")) {
		t.Errorf("Get returned %q, want %q", got, "second")
	}
}

func TestLargeValue(t *testing.T) {
	s, cleanup := newTestStorage(t)
	defer cleanup()

	key := []byte("large-key")
	value := make([]byte, 4096) // 4KB like BluePods max object size
	for i := range value {
		value[i] = byte(i % 256)
	}

	if err := s.Set(key, value); err != nil {
		t.Fatalf("Set failed: %v", err)
	}

	got, err := s.Get(key)
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}

	if !bytes.Equal(got, value) {
		t.Error("Get returned different value for large object")
	}
}
