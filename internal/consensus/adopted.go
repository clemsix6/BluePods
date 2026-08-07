package consensus

import (
	"encoding/binary"
	"fmt"

	"BluePods/internal/storage"
)

// stateAdoptedKey marks a data directory whose committed state the node owning
// it has adopted as its OWN: it either originated that state (a genesis
// bootstrap) or proved it before going live on it (a verified join). The
// marker is what separates a restart from a join, and it is deliberately a
// durable local fact rather than anything derivable from the state itself —
// see HasAdoptedState.
var stateAdoptedKey = metaKey("stateAdopted")

// stateAdoptedMark is the marker's value. Only its presence carries meaning.
var stateAdoptedMark = []byte{1}

// MarkStateAdopted records that the committed state in this data directory is
// the node's own, so a later start resumes from it instead of re-adopting
// state from a peer.
//
// It is called at exactly two points, both of them after the state has stopped
// being anyone else's claim: in cmd/node's runBootstrap, gated on cfg.Bootstrap
// so only a genuine genesis start marks — that function is also Run's general
// fallthrough for any node started with neither --bootstrap nor a resumable
// local state, and marking on that branch too would launder a refused join's
// residue into adopted state; and in performSync, strictly after
// verifySyncedState returns nil, when a join completes verification (the
// snapshot has been attested by a stake quorum of the checkpointed validator
// set). A join the gate refuses never reaches it, which is what keeps the
// unverified snapshot such a join leaves on disk from being resumed as if it
// were the node's own.
func MarkStateAdopted(db *storage.Storage) error {
	if err := db.Set(stateAdoptedKey, stateAdoptedMark); err != nil {
		return fmt.Errorf("persist adopted-state marker:\n%w", err)
	}

	return nil
}

// HasAdoptedState reports whether this data directory holds committed state the
// node may resume from: the marker above, plus the committed state it vouches
// for (a commit cursor past genesis and the live validator set persisted with
// it, both written in the same batch by advanceCommitCursor).
//
// The marker is required, and the committed state alone is NOT enough, because
// a join refused by the verification gate leaves exactly that state behind: the
// sync path applies the source's snapshot to local storage and runs the commit
// loop over it BEFORE the gate decides, so a refused join's directory holds a
// cursor and a validator set that came off the wire. Resuming on those would
// let a supervisor's restart loop walk around the gate one crash at a time.
func HasAdoptedState(db *storage.Storage) bool {
	if mark, err := db.Get(stateAdoptedKey); err != nil || len(mark) == 0 {
		return false
	}

	cursor, err := db.Get(commitCursorKey)
	if err != nil || len(cursor) < 8 || binary.BigEndian.Uint64(cursor) == 0 {
		return false
	}

	return len(LoadLiveValidators(db)) > 0
}
