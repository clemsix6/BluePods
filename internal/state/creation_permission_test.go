package state

import (
	"testing"

	flatbuffers "github.com/google/flatbuffers/go"

	"BluePods/internal/types"
)

// --- creation-permission rule ---

// TestValidateOutput_GiftUnderForeignKeySucceeds verifies that a created object
// declaring a KeyRoot parent that is not the sender's own key is allowed: the
// creator pays the deposit, so rooting a new object at someone else's key is a
// gift, the same consent-free attach the transfer operation already allows.
func TestValidateOutput_GiftUnderForeignKeySucceeds(t *testing.T) {
	db := newTestStorage(t)
	s := New(db, nil)

	sender := Hash{0x01}
	foreign := Hash{0x02}

	tx := buildCreatingTx(sender, 1, nil)
	output := buildCreatedParentsOutput([]createdSpec{{owner: foreign, parentKind: parentKindKeyRoot}})
	out := types.GetRootAsPodExecuteOutput(output, 0)

	if err := s.validateOutput(out, tx, Hash{}, nil); err != nil {
		t.Errorf("expected success gifting an object under a foreign key, got: %v", err)
	}
}

// TestValidateOutput_CreateUnderOwnKeySucceeds verifies that a created object
// rooted at the sender's own key is allowed.
func TestValidateOutput_CreateUnderOwnKeySucceeds(t *testing.T) {
	db := newTestStorage(t)
	s := New(db, nil)

	sender := Hash{0x01}

	tx := buildCreatingTx(sender, 1, nil)
	output := buildCreatedParentsOutput([]createdSpec{{owner: sender, parentKind: parentKindKeyRoot}})
	out := types.GetRootAsPodExecuteOutput(output, 0)

	if err := s.validateOutput(out, tx, Hash{}, nil); err != nil {
		t.Errorf("expected success creating under own key, got: %v", err)
	}
}

// TestValidateOutput_CreateUnderControlledObjectSucceeds verifies that a created
// object hung under an existing object the sender controls is allowed, via the
// consensus cascade-walk callback.
func TestValidateOutput_CreateUnderControlledObjectSucceeds(t *testing.T) {
	db := newTestStorage(t)
	s := New(db, nil)

	sender := Hash{0x01}
	parent := Hash{0xAA}

	// The callback stands in for the tracker walk: sender controls `parent`.
	s.SetParentValidator(func(kind byte, p [32]byte, snd [32]byte, _ *types.Transaction) bool {
		return kind == parentKindObject && Hash(p) == parent && Hash(snd) == sender
	})

	tx := buildCreatingTx(sender, 1, nil)
	output := buildCreatedParentsOutput([]createdSpec{{owner: parent, parentKind: parentKindObject}})
	out := types.GetRootAsPodExecuteOutput(output, 0)

	if err := s.validateOutput(out, tx, Hash{}, nil); err != nil {
		t.Errorf("expected success creating under a controlled object, got: %v", err)
	}
}

// TestValidateOutput_CreateUnderUncontrolledObjectFails verifies that hanging a
// created object under an object the sender does not control is rejected: the
// callback returns false and no exemption applies.
func TestValidateOutput_CreateUnderUncontrolledObjectFails(t *testing.T) {
	db := newTestStorage(t)
	s := New(db, nil)

	sender := Hash{0x01}
	parent := Hash{0xAA}

	s.SetParentValidator(func(byte, [32]byte, [32]byte, *types.Transaction) bool {
		return false
	})

	tx := buildCreatingTx(sender, 1, nil)
	output := buildCreatedParentsOutput([]createdSpec{{owner: parent, parentKind: parentKindObject}})
	out := types.GetRootAsPodExecuteOutput(output, 0)

	if err := s.validateOutput(out, tx, Hash{}, nil); err == nil {
		t.Error("expected error creating under an uncontrolled object")
	}
}

// TestValidateOutput_CreateUnderDomainReferencedTableSucceeds verifies that a
// created object may hang under an object this transaction reaches through a
// mutable domain reference, even without the control callback.
func TestValidateOutput_CreateUnderDomainReferencedTableSucceeds(t *testing.T) {
	db := newTestStorage(t)
	s := New(db, nil)

	sender := Hash{0x01}
	tableID := Hash{0xBB}
	s.domains.set("table.app", tableID)

	tx := buildCreatingTx(sender, 1, []refSpec{{domain: "table.app"}})
	output := buildCreatedParentsOutput([]createdSpec{{owner: tableID, parentKind: parentKindObject}})
	out := types.GetRootAsPodExecuteOutput(output, 0)

	if err := s.validateOutput(out, tx, Hash{}, nil); err != nil {
		t.Errorf("expected success creating under a domain-referenced table, got: %v", err)
	}
}

