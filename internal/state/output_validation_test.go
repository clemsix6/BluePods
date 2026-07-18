package state

import (
	"testing"

	flatbuffers "github.com/google/flatbuffers/go"

	"BluePods/internal/types"
)

// --- pod-output deletion carve-out: domain-registering transactions ---

// TestValidateOutput_ShardedDeletionInDomainRegisteringTxSucceeds verifies that
// a pod output may delete a sharded object when the transaction registers a
// domain (MaxCreateDomains > 0) and creates no objects: the commit-path
// global-execution guard (internal/consensus/commit.go) treats
// MaxCreateDomains > 0 the same as CreatedObjectsReplicationLength > 0 — every
// validator executes the transaction — so the deletion restriction here must
// allow it too.
func TestValidateOutput_ShardedDeletionInDomainRegisteringTxSucceeds(t *testing.T) {
	db := newTestStorage(t)
	s := New(db, nil)

	sender := Hash{0x01}
	shardedID := Hash{0x60}
	inputs := []*types.Object{
		objectFromBytes(buildObjectWithParent(shardedID, sender, 10, parentKindKeyRoot, []byte("sharded"))),
	}

	tx := buildDomainRegisteringTx(sender, 1, []refSpec{{id: shardedID, hasID: true}})
	output := buildDeletedOutput([]Hash{shardedID})
	out := types.GetRootAsPodExecuteOutput(output, 0)

	if err := s.validateOutput(out, tx, Hash{}, inputs); err != nil {
		t.Errorf("expected success deleting a sharded object in a domain-registering tx, got: %v", err)
	}
}

// buildDomainRegisteringTx builds a Transaction with an explicit sender, no
// created_objects_replication entries, MaxCreateDomains set to maxDomains, and
// the given mutable references (direct-ID or domain).
func buildDomainRegisteringTx(sender Hash, maxDomains uint16, refs []refSpec) *types.Transaction {
	builder := flatbuffers.NewBuilder(512)

	refOffsets := make([]flatbuffers.UOffsetT, len(refs))
	for i := len(refs) - 1; i >= 0; i-- {
		refOffsets[i] = mutableRefOffset(builder, refs[i])
	}

	types.TransactionStartMutableRefsVector(builder, len(refs))
	for i := len(refOffsets) - 1; i >= 0; i-- {
		builder.PrependUOffsetT(refOffsets[i])
	}
	mutRefsVec := builder.EndVector(len(refs))

	senderVec := builder.CreateByteVector(sender[:])
	podVec := builder.CreateByteVector(make([]byte, 32))

	types.TransactionStart(builder)
	types.TransactionAddSender(builder, senderVec)
	types.TransactionAddPod(builder, podVec)
	types.TransactionAddMutableRefs(builder, mutRefsVec)
	types.TransactionAddMaxCreateDomains(builder, maxDomains)
	txOffset := types.TransactionEnd(builder)

	builder.Finish(txOffset)

	return types.GetRootAsTransaction(builder.FinishedBytes(), 0)
}
