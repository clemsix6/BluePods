package state

import (
	"encoding/binary"

	"BluePods/internal/storage"
)

// domainKeyPrefix is the Pebble key prefix for domain entries.
var domainKeyPrefix = []byte("d:")

const (
	// legacyDomainValueSize is the length of a pre-rental stored value: a bare
	// 32-byte object ID, with no owner and no lease.
	legacyDomainValueSize = 32

	// domainValueSize is the length of the current stored value: object ID,
	// owner, then the expiry epoch.
	domainValueSize = 32 + 32 + 8
)

// DomainEntry is a domain name's registry leaf: the object the name resolves
// to, the key that owns the lease, and the epoch the lease runs to. It is the
// unit the store, the snapshot, and the authenticated domain tree all carry, so
// every one of them commits to the same four fields.
type DomainEntry struct {
	Name        string // Name is the domain name
	ObjectID    Hash   // ObjectID is the 32-byte object identifier the name resolves to
	Owner       Hash   // Owner is the key that may renew, update, transfer, or delete the name
	ExpiryEpoch uint64 // ExpiryEpoch is the last epoch the lease resolves in
}

// domainStore stores domain name -> leaf mappings in Pebble.
type domainStore struct {
	db *storage.Storage // db is the underlying Pebble storage
}

// newDomainStore creates a domain store backed by the given storage.
func newDomainStore(db *storage.Storage) *domainStore {
	return &domainStore{db: db}
}

// get retrieves a domain name's leaf. Returns false if not found.
func (d *domainStore) get(name string) (DomainEntry, bool) {
	value, err := d.db.Get(d.makeKey(name))
	if err != nil {
		return DomainEntry{}, false
	}

	return decodeDomainValue(name, value)
}

// exists returns true if the domain name is registered, whatever the lease's
// state: an expired leaf still occupies its name until the epoch sweep removes
// it, so the name is not registrable again before then.
func (d *domainStore) exists(name string) bool {
	_, found := d.get(name)
	return found
}

// set stores a domain name's leaf.
func (d *domainStore) set(entry DomainEntry) {
	_ = d.db.Set(d.makeKey(entry.Name), encodeDomainValue(entry))
}

// delete removes a domain name mapping, on an owner's declared deletion or the
// epoch sweep of an expired lease.
func (d *domainStore) delete(name string) {
	key := d.makeKey(name)
	_ = d.db.Delete(key)
}

// prefixIterator is the read surface domain export needs: prefix iteration. Both
// the live *storage.Storage and a consistent *storage.Snapshot satisfy it, so a
// sync snapshot can export domains from the same cut as objects.
type prefixIterator interface {
	// IteratePrefix visits every key-value pair with the given prefix.
	IteratePrefix(prefix []byte, fn func(key, value []byte) error) error
}

// export returns all domain entries for snapshot serialization.
func (d *domainStore) export() []DomainEntry {
	return exportDomainEntries(d.db)
}

// exportDomainEntries decodes every domain entry readable from src.
func exportDomainEntries(src prefixIterator) []DomainEntry {
	var entries []DomainEntry

	_ = src.IteratePrefix(domainKeyPrefix, func(key, value []byte) error {
		entry, ok := decodeDomainValue(string(key[len(domainKeyPrefix):]), value)
		if !ok {
			return nil
		}

		entries = append(entries, entry)

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
			Value: encodeDomainValue(entry),
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

// encodeDomainValue serializes a leaf as objectID ‖ owner ‖ expiry epoch (big
// endian). The name is not in the value: it is the key.
func encodeDomainValue(entry DomainEntry) []byte {
	value := make([]byte, 0, domainValueSize)
	value = append(value, entry.ObjectID[:]...)
	value = append(value, entry.Owner[:]...)

	return binary.BigEndian.AppendUint64(value, entry.ExpiryEpoch)
}

// decodeDomainValue reverses encodeDomainValue for name, reporting ok=false for
// any length it does not recognize. A stored value of the legacy length is a
// pre-rental entry written before leases existed: it decodes as an unowned name
// expiring at epoch 0, which stops resolving from the first epoch boundary
// rather than becoming an immortal squat. Pre-mainnet networks are recreated,
// so no live lease is ever read back through this branch.
func decodeDomainValue(name string, value []byte) (DomainEntry, bool) {
	if len(value) != domainValueSize && len(value) != legacyDomainValueSize {
		return DomainEntry{}, false
	}

	entry := DomainEntry{Name: name}
	copy(entry.ObjectID[:], value[:32])

	if len(value) == legacyDomainValueSize {
		return entry, true
	}

	copy(entry.Owner[:], value[32:64])
	entry.ExpiryEpoch = binary.BigEndian.Uint64(value[64:])

	return entry, true
}
