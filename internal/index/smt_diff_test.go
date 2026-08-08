package index

import (
	"bytes"
	"fmt"
	"math/bits"
	"math/rand"
	"testing"
	"time"

	"github.com/zeebo/blake3"
)

// churnPool returns a key pool skewed to exercise the incremental tree's traps:
// share keys collide on their first byte, so inserting them spills deep and
// deleting them collapses deep, while the rest are spread out. A small pool over
// many steps forces frequent overwrites (position collisions) and deletions that
// empty subtrees.
func churnPool(t *testing.T, shared, spread int) [][]byte {
	t.Helper()
	pool := keysSharingFirstByte(t, shared)
	for i := 0; i < spread; i++ {
		pool = append(pool, []byte(fmt.Sprintf("spread-%d", i)))
	}
	return pool
}

// TestIncrementalMatchesOracle drives a long seeded insert/overwrite/delete
// sequence and, after EVERY step, asserts the incremental root equals a
// from-scratch functional recompute over the live entry set. This is the core
// guarantee: the dirty-path rehash is bit-identical to the O(n) oracle for every
// reachable entry set, including the spill and collapse edge cases the pool forces.
func TestIncrementalMatchesOracle(t *testing.T) {
	rng := rand.New(rand.NewSource(20260720))
	pool := churnPool(t, 64, 160)
	tr := New()

	for step := 0; step < 6000; step++ {
		k := pool[rng.Intn(len(pool))]
		if rng.Intn(10) < 6 {
			tr.Insert(k, randBytes(rng, 16))
		} else {
			tr.Delete(k)
		}

		if got, want := tr.Root(), functionalRoot(tr.entriesSnapshot()); got != want {
			t.Fatalf("step %d: incremental root %x != oracle %x", step, got, want)
		}
		if step%400 == 0 {
			assertProofParity(t, tr, pool, rng)
		}
	}
}

// TestProofParityWithOracle builds a mixed tree and requires the incremental
// Prove to serialize byte-identically to the functional oracle's proof for a
// sample of present and absent keys, covering inclusion, empty-subtree absence,
// and other-leaf absence shapes.
func TestProofParityWithOracle(t *testing.T) {
	rng := rand.New(rand.NewSource(99))
	pool := churnPool(t, 48, 120)
	tr := New()
	for _, k := range pool {
		if rng.Intn(3) != 0 {
			tr.Insert(k, randBytes(rng, 20))
		}
	}
	assertProofParity(t, tr, pool, rng)
}

// assertProofParity checks that tr.Prove serializes identically to the oracle's
// proof for every pool key (present or absent) plus a batch of never-inserted
// absent keys, exercising all three proof shapes.
func assertProofParity(t *testing.T, tr *SMT, pool [][]byte, rng *rand.Rand) {
	t.Helper()
	snap := tr.entriesSnapshot()
	check := func(k []byte) {
		got := tr.Prove(k).Serialize()
		want := functionalProve(snap, blake3.Sum256(k)).Serialize()
		if !bytes.Equal(got, want) {
			t.Fatalf("proof mismatch for %q:\n incr %x\n orac %x", k, got, want)
		}
	}
	for _, k := range pool {
		check(k)
	}
	for i := 0; i < 32; i++ {
		check([]byte(fmt.Sprintf("never-%d-%d", rng.Int(), i)))
	}
}

// TestSingleUpdateHashesLogN asserts a single-key update at 100k entries rehashes
// only O(log n) nodes, and reports the measured count. A full functional recompute
// would rehash ~2n (~200k) nodes; the incremental tree must rehash the mutated
// leaf plus its root-to-leaf internal path, so the count is bounded by the path
// depth, itself O(log n) for random blake3 positions. The bound 2*ceil(log2 n)+32
// is generous slack over the ~log2(n) expected depth while remaining four orders
// of magnitude below n, which is what proves the complexity class changed.
func TestSingleUpdateHashesLogN(t *testing.T) {
	if testing.Short() {
		t.Skip("100k build is too slow for -short")
	}
	const n = 100_000
	tr := buildTree(n)
	bound := uint64(2*bits.Len(uint(n)) + 32)

	var worst uint64
	for i := 0; i < 32; i++ {
		resetHashCount()
		tr.Insert([]byte(fmt.Sprintf("key-%d", i*97%n)), []byte(fmt.Sprintf("upd-%d", i)))
		tr.Root()
		if c := readHashCount(); c > worst {
			worst = c
		}
	}
	t.Logf("100k single-update rehash: worst %d nodes (bound %d, full recompute ~2n=%d)", worst, bound, 2*n)
	if worst > bound {
		t.Fatalf("single update hashed %d nodes, want <= %d (O(log n))", worst, bound)
	}
}

// buildTree returns an n-entry tree with settled (clean) hashes.
func buildTree(n int) *SMT {
	tr := New()
	for i := 0; i < n; i++ {
		tr.Insert([]byte(fmt.Sprintf("key-%d", i)), []byte(fmt.Sprintf("val-%d", i)))
	}
	tr.Root()
	return tr
}

// BenchmarkIncremental measures the steady-state cost of one committed batch's
// pattern at 100k entries: overwrite a leaf, then recompute the root. It reports
// hashes per update, wall time per update, and wall time per Root call.
func BenchmarkIncremental(b *testing.B) {
	const n = 100_000
	tr := New()
	for i := 0; i < n; i++ {
		tr.Insert([]byte(fmt.Sprintf("key-%d", i)), []byte(fmt.Sprintf("val-%d", i)))
	}
	tr.Root()

	b.ResetTimer()
	var totalHashes uint64
	var updateNs, rootNs int64
	for i := 0; i < b.N; i++ {
		k := []byte(fmt.Sprintf("key-%d", i%n))
		v := []byte(fmt.Sprintf("bench-%d", i))
		resetHashCount()

		t0 := time.Now()
		tr.Insert(k, v)
		updateNs += time.Since(t0).Nanoseconds()

		t1 := time.Now()
		tr.Root()
		rootNs += time.Since(t1).Nanoseconds()

		totalHashes += readHashCount()
	}

	b.ReportMetric(float64(totalHashes)/float64(b.N), "hashes/update")
	b.ReportMetric(float64(updateNs)/float64(b.N), "ns/update")
	b.ReportMetric(float64(rootNs)/float64(b.N), "ns/root")
}