// TestValidateOutput_NestedCreationInSameTxSucceeds verifies that a child object
// created under a sibling created earlier in the SAME output is authorized,
// before the tracker has learned the sibling. This is the ordering hole the
// staged set closes.
func TestValidateOutput_NestedCreationInSameTxSucceeds(t *testing.T) {
	db := newTestStorage(t)
	s := New(db, nil)

	sender := Hash{0x01}
	txHash := Hash{0xEE}

	// A at index 0 roots at sender's key; B at index 1 hangs under A's computed ID.
	parentA := computeObjectID(txHash, 0)

	tx := buildCreatingTx(sender, 2, nil)
	output := buildCreatedParentsOutput([]createdSpec{
		{owner: sender, parentKind: parentKindKeyRoot},
		{owner: parentA, parentKind: parentKindObject},
	})
	out := types.GetRootAsPodExecuteOutput(output, 0)

	if err := s.validateOutput(out, tx, txHash, nil); err != nil {
		t.Errorf("expected success for nested same-tx creation, got: %v", err)
	}
}

// TestValidateOutput_NestedCreationWrongOrderFails verifies that referencing a
// sibling created LATER (or never) in the same output is rejected: the staged
// set only holds objects validated before the current one.
func TestValidateOutput_NestedCreationWrongOrderFails(t *testing.T) {
	db := newTestStorage(t)
	s := New(db, nil)

	sender := Hash{0x01}
	txHash := Hash{0xEE}

	// B at index 0 references the ID of A at index 1, which has not been staged yet.
	parentA := computeObjectID(txHash, 1)

	tx := buildCreatingTx(sender, 2, nil)
	output := buildCreatedParentsOutput([]createdSpec{
		{owner: parentA, parentKind: parentKindObject},
		{owner: sender, parentKind: parentKindKeyRoot},
	})
	out := types.GetRootAsPodExecuteOutput(output, 0)

	if err := s.validateOutput(out, tx, txHash, nil); err == nil {
		t.Error("expected error referencing a sibling created later in the output")
	}
}

// --- parent immutability on updated objects ---

// TestValidateOutput_UpdateFlippingOwnerReverts verifies that an updated object
// whose owner bytes differ from its attested input copy is rejected.
func TestValidateOutput_UpdateFlippingOwnerReverts(t *testing.T) {
	db := newTestStorage(t)
	s := New(db, nil)

	id := Hash{0x30}
	input := objectFromBytes(buildObjectWithParent(id, Hash{0xAA}, 10, parentKindKeyRoot, []byte("coin")))

	tx := buildCreatingTx(Hash{0x01}, 0, nil)
	output := buildUpdatedParentsOutput([]updatedSpec{{id: id, owner: Hash{0xCC}, parentKind: parentKindKeyRoot}})
	out := types.GetRootAsPodExecuteOutput(output, 0)

	if err := s.validateOutput(out, tx, Hash{}, []*types.Object{input}); err == nil {
		t.Error("expected error for an update that flips owner bytes")
	}
}

// TestValidateOutput_UpdateFlippingParentKindReverts verifies that an updated
// object whose parent_kind differs from its input copy is rejected, even when the
// owner bytes are unchanged.
func TestValidateOutput_UpdateFlippingParentKindReverts(t *testing.T) {
	db := newTestStorage(t)
	s := New(db, nil)

	id := Hash{0x30}
	owner := Hash{0xAA}
	input := objectFromBytes(buildObjectWithParent(id, owner, 10, parentKindKeyRoot, []byte("coin")))

	tx := buildCreatingTx(Hash{0x01}, 0, nil)
	output := buildUpdatedParentsOutput([]updatedSpec{{id: id, owner: owner, parentKind: parentKindObject}})
	out := types.GetRootAsPodExecuteOutput(output, 0)

	if err := s.validateOutput(out, tx, Hash{}, []*types.Object{input}); err == nil {
		t.Error("expected error for an update that flips parent_kind")
	}
}

