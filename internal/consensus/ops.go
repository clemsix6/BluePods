package consensus

import (
	"BluePods/internal/events"
	"BluePods/internal/genesis"
	"BluePods/internal/types"
)

const (
	// reparentOp moves an object's parent edge; a transfer is a reparent to a
	// KeyRoot. Matches DeclaredOp.kind=0.
	reparentOp byte = 0

	// deleteOp destroys an object. Matches DeclaredOp.kind=1.
	deleteOp byte = 1
)

// commitDeclaredOps applies a transaction's declared operations and emits the
// commit verdict, returning the fee split unchanged so the caller keeps the fee
// the sender already paid regardless of the outcome. A transaction carries
// EITHER declared operations OR a pod call; a mix is rejected here rather than
// half-applied.
func (d *DAG) commitDeclaredOps(tx *types.Transaction, txHash, vertexHash Hash, commitRound uint64, feeSplit FeeSplit) FeeSplit {
	success := !txHasPodCall(tx) && d.handleDeclaredOps(tx)

	reason := FailNone
	commitReason := ""
	if !success {
		reason = FailOps
		commitReason = "declared_ops"
	}

	d.emitTransaction(tx, success, reason)
	events.TxCommitted(txHash, vertexHash, commitRound, success, commitReason)

	return feeSplit
}

// handleDeclaredOps validates a transaction's declared operations as one
// all-or-nothing list, then applies them. Validation runs against a staged view
// so each operation sees the effects of the ones before it; the real tracker is
// mutated only after the whole list is proven valid, because a settled deletion
// (supply burn, deposit release) cannot be undone. The first failing operation
// rejects the whole list with no effect. An all-zero sender is rejected outright
// so it can never reach the cascade control walk and seize a frozen object.
func (d *DAG) handleDeclaredOps(tx *types.Transaction) bool {
	sender, ok := hash32(tx.SenderBytes())
	if !ok || sender == (Hash{}) {
		return false
	}

	ops := genesis.ExtractOperations(tx)
	if len(ops) == 0 {
		return false
	}

	refs := mutableRefIDSet(tx)
	staged := newStagedView(d.tracker)

	for i := range ops {
		if !staged.validate(sender, ops[i], refs) {
			return false
		}
	}

	d.applyDeclaredOps(tx, ops)

	return true
}

// applyDeclaredOps runs the effects of an already-validated operation list in
// order against the real tracker. Reparents rebind the parent edge; deletes
// settle the deposit and drop the held body.
func (d *DAG) applyDeclaredOps(tx *types.Transaction, ops []genesis.DeclaredOp) {
	txHash, _ := hash32(tx.HashBytes())
	gasCoinID, hasGasCoin := txGasCoinID(tx)

	for i := range ops {
		if ops[i].Kind == reparentOp {
			d.applyReparent(txHash, ops[i])
			continue
		}

		d.applyDelete(txHash, gasCoinID, hasGasCoin, ops[i])
	}
}

// applyReparent rebinds an object's parent edge, rewrites the stored body's
// owner bytes to mirror the new parent reference, and emits the reparent event
// with the object's current (already version-bumped) version. The tracker edge
// and the body owner are rewritten in the same apply step, so no reader on this
// node ever sees the tracker parent and the body owner disagree.
func (d *DAG) applyReparent(txHash Hash, op genesis.DeclaredOp) {
	objID := toHash(op.ObjectID)
	parent := toHash(op.Target)

	d.tracker.setParent(objID, op.TargetKind, parent)
	d.rewriteBodyOwner(objID, op.TargetKind, parent)
	events.ObjectReparented(objID, txHash, op.TargetKind, parent, d.tracker.getVersion(objID))
}

// rewriteBodyOwner rewrites a reparented object's stored body owner bytes and
// parent kind wherever a copy lives, restoring the invariant that body owner
// bytes equal the current parent bytes for every reader (gas ownership,
// mutable-ref ownership, GetObject, pod execution). It rewrites the
// consensus-side coin store, where singletons and held copies the ownership
// checks read live, and fires the state hook for the state-held body. A body
// absent from either store is a no-op.
func (d *DAG) rewriteBodyOwner(objID Hash, kind byte, parent Hash) {
	if d.coinStore != nil {
		if data := d.coinStore.GetObject(objID); data != nil {
			d.coinStore.SetObject(writeObjectOwner(data, parent, kind))
		}
	}

	if d.onObjectReparented != nil {
		d.onObjectReparented(objID, kind, parent)
	}
}

// applyDelete settles a deletion — release the deposit, refund/burn 95/5, remove
// the tracker entry, and emit state.object.deleted — then fires the body-drop
// hook so a holder discards the stored content (a non-holder no-ops).
func (d *DAG) applyDelete(txHash, gasCoinID Hash, hasGasCoin bool, op genesis.DeclaredOp) {
	objID := toHash(op.ObjectID)

	refund := d.settleDeletion(objID, gasCoinID, hasGasCoin)
	events.ObjectDeleted(objID, txHash, refund)

	if d.onObjectDeleted != nil {
		d.onObjectDeleted(objID)
	}
}

// txHasPodCall reports whether a transaction carries a pod call — a non-empty
// function name or a non-zero pod target. A declared-operation transaction
// carries neither: operations and pod execution are mutually exclusive.
func txHasPodCall(tx *types.Transaction) bool {
	if len(tx.FunctionName()) > 0 {
		return true
	}

	for _, b := range tx.PodBytes() {
		if b != 0 {
			return true
		}
	}

	return false
}

// mutableRefIDSet collects the 32-byte IDs of a transaction's mutable refs, so a
// declared operation can require its target be a version-tracked mutable ref —
// checkAndUpdate has already verified those versions and bumped them.
func mutableRefIDSet(tx *types.Transaction) map[Hash]bool {
	set := make(map[Hash]bool, tx.MutableRefsLength())

	var ref types.ObjectRef
	for i := 0; i < tx.MutableRefsLength(); i++ {
		if !tx.MutableRefs(&ref, i) {
			continue
		}

		if id, ok := mutableRefID(&ref); ok {
			set[id] = true
		}
	}

	return set
}

// hash32 copies a 32-byte slice into a Hash, reporting ok=false for any other
// length.
func hash32(b []byte) (Hash, bool) {
	if len(b) != 32 {
		return Hash{}, false
	}

	var h Hash
	copy(h[:], b)

	return h, true
}

// toHash copies up to 32 bytes into a Hash. Callers use it only after hash32 has
// already proven the slice is exactly 32 bytes during validation.
func toHash(b []byte) Hash {
	var h Hash
	copy(h[:], b)

	return h
}
