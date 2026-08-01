package consensus

import (
	"strings"

	"BluePods/internal/events"
	"BluePods/internal/genesis"
)

const (
	// domainRegisterOp binds an unregistered name to an object for a rental
	// term. Matches DeclaredOp.kind=2.
	domainRegisterOp byte = 2

	// domainRenewOp extends a name's lease. Matches DeclaredOp.kind=3.
	domainRenewOp byte = 3

	// domainUpdateOp repoints a name at another object. Matches
	// DeclaredOp.kind=4.
	domainUpdateOp byte = 4

	// domainTransferOp hands a name to a new owner. Matches DeclaredOp.kind=5.
	domainTransferOp byte = 5

	// domainDeleteOp removes a name from the registry. Matches
	// DeclaredOp.kind=6.
	domainDeleteOp byte = 6
)

const (
	// maxTermEpochs caps how far past the current epoch a lease may run. An
	// operation whose term would push the expiry beyond it REVERTS rather than
	// being clamped: the rent charged is rate x the declared term, read from
	// the transaction header, so a clamped term would charge for epochs the
	// lease does not get. Without the cap, one prepayment would hold a name
	// effectively forever, the exact squat recurring rent exists to prevent.
	maxTermEpochs uint64 = 256

	// maxDomainNameLen bounds a registrable name, matching the length the pod
	// write path has always enforced.
	maxDomainNameLen = 253

	// reservedDomainRoot is the protocol's reserved name, held for protocol
	// objects. Because a dotted name is authorized by the owner of the name
	// after its first label, leaving "system" permanently unregistered also
	// leaves every name under it (x.system) permanently unregistrable: no key
	// can ever own the parent that would authorize one.
	reservedDomainRoot = "system"

	// reservedDomainPrefix additionally holds the literal system.* names the
	// whitepaper reserves for protocol objects (system.validators and its
	// siblings), which the parent rule alone would leave open.
	reservedDomainPrefix = reservedDomainRoot + "."
)

// DomainStore is the narrow domain-registry surface the commit path reads and
// writes as it applies declared domain operations. *state.State implements it;
// consensus never sees the rest of the state package through this seam, and a
// DAG with no store wired rejects every domain operation rather than panicking.
type DomainStore interface {
	// DomainLeaf returns a name's leaf as stored — expiry unfiltered, so an
	// expired lease is still visible to the operations its owner may still
	// perform on it.
	DomainLeaf(name string) (objectID, owner [32]byte, expiry uint64, ok bool)

	// SetDomainLeaf writes a name's leaf.
	SetDomainLeaf(name string, objectID, owner [32]byte, expiry uint64)

	// DeleteDomainLeaf removes a name from the registry.
	DeleteDomainLeaf(name string)
}

// SetDomainStore wires the registry declared domain operations act on. Left
// unset, every domain operation is rejected, so a DAG built without it behaves
// exactly as it did before domain operations existed.
func (d *DAG) SetDomainStore(store DomainStore) {
	d.domains = store
}

// applyDomainOp runs the effect of an already-validated domain operation
// against the real registry, in list order. Each operation re-derives its
// result from the store, which already carries the effects of the operations
// before it in this same list — the identical sequence the staged view
// validated against, so validation and application cannot disagree.
func (d *DAG) applyDomainOp(txHash, sender Hash, epoch uint64, op genesis.DeclaredOp) {
	switch op.Kind {
	case domainRegisterOp:
		d.applyDomainRegister(txHash, sender, epoch, op)
	case domainRenewOp:
		d.applyDomainRenew(txHash, epoch, op)
	case domainUpdateOp:
		d.applyDomainUpdate(txHash, op)
	case domainTransferOp:
		d.applyDomainTransfer(txHash, op)
	case domainDeleteOp:
		d.applyDomainDelete(txHash, op)
	}
}

// applyDomainRegister binds a name to an object under the sender's ownership
// for the declared term.
func (d *DAG) applyDomainRegister(txHash, sender Hash, epoch uint64, op genesis.DeclaredOp) {
	objectID := toHash(op.ObjectID)
	expiry, _ := domainExpiry(0, epoch, op.TermEpochs)

	d.writeDomainLeaf(op.Name, objectID, sender, expiry)
	events.DomainRegistered(op.Name, objectID, txHash)
}

