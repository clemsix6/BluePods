package storage

import (
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
)

// benchStorage creates a storage for benchmarks.
func benchStorage(b *testing.B) (*Storage, func()) {
	b.Helper()

	dir, err := os.MkdirTemp("", "storage-bench-*")
	if err != nil {
		b.Fatalf("failed to create temp dir: %v", err)
	}

	s, err := New(filepath.Join(dir, "db"))
	if err != nil {
		os.RemoveAll(dir)
		b.Fatalf("failed to create storage: %v", err)
	}

	cleanup := func() {
		s.Close()
		os.RemoveAll(dir)
	}

	return s, cleanup
}

// makeKey creates a key from an integer.
func makeKey(i int) []byte {
	key := make([]byte, 32)
	binary.BigEndian.PutUint64(key, uint64(i))
	return key
}

// makeValue creates a random value of the given size.
func makeValue(size int) []byte {
	value := make([]byte, size)
	rand.Read(value)
	return value
}

// BenchmarkSet benchmarks sequential Set operations.
func BenchmarkSet(b *testing.B) {
	sizes := []int{64, 512, 2048, 4096}

	for _, size := range sizes {
		b.Run(fmt.Sprintf("size=%d", size), func(b *testing.B) {
			s, cleanup := benchStorage(b)
			defer cleanup()

			value := makeValue(size)

			b.ResetTimer()
			b.SetBytes(int64(size))

			for i := 0; i < b.N; i++ {
				key := makeKey(i)
				if err := s.Set(key, value); err != nil {
					b.Fatalf("Set failed: %v", err)
				}
			}
		})
	}
}

// BenchmarkGet benchmarks sequential Get operations on pre-populated data.
func BenchmarkGet(b *testing.B) {
	sizes := []int{64, 512, 2048, 4096}

	for _, size := range sizes {
		b.Run(fmt.Sprintf("size=%d", size), func(b *testing.B) {
			s, cleanup := benchStorage(b)
			defer cleanup()

			// Pre-populate with 100k entries
			const numEntries = 100_000
			value := makeValue(size)

			for i := 0; i < numEntries; i++ {
				key := makeKey(i)
				if err := s.Set(key, value); err != nil {
					b.Fatalf("Set failed: %v", err)
				}
			}

			b.ResetTimer()
			b.SetBytes(int64(size))

			for i := 0; i < b.N; i++ {
				key := makeKey(i % numEntries)
				_, err := s.Get(key)
				if err != nil {
					b.Fatalf("Get failed: %v", err)
				}
			}
		})
	}
}

// BenchmarkSetBatch benchmarks batch Set operations.
func BenchmarkSetBatch(b *testing.B) {
	batchSizes := []int{1, 8, 16, 32, 64}
	valueSize := 512 // typical object size

	for _, batchSize := range batchSizes {
		b.Run(fmt.Sprintf("batch=%d", batchSize), func(b *testing.B) {
			s, cleanup := benchStorage(b)
			defer cleanup()

			b.ResetTimer()
			b.SetBytes(int64(batchSize * valueSize))

			for i := 0; i < b.N; i++ {
				pairs := make([]KeyValue, batchSize)
				for j := 0; j < batchSize; j++ {
					pairs[j] = KeyValue{
						Key:   makeKey(i*batchSize + j),
						Value: makeValue(valueSize),
					}
				}
				if err := s.SetBatch(pairs); err != nil {
					b.Fatalf("SetBatch failed: %v", err)
				}
			}
		})
	}
}

// BenchmarkParallelSet benchmarks concurrent Set operations.
func BenchmarkParallelSet(b *testing.B) {
	goroutines := []int{1, 4, 8, 16, 32}
	valueSize := 512

	for _, numG := range goroutines {
		b.Run(fmt.Sprintf("goroutines=%d", numG), func(b *testing.B) {
			s, cleanup := benchStorage(b)
			defer cleanup()

			value := makeValue(valueSize)
			var counter atomic.Int64

			b.ResetTimer()
			b.SetBytes(int64(valueSize))

			b.RunParallel(func(pb *testing.PB) {
				for pb.Next() {
					i := counter.Add(1)
					key := makeKey(int(i))
					if err := s.Set(key, value); err != nil {
						b.Errorf("Set failed: %v", err)
					}
				}
			})
		})
	}
}

