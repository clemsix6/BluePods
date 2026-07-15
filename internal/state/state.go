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
	db              *storage.Storage                                                      // db is the underlying storage, retained for protocol-counter persistence
	objects         *objectStore                                                          // objects is the object storage
	domains         *domainStore                                                          // domains stores domain name → ObjectID mappings
	pods            *podvm.Pool                                                           // pods is the WASM runtime pool
	isHolder        func(objectID [32]byte, replication uint16) bool                      // isHolder checks if this node stores an object
	onObjectCreated func(id [32]byte, version uint64, replication uint16, fees uint64)    // onObjectCreated is called when a new object is created
	signObject      func(id [32]byte, content []byte, version uint64, replication uint16) // signObject eagerly attests a held object at the version actually persisted

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
// Used by the consensus tracker to register created objects.
func (s *State) SetOnObjectCreated(fn func(id [32]byte, version uint64, replication uint16, fees uint64)) {
	s.onObjectCreated = fn
}

// SetObjectSigner sets a callback that fires when a held, replicated object is
// persisted at a new version, so the node can eagerly produce and store its BLS
// attestation. The callback is invoked with the version actually written.
// State holds only the func, so it never imports the aggregation package.
func (s *State) SetObjectSigner(fn func(id [32]byte, content []byte, version uint64, replication uint16)) {
	s.signObject = fn
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

	s.applyUpdatedObjects(output, txHash)
	s.applyCreatedObjects(output, txHash)
	s.applyDeletedObjects(output, tx)
	s.applyRegisteredDomains(output, txHash)

	return nil
}

// validateOutput checks creation limits and domain collisions.
// Returns error if any limit is exceeded or a domain already exists.
func (s *State) validateOutput(output *types.PodExecuteOutput, tx *types.Transaction) error {
	createdCount := output.CreatedObjectsLength()
	maxCreate := tx.CreatedObjectsReplicationLength()
	if createdCount > maxCreate {
		return fmt.Errorf("created %d objects, max allowed %d", createdCount, maxCreate)
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

		// Eagerly sign the version actually persisted (old version + 1).
		// There is no holder filter on updates, so guard it explicitly.
		s.eagerlySign(id, obj.ContentBytes(), newVersion, obj.Replication())
	}
}

