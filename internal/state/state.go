package state

import (
	"encoding/binary"
	"fmt"

	flatbuffers "github.com/google/flatbuffers/go"
	"github.com/zeebo/blake3"

	"BluePods/internal/logger"
	"BluePods/internal/podvm"
	"BluePods/internal/storage"
	"BluePods/internal/types"
)

const (
	// defaultGasLimit is the default gas limit for transaction execution.
	defaultGasLimit = 10_000_000
)

// State manages objects and transaction execution.
type State struct {
	objects        *objectStore
	pods           *podvm.Pool
	isHolder       func(objectID [32]byte, replication uint16) bool   // isHolder checks if this node stores an object
	onObjectCreated func(id [32]byte, version uint64, replication uint16) // onObjectCreated is called when a new object is created
}

// New creates a new State with the given storage and podvm pool.
func New(db *storage.Storage, pods *podvm.Pool) *State {
	return &State{
		objects: newObjectStore(db),
		pods:    pods,
	}
}

// SetIsHolder sets the holder check function for storage sharding.
// Objects where isHolder returns false will not be stored locally.
func (s *State) SetIsHolder(fn func(objectID [32]byte, replication uint16) bool) {
	s.isHolder = fn
}

// SetOnObjectCreated sets a callback that fires when a new object is created.
// Used by the consensus tracker to register created objects.
func (s *State) SetOnObjectCreated(fn func(id [32]byte, version uint64, replication uint16)) {
	s.onObjectCreated = fn
}

// Execute runs an attested transaction and updates the state.
// The atxData contains the AttestedTransaction with embedded objects.
// Returns error if execution fails.
func (s *State) Execute(atxData []byte) error {
	atx := types.GetRootAsAttestedTransaction(atxData, 0)
	tx := atx.Transaction(nil)
	if tx == nil {
		return fmt.Errorf("no transaction in attested transaction")
	}

	objects := s.extractObjects(atx)
	objects = s.resolveMutableObjects(tx, objects)

	input, err := s.serializeInput(tx, objects)
	if err != nil {
		return fmt.Errorf("build input:\n%w", err)
	}

	podID := extractPodID(tx)
	output, _, err := s.pods.Execute(podID, input, defaultGasLimit)
	if err != nil {
		return fmt.Errorf("execute pod:\n%w", err)
	}

	txHash := extractTxHash(tx)

	if err := s.processOutput(output, txHash); err != nil {
		return fmt.Errorf("process output:\n%w", err)
	}

	s.ensureMutableVersions(tx)

	return nil
}

// extractTxHash extracts the 32-byte hash from a Transaction.
func extractTxHash(tx *types.Transaction) [32]byte {
	var h [32]byte
	if b := tx.HashBytes(); len(b) == 32 {
		copy(h[:], b)
	}
	return h
}

// extractObjects extracts objects from an AttestedTransaction.
func (s *State) extractObjects(atx *types.AttestedTransaction) []*types.Object {
	count := atx.ObjectsLength()
	objects := make([]*types.Object, count)

	for i := 0; i < count; i++ {
		obj := &types.Object{}
		if atx.Objects(obj, i) {
			objects[i] = obj
		}
	}

	return objects
}

// resolveMutableObjects loads missing mutable objects from local state.
// Singletons (replication=0) are not included in the ATX body,
// so the node must resolve them from its own state store.
func (s *State) resolveMutableObjects(tx *types.Transaction, atxObjects []*types.Object) []*types.Object {
	present := make(map[Hash]bool, len(atxObjects))
	for _, obj := range atxObjects {
		if obj == nil {
			continue
		}

		var id Hash
		copy(id[:], obj.IdBytes())
		present[id] = true
	}

	result := append([]*types.Object{}, atxObjects...)

	data := tx.MutableObjectsBytes()
	for i := 0; i+40 <= len(data); i += 40 {
		var id Hash
		copy(id[:], data[i:i+32])

		if present[id] {
			continue
		}

		// Missing from ATX → load from local state (singleton)
		objData := s.objects.get(id)
		if objData == nil {
			continue
		}

		obj := types.GetRootAsObject(objData, 0)
		result = append(result, obj)
	}

	return result
}

// GetObject retrieves an object by ID.
func (s *State) GetObject(id [32]byte) []byte {
	return s.objects.get(id)
}

// SetObject stores an object (used for initialization).
func (s *State) SetObject(data []byte) {
	id := extractObjectID(data)
	s.objects.set(id, data)
}