// TestValidateOutput_ContentOnlyUpdateSucceeds verifies that an update that keeps
// owner and parent_kind identical to the input copy passes: pods may change
// content freely.
func TestValidateOutput_ContentOnlyUpdateSucceeds(t *testing.T) {
	db := newTestStorage(t)
	s := New(db, nil)

	id := Hash{0x30}
	owner := Hash{0xAA}
	input := objectFromBytes(buildObjectWithParent(id, owner, 10, parentKindKeyRoot, []byte("old")))

	tx := buildCreatingTx(Hash{0x01}, 0, nil)
	output := buildUpdatedParentsOutput([]updatedSpec{{id: id, owner: owner, parentKind: parentKindKeyRoot}})
	out := types.GetRootAsPodExecuteOutput(output, 0)

	if err := s.validateOutput(out, tx, Hash{}, []*types.Object{input}); err != nil {
		t.Errorf("expected success for a content-only update, got: %v", err)
	}
}

// --- pod-output deletion carve-out ---

// TestValidateOutput_MergeSingletonDeletionSucceeds verifies that a pod output
// deleting a source object is allowed when every mutable reference is a
// singleton, the merge carve-out: all validators execute the transaction.
func TestValidateOutput_MergeSingletonDeletionSucceeds(t *testing.T) {
	db := newTestStorage(t)
	s := New(db, nil)

	dst := Hash{0x40}
	src := Hash{0x41}
	inputs := []*types.Object{
		objectFromBytes(buildObjectWithParent(dst, Hash{0x01}, 0, parentKindKeyRoot, []byte("dst"))),
		objectFromBytes(buildObjectWithParent(src, Hash{0x01}, 0, parentKindKeyRoot, []byte("src"))),
	}

	tx := buildCreatingTx(Hash{0x01}, 0, []refSpec{{id: dst, hasID: true}, {id: src, hasID: true}})
	output := buildDeletedOutput([]Hash{src})
	out := types.GetRootAsPodExecuteOutput(output, 0)

	if err := s.validateOutput(out, tx, Hash{}, inputs); err != nil {
		t.Errorf("expected success deleting a singleton source in a merge, got: %v", err)
	}
}

// TestValidateOutput_ShardedDeletionInNonCreatingTxReverts verifies that a pod
// output deleting a sharded (replicated) object in a transaction that neither
// creates objects nor references only singletons is rejected.
func TestValidateOutput_ShardedDeletionInNonCreatingTxReverts(t *testing.T) {
	db := newTestStorage(t)
	s := New(db, nil)

	shardedID := Hash{0x50}
	inputs := []*types.Object{
		objectFromBytes(buildObjectWithParent(shardedID, Hash{0x01}, 10, parentKindKeyRoot, []byte("sharded"))),
	}

	tx := buildCreatingTx(Hash{0x01}, 0, []refSpec{{id: shardedID, hasID: true}})
	output := buildDeletedOutput([]Hash{shardedID})
	out := types.GetRootAsPodExecuteOutput(output, 0)

	if err := s.validateOutput(out, tx, Hash{}, inputs); err == nil {
		t.Error("expected error deleting a sharded object in a non-globally-executed tx")
	}
}

// TestValidateOutput_ShardedDeletionInCreatingTxSucceeds verifies that a pod
// output may delete even a sharded object when the transaction creates objects,
// since every validator executes a creating transaction.
func TestValidateOutput_ShardedDeletionInCreatingTxSucceeds(t *testing.T) {
	db := newTestStorage(t)
	s := New(db, nil)

	sender := Hash{0x01}
	shardedID := Hash{0x50}
	inputs := []*types.Object{
		objectFromBytes(buildObjectWithParent(shardedID, sender, 10, parentKindKeyRoot, []byte("sharded"))),
	}

	tx := buildCreatingTx(sender, 1, []refSpec{{id: shardedID, hasID: true}})
	output := buildCreatedAndDeletedOutput(
		[]createdSpec{{owner: sender, parentKind: parentKindKeyRoot}},
		[]Hash{shardedID},
	)
	out := types.GetRootAsPodExecuteOutput(output, 0)

	if err := s.validateOutput(out, tx, Hash{}, inputs); err != nil {
		t.Errorf("expected success deleting in a creating tx, got: %v", err)
	}
}

// --- helpers ---

// createdSpec describes a created object's declared parent for a test output.
type createdSpec struct {
	owner       Hash   // owner holds the parent bytes (a key or an object ID)
	parentKind  byte   // parentKind selects how owner is read
	replication uint16 // replication is the created object's holder count
}

