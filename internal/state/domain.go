package state

import (
	"BluePods/internal/storage"
)

// domainKeyPrefix is the Pebble key prefix for domain entries.
var domainKeyPrefix = []byte("d:")

// DomainEntry holds a domain name and its associated object ID.
type DomainEntry struct {
	Name     string // Name is the domain name
	ObjectID Hash   // ObjectID is the 32-byte object identifier
}

// domainStore stores domain name -> ObjectID mappings in Pebble.
type domainStore struct {
	db *storage.Storage // db is the underlying Pebble storage
}

// newDomainStore creates a domain store backed by the given storage.
func newDomainStore(db *storage.Storage) *domainStore {
	return &domainStore{db: db}
}

// get retrieves the ObjectID for a domain name. Returns false if not found.
func (d *domainStore) get(name string) (Hash, bool) {
	key := d.makeKey(name)

	value, err := d.db.Get(key)
	if err != nil || value == nil || len(value) != 32 {
		return Hash{}, false
	}

	var id Hash
	copy(id[:], value)

	return id, true
}

// exists returns true if the domain name is registered.
func (d *domainStore) exists(name string) bool {
	_, found := d.get(name)
	return found
}

// set stores a domain name -> ObjectID mapping.
func (d *domainStore) set(name string, objectID Hash) {
	key := d.makeKey(name)
	_ = d.db.Set(key, objectID[:])
}

// delete removes a domain name mapping.
func (d *domainStore) delete(name string) {
	key := d.makeKey(name)
	_ = d.db.Delete(key)
}

// export returns all domain entries for snapshot serialization.
func (d *domainStore) export() []DomainEntry {
	var entries []DomainEntry

	_ = d.db.IteratePrefix(domainKeyPrefix, func(key, value []byte) error {
		if len(value) != 32 {
			return nil
		}

		name := string(key[len(domainKeyPrefix):])

		var id Hash
		copy(id[:], value)

		entries = append(entries, DomainEntry{Name: name, ObjectID: id})

		return nil
	})

	return entries
}

// importBatch loads domain entries from snapshot data.
func (d *domainStore) importBatch(entries []DomainEntry) {
	pairs := make([]storage.KeyValue, len(entries))

	for i, entry := range entries {
		pairs[i] = storage.KeyValue{
			Key:   d.makeKey(entry.Name),
			Value: entry.ObjectID[:],
		}
	}

	_ = d.db.SetBatch(pairs)
}

// makeKey builds the Pebble key for a domain: "d:" + name bytes.
func (d *domainStore) makeKey(name string) []byte {
	key := make([]byte, len(domainKeyPrefix)+len(name))
	copy(key, domainKeyPrefix)
	copy(key[len(domainKeyPrefix):], name)

	return key
}