// serializeInput builds the FlatBuffers PodExecuteInput.
// Must rebuild nested tables (Transaction, Objects) field by field.
func (s *State) serializeInput(tx *types.Transaction, objects []*types.Object) ([]byte, error) {
	builder := flatbuffers.NewBuilder(4096)

	// Rebuild Object tables
	objectOffsets := make([]flatbuffers.UOffsetT, 0, len(objects))
	for _, obj := range objects {
		if obj == nil {
			continue
		}
		offset := rebuildObject(builder, obj)
		objectOffsets = append(objectOffsets, offset)
	}

	// Build local objects vector
	types.PodExecuteInputStartLocalObjectsVector(builder, len(objectOffsets))
	for i := len(objectOffsets) - 1; i >= 0; i-- {
		builder.PrependUOffsetT(objectOffsets[i])
	}
	localObjectsVec := builder.EndVector(len(objectOffsets))

	// Build sender
	senderVec := builder.CreateByteVector(tx.SenderBytes())

	// Rebuild Transaction table (must rebuild, not copy raw bytes)
	txOffset := rebuildTransaction(builder, tx)

	// Build PodExecuteInput
	types.PodExecuteInputStart(builder)
	types.PodExecuteInputAddTransaction(builder, txOffset)
	types.PodExecuteInputAddSender(builder, senderVec)
	types.PodExecuteInputAddLocalObjects(builder, localObjectsVec)
	inputOffset := types.PodExecuteInputEnd(builder)

	builder.Finish(inputOffset)

	return builder.FinishedBytes(), nil
}

// rebuildTransaction rebuilds a Transaction table in the builder.
func rebuildTransaction(builder *flatbuffers.Builder, tx *types.Transaction) flatbuffers.UOffsetT {
	if tx == nil {
		types.TransactionStart(builder)
		return types.TransactionEnd(builder)
	}

	hashVec := builder.CreateByteVector(tx.HashBytes())
	readObjVec := builder.CreateByteVector(tx.ReadObjectsBytes())
	mutObjVec := builder.CreateByteVector(tx.MutableObjectsBytes())
	senderVec := builder.CreateByteVector(tx.SenderBytes())
	sigVec := builder.CreateByteVector(tx.SignatureBytes())
	podVec := builder.CreateByteVector(tx.PodBytes())
	funcNameOff := builder.CreateString(string(tx.FunctionName()))
	argsVec := builder.CreateByteVector(tx.ArgsBytes())

	types.TransactionStart(builder)
	types.TransactionAddHash(builder, hashVec)
	types.TransactionAddReadObjects(builder, readObjVec)
	types.TransactionAddMutableObjects(builder, mutObjVec)
	types.TransactionAddCreatesObjects(builder, tx.CreatesObjects())
	types.TransactionAddSender(builder, senderVec)
	types.TransactionAddSignature(builder, sigVec)
	types.TransactionAddPod(builder, podVec)
	types.TransactionAddFunctionName(builder, funcNameOff)
	types.TransactionAddArgs(builder, argsVec)

	return types.TransactionEnd(builder)
}

// rebuildObject rebuilds an Object table in the builder.
func rebuildObject(builder *flatbuffers.Builder, obj *types.Object) flatbuffers.UOffsetT {
	idVec := builder.CreateByteVector(obj.IdBytes())
	ownerVec := builder.CreateByteVector(obj.OwnerBytes())
	contentVec := builder.CreateByteVector(obj.ContentBytes())

	types.ObjectStart(builder)
	types.ObjectAddId(builder, idVec)
	types.ObjectAddVersion(builder, obj.Version())
	types.ObjectAddOwner(builder, ownerVec)
	types.ObjectAddReplication(builder, obj.Replication())
	types.ObjectAddContent(builder, contentVec)

	return types.ObjectEnd(builder)
}

// processOutput applies PodExecuteOutput to the state.
func (s *State) processOutput(outputData []byte, txHash [32]byte) error {
	if len(outputData) == 0 {
		return nil
	}

	output := types.GetRootAsPodExecuteOutput(outputData, 0)

	// Print pod logs
	for i := 0; i < output.LogsLength(); i++ {
		logger.Debug("pod log", "msg", string(output.Logs(i)))
	}

	if errCode := output.Error(); errCode != 0 {
		return fmt.Errorf("pod execution error: %d", errCode)
	}

	s.applyUpdatedObjects(output)
	s.applyCreatedObjects(output, txHash)
	s.applyDeletedObjects(output)

	return nil
}

// applyUpdatedObjects stores updated objects with incremented versions.
// The version must be incremented to stay consistent with the consensus version tracker,
// which increments versions for all mutable objects after each successful execution.
func (s *State) applyUpdatedObjects(output *types.PodExecuteOutput) {
	var obj types.Object

	for i := 0; i < output.UpdatedObjectsLength(); i++ {
		if !output.UpdatedObjects(&obj, i) {
			continue
		}

		data := rebuildObjectIncrementVersion(&obj)
		id := extractObjectID(data)
		s.objects.set(id, data)
	}
}

// rebuildObjectIncrementVersion rebuilds an Object as a standalone FlatBuffer with version+1.
func rebuildObjectIncrementVersion(obj *types.Object) []byte {
	builder := flatbuffers.NewBuilder(512)

	idVec := builder.CreateByteVector(obj.IdBytes())
	ownerVec := builder.CreateByteVector(obj.OwnerBytes())
	contentVec := builder.CreateByteVector(obj.ContentBytes())

	types.ObjectStart(builder)
	types.ObjectAddId(builder, idVec)
	types.ObjectAddVersion(builder, obj.Version()+1)
	types.ObjectAddOwner(builder, ownerVec)
	types.ObjectAddReplication(builder, obj.Replication())
	types.ObjectAddContent(builder, contentVec)
	offset := types.ObjectEnd(builder)

	builder.Finish(offset)

	return builder.FinishedBytes()
}

