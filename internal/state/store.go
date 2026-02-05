package state

import (
	"BluePods/internal/storage"
	"BluePods/internal/types"
)

// Hash is a 32-byte identifier.
type Hash [32]byte

// objectStore holds objects indexed by ID, backed by persistent storage.
type objectStore struct {
	db *storage.Storage
}

// newObjectStore creates an object store backed by the given storage.
func newObjectStore(db *storage.Storage) *objectStore {
	return &objectStore{db: db}
}

// get retrieves an object by ID. Returns nil if not found.
func (s *objectStore) get(id Hash) []byte {
	data, err := s.db.Get(id[:])
	if err != nil {
		return nil
	}
	return data
}

// set stores an object.
func (s *objectStore) set(id Hash, data []byte) {
	_ = s.db.Set(id[:], data)
}

// delete removes an object.
func (s *objectStore) delete(id Hash) {
	_ = s.db.Delete(id[:])
}

// extractObjectID extracts the object ID from raw object bytes.
func extractObjectID(data []byte) Hash {
	obj := types.GetRootAsObject(data, 0)

	var id Hash
	if idBytes := obj.IdBytes(); len(idBytes) == 32 {
		copy(id[:], idBytes)
	}

	return id
}
