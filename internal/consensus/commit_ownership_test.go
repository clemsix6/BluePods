package consensus

import (
	"bytes"
	"testing"

	flatbuffers "github.com/google/flatbuffers/go"

	"BluePods/internal/events"
	"BluePods/internal/types"
)

// TestExecuteTx_ReplicatedMutationVerdictUniformAcrossHoldership replays the
// SAME committed attested transaction — a transfer mutating a replication-3
// object whose attested copy rides in the ATX — on two nodes: one that holds
// the object's content locally and one that does not. A replicated object's
// content lives only on its holders, so the commit verdict must be derived
// from the committed artifact (identical on every node), never from a
// holdership-dependent local lookup. The two nodes must therefore agree on
// the tx.committed verdict (success and fail reason).
func TestExecuteTx_ReplicatedMutationVerdictUniformAcrossHoldership(t *testing.T) {
	buf := captureEvents(t)

	sender := Hash{0x11}
	objID := Hash{0x77}

	// The single committed artifact both nodes replay. The attested object in
	// the ATX is owned by the sender, so the transfer is legitimate.
	atxBytes := buildReplicatedTransferATX(t, sender, objID, sender, 3)

	holderSuccess, holderReason := replayReplicatedMutationVerdict(t, buf, atxBytes, objID, sender, true)
	nonHolderSuccess, nonHolderReason := replayReplicatedMutationVerdict(t, buf, atxBytes, objID, sender, false)

	if holderSuccess != nonHolderSuccess || holderReason != nonHolderReason {
		t.Fatalf("commit verdict diverges by holdership: holder=(success=%v reason=%q) non-holder=(success=%v reason=%q)",
			holderSuccess, holderReason, nonHolderSuccess, nonHolderReason)
	}
}

// replayReplicatedMutationVerdict commits atxBytes on a fresh node and returns
// its tx.committed verdict. hasContent decides whether this node holds the
// replicated object locally: a holder has it in its coin store and executes,
// a non-holder has neither the content nor holdership. buf accumulates the
// captured event stream and is reset before the replay.
func replayReplicatedMutationVerdict(t *testing.T, buf *bytes.Buffer, atxBytes []byte, objID, objOwner Hash, hasContent bool) (bool, string) {
	t.Helper()

	buf.Reset()

	db := newTestStorage(t)
	validators, vs := newTestValidatorSet(3)
	dag := New(db, vs, &mockBroadcaster{}, testSystemPod, 0, validators[0].privKey, nil)
	t.Cleanup(dag.Close)
	disableTxAuth(dag)

	// The ownership check needs a coin store; feeParams stays nil so fee
	// deduction is a no-op and the ownership check is exercised in isolation.
	coinStore := newMockCoinStore()
	if hasContent {
		coinStore.SetObject(buildTestCoinObject(objID, 0, objOwner, 3))
	}
	dag.coinStore = coinStore

	dag.SetIsHolder(func(id [32]byte, replication uint16) bool {
		return replication == 0 || hasContent
	})

	atx := types.GetRootAsAttestedTransaction(atxBytes, 0)
	dag.executeTx(atx, 1, validators[0].pubKey, nil, Hash{})

	recs := eventsNamed(t, buf, events.EvTxCommitted)
	if len(recs) != 1 {
		t.Fatalf("want exactly 1 tx.committed event, got %d: %v", len(recs), recs)
	}

	success, _ := recs[0]["success"].(bool)
	reason, _ := recs[0]["reason"].(string)

	return success, reason
}

// buildReplicatedTransferATX builds an attested "transfer" transaction mutating
// objID, with the attested replicated object (owner objOwner, given replication)
// carried in the ATX Objects vector. It carries no proofs, so the proof gate is
// skipped and the transaction reaches the ownership check.
func buildReplicatedTransferATX(t *testing.T, sender, objID, objOwner Hash, replication uint16) []byte {
	t.Helper()

	builder := flatbuffers.NewBuilder(1024)

	mutVec := buildObjectRefVector(builder, []objectRef{{id: objID, version: 0}}, true)

	hashVec := builder.CreateByteVector(make([]byte, 32))
	senderVec := builder.CreateByteVector(sender[:])
	podVec := builder.CreateByteVector(make([]byte, 32))
	funcNameOff := builder.CreateString("transfer")

	types.TransactionStart(builder)
	types.TransactionAddHash(builder, hashVec)
	types.TransactionAddSender(builder, senderVec)
	types.TransactionAddPod(builder, podVec)
	types.TransactionAddFunctionName(builder, funcNameOff)
	types.TransactionAddMutableRefs(builder, mutVec)
	txOff := types.TransactionEnd(builder)

	objIDVec := builder.CreateByteVector(objID[:])
	objOwnerVec := builder.CreateByteVector(objOwner[:])
	objContentVec := builder.CreateByteVector([]byte("replicated payload"))

	types.ObjectStart(builder)
	types.ObjectAddId(builder, objIDVec)
	types.ObjectAddVersion(builder, 0)
	types.ObjectAddOwner(builder, objOwnerVec)
	types.ObjectAddReplication(builder, replication)
	types.ObjectAddContent(builder, objContentVec)
	objOff := types.ObjectEnd(builder)

	types.AttestedTransactionStartObjectsVector(builder, 1)
	builder.PrependUOffsetT(objOff)
	objVec := builder.EndVector(1)

	types.AttestedTransactionStartProofsVector(builder, 0)
	prfVec := builder.EndVector(0)

	types.AttestedTransactionStart(builder)
	types.AttestedTransactionAddTransaction(builder, txOff)
	types.AttestedTransactionAddObjects(builder, objVec)
	types.AttestedTransactionAddProofs(builder, prfVec)
	atxOff := types.AttestedTransactionEnd(builder)

	builder.Finish(atxOff)

	return builder.FinishedBytes()
}