// updatedSpec describes an updated object for a test output.
type updatedSpec struct {
	id         Hash // id identifies the object being updated
	owner      Hash // owner holds the parent bytes returned by the pod
	parentKind byte // parentKind is the parent kind returned by the pod
}

// refSpec describes a mutable reference: either a direct ID or a domain name.
type refSpec struct {
	id     Hash   // id is the referenced object's ID (when hasID)
	hasID  bool   // hasID marks a direct-ID reference
	domain string // domain is the domain name (when not a direct-ID reference)
}

// objectFromBytes parses serialized object bytes into an Object accessor.
func objectFromBytes(data []byte) *types.Object {
	return types.GetRootAsObject(data, 0)
}

// buildObjectWithParent serializes an Object carrying an explicit owner,
// replication, and parent_kind.
func buildObjectWithParent(id, owner Hash, replication uint16, parentKind byte, content []byte) []byte {
	builder := flatbuffers.NewBuilder(256)

	idVec := builder.CreateByteVector(id[:])
	ownerVec := builder.CreateByteVector(owner[:])
	contentVec := builder.CreateByteVector(content)

	types.ObjectStart(builder)
	types.ObjectAddId(builder, idVec)
	types.ObjectAddVersion(builder, 1)
	types.ObjectAddOwner(builder, ownerVec)
	types.ObjectAddReplication(builder, replication)
	types.ObjectAddContent(builder, contentVec)
	types.ObjectAddParentKind(builder, parentKind)
	objOffset := types.ObjectEnd(builder)

	builder.Finish(objOffset)

	return builder.FinishedBytes()
}

// createdObjectOffset builds one created Object table in the builder.
func createdObjectOffset(builder *flatbuffers.Builder, spec createdSpec) flatbuffers.UOffsetT {
	idVec := builder.CreateByteVector(make([]byte, 32))
	ownerVec := builder.CreateByteVector(spec.owner[:])
	contentVec := builder.CreateByteVector([]byte("created"))

	types.ObjectStart(builder)
	types.ObjectAddId(builder, idVec)
	types.ObjectAddVersion(builder, 0)
	types.ObjectAddOwner(builder, ownerVec)
	types.ObjectAddReplication(builder, spec.replication)
	types.ObjectAddContent(builder, contentVec)
	types.ObjectAddParentKind(builder, spec.parentKind)

	return types.ObjectEnd(builder)
}

// updatedObjectOffset builds one updated Object table in the builder.
func updatedObjectOffset(builder *flatbuffers.Builder, spec updatedSpec) flatbuffers.UOffsetT {
	idVec := builder.CreateByteVector(spec.id[:])
	ownerVec := builder.CreateByteVector(spec.owner[:])
	contentVec := builder.CreateByteVector([]byte("updated"))

	types.ObjectStart(builder)
	types.ObjectAddId(builder, idVec)
	types.ObjectAddVersion(builder, 1)
	types.ObjectAddOwner(builder, ownerVec)
	types.ObjectAddReplication(builder, 10)
	types.ObjectAddContent(builder, contentVec)
	types.ObjectAddParentKind(builder, spec.parentKind)

	return types.ObjectEnd(builder)
}

// buildCreatedParentsOutput serializes a PodExecuteOutput with the given created
// objects and nothing else.
func buildCreatedParentsOutput(specs []createdSpec) []byte {
	builder := flatbuffers.NewBuilder(1024)

	offsets := make([]flatbuffers.UOffsetT, len(specs))
	for i := len(specs) - 1; i >= 0; i-- {
		offsets[i] = createdObjectOffset(builder, specs[i])
	}

	types.PodExecuteOutputStartCreatedObjectsVector(builder, len(specs))
	for i := len(offsets) - 1; i >= 0; i-- {
		builder.PrependUOffsetT(offsets[i])
	}
	createdVec := builder.EndVector(len(specs))

	return finishOutput(builder, createdVec, 0, 0)
}

// buildUpdatedParentsOutput serializes a PodExecuteOutput with the given updated
// objects and nothing else.
func buildUpdatedParentsOutput(specs []updatedSpec) []byte {
	builder := flatbuffers.NewBuilder(1024)

	offsets := make([]flatbuffers.UOffsetT, len(specs))
	for i := len(specs) - 1; i >= 0; i-- {
		offsets[i] = updatedObjectOffset(builder, specs[i])
	}

	types.PodExecuteOutputStartUpdatedObjectsVector(builder, len(specs))
	for i := len(offsets) - 1; i >= 0; i-- {
		builder.PrependUOffsetT(offsets[i])
	}
	updatedVec := builder.EndVector(len(specs))

	return finishOutput(builder, 0, updatedVec, 0)
}

