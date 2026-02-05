package network

import (
	"sync"
	"time"

	"github.com/zeebo/blake3"
)

const (
	// defaultDedupTTL is the default time-to-live for seen message hashes.
	defaultDedupTTL = 5 * time.Second

	// cleanupInterval is the interval between cleanup runs.
	cleanupInterval = 1 * time.Second
)

// Dedup tracks recently seen messages to prevent duplicate processing.
// It uses blake3 hashing and automatically expires entries after a TTL.
type Dedup struct {
	seen map[[32]byte]int64 // seen maps message hash to timestamp (unix nano)
	mu   sync.RWMutex       // mu protects the seen map
	ttl  int64              // ttl in nanoseconds
	stop chan struct{}      // stop signals the cleanup goroutine to stop
	wg   sync.WaitGroup     // wg waits for the cleanup goroutine
}

// NewDedup creates a new message deduplication tracker.
func NewDedup() *Dedup {
	d := &Dedup{
		seen: make(map[[32]byte]int64),
		ttl:  int64(defaultDedupTTL),
		stop: make(chan struct{}),
	}

	d.startCleanup()

	return d
}

// Check returns true if the message is new (not seen before).
// If new, the message hash is recorded for future deduplication.
func (d *Dedup) Check(data []byte) bool {
	hash := blake3.Sum256(data)
	now := time.Now().UnixNano()

	// Fast path: check if already seen with read lock
	d.mu.RLock()
	ts, exists := d.seen[hash]
	d.mu.RUnlock()

	if exists && now-ts < d.ttl {
		return false // Duplicate
	}

	// Slow path: add to seen map with write lock
	d.mu.Lock()
	// Double-check after acquiring write lock
	ts, exists = d.seen[hash]
	if exists && now-ts < d.ttl {
		d.mu.Unlock()
		return false // Duplicate
	}

	d.seen[hash] = now
	d.mu.Unlock()

	return true // New message
}

// Close stops the cleanup goroutine and releases resources.
func (d *Dedup) Close() {
	close(d.stop)
	d.wg.Wait()
}

// startCleanup starts the background cleanup goroutine.
func (d *Dedup) startCleanup() {
	d.wg.Add(1)

	go func() {
		defer d.wg.Done()

		ticker := time.NewTicker(cleanupInterval)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				d.cleanup()
			case <-d.stop:
				return
			}
		}
	}()
}

// cleanup removes expired entries from the seen map.
func (d *Dedup) cleanup() {
	now := time.Now().UnixNano()
	ttl := d.ttl

	d.mu.Lock()

	for hash, ts := range d.seen {
		if now-ts >= ttl {
			delete(d.seen, hash)
		}
	}

	d.mu.Unlock()
}
