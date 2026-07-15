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

// DomainRegistered marks a new domain name bound to object.
func DomainRegistered(name string, object, tx [32]byte) {
	emit(EvDomainRegistered, slog.String("name", name), hexAttr("object", object), hexAttr("tx", tx))
}

// DomainUpdated marks a domain name rebound to a different object.
func DomainUpdated(name string, object, tx [32]byte) {
	emit(EvDomainUpdated, slog.String("name", name), hexAttr("object", object), hexAttr("tx", tx))
}

// DomainDeleted marks a domain name removed from the registry.
func DomainDeleted(name string, tx [32]byte) {
	emit(EvDomainDeleted, slog.String("name", name), hexAttr("tx", tx))
}
