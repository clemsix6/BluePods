package state

import (
	"fmt"

	flatbuffers "github.com/google/flatbuffers/go"

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
	objects *objectStore
	pods    *podvm.Pool
}

// New creates a new State with the given storage and podvm pool.
func New(db *storage.Storage, pods *podvm.Pool) *State {
	return &State{
		objects: newObjectStore(db),
		pods:    pods,
	}
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

	input, err := s.serializeInput(tx, objects)
	if err != nil {
		return fmt.Errorf("build input:\n%w", err)
	}

	podID := extractPodID(tx)
	output, _, err := s.pods.Execute(podID, input, defaultGasLimit)
	if err != nil {
		return fmt.Errorf("execute pod:\n%w", err)
	}

	if err := s.processOutput(output); err != nil {
		return fmt.Errorf("process output:\n%w", err)
	}

	return nil
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

// GetObject retrieves an object by ID.
func (s *State) GetObject(id Hash) []byte {
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
func (s *State) processOutput(outputData []byte) error {
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
	s.applyCreatedObjects(output)
	s.applyDeletedObjects(output)

	return nil
}

// applyUpdatedObjects stores updated objects.
func (s *State) applyUpdatedObjects(output *types.PodExecuteOutput) {
	var obj types.Object

	for i := 0; i < output.UpdatedObjectsLength(); i++ {
		if !output.UpdatedObjects(&obj, i) {
			continue
		}

		data := obj.Table().Bytes
		id := extractObjectID(data)
		s.objects.set(id, data)
	}
}

// applyCreatedObjects stores newly created objects.
func (s *State) applyCreatedObjects(output *types.PodExecuteOutput) {
	var obj types.Object

	for i := 0; i < output.CreatedObjectsLength(); i++ {
		if !output.CreatedObjects(&obj, i) {
			continue
		}

		data := obj.Table().Bytes
		id := extractObjectID(data)
		s.objects.set(id, data)
	}
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

// extractPodID extracts the pod hash from a transaction.
func extractPodID(tx *types.Transaction) Hash {
	var id Hash
	if podBytes := tx.PodBytes(); len(podBytes) == 32 {
		copy(id[:], podBytes)
	}
	return id
}