// applyCreatedObjects stores newly created objects with deterministic IDs.
// Each object ID is computed as blake3(txHash || index_u32_LE) per the spec.
// Storage sharding: only stores objects where this node is a holder.
// Notifies the tracker callback for each created object (all validators track all objects).
func (s *State) applyCreatedObjects(output *types.PodExecuteOutput, txHash [32]byte) {
	var obj types.Object

	for i := 0; i < output.CreatedObjectsLength(); i++ {
		if !output.CreatedObjects(&obj, i) {
			continue
		}

		id := computeObjectID(txHash, uint32(i))

		// Notify tracker (all validators track all objects regardless of holding)
		if s.onObjectCreated != nil {
			s.onObjectCreated(id, obj.Version(), obj.Replication())
		}

		// Storage sharding: skip objects this node doesn't hold
		if s.isHolder != nil && !s.isHolder(id, obj.Replication()) {
			continue
		}

		data := rebuildObjectWithID(id, &obj)
		s.objects.set(id, data)
	}
}

// computeObjectID generates a deterministic object ID: blake3(txHash || index_u32_LE).
func computeObjectID(txHash [32]byte, index uint32) Hash {
	var buf [36]byte
	copy(buf[:32], txHash[:])
	binary.LittleEndian.PutUint32(buf[32:], index)

	return blake3.Sum256(buf[:])
}

// rebuildObjectWithID rebuilds an Object FlatBuffers with a new ID.
func rebuildObjectWithID(id Hash, obj *types.Object) []byte {
	builder := flatbuffers.NewBuilder(512)
	offset := rebuildObjectCustomID(builder, id, obj)
	builder.Finish(offset)

	return builder.FinishedBytes()
}

// rebuildObjectCustomID rebuilds an Object table with a custom ID in the builder.
func rebuildObjectCustomID(builder *flatbuffers.Builder, id Hash, obj *types.Object) flatbuffers.UOffsetT {
	idVec := builder.CreateByteVector(id[:])
	ownerVec := builder.CreateByteVector(obj.OwnerBytes())
	contentVec := builder.CreateByteVector(obj.ContentBytes())

	types.ObjectStart(builder)
	types.ObjectAddId(builder, idVec)
	types.ObjectAddVersion(builder, obj.Version())
	types.ObjectAddOwner(builder, ownerVec)
	types.ObjectAddReplication(builder, obj.Replication())
	types.ObjectAddContent(builder, contentVec)

	return types.ObjectEnd(builder)
}

// applyDeletedObjects removes deleted objects.
func (s *State) applyDeletedObjects(output *types.PodExecuteOutput) {
	data := output.DeletedObjectsBytes()
	const idSize = 32

	for i := 0; i < len(data); i += idSize {
		var id Hash
		copy(id[:], data[i:i+idSize])
		s.objects.delete(id)
	}
}

// ensureMutableVersions guarantees that all objects declared in MutableObjects
// have their version incremented to expectedVersion+1 after execution.
// The consensus versionTracker increments ALL mutable objects, but the pod
// may not return all of them in UpdatedObjects. This closes the gap.
func (s *State) ensureMutableVersions(tx *types.Transaction) {
	data := tx.MutableObjectsBytes()

	for i := 0; i+40 <= len(data); i += 40 {
		var id Hash
		copy(id[:], data[i:i+32])

		expectedVersion := binary.LittleEndian.Uint64(data[i+32 : i+40])
		targetVersion := expectedVersion + 1

		objData := s.objects.get(id)
		if objData == nil {
			continue
		}

		obj := types.GetRootAsObject(objData, 0)
		if obj.Version() == targetVersion {
			continue // already up to date (pod returned it in UpdatedObjects)
		}

		updated := rebuildObjectWithVersion(obj, targetVersion)
		s.objects.set(id, updated)
	}
}

// rebuildObjectWithVersion rebuilds an Object as a standalone FlatBuffer with an explicit version.
func rebuildObjectWithVersion(obj *types.Object, version uint64) []byte {
	builder := flatbuffers.NewBuilder(512)

	idVec := builder.CreateByteVector(obj.IdBytes())
	ownerVec := builder.CreateByteVector(obj.OwnerBytes())
	contentVec := builder.CreateByteVector(obj.ContentBytes())

	types.ObjectStart(builder)
	types.ObjectAddId(builder, idVec)
	types.ObjectAddVersion(builder, version)
	types.ObjectAddOwner(builder, ownerVec)
	types.ObjectAddReplication(builder, obj.Replication())
	types.ObjectAddContent(builder, contentVec)
	offset := types.ObjectEnd(builder)

	builder.Finish(offset)

	return builder.FinishedBytes()
}

// extractPodID extracts the pod hash from a transaction.
func extractPodID(tx *types.Transaction) Hash {
	var id Hash
	if podBytes := tx.PodBytes(); len(podBytes) == 32 {
		copy(id[:], podBytes)
	}
	return id
}
