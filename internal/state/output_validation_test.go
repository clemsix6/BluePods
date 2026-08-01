package state

import (
	"testing"

	flatbuffers "github.com/google/flatbuffers/go"

	"BluePods/internal/types"
)

// --- pod-output deletion carve-out: max_create_domains is deprecated ---

// TestValidateOutput_ShardedDeletionWithMaxCreateDomainsRejected verifies that
// a deprecated, nonzero max_create_domains no longer grants the pod-output
// deletion carve-out: it used to be treated the same as
// CreatedObjectsReplicationLength > 0 (the commit-path global-execution guard
// ran the transaction on every node), but the pod domain write path is
// retired and max_create_domains no longer forces global execution, so a
// sharded object deletion in such a transaction must be rejected like any
// other non-globally-executed deletion.
func TestValidateOutput_ShardedDeletionWithMaxCreateDomainsRejected(t *testing.T) {
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

	if err := s.validateOutput(out, tx, Hash{}, inputs); err == nil {
		t.Error("expected rejection: a deprecated max_create_domains must not grant the deletion carve-out")
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
