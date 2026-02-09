package consensus

import (
	"encoding/binary"

	"BluePods/internal/logger"
	"BluePods/internal/storage"
	"BluePods/internal/types"
)

// trackerKeyPrefix is the Pebble key prefix for tracker entries.
var trackerKeyPrefix = []byte("t:")

// ObjectTrackerEntry represents an object with its version and replication factor.
type ObjectTrackerEntry struct {
	ID          Hash   // ID is the 32-byte object identifier
	Version     uint64 // Version is the current version number
	Replication uint16 // Replication is the number of holders for this object
}

// objectTracker tracks object versions and replication from committed transactions.
// Persisted to Pebble with "t:" prefix. Replaces the in-memory versionTracker.
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
			continue
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

// incrementMutableObjects increments version for each mutable object.
func (ot *objectTracker) incrementMutableObjects(tx *types.Transaction) {
	var ref types.ObjectRef

	for i := 0; i < tx.MutableRefsLength(); i++ {
		if !tx.MutableRefs(&ref, i) {
			continue
		}

		idBytes := ref.IdBytes()
		if len(idBytes) != 32 {
			continue
		}

		var objectID Hash
		copy(objectID[:], idBytes)

		version := ot.getVersion(objectID)
		replication := ot.getReplication(objectID)
		ot.trackObject(objectID, version+1, replication)
	}
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

// trackObject stores or updates an object's version and replication.
func (ot *objectTracker) trackObject(objectID Hash, version uint64, replication uint16) {
	key := ot.makeKey(objectID)
	value := ot.encodeValue(version, replication)

	_ = ot.db.Set(key, value)
}

// deleteObject removes an object from the tracker.
func (ot *objectTracker) deleteObject(objectID Hash) {
	key := ot.makeKey(objectID)
	_ = ot.db.Delete(key)
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

		entries = append(entries, ObjectTrackerEntry{
			ID:          id,
			Version:     binary.LittleEndian.Uint64(value[:8]),
			Replication: binary.LittleEndian.Uint16(value[8:10]),
		})

		return nil
	})

	return entries
}

// Import loads tracker entries from snapshot data.
func (ot *objectTracker) Import(entries []ObjectTrackerEntry) {
	pairs := make([]storage.KeyValue, len(entries))

	for i, entry := range entries {
		pairs[i] = storage.KeyValue{
			Key:   ot.makeKey(entry.ID),
			Value: ot.encodeValue(entry.Version, entry.Replication),
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

// encodeValue encodes version (8 bytes LE) + replication (2 bytes LE) = 10 bytes.
func (ot *objectTracker) encodeValue(version uint64, replication uint16) []byte {
	value := make([]byte, 10)
	binary.LittleEndian.PutUint64(value[:8], version)
	binary.LittleEndian.PutUint16(value[8:10], replication)
	return value
}

