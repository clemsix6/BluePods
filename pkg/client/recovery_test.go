package client

import (
	"crypto/ed25519"
	"testing"
)

// walletOverKey builds a bare Wallet whose public key is pk, with no
// signing capability exercised by these tests (recovery never signs
// anything) and a nil-safe coins/objects map.
func walletOverKey(pk [32]byte, tracked ...[32]byte) *Wallet {
	w := &Wallet{
		privKey: make(ed25519.PrivateKey, ed25519.PrivateKeySize),
		pubKey:  ed25519.PublicKey(append([]byte{}, pk[:]...)),
		coins:   make(map[[32]byte]*CoinInfo),
		objects: make(map[[32]byte]bool),
	}

	for _, id := range tracked {
		w.objects[id] = true
	}

	return w
}

// TestRecoverObjectsWalksTheIndexFromABareKey verifies wallet recovery from a
// bare key repopulates its tracked object set purely from what the index
// reports reachable under that key: two direct children plus one nested
// grandchild reached by recursing into an object-parented subtree (the
// fixture's owner/child/other/nested tree — see fixture_test.go).
func TestRecoverObjectsWalksTheIndexFromABareKey(t *testing.T) {
	committee := newCommittee(t, 4, 100)
	f := newFixture(t, committee)
	lc := &LightClient{src: f, checkpoint: f.checkpointOf(committee)}

	w := walletOverKey(f.owner)

	discovered, err := w.RecoverObjects(lc)
	if err != nil {
		t.Fatalf("recover objects: %v", err)
	}

	want := map[[32]byte]bool{f.child: true, f.other: true, f.nested: true}
	if len(discovered) != len(want) {
		t.Fatalf("discovered %d objects, want %d: %x", len(discovered), len(want), discovered)
	}
	for _, id := range discovered {
		if !want[id] {
			t.Errorf("unexpected discovered id %x", id[:8])
		}
	}

	ids := w.ObjectIDs()
	if len(ids) != len(want) {
		t.Fatalf("wallet tracks %d objects after recovery, want %d", len(ids), len(want))
	}
	for _, id := range ids {
		if !want[id] {
			t.Errorf("wallet tracks unexpected id %x after recovery", id[:8])
		}
	}
}

// TestRecoverObjectsMergesRatherThanOverwrites verifies a locally tracked
// object the index walk does not reach — its creating transaction has not
// committed, or the read caught the tree between a mutation and its anchor —
// survives recovery instead of being dropped: reconciliation is a union, not
// a replace.
func TestRecoverObjectsMergesRatherThanOverwrites(t *testing.T) {
	committee := newCommittee(t, 4, 100)
	f := newFixture(t, committee)
	lc := &LightClient{src: f, checkpoint: f.checkpointOf(committee)}

	var uncommitted [32]byte
	uncommitted[0] = 0xFE

	w := walletOverKey(f.owner, uncommitted)

	if _, err := w.RecoverObjects(lc); err != nil {
		t.Fatalf("recover objects: %v", err)
	}

	found := false
	for _, id := range w.ObjectIDs() {
		if id == uncommitted {
			found = true
		}
	}
	if !found {
		t.Fatal("a locally tracked object not yet visible in the index was dropped by recovery")
	}

	// The index-reachable objects are still merged in alongside it.
	if len(w.ObjectIDs()) != 4 {
		t.Fatalf("wallet tracks %d objects after recovery, want 4 (3 discovered + 1 local)", len(w.ObjectIDs()))
	}
}

// loopingChildrenSource answers every ListChildren call with exactly one
// child, deterministically derived from the parent, so a walk starting from
// any root never terminates on its own.
type loopingChildrenSource struct{}

// ListChildren always answers with one more hop.
func (loopingChildrenSource) ListChildren(parent [32]byte) ([][32]byte, error) {
	next := parent
	next[0]++

	return [][32]byte{next}, nil
}

// TestRecoverObjectsStopsAtTheDepthBound verifies a source with no
// terminating depth is stopped by recoveryDepthLimit rather than recursed
// into forever — the defense the bound exists for, since the protocol's own
// walkDepthLimit means a genuine object tree can never trigger it honestly.
func TestRecoverObjectsStopsAtTheDepthBound(t *testing.T) {
	w := walletOverKey([32]byte{})

	if _, err := w.RecoverObjects(loopingChildrenSource{}); err == nil {
		t.Fatal("recovery against a source with no terminating depth was accepted")
	}
}
