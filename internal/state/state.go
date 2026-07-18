package state

import (
	"encoding/binary"
	"fmt"
	"sync"

	flatbuffers "github.com/google/flatbuffers/go"
	"github.com/zeebo/blake3"

	"BluePods/internal/events"
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
	db              *storage.Storage                                                                                     // db is the underlying storage, retained for protocol-counter persistence
	objects         *objectStore                                                                                         // objects is the object storage
	domains         *domainStore                                                                                         // domains stores domain name → ObjectID mappings
	pods            *podvm.Pool                                                                                          // pods is the WASM runtime pool
	isHolder        func(objectID [32]byte, replication uint16) bool                                                     // isHolder checks if this node stores an object
	onObjectCreated func(id [32]byte, version uint64, replication uint16, fees uint64, parentKind byte, parent [32]byte) // onObjectCreated is called when a new object is created
	signObject      func(id [32]byte, content []byte, version uint64, replication uint16, owner []byte)                  // signObject eagerly attests a held object at the version actually persisted
	parentValidator func(kind byte, parent [32]byte, sender [32]byte, tx *types.Transaction) bool                        // parentValidator asks consensus whether sender controls a created object's declared object-parent

	// Fee system: storage deposits and refunds.
	storageFee       uint64     // storageFee is the per-object storage fee (0 = disabled)
	storageRefundBPS uint64     // storageRefundBPS is the refund ratio in basis points (9500 = 95%)
	totalValidators  int        // totalValidators is the fallback validator count when validatorCount is unset
	validatorCount   func() int // validatorCount returns the live validator count; set to the consensus set so the storage-deposit formula matches consensus

	// Supply accounting.
	supplyMu    sync.Mutex // supplyMu guards totalSupply and its persistence
	totalSupply uint64     // totalSupply is the protocol-maintained total token supply

	// Coin accounting: the sum of every coin's balance, maintained alongside
	// totalSupply at each protocol touchpoint that moves value into or out of
	// coins (coins carry no type tag, so this cannot be recomputed by scanning
	// objects).
	coinsTotalMu sync.Mutex // coinsTotalMu guards coinsTotal and its persistence
	coinsTotal   uint64     // coinsTotal is the protocol-maintained sum of coin balances
}

