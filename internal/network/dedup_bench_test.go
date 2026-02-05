package network

import (
	"crypto/rand"
	"sync"
	"testing"
)

// BenchmarkDedupCheck benchmarks the Check method with new messages.
func BenchmarkDedupCheck(b *testing.B) {
	d := NewDedup()
	defer d.Close()

	// Pre-generate messages
	messages := make([][]byte, b.N)
	for i := range messages {
		messages[i] = make([]byte, 256)
		rand.Read(messages[i])
	}

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		d.Check(messages[i])
	}
}

// BenchmarkDedupCheckDuplicate benchmarks the Check method with duplicates.
func BenchmarkDedupCheckDuplicate(b *testing.B) {
	d := NewDedup()
	defer d.Close()

	msg := make([]byte, 256)
	rand.Read(msg)
	d.Check(msg) // Add to seen

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		d.Check(msg)
	}
}

// BenchmarkDedupCheckParallel benchmarks concurrent Check calls.
func BenchmarkDedupCheckParallel(b *testing.B) {
	d := NewDedup()
	defer d.Close()

	// Pre-generate messages
	messages := make([][]byte, 10000)
	for i := range messages {
		messages[i] = make([]byte, 256)
		rand.Read(messages[i])
	}

	b.ResetTimer()

	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			d.Check(messages[i%len(messages)])
			i++
		}
	})
}

// BenchmarkDedupCheckMixed benchmarks mixed new/duplicate messages.
func BenchmarkDedupCheckMixed(b *testing.B) {
	d := NewDedup()
	defer d.Close()

	// Pre-generate messages (some will be duplicates)
	messages := make([][]byte, 1000)
	for i := range messages {
		messages[i] = make([]byte, 256)
		rand.Read(messages[i])
	}

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		d.Check(messages[i%len(messages)])
	}
}

// BenchmarkDedupHighContention benchmarks with high contention on same message.
func BenchmarkDedupHighContention(b *testing.B) {
	d := NewDedup()
	defer d.Close()

	msg := make([]byte, 256)
	rand.Read(msg)

	b.ResetTimer()

	var wg sync.WaitGroup

	for i := 0; i < b.N; i++ {
		wg.Add(1)
		go func() {
			d.Check(msg)
			wg.Done()
		}()
	}

	wg.Wait()
}