// BenchmarkParallelGet benchmarks concurrent Get operations.
func BenchmarkParallelGet(b *testing.B) {
	goroutines := []int{1, 4, 8, 16, 32}
	valueSize := 512

	for _, numG := range goroutines {
		b.Run(fmt.Sprintf("goroutines=%d", numG), func(b *testing.B) {
			s, cleanup := benchStorage(b)
			defer cleanup()

			// Pre-populate
			const numEntries = 100_000
			value := makeValue(valueSize)

			for i := 0; i < numEntries; i++ {
				if err := s.Set(makeKey(i), value); err != nil {
					b.Fatalf("Set failed: %v", err)
				}
			}

			var counter atomic.Int64

			b.ResetTimer()
			b.SetBytes(int64(valueSize))

			b.RunParallel(func(pb *testing.PB) {
				for pb.Next() {
					i := counter.Add(1)
					key := makeKey(int(i) % numEntries)
					_, err := s.Get(key)
					if err != nil {
						b.Errorf("Get failed: %v", err)
					}
				}
			})
		})
	}
}

// BenchmarkMixedWorkload simulates realistic blockchain workload.
// 80% reads, 20% writes as typical for transaction validation.
func BenchmarkMixedWorkload(b *testing.B) {
	s, cleanup := benchStorage(b)
	defer cleanup()

	const numEntries = 100_000
	const valueSize = 512

	// Pre-populate
	value := makeValue(valueSize)
	for i := 0; i < numEntries; i++ {
		if err := s.Set(makeKey(i), value); err != nil {
			b.Fatalf("Set failed: %v", err)
		}
	}

	var readCounter atomic.Int64
	var writeCounter atomic.Int64

	b.ResetTimer()
	b.SetBytes(int64(valueSize))

	b.RunParallel(func(pb *testing.PB) {
		localOp := 0
		for pb.Next() {
			localOp++
			if localOp%5 == 0 {
				// 20% writes
				i := writeCounter.Add(1)
				key := makeKey(int(i) % numEntries)
				if err := s.Set(key, value); err != nil {
					b.Errorf("Set failed: %v", err)
				}
			} else {
				// 80% reads
				i := readCounter.Add(1)
				key := makeKey(int(i) % numEntries)
				_, err := s.Get(key)
				if err != nil {
					b.Errorf("Get failed: %v", err)
				}
			}
		}
	})
}

// BenchmarkHighThroughput simulates high TPS scenario.
// Multiple writers doing batched writes + readers.
func BenchmarkHighThroughput(b *testing.B) {
	s, cleanup := benchStorage(b)
	defer cleanup()

	const numEntries = 100_000
	const valueSize = 512
	const batchSize = 8

	// Pre-populate
	value := makeValue(valueSize)
	for i := 0; i < numEntries; i++ {
		if err := s.Set(makeKey(i), value); err != nil {
			b.Fatalf("Set failed: %v", err)
		}
	}

	var batchCounter atomic.Int64
	var readCounter atomic.Int64

	b.ResetTimer()
	b.SetBytes(int64(valueSize * batchSize))

	var wg sync.WaitGroup

	// Writer goroutines (simulating transaction execution)
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				i := batchCounter.Add(1)
				if int(i) > b.N/batchSize {
					return
				}

				pairs := make([]KeyValue, batchSize)
				for j := 0; j < batchSize; j++ {
					pairs[j] = KeyValue{
						Key:   makeKey((int(i)*batchSize + j) % numEntries),
						Value: value,
					}
				}
				s.SetBatch(pairs)
			}
		}()
	}

	// Reader goroutines (simulating attestation collection)
	for r := 0; r < 16; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				i := readCounter.Add(1)
				if int(i) > b.N*4 {
					return
				}
				key := makeKey(int(i) % numEntries)
				s.Get(key)
			}
		}()
	}

	wg.Wait()
}

// BenchmarkBurstWrite simulates burst of writes (e.g., after block commit).
func BenchmarkBurstWrite(b *testing.B) {
	s, cleanup := benchStorage(b)
	defer cleanup()

	const burstSize = 1000
	const valueSize = 512

	b.ResetTimer()
	b.SetBytes(int64(burstSize * valueSize))

	for i := 0; i < b.N; i++ {
		// Simulate committing a block with many object updates
		pairs := make([]KeyValue, burstSize)
		for j := 0; j < burstSize; j++ {
			pairs[j] = KeyValue{
				Key:   makeKey(i*burstSize + j),
				Value: makeValue(valueSize),
			}
		}
		if err := s.SetBatch(pairs); err != nil {
			b.Fatalf("SetBatch failed: %v", err)
		}
	}
}
