package aggregation

import (
	"testing"

	"BluePods/internal/consensus"
)

// makeValidators creates a ValidatorSet with n validators.
func makeValidators(n int) *consensus.ValidatorSet {
	validators := make([]consensus.Hash, n)
	for i := range validators {
		// Create deterministic validator pubkeys
		validators[i][0] = byte(i)
		validators[i][1] = byte(i >> 8)
	}

	return consensus.NewValidatorSet(validators)
}

// TestRendezvousDeterminism verifies that the same inputs produce the same outputs.
func TestRendezvousDeterminism(t *testing.T) {
	vs := makeValidators(10)
	rv := NewRendezvous(vs)

	var objectID [32]byte
	objectID[0] = 0x42

	holders1 := rv.ComputeHolders(objectID, 3)
	holders2 := rv.ComputeHolders(objectID, 3)

	if len(holders1) != len(holders2) {
		t.Fatalf("different lengths: %d vs %d", len(holders1), len(holders2))
	}

	for i := range holders1 {
		if holders1[i] != holders2[i] {
			t.Errorf("holder %d differs: %v vs %v", i, holders1[i], holders2[i])
		}
	}
}

// TestRendezvousStability verifies that adding validators doesn't change existing mappings much.
func TestRendezvousStability(t *testing.T) {
	vs1 := makeValidators(10)
	rv1 := NewRendezvous(vs1)

	// Add one more validator
	validators := make([]consensus.Hash, 11)
	for i := 0; i < 10; i++ {
		validators[i][0] = byte(i)
		validators[i][1] = byte(i >> 8)
	}
	validators[10][0] = 0xAA

	vs2 := consensus.NewValidatorSet(validators)
	rv2 := NewRendezvous(vs2)

	// Test multiple objects
	changedCount := 0
	totalHolders := 0

	for objIdx := 0; objIdx < 100; objIdx++ {
		var objectID [32]byte
		objectID[0] = byte(objIdx)

		holders1 := rv1.ComputeHolders(objectID, 3)
		holders2 := rv2.ComputeHolders(objectID, 3)

		// Count how many holders changed
		for _, h1 := range holders1 {
			found := false
			for _, h2 := range holders2 {
				if h1 == h2 {
					found = true
					break
				}
			}
			if !found {
				changedCount++
			}
			totalHolders++
		}
	}

	// With rendezvous hashing, only ~1/N holders should change
	changeRate := float64(changedCount) / float64(totalHolders)
	if changeRate > 0.2 { // Allow up to 20% change (1/10 + margin)
		t.Errorf("too many holders changed: %d/%d (%.2f%%)", changedCount, totalHolders, changeRate*100)
	}
}

// TestRendezvousDifferentObjects verifies different objects get different holders.
func TestRendezvousDifferentObjects(t *testing.T) {
	vs := makeValidators(10)
	rv := NewRendezvous(vs)

	var obj1, obj2 [32]byte
	obj1[0] = 0x01
	obj2[0] = 0x02

	holders1 := rv.ComputeHolders(obj1, 3)
	holders2 := rv.ComputeHolders(obj2, 3)

	// Check that at least one holder is different
	allSame := true
	for i := range holders1 {
		if holders1[i] != holders2[i] {
			allSame = false
			break
		}
	}

	if allSame {
		t.Error("different objects should have at least some different holders")
	}
}

// TestRendezvousEdgeCases tests edge cases.
func TestRendezvousEdgeCases(t *testing.T) {
	vs := makeValidators(5)
	rv := NewRendezvous(vs)

	var objectID [32]byte

	// Replication = 0 should return empty
	if holders := rv.ComputeHolders(objectID, 0); len(holders) != 0 {
		t.Errorf("replication=0 should return empty, got %d", len(holders))
	}

	// Replication > validators should return all validators
	if holders := rv.ComputeHolders(objectID, 10); len(holders) != 5 {
		t.Errorf("replication>n should return n, got %d", len(holders))
	}

	// Replication = n should return all validators
	if holders := rv.ComputeHolders(objectID, 5); len(holders) != 5 {
		t.Errorf("replication=n should return n, got %d", len(holders))
	}
}

// TestQuorumSize tests the quorum calculation.
func TestQuorumSize(t *testing.T) {
	tests := []struct {
		replication int
		expected    int
	}{
		{1, 1},   // 67% of 1 = 0.67, ceil = 1
		{2, 2},   // 67% of 2 = 1.34, ceil = 2
		{3, 3},   // 67% of 3 = 2.01, ceil = 3
		{4, 3},   // 67% of 4 = 2.68, ceil = 3
		{5, 4},   // 67% of 5 = 3.35, ceil = 4
		{10, 7},  // 67% of 10 = 6.7, ceil = 7
		{100, 67}, // 67% of 100 = 67
	}

	for _, tc := range tests {
		got := QuorumSize(tc.replication)
		if got != tc.expected {
			t.Errorf("QuorumSize(%d) = %d, want %d", tc.replication, got, tc.expected)
		}
	}
}

// BenchmarkRendezvousCompute benchmarks holder computation.
func BenchmarkRendezvousCompute10(b *testing.B) {
	benchmarkRendezvousCompute(b, 10)
}

// BenchmarkRendezvousCompute100 benchmarks with 100 validators.
func BenchmarkRendezvousCompute100(b *testing.B) {
	benchmarkRendezvousCompute(b, 100)
}

// BenchmarkRendezvousCompute1000 benchmarks with 1000 validators.
func BenchmarkRendezvousCompute1000(b *testing.B) {
	benchmarkRendezvousCompute(b, 1000)
}

func benchmarkRendezvousCompute(b *testing.B, numValidators int) {
	vs := makeValidators(numValidators)
	rv := NewRendezvous(vs)

	var objectID [32]byte

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		objectID[0] = byte(i)
		rv.ComputeHolders(objectID, 3)
	}
}
