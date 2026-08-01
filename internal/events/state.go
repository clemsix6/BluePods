package events

import "log/slog"

// ObjectCreated marks a new object stored, carrying its owner and its tracker
// replication factor (0 for a singleton).
func ObjectCreated(object, tx [32]byte, version uint64, replication uint16, owner [32]byte) {
	emit(EvObjectCreated,
		hexAttr("object", object),
		hexAttr("tx", tx),
		slog.Uint64("version", version),
		slog.Uint64("replication", uint64(replication)),
		hexAttr("owner", owner))
}

// ObjectUpdated marks an existing object's content replaced at a new version.
func ObjectUpdated(object, tx [32]byte, version uint64) {
	emit(EvObjectUpdated, hexAttr("object", object), hexAttr("tx", tx), slog.Uint64("version", version))
}

// ObjectDeleted marks an object removed from state, carrying the deposit
// refund credited to its deleter (0 when none).
func ObjectDeleted(object, tx [32]byte, refund uint64) {
	emit(EvObjectDeleted, hexAttr("object", object), hexAttr("tx", tx), slog.Uint64("refund", refund))
}

// ObjectReparented marks an object's parent edge changed by a protocol-declared
// reparent operation (a transfer is a reparent to a KeyRoot). kind selects how
// to read parent: 0 = KeyRoot (an Ed25519 key), 1 = ObjectParent (another
// object's ID). version is the object's post-operation version.
func ObjectReparented(object, tx [32]byte, kind byte, parent [32]byte, version uint64) {
	emit(EvObjectReparented,
		hexAttr("object", object),
		hexAttr("tx", tx),
		slog.Uint64("kind", uint64(kind)),
		hexAttr("parent", parent),
		slog.Uint64("version", version))
}

// DomainRegistered marks a new domain name bound to object.
func DomainRegistered(name string, object, tx [32]byte) {
	emit(EvDomainRegistered, slog.String("name", name), hexAttr("object", object), hexAttr("tx", tx))
}

// DomainUpdated marks a domain name rebound to a different object.
func DomainUpdated(name string, object, tx [32]byte) {
	emit(EvDomainUpdated, slog.String("name", name), hexAttr("object", object), hexAttr("tx", tx))
}

// DomainRenewed marks a domain lease extended, carrying the epoch the lease
// now runs to. A renewal changes nothing else: the name keeps its object and
// its owner.
func DomainRenewed(name string, expiry uint64, tx [32]byte) {
	emit(EvDomainRenewed, slog.String("name", name), slog.Uint64("expiry", expiry), hexAttr("tx", tx))
}

// DomainTransferred marks a domain name handed to a new owner, who inherits
// the renewal, update, transfer, and deletion rights over it. The name keeps
// its object and its expiry.
func DomainTransferred(name string, owner, tx [32]byte) {
	emit(EvDomainTransferred, slog.String("name", name), hexAttr("owner", owner), hexAttr("tx", tx))
}

// DomainDeleted marks a domain name removed from the registry, by its owner's
// declared operation or by the expiry sweep.
func DomainDeleted(name string, tx [32]byte) {
	emit(EvDomainDeleted, slog.String("name", name), hexAttr("tx", tx))
}