// New creates a new State with the given storage and podvm pool.
// The total-supply counter is loaded from storage so it survives a reopen.
func New(db *storage.Storage, pods *podvm.Pool) *State {
	s := &State{
		db:      db,
		objects: newObjectStore(db),
		domains: newDomainStore(db),
		pods:    pods,
	}

	s.loadSupply()
	s.loadCoinsTotal()

	return s
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

// ExportDomainsFrom returns all domain entries read from the given consistent
// storage view. A sync snapshot exports domains through the view captured at its
// commit cut, so a domain registered after the cut can never reference an object
// missing from the cut's exported state.
func (s *State) ExportDomainsFrom(snap *storage.Snapshot) []DomainEntry {
	return exportDomainEntries(snap)
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
// Used by the consensus tracker to register created objects, threading
// through the object's declared parent (kind + bytes, read from the object
// body's owner and parent_kind fields) so the tracker can maintain
// parent/child-count bookkeeping from the moment the object is created.
func (s *State) SetOnObjectCreated(fn func(id [32]byte, version uint64, replication uint16, fees uint64, parentKind byte, parent [32]byte)) {
	s.onObjectCreated = fn
}

// SetObjectSigner sets a callback that fires when a held, replicated object is
// persisted at a new version, so the node can eagerly produce and store its BLS
// attestation. The callback is invoked with the version actually written and the
// object's owner, which the attestation hash binds so the eager signature covers
// the same owner the verifier recomputes against at commit.
// State holds only the func, so it never imports the aggregation package.
func (s *State) SetObjectSigner(fn func(id [32]byte, content []byte, version uint64, replication uint16, owner []byte)) {
	s.signObject = fn
}

// SetParentValidator wires the consensus cascade walk that decides whether a
// transaction sender controls a created object's declared object-parent. State
// resolves the sender's own key and same-transaction parents locally; only the
// ObjectParent-under-an-existing-object case needs the global parent walk, which
// this callback answers. Holding only the func keeps state from importing
// consensus.
func (s *State) SetParentValidator(fn func(kind byte, parent [32]byte, sender [32]byte, tx *types.Transaction) bool) {
	s.parentValidator = fn
}

// SetStorageFees configures protocol-level storage deposits and refunds.
// When storageFee > 0, created objects get a storage deposit in their fees field.
// totalValidators is only a fallback; SetValidatorCount supplies the live count.
func (s *State) SetStorageFees(storageFee uint64, storageRefundBPS uint64, totalValidators int) {
	s.storageFee = storageFee
	s.storageRefundBPS = storageRefundBPS
	s.totalValidators = totalValidators
}

// SetValidatorCount wires a live validator-count source for the storage-deposit
// formula. It must be bound to the SAME validator set consensus reads (its Len),
// so the deposit stamped here always equals the storage fee debited at commit,
// across both validator-set growth and shrinkage.
func (s *State) SetValidatorCount(fn func() int) {
	s.validatorCount = fn
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

	if err := s.processOutput(output, txHash, tx, objects); err != nil {
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

// DeleteObject removes an object by ID. Used by consensus to destroy a
// delegation position on undelegate (a protocol mutation, not pod execution).
func (s *State) DeleteObject(id [32]byte) {
	s.objects.delete(id)
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
	types.ObjectAddFees(builder, obj.Fees())

	return types.ObjectEnd(builder)
}

// processOutput validates and applies PodExecuteOutput to the state.
// Validates creation limits, domain collisions, the creation-permission rule,
// parent immutability, and the pod-output deletion carve-out before applying any
// mutations. The inputs are the attested object copies the pod ran against, used
// as the local comparison basis for the parent and deletion checks. If any
// validation fails, no state changes are made (rollback semantics).
func (s *State) processOutput(outputData []byte, txHash [32]byte, tx *types.Transaction, inputs []*types.Object) error {
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

	if err := s.validateOutput(output, tx, txHash, inputs); err != nil {
		return fmt.Errorf("output validation failed:\n%w", err)
	}

	s.applyUpdatedObjects(output, txHash)
	s.applyCreatedObjects(output, txHash, txLocksDeposits(tx))
	s.applyDeletedObjects(tx)
	s.applyRegisteredDomains(output, txHash)

	return nil
}

// applyRegisteredDomains stores domain name → ObjectID mappings from pod output.
// Each RegisteredDomain references either an object_index (into created objects) or a direct object_id.
// Emits DomainUpdated when the name was already bound (rebound to a possibly
// different object) and DomainRegistered for a first-time binding, so the event
// stream reports which case actually happened.
func (s *State) applyRegisteredDomains(output *types.PodExecuteOutput, txHash [32]byte) {
	var dom types.RegisteredDomain

	for i := 0; i < output.RegisteredDomainsLength(); i++ {
		if !output.RegisteredDomains(&dom, i) {
			continue
		}

		name := string(dom.Name())
		objectID := s.resolveDomainObjectID(&dom, txHash)
		existed := s.domains.exists(name)

		s.domains.set(name, objectID)

		if existed {
			events.DomainUpdated(name, objectID, txHash)
		} else {
			events.DomainRegistered(name, objectID, txHash)
		}
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
func (s *State) applyUpdatedObjects(output *types.PodExecuteOutput, txHash [32]byte) {
	var obj types.Object

	for i := 0; i < output.UpdatedObjectsLength(); i++ {
		if !output.UpdatedObjects(&obj, i) {
			continue
		}

		data := rebuildObjectIncrementVersion(&obj)
		id := extractObjectID(data)
		newVersion := obj.Version() + 1
		s.objects.set(id, data)

		events.ObjectUpdated(id, txHash, newVersion)

		// Eagerly sign the version actually persisted (old version + 1). The owner
		// is bound into the attestation hash, so pass the persisted object's owner.
		// There is no holder filter on updates, so guard it explicitly.
		s.eagerlySign(id, obj.ContentBytes(), newVersion, obj.Replication(), obj.OwnerBytes())
	}
}

// eagerlySign invokes the object-signer callback for a held, replicated object.
// Singletons (replication 0) are never attested, and objects this node does not
// hold are skipped, so the work stays bounded by the held-object count. The owner
// is threaded through because the attestation hash binds it.
func (s *State) eagerlySign(id Hash, content []byte, version uint64, replication uint16, owner []byte) {
	if s.signObject == nil || replication == 0 {
		return
	}

	if s.isHolder != nil && !s.isHolder(id, replication) {
		return
	}

	s.signObject(id, content, version, replication, owner)
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
	types.ObjectAddFees(builder, obj.Fees())
	offset := types.ObjectEnd(builder)

	builder.Finish(offset)

	return builder.FinishedBytes()
}

// applyCreatedObjects stores newly created objects with deterministic IDs.
// Each object ID is computed as blake3(txHash || index_u32_LE) per the spec.
// Storage sharding: only stores objects where this node is a holder.
// Notifies the tracker callback for each created object (all validators track all objects).
// Protocol sets the storage deposit in object.fees, but only when locksDeposit is
// true: a fee-exempt transaction paid no coin, so it locks a zero deposit.
func (s *State) applyCreatedObjects(output *types.PodExecuteOutput, txHash [32]byte, locksDeposit bool) {
	var obj types.Object

	for i := 0; i < output.CreatedObjectsLength(); i++ {
		if !output.CreatedObjects(&obj, i) {
			continue
		}

		id := computeObjectID(txHash, uint32(i))

		// Storage deposit (protocol-level, overrides pod value). A deposit is
		// locked only against a coin that was actually debited: a fee-exempt
		// transaction (locksDeposit false) debited none, so it locks zero.
		var fees uint64
		if locksDeposit {
			fees = s.computeStorageDeposit(obj.Replication())
		}

		// owner doubles as the parent reference (parent_kind selects how to read
		// it: an Ed25519 key under KeyRoot, another object's ID under
		// ObjectParent).
		var owner Hash
		if ownerBytes := obj.OwnerBytes(); len(ownerBytes) == 32 {
			copy(owner[:], ownerBytes)
		}

		// Notify tracker (all validators track all objects regardless of holding)
		if s.onObjectCreated != nil {
			s.onObjectCreated(id, obj.Version(), obj.Replication(), fees, obj.ParentKind(), owner)
		}

		// state.object.created and fees.deposit.locked describe the tracker-level
		// mutation and protocol deposit, not the local storage write below, so they
		// fire for every created object regardless of whether this node holds it
		// (all validators track all objects).
		events.ObjectCreated(id, txHash, obj.Version(), obj.Replication(), owner)
		if fees > 0 {
			events.DepositLocked(id, fees)
		}

		// Storage sharding: skip objects this node doesn't hold
		if s.isHolder != nil && !s.isHolder(id, obj.Replication()) {
			continue
		}

		data := rebuildObjectWithIDAndFees(id, &obj, fees)
		s.objects.set(id, data)

		// Eagerly sign the created object at its initial version, binding its owner
		// into the attestation hash.
		s.eagerlySign(id, obj.ContentBytes(), obj.Version(), obj.Replication(), obj.OwnerBytes())
	}
}

// txLocksDeposits reports whether the objects a committed transaction creates
// should lock a storage deposit. A deposit is locked only when a coin was
// actually debited to fund it: the consensus fee path debits a transaction's
// storage fee from its gas coin and stamps the equal deposit on the created
// object. A transaction that references no gas coin is fee-exempt (the validator
// register/deregister path deductFees waives), debits no coin, and so must lock
// a zero deposit. Otherwise the supply identity
// coins_total + total_bonded + deposits + fees_in_flight == total_supply would
// inflate by one unpaid deposit per such transaction, permanently, on every
// node. The decision reads only the committed transaction's gas_coin field, so
// every validator reaches it identically.
func txLocksDeposits(tx *types.Transaction) bool {
	return tx != nil && len(tx.GasCoinBytes()) == 32
}

// computeObjectID generates a deterministic object ID: blake3(txHash || index_u32_LE).
func computeObjectID(txHash [32]byte, index uint32) Hash {
	var buf [36]byte
	copy(buf[:32], txHash[:])
	binary.LittleEndian.PutUint32(buf[32:], index)

	return blake3.Sum256(buf[:])
}

// rebuildObjectWithIDAndFees rebuilds an Object with a custom ID and overridden fees.
func rebuildObjectWithIDAndFees(id Hash, obj *types.Object, fees uint64) []byte {
	builder := flatbuffers.NewBuilder(512)

	idVec := builder.CreateByteVector(id[:])
	ownerVec := builder.CreateByteVector(obj.OwnerBytes())
	contentVec := builder.CreateByteVector(obj.ContentBytes())

	types.ObjectStart(builder)
	types.ObjectAddId(builder, idVec)
	types.ObjectAddVersion(builder, obj.Version())
	types.ObjectAddOwner(builder, ownerVec)
	types.ObjectAddReplication(builder, obj.Replication())
	types.ObjectAddContent(builder, contentVec)
	types.ObjectAddFees(builder, fees)

	offset := types.ObjectEnd(builder)
	builder.Finish(offset)

	return builder.FinishedBytes()
}

// applyDeletedObjects removes the stored content of every object the transaction
// declares deleted (tx.deleted_objects). It runs in the execution path, so only a
// holder reaches it: a holder removes the object it stores, guarded by the
// owner-only check that reads content only a holder has. The deposit accounting
// (deposit release, refund, burn) does NOT run here — it runs once per deletion in
// the commit loop on every node from the same declared set, so a holder never
// accounts a deletion twice and a non-holder still accounts it. Content removal
// reads the same network-uniform declared set the accounting uses, not the pod
// output, so on every holder the two agree.
func (s *State) applyDeletedObjects(tx *types.Transaction) {
	data := tx.DeletedObjectsBytes()
	const idSize = 32

	sender := extractSender(tx)

	for i := 0; i+idSize <= len(data); i += idSize {
		var id Hash
		copy(id[:], data[i:i+idSize])

		s.deleteHeldContent(id, sender)
	}
}

// deleteHeldContent removes a deleted object's stored content, but only for a
// holder that owns it. A non-holder never stored the object, so there is nothing
// to remove. The owner-only guard is a defense-in-depth check on content only a
// holder has; in production the deletion set is already owner-authorized, so it
// never blocks a legitimate deletion.
func (s *State) deleteHeldContent(id, sender Hash) {
	objData := s.objects.get(id)
	if objData == nil {
		return
	}

	obj := types.GetRootAsObject(objData, 0)

	var owner Hash
	if ownerBytes := obj.OwnerBytes(); len(ownerBytes) == 32 {
		copy(owner[:], ownerBytes)
	}

	if owner != sender {
		logger.Warn("non-owner deletion blocked",
			"id_prefix", id[:4],
			"owner_prefix", owner[:4],
			"sender_prefix", sender[:4],
		)
		return
	}

	s.objects.delete(id)
}

// extractSender reads the 32-byte sender from a transaction, or the zero hash
// when it is absent or malformed.
func extractSender(tx *types.Transaction) Hash {
	var sender Hash
	if b := tx.SenderBytes(); len(b) == 32 {
		copy(sender[:], b)
	}

	return sender
}

// computeStorageDeposit calculates the storage deposit for a new object.
// It reads the live validator count (so the deposit matches the storage fee
// debited at commit), falling back to the init-time count if none is wired.
func (s *State) computeStorageDeposit(replication uint16) uint64 {
	total := s.totalValidators
	if s.validatorCount != nil {
		total = s.validatorCount()
	}

	if s.storageFee == 0 || total == 0 {
		return 0
	}

	effRep := int(replication)
	if replication == 0 {
		effRep = total
	}

	return uint64(effRep) * s.storageFee / uint64(total)
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
	types.ObjectAddFees(builder, obj.Fees())
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
