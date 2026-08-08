package genesis

import (
	flatbuffers "github.com/google/flatbuffers/go"

	"BluePods/internal/types"
)

// DeclaredOp is a protocol-level operation applied at commit by every node
// without pod execution. Kind: 0=reparent (a transfer is a reparent to a
// KeyRoot), 1=delete, 2=domain_register, 3=domain_renew, 4=domain_update,
// 5=domain_transfer, 6=domain_delete. This type only carries the wire data
// through the canonical body; op semantics belong to the commit-time handler.
type DeclaredOp struct {
	Kind       uint8  // Kind selects the operation (see type docstring for the mapping).
	ObjectID   []byte // ObjectID is the target object (reparent, delete, domain_register/update pointee).
	TargetKind uint8  // TargetKind is the reparent target's kind (0=KeyRoot, 1=ObjectParent).
	Target     []byte // Target is the reparent new-parent bytes, or the domain_transfer new owner.
	Name       string // Name is the domain name for domain ops.
	TermEpochs uint32 // TermEpochs is the rental term for domain_register/renew.
}

// ExtractOperations reads a transaction's declared operations into Go-level
// DeclaredOp values. Validation ingress and the commit-time authenticity check
// both call this to reconstruct the canonical unsigned body, so the two sites
// cannot drift on how an operation's bytes are read.
func ExtractOperations(tx *types.Transaction) []DeclaredOp {
	count := tx.OperationsLength()
	if count == 0 {
		return nil
	}

	result := make([]DeclaredOp, count)
	var op types.DeclaredOp

	for i := 0; i < count; i++ {
		tx.Operations(&op, i)
		result[i] = DeclaredOp{
			Kind:       op.Kind(),
			ObjectID:   op.ObjectIdBytes(),
			TargetKind: op.TargetKind(),
			Target:     op.TargetBytes(),
			Name:       string(op.Name()),
			TermEpochs: op.TermEpochs(),
		}
	}

	return result
}

// buildDeclaredOpsVector builds the operations vector from op data, or returns
// 0 when empty so a transaction with no declared ops serializes byte-identically
// to one built before the field existed (the same absent-when-empty rule
// deleted_objects and the sponsorship fields follow).
func buildDeclaredOpsVector(builder *flatbuffers.Builder, ops []DeclaredOp) flatbuffers.UOffsetT {
	if len(ops) == 0 {
		return 0
	}

	offsets := make([]flatbuffers.UOffsetT, len(ops))
	for i, op := range ops {
		offsets[i] = buildSingleDeclaredOp(builder, op)
	}

	types.TransactionStartOperationsVector(builder, len(offsets))
	for i := len(offsets) - 1; i >= 0; i-- {
		builder.PrependUOffsetT(offsets[i])
	}

	return builder.EndVector(len(offsets))
}

// buildSingleDeclaredOp builds a single DeclaredOp table in the builder.
func buildSingleDeclaredOp(builder *flatbuffers.Builder, op DeclaredOp) flatbuffers.UOffsetT {
	var objectIDVec, targetVec flatbuffers.UOffsetT
	if len(op.ObjectID) > 0 {
		objectIDVec = builder.CreateByteVector(op.ObjectID)
	}

	if len(op.Target) > 0 {
		targetVec = builder.CreateByteVector(op.Target)
	}

	var nameOff flatbuffers.UOffsetT
	if op.Name != "" {
		nameOff = builder.CreateString(op.Name)
	}

	types.DeclaredOpStart(builder)
	types.DeclaredOpAddKind(builder, op.Kind)

	if objectIDVec != 0 {
		types.DeclaredOpAddObjectId(builder, objectIDVec)
	}

	types.DeclaredOpAddTargetKind(builder, op.TargetKind)

	if targetVec != 0 {
		types.DeclaredOpAddTarget(builder, targetVec)
	}

	if nameOff != 0 {
		types.DeclaredOpAddName(builder, nameOff)
	}

	types.DeclaredOpAddTermEpochs(builder, op.TermEpochs)

	return types.DeclaredOpEnd(builder)
}
