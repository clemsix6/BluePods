package storage

import (
	"sync"
	"time"

	"github.com/cockroachdb/pebble"
)

const (
	// defaultSyncInterval is the default interval between WAL syncs.
	defaultSyncInterval = 100 * time.Millisecond
)

// KeyValue represents a key-value pair for batch operations.
type KeyValue struct {
	Key   []byte // Key is the key to store
	Value []byte // Value is the value to store
}

// Storage provides a simple key-value store backed by Pebble.
// Writes are non-blocking (NoSync) and a background goroutine
// periodically syncs the WAL to disk for durability.
type Storage struct {
	db       *pebble.DB    // db is the underlying Pebble database
	stopSync chan struct{} // stopSync signals the sync goroutine to stop
	wg       sync.WaitGroup
}

// New creates a new Storage instance at the given path.
// It starts a background goroutine that syncs the WAL periodically.
func New(path string) (*Storage, error) {
	opts := &pebble.Options{
		Cache:                       pebble.NewCache(32 << 20), // 32 MB cache
		MemTableSize:                16 << 20,                  // 16 MB memtable
		MemTableStopWritesThreshold: 2,
	}

	db, err := pebble.Open(path, opts)
	if err != nil {
		return nil, err
	}

	s := &Storage{
		db:       db,
		stopSync: make(chan struct{}),
	}

	s.startSyncLoop()

	return s, nil
}

// Get retrieves the value for the given key.
// Returns nil if the key does not exist.
func (s *Storage) Get(key []byte) ([]byte, error) {
	value, closer, err := s.db.Get(key)
	if err == pebble.ErrNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer closer.Close()

	// Copy the value since it's invalid after closer.Close()
	result := make([]byte, len(value))
	copy(result, value)

	return result, nil
}

// Set stores a key-value pair.
// The write is buffered and synced periodically by the background goroutine.
func (s *Storage) Set(key, value []byte) error {
	return s.db.Set(key, value, pebble.NoSync)
}

// Delete removes a key from the store.
// The write is buffered and synced periodically by the background goroutine.
func (s *Storage) Delete(key []byte) error {
	return s.db.Delete(key, pebble.NoSync)
}

// SetBatch atomically stores multiple key-value pairs.
// Either all pairs are written or none.
func (s *Storage) SetBatch(pairs []KeyValue) error {
	batch := s.db.NewBatch()
	defer batch.Close()

	for _, kv := range pairs {
		if err := batch.Set(kv.Key, kv.Value, nil); err != nil {
			return err
		}
	}

	return batch.Commit(pebble.NoSync)
}

// Iterate calls fn for each key-value pair in the database.
// If fn returns an error, iteration stops and the error is returned.
// Keys are visited in lexicographic order.
func (s *Storage) Iterate(fn func(key, value []byte) error) error {
	iter, err := s.db.NewIter(nil)
	if err != nil {
		return err
	}
	defer iter.Close()

	for iter.First(); iter.Valid(); iter.Next() {
		key := iter.Key()
		value, err := iter.ValueAndErr()
		if err != nil {
			return err
		}

		if err := fn(key, value); err != nil {
			return err
		}
	}

	return iter.Error()
}

// IteratePrefix calls fn for each key-value pair with the given prefix.
// Uses Pebble's iterator bounds for efficient prefix scanning.
func (s *Storage) IteratePrefix(prefix []byte, fn func(key, value []byte) error) error {
	upperBound := prefixUpperBound(prefix)

	iter, err := s.db.NewIter(&pebble.IterOptions{
		LowerBound: prefix,
		UpperBound: upperBound,
	})
	if err != nil {
		return err
	}
	defer iter.Close()

	for iter.First(); iter.Valid(); iter.Next() {
		key := iter.Key()
		value, err := iter.ValueAndErr()
		if err != nil {
			return err
		}

		if err := fn(key, value); err != nil {
			return err
		}
	}

	return iter.Error()
}

// prefixUpperBound computes the exclusive upper bound for a prefix scan.
// Increments the last byte; returns nil if prefix is all 0xFF (full range).
func prefixUpperBound(prefix []byte) []byte {
	upper := make([]byte, len(prefix))
	copy(upper, prefix)

	for i := len(upper) - 1; i >= 0; i-- {
		upper[i]++
		if upper[i] != 0 {
			return upper
		}
	}

	return nil // all 0xFF → unbounded
}

// Close stops the sync goroutine and closes the database.
// It performs a final sync before closing to ensure durability.
func (s *Storage) Close() error {
	close(s.stopSync)
	s.wg.Wait()

	// Final sync before closing
	if err := s.sync(); err != nil {
		return err
	}

	return s.db.Close()
}

// startSyncLoop starts the background goroutine that periodically syncs the WAL.
func (s *Storage) startSyncLoop() {
	s.wg.Add(1)

	go func() {
		defer s.wg.Done()

		ticker := time.NewTicker(defaultSyncInterval)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				_ = s.sync()
			case <-s.stopSync:
				return
			}
		}
	}()
}

// sync forces a WAL sync to disk.
func (s *Storage) sync() error {
	return s.db.LogData(nil, pebble.Sync)
}
