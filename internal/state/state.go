package state

import (
	"encoding/binary"
	"fmt"

	flatbuffers "github.com/google/flatbuffers/go"
	"github.com/zeebo/blake3"

	"BluePods/internal/genesis"
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
	objects         *objectStore                                          // objects is the object storage
	domains         *domainStore                                          // domains stores domain name → ObjectID mappings
	pods            *podvm.Pool                                           // pods is the WASM runtime pool
	isHolder        func(objectID [32]byte, replication uint16) bool      // isHolder checks if this node stores an object
	onObjectCreated func(id [32]byte, version uint64, replication uint16) // onObjectCreated is called when a new object is created
}

// New creates a new State with the given storage and podvm pool.
func New(db *storage.Storage, pods *podvm.Pool) *State {
	return &State{
		objects: newObjectStore(db),
		domains: newDomainStore(db),
		pods:    pods,
	}
}

// ResolveDomain resolves a domain name to its ObjectID.
// Returns the ObjectID and true if found, zero hash and false otherwise.
func (s *State) ResolveDomain(name string) ([32]byte, bool) {
	return s.domains.get(name)
}

// ExportDomains returns all domain entries for snapshot serialization.
func (s *State) ExportDomains() []DomainEntry {
	return s.domains.export()
}

// ImportDomains loads domain entries from snapshot data.
func (s *State) ImportDomains(entries []DomainEntry) {
	s.domains.importBatch(entries)
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

	if err := s.processOutput(output, txHash, tx); err != nil {
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

	var ref types.ObjectRef
	for i := 0; i < tx.MutableRefsLength(); i++ {
		if !tx.MutableRefs(&ref, i) {
			continue
		}

		idBytes := ref.IdBytes()
		if len(idBytes) != 32 {
			continue
		}

		var id Hash
		copy(id[:], idBytes)

		if present[id] {
			continue
		}

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

	return genesis.RebuildTxInBuilder(builder, tx)
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

// processOutput validates and applies PodExecuteOutput to the state.
// Validates creation limits and domain collisions before applying any mutations.
// If any validation fails, no state changes are made (rollback semantics).
func (s *State) processOutput(outputData []byte, txHash [32]byte, tx *types.Transaction) error {
	if len(outputData) == 0 {
		return nil
	}

	output := types.GetRootAsPodExecuteOutput(outputData, 0)

	for i := 0; i < output.LogsLength(); i++ {
		logger.Debug("pod log", "msg", string(output.Logs(i)))
	}

	if errCode := output.Error(); errCode != 0 {
		return fmt.Errorf("pod execution error: %d", errCode)
	}

	if err := s.validateOutput(output, tx); err != nil {
		return fmt.Errorf("output validation failed:\n%w", err)
	}

	s.applyUpdatedObjects(output)
	s.applyCreatedObjects(output, txHash)
	s.applyDeletedObjects(output)
	s.applyRegisteredDomains(output, txHash)

	return nil
}

// validateOutput checks creation limits and domain collisions.
// Returns error if any limit is exceeded or a domain already exists.
func (s *State) validateOutput(output *types.PodExecuteOutput, tx *types.Transaction) error {
	createdCount := output.CreatedObjectsLength()
	if createdCount > int(tx.MaxCreateObjects()) {
		return fmt.Errorf("created %d objects, max allowed %d", createdCount, tx.MaxCreateObjects())
	}

	domainCount := output.RegisteredDomainsLength()
	if domainCount > int(tx.MaxCreateDomains()) {
		return fmt.Errorf("registered %d domains, max allowed %d", domainCount, tx.MaxCreateDomains())
	}

	seen := make(map[string]bool, domainCount)
	var dom types.RegisteredDomain

	for i := 0; i < domainCount; i++ {
		if !output.RegisteredDomains(&dom, i) {
			continue
		}

		name := string(dom.Name())

		if len(name) == 0 {
			return fmt.Errorf("empty domain name at index %d", i)
		}

		if len(name) > 253 {
			return fmt.Errorf("domain name too long: %d bytes (max 253)", len(name))
		}

		if seen[name] {
			return fmt.Errorf("duplicate domain name in output: %q", name)
		}
		seen[name] = true

		if s.domains.exists(name) {
			return fmt.Errorf("domain collision: %q already registered", name)
		}
	}

	return nil
}

// applyRegisteredDomains stores domain name → ObjectID mappings from pod output.
// Each RegisteredDomain references either an object_index (into created objects) or a direct object_id.
func (s *State) applyRegisteredDomains(output *types.PodExecuteOutput, txHash [32]byte) {
	var dom types.RegisteredDomain

	for i := 0; i < output.RegisteredDomainsLength(); i++ {
		if !output.RegisteredDomains(&dom, i) {
			continue
		}

		name := string(dom.Name())
		objectID := s.resolveDomainObjectID(&dom, txHash)
		s.domains.set(name, objectID)
	}
}

// resolveDomainObjectID determines the ObjectID for a RegisteredDomain entry.
// Prefers direct object_id if present (32 bytes), falls back to object_index.
// object_id is checked first because FlatBuffers defaults object_index to 0,
// which would silently use the first created object instead of the direct ID.
func (s *State) resolveDomainObjectID(dom *types.RegisteredDomain, txHash [32]byte) Hash {
	if idBytes := dom.ObjectIdBytes(); len(idBytes) == 32 {
		var id Hash
		copy(id[:], idBytes)
		return id
	}

	return computeObjectID(txHash, uint32(dom.ObjectIndex()))
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

	for i := 0; i+idSize <= len(data); i += idSize {
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
	var ref types.ObjectRef

	for i := 0; i < tx.MutableRefsLength(); i++ {
		if !tx.MutableRefs(&ref, i) {
			continue
		}

		idBytes := ref.IdBytes()
		if len(idBytes) != 32 {
			continue
		}

		var id Hash
		copy(id[:], idBytes)

		expectedVersion := ref.Version()
		targetVersion := expectedVersion + 1

		objData := s.objects.get(id)
		if objData == nil {
			continue
		}

		obj := types.GetRootAsObject(objData, 0)
		if obj.Version() == targetVersion {
			continue
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
