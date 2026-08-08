package consensus

import (
	"testing"

	"BluePods/internal/storage"
)

// adoptedTestStore opens a storage handle over a temp directory.
func adoptedTestStore(t *testing.T) *storage.Storage {
	t.Helper()

	db, err := storage.New(t.TempDir())
	if err != nil {
		t.Fatalf("storage: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	return db
}

// persistCommittedState writes the pair a committed round persists atomically:
// the commit cursor and the live validator set that produced it (see
// advanceCommitCursor).
func persistCommittedState(t *testing.T, db *storage.Storage, round uint64) {
	t.Helper()

	var pk Hash
	pk[0] = 0xAB

	set := NewValidatorSet(nil)
	set.AddWithStake(pk, "10.0.0.1:9000", [48]byte{0x01}, 1000, 0, false)

	s := &store{db: db}
	s.saveCommitCursorBatch(round, []storage.KeyValue{{Key: liveValidatorsKey, Value: encodeLiveValidators(set)}})
}

// TestHasAdoptedState_FreshDirectory is the joiner's own shape: a node starting
// on an empty data directory owns no state, so it must take the sync path and
// its verification gate.
func TestHasAdoptedState_FreshDirectory(t *testing.T) {
	db := adoptedTestStore(t)

	if HasAdoptedState(db) {
		t.Fatal("an empty data directory reported adopted state: a joiner would skip the verification gate")
	}
}

// TestHasAdoptedState_UnverifiedSyncResidue is the security case. A join
// refused by the checkpoint gate has ALREADY written the source's snapshot into
// this directory and let the commit loop persist a cursor and a live validator
// set from it. None of that is this node's own state, so the next start must
// still take the sync path and face the gate again.
func TestHasAdoptedState_UnverifiedSyncResidue(t *testing.T) {
	db := adoptedTestStore(t)

	persistCommittedState(t, db, 41)

	if HasAdoptedState(db) {
		t.Fatal("committed state alone reported as adopted: a refused join would be resumed as its own on the next start, walking around the gate")
	}
}

// TestHasAdoptedState_MarkedWithoutState covers the reverse residue: a marker
// with no committed state behind it (a node marked at genesis that never
// committed a round) is nothing to resume from.
func TestHasAdoptedState_MarkedWithoutState(t *testing.T) {
	db := adoptedTestStore(t)

	if err := MarkStateAdopted(db); err != nil {
		t.Fatalf("mark state adopted: %v", err)
	}

	if HasAdoptedState(db) {
		t.Fatal("a marker with no commit cursor reported adopted state: there is nothing to resume from")
	}
}

// TestHasAdoptedState_AdoptedAndCommitted is the resume shape: a node that
// marked this directory as its own and committed rounds into it boots from it.
func TestHasAdoptedState_AdoptedAndCommitted(t *testing.T) {
	db := adoptedTestStore(t)

	persistCommittedState(t, db, 77)
	if err := MarkStateAdopted(db); err != nil {
		t.Fatalf("mark state adopted: %v", err)
	}

	if !HasAdoptedState(db) {
		t.Fatal("a node's own committed state reported as foreign: it would re-sync and re-verify state it produced itself")
	}
}