// buildDeletedOutput serializes a PodExecuteOutput carrying only the pod-output
// deleted_objects field (concatenated 32-byte IDs).
func buildDeletedOutput(ids []Hash) []byte {
	builder := flatbuffers.NewBuilder(512)
	deletedVec := builder.CreateByteVector(concatIDs(ids))

	return finishOutput(builder, 0, 0, deletedVec)
}

// buildCreatedAndDeletedOutput serializes a PodExecuteOutput carrying both
// created objects and pod-output deleted_objects.
func buildCreatedAndDeletedOutput(specs []createdSpec, ids []Hash) []byte {
	builder := flatbuffers.NewBuilder(1024)

	offsets := make([]flatbuffers.UOffsetT, len(specs))
	for i := len(specs) - 1; i >= 0; i-- {
		offsets[i] = createdObjectOffset(builder, specs[i])
	}

	types.PodExecuteOutputStartCreatedObjectsVector(builder, len(specs))
	for i := len(offsets) - 1; i >= 0; i-- {
		builder.PrependUOffsetT(offsets[i])
	}
	createdVec := builder.EndVector(len(specs))

	deletedVec := builder.CreateByteVector(concatIDs(ids))

	return finishOutput(builder, createdVec, 0, deletedVec)
}

// finishOutput assembles a PodExecuteOutput from already-built vectors. A zero
// offset means the corresponding field is left absent.
func finishOutput(builder *flatbuffers.Builder, created, updated, deleted flatbuffers.UOffsetT) []byte {
	types.PodExecuteOutputStart(builder)
	if created != 0 {
		types.PodExecuteOutputAddCreatedObjects(builder, created)
	}
	if updated != 0 {
		types.PodExecuteOutputAddUpdatedObjects(builder, updated)
	}
	if deleted != 0 {
		types.PodExecuteOutputAddDeletedObjects(builder, deleted)
	}
	outputOffset := types.PodExecuteOutputEnd(builder)

	builder.Finish(outputOffset)

	return builder.FinishedBytes()
}

// concatIDs flattens a slice of hashes into concatenated 32-byte IDs.
func concatIDs(ids []Hash) []byte {
	out := make([]byte, 0, len(ids)*32)
	for _, id := range ids {
		out = append(out, id[:]...)
	}

	return out
}

// buildCreatingTx builds a Transaction with an explicit sender, a
// created_objects_replication vector of maxCreate entries, and the given mutable
// references (direct-ID or domain).
func buildCreatingTx(sender Hash, maxCreate uint16, refs []refSpec) *types.Transaction {
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

	var corVec flatbuffers.UOffsetT
	if maxCreate > 0 {
		types.TransactionStartCreatedObjectsReplicationVector(builder, int(maxCreate))
		for i := uint16(0); i < maxCreate; i++ {
			builder.PrependUint16(0)
		}
		corVec = builder.EndVector(int(maxCreate))
	}

	senderVec := builder.CreateByteVector(sender[:])
	podVec := builder.CreateByteVector(make([]byte, 32))

	types.TransactionStart(builder)
	types.TransactionAddSender(builder, senderVec)
	types.TransactionAddPod(builder, podVec)
	types.TransactionAddMutableRefs(builder, mutRefsVec)
	if corVec != 0 {
		types.TransactionAddCreatedObjectsReplication(builder, corVec)
	}
	txOffset := types.TransactionEnd(builder)

	builder.Finish(txOffset)

	return types.GetRootAsTransaction(builder.FinishedBytes(), 0)
}

// mutableRefOffset builds one ObjectRef table from a refSpec.
func mutableRefOffset(builder *flatbuffers.Builder, ref refSpec) flatbuffers.UOffsetT {
	var idVec, domainOff flatbuffers.UOffsetT
	if ref.hasID {
		idVec = builder.CreateByteVector(ref.id[:])
	}
	if ref.domain != "" {
		domainOff = builder.CreateString(ref.domain)
	}

	types.ObjectRefStart(builder)
	if idVec != 0 {
		types.ObjectRefAddId(builder, idVec)
	}
	if domainOff != 0 {
		types.ObjectRefAddDomain(builder, domainOff)
	}

	return types.ObjectRefEnd(builder)
}
