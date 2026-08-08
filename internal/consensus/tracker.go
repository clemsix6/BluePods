package consensus

import (
	"encoding/binary"

	"BluePods/internal/logger"
	"BluePods/internal/storage"
	"BluePods/internal/types"
)

// trackerKeyPrefix is the Pebble key prefix for tracker entries.
var trackerKeyPrefix = []byte("t:")

// committedTxPrefix is the Pebble key prefix for committed transaction hashes.
// It records every transaction hash already processed at commit, so a transaction
// that reaches the commit path more than once (for example, the same gossiped
// transaction included by several producers) executes exactly once.
var committedTxPrefix = []byte("c:")

const (
	// keyRootKind marks a parent reference as an Ed25519 public key: the
	// object is owned directly by an account, not by another tracked object.
	// Matches parent_kind=0 in types/object.fbs.
	keyRootKind byte = 0

	// objectParentKind marks a parent reference as another tracked object's
	// ID: the object is a child in the parent/child object hierarchy, and its
	// parent's child count is maintained by this tracker. Matches
	// parent_kind=1 in types/object.fbs.
	objectParentKind byte = 1
)

// ObjectTrackerEntry represents an object with its version, replication,
// fees, parent reference, and child count.
type ObjectTrackerEntry struct {
	ID          Hash   // ID is the 32-byte object identifier
	Version     uint64 // Version is the current version number
	Replication uint16 // Replication is the number of holders for this object
	Fees        uint64 // Fees is the storage deposit set at creation
	ParentKind  byte   // ParentKind is keyRootKind (Parent is an Ed25519 key) or objectParentKind (Parent is another tracked object's ID)
	Parent      Hash   // Parent is the parent reference: a key under KeyRoot, an object ID under ObjectParent
	ChildCount  uint32 // ChildCount is the number of ObjectParent children currently tracked under this object
}

// objectTracker tracks object versions, replication, fees, and parent/child-count
// bookkeeping from committed transactions. Persisted to Pebble with "t:"
// prefix. Replaces the in-memory versionTracker.
type objectTracker struct {
	db *storage.Storage // db is the underlying Pebble storage
}

// newObjectTracker creates an object tracker backed by the given storage.
func newObjectTracker(db *storage.Storage) *objectTracker {
	return &objectTracker{db: db}
}

// checkAndUpdate verifies expected versions and increments mutable objects.
// Returns true if all versions match, false if conflict detected.
func (ot *objectTracker) checkAndUpdate(tx *types.Transaction) bool {
	if !ot.checkVersions(tx) {
		logger.Debug("version check failed")
		return false
	}

	ot.incrementMutableObjects(tx)

	return true
}

// checkVersions verifies all object versions match expected values.
func (ot *objectTracker) checkVersions(tx *types.Transaction) bool {
	if !ot.checkRefList(tx, false) {
		logger.Debug("version mismatch on read refs", "count", tx.ReadRefsLength())
		return false
	}

	if !ot.checkRefList(tx, true) {
		logger.Debug("version mismatch on mutable refs", "count", tx.MutableRefsLength())
		return false
	}

	return true
}

// checkRefList verifies versions for a list of ObjectRef references.
func (ot *objectTracker) checkRefList(tx *types.Transaction, mutable bool) bool {
	var count int
	if mutable {
		count = tx.MutableRefsLength()
	} else {
		count = tx.ReadRefsLength()
	}

	var ref types.ObjectRef

	for i := 0; i < count; i++ {
		if mutable {
			tx.MutableRefs(&ref, i)
		} else {
			tx.ReadRefs(&ref, i)
		}

		idBytes := ref.IdBytes()
		if len(idBytes) != 32 {
			// Domain refs have no ID — skip them (version tracking is by object ID)
			if len(ref.Domain()) > 0 {
				continue
			}
			// Malformed standard ref — reject transaction
			return false
		}

		var objectID Hash
		copy(objectID[:], idBytes)

		expectedVersion := ref.Version()
		currentVersion := ot.getVersion(objectID)

		if currentVersion != expectedVersion {
			return false
		}
	}

	return true
}

// incrementMutableObjects increments version for each mutable object,
// preserving its existing parent reference. An object's parent changes only
// through setParent, never as a side effect of a version bump.
func (ot *objectTracker) incrementMutableObjects(tx *types.Transaction) {
	var ref types.ObjectRef

	for i := 0; i < tx.MutableRefsLength(); i++ {
		if !tx.MutableRefs(&ref, i) {
			continue
		}

		idBytes := ref.IdBytes()
		if len(idBytes) != 32 {
			// Domain refs have no ID — skip them
			if len(ref.Domain()) > 0 {
				continue
			}
			continue
		}

		var objectID Hash
		copy(objectID[:], idBytes)

		version := ot.getVersion(objectID)
		replication := ot.getReplication(objectID)
		fees := ot.getFees(objectID)
		kind, parent, _ := ot.getParent(objectID)
		ot.trackObject(objectID, version+1, replication, fees, kind, parent)
	}
}

