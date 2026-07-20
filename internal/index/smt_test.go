package index

import (
	"bytes"
	"fmt"
	"math/rand"
	"sort"
	"testing"

	"github.com/zeebo/blake3"
)

// randBytes returns n deterministic pseudo-random bytes from r.
func randBytes(r *rand.Rand, n int) []byte {
	b := make([]byte, n)
	r.Read(b)
	return b
}

// TestEmptyTreeRootIsDefault checks that a fresh tree hashes to defaultHashes[0].
func TestEmptyTreeRootIsDefault(t *testing.T) {
	if New().Root() != defaultHashes[0] {
		t.Fatal("empty tree root does not equal defaultHashes[0]")
	}
}

// TestGetInsertOverwriteDelete exercises the basic key/value lifecycle.
func TestGetInsertOverwriteDelete(t *testing.T) {
	tr := New()
	if _, ok := tr.Get([]byte("x")); ok {
		t.Fatal("empty tree should not contain a key")
	}
	tr.Insert([]byte("x"), []byte("1"))
	if v, ok := tr.Get([]byte("x")); !ok || string(v) != "1" {
		t.Fatalf("get after insert: %q %v", v, ok)
	}
	tr.Insert([]byte("x"), []byte("2"))
	if v, ok := tr.Get([]byte("x")); !ok || string(v) != "2" {
		t.Fatalf("get after overwrite: %q %v", v, ok)
	}
	tr.Delete([]byte("x"))
	if _, ok := tr.Get([]byte("x")); ok {
		t.Fatal("get after delete should miss")
	}
}

// TestInsertionOrderIndependence builds the same set twice in different orders and
// requires identical roots.
func TestInsertionOrderIndependence(t *testing.T) {
	rng := rand.New(rand.NewSource(42))
	type kv struct{ k, v []byte }
	var pairs []kv
	for i := 0; i < 300; i++ {
		pairs = append(pairs, kv{[]byte(fmt.Sprintf("k%d", i)), randBytes(rng, 24)})
	}
	a := New()
	for _, p := range pairs {
		a.Insert(p.k, p.v)
	}
	b := New()
	for _, idx := range rng.Perm(len(pairs)) {
		b.Insert(pairs[idx].k, pairs[idx].v)
	}
	if a.Root() != b.Root() {
		t.Fatal("root depends on insertion order")
	}
}

// TestIncrementalMatchesRebuild runs a mixed insert/delete sequence and requires the
// resulting root to match a from-scratch rebuild of the surviving set.
func TestIncrementalMatchesRebuild(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	tree := New()
	survivors := map[string][]byte{}
	for i := 0; i < 800; i++ {
		k := []byte(fmt.Sprintf("k%d", i))
		v := randBytes(rng, 16)
		tree.Insert(k, v)
		survivors[string(k)] = v
		if rng.Intn(3) == 0 && len(survivors) > 0 {
			dk := pickKey(rng, survivors)
			tree.Delete([]byte(dk))
			delete(survivors, dk)
		}
	}
	rebuilt := New()
	for _, k := range sortedKeys(survivors) {
		rebuilt.Insert([]byte(k), survivors[k])
	}
	if tree.Root() != rebuilt.Root() {
		t.Fatal("incremental root does not match rebuild")
	}
}

// TestLargeRootStable builds a 10k-entry tree twice with different insertion orders and
// requires a stable, bit-identical root.
func TestLargeRootStable(t *testing.T) {
	build := func(seed int64) [32]byte {
		rng := rand.New(rand.NewSource(seed))
		tr := New()
		for _, i := range rng.Perm(10000) {
			tr.Insert([]byte(fmt.Sprintf("k%d", i)), []byte(fmt.Sprintf("v%d", i)))
		}
		return tr.Root()
	}
	if build(1) != build(2) {
		t.Fatal("10k-entry root not stable across builds")
	}
}

// TestSharedPrefixSpill forces deep leaf spills using keys whose positions share their
// first byte, then checks order independence, retrieval, and rebuild equivalence.
func TestSharedPrefixSpill(t *testing.T) {
	keys := keysSharingFirstByte(t, 6)
	val := func(i int) []byte { return []byte(fmt.Sprintf("v%d", i)) }

	a := New()
	for i, k := range keys {
		a.Insert(k, val(i))
	}
	b := New()
	for i := len(keys) - 1; i >= 0; i-- {
		b.Insert(keys[i], val(i))
	}
	if a.Root() != b.Root() {
		t.Fatal("shared-prefix root depends on insertion order")
	}
	for i, k := range keys {
		if got, ok := a.Get(k); !ok || !bytes.Equal(got, val(i)) {
			t.Fatalf("shared-prefix key %d not retrievable", i)
		}
	}
}

// TestDeleteRestoresRoot inserts then deletes a fresh key at every step of a randomized
// sequence and requires the root to return exactly to its pre-insert value each time.
func TestDeleteRestoresRoot(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	tree := New()
	counter := 0
	for step := 0; step < 2000; step++ {
		if rng.Intn(2) == 0 {
			tree.Insert([]byte(fmt.Sprintf("perm-%d", counter)), randBytes(rng, 12))
			counter++
		}
		before := tree.Root()
		tmp := []byte(fmt.Sprintf("tmp-%d", step))
		tree.Insert(tmp, randBytes(rng, 12))
		if tree.Root() == before {
			t.Fatalf("step %d: insert did not change the root", step)
		}
		tree.Delete(tmp)
		if tree.Root() != before {
			t.Fatalf("step %d: delete did not restore the root", step)
		}
	}
}

// pickKey returns a deterministically chosen key from m.
func pickKey(r *rand.Rand, m map[string][]byte) string {
	keys := sortedKeys(m)
	return keys[r.Intn(len(keys))]
}

// sortedKeys returns the keys of m in ascending order.
func sortedKeys(m map[string][]byte) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// keysSharingFirstByte brute-forces count keys whose blake3 positions share their first
// byte, so every pair shares at least 8 leading bits and spills deep into the tree.
func keysSharingFirstByte(t *testing.T, count int) [][]byte {
	t.Helper()
	var target byte
	var out [][]byte
	for i := 0; i < 5_000_000 && len(out) < count; i++ {
		k := []byte(fmt.Sprintf("sp-%d", i))
		h := blake3.Sum256(k)
		if len(out) == 0 {
			target = h[0]
			out = append(out, k)
		} else if h[0] == target {
			out = append(out, k)
		}
	}
	if len(out) < count {
		t.Fatalf("found only %d shared-prefix keys", len(out))
	}
	return out
}