// eagerlySign invokes the object-signer callback for a held, replicated object.
// Singletons (replication 0) are never attested, and objects this node does not
// hold are skipped, so the work stays bounded by the held-object count.
func (s *State) eagerlySign(id Hash, content []byte, version uint64, replication uint16) {
	if s.signObject == nil || replication == 0 {
		return
	}

	if s.isHolder != nil && !s.isHolder(id, replication) {
		return
	}

	s.signObject(id, content, version, replication)
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
// Protocol sets the storage deposit in object.fees.
func (s *State) applyCreatedObjects(output *types.PodExecuteOutput, txHash [32]byte) {
	var obj types.Object

	for i := 0; i < output.CreatedObjectsLength(); i++ {
		if !output.CreatedObjects(&obj, i) {
			continue
		}

		id := computeObjectID(txHash, uint32(i))

		// Compute storage deposit (protocol-level, overrides pod value)
		fees := s.computeStorageDeposit(obj.Replication())

		// Notify tracker (all validators track all objects regardless of holding)
		if s.onObjectCreated != nil {
			s.onObjectCreated(id, obj.Version(), obj.Replication(), fees)
		}

		// state.object.created and fees.deposit.locked describe the tracker-level
		// mutation and protocol deposit, not the local storage write below, so they
		// fire for every created object regardless of whether this node holds it
		// (all validators track all objects).
		var owner Hash
		if ownerBytes := obj.OwnerBytes(); len(ownerBytes) == 32 {
			copy(owner[:], ownerBytes)
		}
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

		// Eagerly sign the created object at its initial version.
		s.eagerlySign(id, obj.ContentBytes(), obj.Version(), obj.Replication())
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
	types.ObjectAddFees(builder, obj.Fees())

	return types.ObjectEnd(builder)
}

// applyDeletedObjects removes deleted objects with ownership verification.
// Protocol-level ownership check: only the object owner can delete.
// Computes refund (95% of fees) and credits the sender's gas_coin.
//
// Determinism limit: the deletion supply/refund accounting here (SubSupply for
// the burn, creditGasCoin for the refund) runs on the SHARDED execution path
// (only object holders execute), so it is deterministic ONLY for singleton
// objects, which every node holds and therefore applies identically. The current
// system pod deletes no objects and coins are singletons, so this is latent today.
//
// TODO: before any pod is allowed to delete REPLICATED objects carrying a storage
// deposit, move this supply/refund accounting to a deterministic all-nodes path
// driven by a committed/attested deletion set, applied identically on every node.
// Otherwise total_supply and coin balances will diverge across nodes (a fork),
// because holders would burn/refund while non-holders would not.
func (s *State) applyDeletedObjects(output *types.PodExecuteOutput, tx *types.Transaction) {
	data := output.DeletedObjectsBytes()
	const idSize = 32

	txHash := extractTxHash(tx)

	// Extract sender for ownership check
	var sender Hash
	if senderBytes := tx.SenderBytes(); len(senderBytes) == 32 {
		copy(sender[:], senderBytes)
	}

	// Extract gas_coin for refund credit
	var gasCoinID Hash
	hasGasCoin := false
	if gasCoinBytes := tx.GasCoinBytes(); len(gasCoinBytes) == 32 {
		copy(gasCoinID[:], gasCoinBytes)
		hasGasCoin = true
	}

	for i := 0; i+idSize <= len(data); i += idSize {
		var id Hash
		copy(id[:], data[i:i+idSize])

		// Load object to verify ownership
		objData := s.objects.get(id)
		if objData == nil {
			continue // object not in local storage, skip
		}

		// Ownership check: only owner can delete
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
			continue
		}

		// The full objFees was locked supply, so on deletion it must be fully
		// accounted: part refunded to a coin (stays in supply) and the remainder
		// burned (leaves supply). It must never silently vanish, or total_supply
		// would overstate the coins backing it.
		refund := s.settleDeletionDeposit(id, gasCoinID, obj.Fees(), hasGasCoin)

		s.objects.delete(id)
		events.ObjectDeleted(id, txHash, refund)
	}
}

// settleDeletionDeposit fully accounts a deleted object's locked storage
// deposit: it refunds storageRefundBPS to the gas_coin and burns the
// remainder, or burns the whole deposit when there is no recipient. It
// returns the amount actually refunded (0 when nothing was credited). Every
// burn emits supply.burned; a landed refund also emits fees.deposit.refunded.
// The burn is gated on the refund actually landing (creditGasCoin succeeding),
// so a failed refund never reduces supply while the refund vanishes.
func (s *State) settleDeletionDeposit(id, gasCoinID Hash, objFees uint64, hasGasCoin bool) uint64 {
	if objFees == 0 {
		return 0
	}

	if !hasGasCoin || s.storageRefundBPS == 0 {
		s.SubSupply(objFees) // no recipient: burn the full locked deposit
		events.SupplyBurned(objFees, "deletion")
		return 0
	}

	refund := objFees * s.storageRefundBPS / 10000
	burned := objFees - refund

	// Only the refund leaves supply (into a coin); gate the burn on it landing.
	if !s.creditGasCoin(gasCoinID, refund) {
		s.SubSupply(objFees) // refund failed: burn the full deposit, none leaks
		events.SupplyBurned(objFees, "deletion")
		return 0
	}

	// The refund landed in the gas coin's balance: coins_total rises by exactly
	// the refunded amount, mirroring the supply burn below.
	s.AddCoins(refund)
	s.SubSupply(burned)

	events.DepositRefunded(id, gasCoinID, refund)
	events.SupplyBurned(burned, "deletion")

	return refund
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

// creditGasCoin adds a refund amount to a gas_coin balance and reports whether
// the credit landed. It returns false when the coin is missing, malformed, or
// would overflow, so the caller can avoid burning supply against a refund that
// never reached a coin. A zero amount is a no-op and reports success (there is
// nothing to credit and nothing to leak). Version is NOT incremented (implicit
// protocol modification).
func (s *State) creditGasCoin(coinID Hash, amount uint64) bool {
	if amount == 0 {
		return true
	}

	data := s.objects.get(coinID)
	if data == nil {
		return false
	}

	obj := types.GetRootAsObject(data, 0)
	content := obj.ContentBytes()
	if len(content) < 8 {
		return false
	}

	balance := binary.LittleEndian.Uint64(content[:8])
	newBalance := balance + amount

	// Overflow check
	if newBalance < balance {
		return false
	}

	// Rebuild with new balance
	newContent := make([]byte, len(content))
	copy(newContent, content)
	binary.LittleEndian.PutUint64(newContent[:8], newBalance)

	builder := flatbuffers.NewBuilder(256)

	idVec := builder.CreateByteVector(obj.IdBytes())
	ownerVec := builder.CreateByteVector(obj.OwnerBytes())
	contentVec := builder.CreateByteVector(newContent)

	types.ObjectStart(builder)
	types.ObjectAddId(builder, idVec)
	types.ObjectAddVersion(builder, obj.Version())
	types.ObjectAddOwner(builder, ownerVec)
	types.ObjectAddReplication(builder, obj.Replication())
	types.ObjectAddContent(builder, contentVec)
	types.ObjectAddFees(builder, obj.Fees())

	offset := types.ObjectEnd(builder)
	builder.Finish(offset)

	s.objects.set(coinID, builder.FinishedBytes())
	return true
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