// wasCommitted reports whether a transaction with the given hash has already
// been processed at commit.
func (ot *objectTracker) wasCommitted(txHash Hash) bool {
	value, err := ot.db.Get(ot.committedKey(txHash))

	return err == nil && value != nil
}

// markCommitted records a transaction hash as processed at commit.
// TODO: prune committed-tx records past a retention window so the set does not
// grow without bound over a long-running chain.
func (ot *objectTracker) markCommitted(txHash Hash) {
	_ = ot.db.Set(ot.committedKey(txHash), []byte{1})
}

// committedKey builds the Pebble key for a committed transaction hash.
func (ot *objectTracker) committedKey(txHash Hash) []byte {
	key := make([]byte, len(committedTxPrefix)+32)
	copy(key, committedTxPrefix)
	copy(key[len(committedTxPrefix):], txHash[:])

	return key
}

// getVersion returns the current version of an object.
// Returns 0 if the object is not tracked (matches Go zero-value behavior).
func (ot *objectTracker) getVersion(objectID Hash) uint64 {
	value, err := ot.db.Get(ot.makeKey(objectID))
	if err != nil || value == nil {
		return 0
	}

	if len(value) < 8 {
		return 0
	}

	return binary.LittleEndian.Uint64(value[:8])
}

// getReplication returns the replication factor of an object.
// Returns 0 if the object is not tracked.
func (ot *objectTracker) getReplication(objectID Hash) uint16 {
	value, err := ot.db.Get(ot.makeKey(objectID))
	if err != nil || value == nil || len(value) < 10 {
		return 0
	}

	return binary.LittleEndian.Uint16(value[8:10])
}

// getFees returns the storage deposit of an object.
// Returns 0 if the object is not tracked.
func (ot *objectTracker) getFees(objectID Hash) uint64 {
	value, err := ot.db.Get(ot.makeKey(objectID))
	if err != nil || value == nil || len(value) < 18 {
		return 0
	}

	return binary.LittleEndian.Uint64(value[10:18])
}

// getParent returns the stored parent kind and bytes for objectID, and
// whether the object is tracked at all. A value written before this field
// existed (18 bytes: version + replication + fees only) decodes as KeyRoot
// with a zero parent — such objects are effectively frozen under the
// unspendable zero key. That is acceptable only pre-mainnet, where a network
// carrying such entries is recreated from genesis rather than migrated
// forward.
func (ot *objectTracker) getParent(objectID Hash) (kind byte, parent Hash, ok bool) {
	value, err := ot.db.Get(ot.makeKey(objectID))
	if err != nil || value == nil {
		return keyRootKind, Hash{}, false
	}

	if len(value) < 55 {
		return keyRootKind, Hash{}, true
	}

	kind = value[18]
	copy(parent[:], value[19:51])

	return kind, parent, true
}

// childCount returns the number of ObjectParent children currently tracked
// under parentID. An entry written before this field existed (18-byte legacy
// value) carries no child bookkeeping and reports zero.
func (ot *objectTracker) childCount(parentID Hash) uint32 {
	value, err := ot.db.Get(ot.makeKey(parentID))
	if err != nil || value == nil || len(value) < 55 {
		return 0
	}

	return binary.LittleEndian.Uint32(value[51:55])
}

// trackObject stores or updates an object's version, replication, fees, and
// parent reference, then rebinds child-count bookkeeping on the old and new
// parent (see rebindChildCount). Re-tracking an object whose parent kind and
// bytes are unchanged — a version bump, for example — leaves child counts
// untouched.
func (ot *objectTracker) trackObject(objectID Hash, version uint64, replication uint16, fees uint64, parentKind byte, parent Hash) {
	oldKind, oldParent, existed := ot.getParent(objectID)
	selfChildren := ot.childCount(objectID)

	key := ot.makeKey(objectID)
	value := ot.encodeValue(version, replication, fees, parentKind, parent, selfChildren)
	_ = ot.db.Set(key, value)

	ot.rebindChildCount(existed, oldKind, oldParent, parentKind, parent)
}

// setParent updates objectID's parent reference in place, preserving its
// version, replication, fees, and own child count, then rebinds child-count
// bookkeeping on the old and new parent.
func (ot *objectTracker) setParent(objectID Hash, kind byte, parent Hash) {
	version := ot.getVersion(objectID)
	replication := ot.getReplication(objectID)
	fees := ot.getFees(objectID)
	selfChildren := ot.childCount(objectID)
	oldKind, oldParent, existed := ot.getParent(objectID)

	key := ot.makeKey(objectID)
	value := ot.encodeValue(version, replication, fees, kind, parent, selfChildren)
	_ = ot.db.Set(key, value)

	ot.rebindChildCount(existed, oldKind, oldParent, kind, parent)
}

