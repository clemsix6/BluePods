package consensus

import (
	"fmt"
	"sync"

	"BluePods/internal/types"
)

// versionTracker tracks object versions from committed transactions.
// Used to detect version conflicts without executing transactions.
type versionTracker struct {
	mu       sync.RWMutex
	versions map[Hash]uint64 // objectID -> current version
}

// newVersionTracker creates an empty version tracker.
func newVersionTracker() *versionTracker {
	return &versionTracker{
		versions: make(map[Hash]uint64),
	}
}

// checkAndUpdate verifies expected versions and increments mutable objects.
// Returns true if all versions match, false if conflict detected.
func (vt *versionTracker) checkAndUpdate(tx *types.Transaction) bool {
	vt.mu.Lock()
	defer vt.mu.Unlock()

	if !vt.checkVersions(tx) {
		fmt.Printf("[versions] checkAndUpdate returning false (checkVersions failed)\n")
		return false
	}

	vt.incrementMutableObjects(tx)
	fmt.Printf("[versions] checkAndUpdate returning true\n")
	return true
}

// checkVersions verifies all object versions match expected values.
func (vt *versionTracker) checkVersions(tx *types.Transaction) bool {
	readObjs := tx.ReadObjectsBytes()
	mutObjs := tx.MutableObjectsBytes()

	fmt.Printf("[versions] checkVersions: readLen=%d mutLen=%d\n", len(readObjs), len(mutObjs))

	// Check read objects
	if !vt.checkObjectList(readObjs) {
		fmt.Printf("[versions] checkVersions failed on readObjects\n")
		return false
	}

	// Check mutable objects
	if !vt.checkObjectList(mutObjs) {
		fmt.Printf("[versions] checkVersions failed on mutableObjects\n")
		return false
	}

	return true
}

// checkObjectList verifies versions for a list of object references.
// Each reference is 40 bytes: 32 bytes objectID + 8 bytes expected version.
func (vt *versionTracker) checkObjectList(data []byte) bool {
	const refSize = 40 // 32 bytes ID + 8 bytes version

	if len(data)%refSize != 0 {
		fmt.Printf("[versions] checkObjectList failed: len=%d not multiple of %d\n", len(data), refSize)
		return false
	}

	for i := 0; i < len(data); i += refSize {
		var objectID Hash
		copy(objectID[:], data[i:i+32])

		expectedVersion := decodeVersion(data[i+32 : i+40])
		currentVersion := vt.versions[objectID]

		if currentVersion != expectedVersion {
			return false
		}
	}

	return true
}

// incrementMutableObjects increments version for each mutable object.
func (vt *versionTracker) incrementMutableObjects(tx *types.Transaction) {
	data := tx.MutableObjectsBytes()
	const refSize = 40

	for i := 0; i < len(data); i += refSize {
		var objectID Hash
		copy(objectID[:], data[i:i+32])

		vt.versions[objectID]++
	}
}

// setVersion sets the version for an object (used for new objects).
func (vt *versionTracker) setVersion(objectID Hash, version uint64) {
	vt.mu.Lock()
	defer vt.mu.Unlock()

	vt.versions[objectID] = version
}

// getVersion returns the current version of an object.
func (vt *versionTracker) getVersion(objectID Hash) uint64 {
	vt.mu.RLock()
	defer vt.mu.RUnlock()

	return vt.versions[objectID]
}

// decodeVersion decodes a little-endian uint64 from 8 bytes.
func decodeVersion(b []byte) uint64 {
	return uint64(b[0]) |
		uint64(b[1])<<8 |
		uint64(b[2])<<16 |
		uint64(b[3])<<24 |
		uint64(b[4])<<32 |
		uint64(b[5])<<40 |
		uint64(b[6])<<48 |
		uint64(b[7])<<56
}