// applyDomainRenew extends a lease, leaving the name's object and owner alone.
func (d *DAG) applyDomainRenew(txHash Hash, epoch uint64, op genesis.DeclaredOp) {
	objectID, owner, current, ok := d.domains.DomainLeaf(op.Name)
	if !ok {
		return
	}

	expiry, _ := domainExpiry(current, epoch, op.TermEpochs)

	d.writeDomainLeaf(op.Name, objectID, owner, expiry)
	events.DomainRenewed(op.Name, expiry, txHash)
}

// applyDomainUpdate repoints a name at another object, leaving its owner and
// its lease alone.
func (d *DAG) applyDomainUpdate(txHash Hash, op genesis.DeclaredOp) {
	_, owner, expiry, ok := d.domains.DomainLeaf(op.Name)
	if !ok {
		return
	}

	objectID := toHash(op.ObjectID)

	d.writeDomainLeaf(op.Name, objectID, owner, expiry)
	events.DomainUpdated(op.Name, objectID, txHash)
}

// applyDomainTransfer hands a name to a new owner, leaving its object and its
// lease alone.
func (d *DAG) applyDomainTransfer(txHash Hash, op genesis.DeclaredOp) {
	objectID, _, expiry, ok := d.domains.DomainLeaf(op.Name)
	if !ok {
		return
	}

	owner := toHash(op.Target)

	d.writeDomainLeaf(op.Name, objectID, owner, expiry)
	events.DomainTransferred(op.Name, owner, txHash)
}

// applyDomainDelete removes a name from the registry and from the index. The
// rent already consumed is not refunded: a lease buys epochs, not an asset.
func (d *DAG) applyDomainDelete(txHash Hash, op genesis.DeclaredOp) {
	d.domains.DeleteDomainLeaf(op.Name)

	if d.indexer != nil {
		d.indexer.RemoveDomain(op.Name)
	}

	events.DomainDeleted(op.Name, txHash)
}

// writeDomainLeaf persists a leaf and feeds it to the authenticated domain
// tree. Every field of the leaf is hashed into the tree, so a mutation that
// reached the store without reaching the index would leave this node anchoring
// a root no other node computes.
func (d *DAG) writeDomainLeaf(name string, objectID, owner Hash, expiry uint64) {
	d.domains.SetDomainLeaf(name, objectID, owner, expiry)

	if d.indexer != nil {
		d.indexer.ApplyDomain(name, objectID, owner, expiry)
	}
}

// domainExpiry computes the expiry a register or renew produces and reports
// whether it is allowed. A renewal runs from the later of the current expiry
// and the current epoch, so prepaying ahead and renewing after an expiry are
// the same operation. A zero term is rejected (it would buy no lease at no
// rent), and so is any result past the cap — reverted, never clamped.
func domainExpiry(current, epoch uint64, term uint32) (uint64, bool) {
	if term == 0 {
		return 0, false
	}

	base := current
	if epoch > base {
		base = epoch
	}

	expiry := safeAdd(base, uint64(term))
	if expiry > safeAdd(epoch, maxTermEpochs) {
		return 0, false
	}

	return expiry, true
}

// validDomainName reports whether a name may be registered: non-empty, within
// the length bound, made of non-empty dot-separated labels, and outside the
// reserved system namespace.
func validDomainName(name string) bool {
	if len(name) == 0 || len(name) > maxDomainNameLen {
		return false
	}

	if name == reservedDomainRoot || strings.HasPrefix(name, reservedDomainPrefix) {
		return false
	}

	for _, label := range strings.Split(name, ".") {
		if label == "" {
			return false
		}
	}

	return true
}

// parentDomainName returns the namespace a dotted name sits in — everything
// after its first label — and false for a bare root, which is first come,
// first served.
func parentDomainName(name string) (string, bool) {
	dot := strings.Index(name, ".")
	if dot < 0 {
		return "", false
	}

	return name[dot+1:], true
}