// rebindChildCount adjusts the old and new parent's child counts when an
// object's parent edge changes from (oldKind, oldParent) to (newKind,
// newParent). A brand new object (existed=false) only adds the new edge; an
// existing object removes the old edge and adds the new one, and does
// nothing when the edge is unchanged. KeyRoot parents receive no bookkeeping
// (see adjustChildCount).
func (ot *objectTracker) rebindChildCount(existed bool, oldKind byte, oldParent Hash, newKind byte, newParent Hash) {
	if existed {
		if oldKind == newKind && oldParent == newParent {
			return
		}

		ot.adjustChildCount(oldKind, oldParent, -1)
	}

	ot.adjustChildCount(newKind, newParent, 1)
}

// adjustChildCount changes parentID's own child-count field by delta. KeyRoot
// parents are keys, not tracked objects — their children are enumerated
// later from the index, not counted here — so they receive no bookkeeping.
// An ObjectParent parent absent from the tracker (or written before the
// child-count field existed) is left untouched.
func (ot *objectTracker) adjustChildCount(kind byte, parentID Hash, delta int32) {
	if kind != objectParentKind {
		return
	}

	key := ot.makeKey(parentID)
	value, err := ot.db.Get(key)
	if err != nil || value == nil || len(value) < 55 {
		return
	}

	count := uint32(int64(binary.LittleEndian.Uint32(value[51:55])) + int64(delta))
	binary.LittleEndian.PutUint32(value[51:55], count)

	_ = ot.db.Set(key, value)
}

// deleteObject removes an object from the tracker, decrementing its
// ObjectParent parent's child count (a KeyRoot parent receives no
// bookkeeping).
func (ot *objectTracker) deleteObject(objectID Hash) {
	kind, parent, existed := ot.getParent(objectID)

	_ = ot.db.Delete(ot.makeKey(objectID))

	if existed {
		ot.adjustChildCount(kind, parent, -1)
	}
}

// Export returns all tracked objects for snapshot serialization.
func (ot *objectTracker) Export() []ObjectTrackerEntry {
	var entries []ObjectTrackerEntry

	_ = ot.db.IteratePrefix(trackerKeyPrefix, func(key, value []byte) error {
		if len(key) != len(trackerKeyPrefix)+32 || len(value) < 10 {
			return nil
		}

		var id Hash
		copy(id[:], key[len(trackerKeyPrefix):])

		entry := ObjectTrackerEntry{
			ID:          id,
			Version:     binary.LittleEndian.Uint64(value[:8]),
			Replication: binary.LittleEndian.Uint16(value[8:10]),
		}

		// Read fees from extended value (18 bytes: version:8 + replication:2 + fees:8)
		if len(value) >= 18 {
			entry.Fees = binary.LittleEndian.Uint64(value[10:18])
		}

		// A pre-migration 18-byte value carries no parent/child-count fields;
		// it decodes as KeyRoot with a zero parent, per getParent's docstring.
		if len(value) >= 55 {
			entry.ParentKind = value[18]
			copy(entry.Parent[:], value[19:51])
			entry.ChildCount = binary.LittleEndian.Uint32(value[51:55])
		}

		entries = append(entries, entry)

		return nil
	})

	return entries
}

// Import loads tracker entries from snapshot data, writing each entry's
// fields — including its parent reference and child count — verbatim. The
// source tracker already computed a consistent child count for every entry,
// so Import does not recompute bookkeeping from the imported parent edges.
func (ot *objectTracker) Import(entries []ObjectTrackerEntry) {
	pairs := make([]storage.KeyValue, len(entries))

	for i, entry := range entries {
		pairs[i] = storage.KeyValue{
			Key:   ot.makeKey(entry.ID),
			Value: ot.encodeValue(entry.Version, entry.Replication, entry.Fees, entry.ParentKind, entry.Parent, entry.ChildCount),
		}
	}

	_ = ot.db.SetBatch(pairs)
}

// makeKey builds the Pebble key for an object: "t:" + objectID (34 bytes total).
func (ot *objectTracker) makeKey(objectID Hash) []byte {
	key := make([]byte, len(trackerKeyPrefix)+32)
	copy(key, trackerKeyPrefix)
	copy(key[len(trackerKeyPrefix):], objectID[:])
	return key
}

// encodeValue encodes version (8 bytes LE) + replication (2 bytes LE) + fees
// (8 bytes LE) + parent kind (1 byte) + parent (32 bytes) + child count
// (4 bytes LE) = 55 bytes.
func (ot *objectTracker) encodeValue(version uint64, replication uint16, fees uint64, parentKind byte, parent Hash, childCount uint32) []byte {
	value := make([]byte, 55)
	binary.LittleEndian.PutUint64(value[:8], version)
	binary.LittleEndian.PutUint16(value[8:10], replication)
	binary.LittleEndian.PutUint64(value[10:18], fees)
	value[18] = parentKind
	copy(value[19:51], parent[:])
	binary.LittleEndian.PutUint32(value[51:55], childCount)
	return value
}
