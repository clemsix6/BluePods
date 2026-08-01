package consensus

import (
	"strings"

	"BluePods/internal/events"
	"BluePods/internal/genesis"
	"BluePods/internal/logger"
	"BluePods/internal/state"
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

	// ExportDomains returns every registered name's leaf, unfiltered by
	// expiry. The epoch boundary's sweep is its only consensus-side reader
	// (a full-registry read, done once per boundary rather than tracked
	// incrementally, keeps the sweep a pure function of committed state with
	// no additional index to keep consistent); consensus.indexDomainLeaves,
	// the construction-time index backfill, is its other reader. The sync
	// snapshot and the convergence fingerprint do NOT use it — they read a
	// consistent cut through *state.State.ExportDomainsFrom instead.
	ExportDomains() []state.DomainEntry
}

// SetDomainStore wires the registry declared domain operations act on after
// construction. It is a TEST-ONLY seam: package tests inject recording fakes
// and hand-built stores into a DAG whose loops they control, which no
// construction option can do. Production wires the registry through
// WithDomainStore instead — writing d.domains once the commit loop is already
// running rejects every domain operation decided before the wire lands, since
// applyDomainOp's staged view has nothing to validate against until then.
func (d *DAG) SetDomainStore(store DomainStore) {
	d.domains = store
}

// applyDomainOp runs the effect of an already-validated domain operation
// against the real registry, in list order. Each operation re-derives its
// result from the store, which already carries the effects of the operations
// before it in this same list — the identical sequence the staged view
// validated against, so validation and application cannot disagree. maxTerm is
// threaded from the SAME staged view that validated the list, so the lease
// cap priced at apply time is always the one proven to fit at validation.
func (d *DAG) applyDomainOp(txHash, sender Hash, epoch, maxTerm uint64, op genesis.DeclaredOp) {
	switch op.Kind {
	case domainRegisterOp:
		d.applyDomainRegister(txHash, sender, epoch, maxTerm, op)
	case domainRenewOp:
		d.applyDomainRenew(txHash, epoch, maxTerm, op)
	case domainUpdateOp:
		d.applyDomainUpdate(txHash, op)
	case domainTransferOp:
		d.applyDomainTransfer(txHash, op)
	case domainDeleteOp:
		d.applyDomainDelete(txHash, op)
	}
}

// applyDomainRegister binds a name to an object under the sender's ownership
// for the declared term. maxTerm is the cap the staged view already proved
// this term fits under; a disagreement here means apply diverged from
// validation, an invariant violation logged loud rather than silently written
// as an instantly-expired lease.
func (d *DAG) applyDomainRegister(txHash, sender Hash, epoch, maxTerm uint64, op genesis.DeclaredOp) {
	objectID := toHash(op.ObjectID)

	expiry, ok := domainExpiry(0, epoch, op.TermEpochs, maxTerm)
	if !ok {
		logger.Error("domain register: apply disagreed with staged validation", "name", op.Name)
		return
	}

	d.writeDomainLeaf(op.Name, objectID, sender, expiry)
	events.DomainRegistered(op.Name, objectID, txHash)
}

// applyDomainRenew extends a lease, leaving the name's object and owner alone.
// maxTerm is the cap the staged view already proved this term fits under; a
// disagreement here means apply diverged from validation, an invariant
// violation logged loud rather than silently written as an instantly-expired
// lease.
func (d *DAG) applyDomainRenew(txHash Hash, epoch, maxTerm uint64, op genesis.DeclaredOp) {
	objectID, owner, current, ok := d.domains.DomainLeaf(op.Name)
	if !ok {
		return
	}

	expiry, ok := domainExpiry(current, epoch, op.TermEpochs, maxTerm)
	if !ok {
		logger.Error("domain renew: apply disagreed with staged validation", "name", op.Name)
		return
	}

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
// reason is empty: the deletion carries a transaction, unlike the epoch
// boundary's expiry sweep (reason "expired", no transaction to name).
func (d *DAG) applyDomainDelete(txHash Hash, op genesis.DeclaredOp) {
	d.domains.DeleteDomainLeaf(op.Name)

	if d.indexer != nil {
		d.indexer.RemoveDomain(op.Name)
	}

	events.DomainDeleted(op.Name, txHash, "")
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

// maxTermEpochs returns the governed cap on how far past the current epoch a
// lease may run. It comes from the same frozen FeeParams that charges rate x
// term, so the cap that reverts an operation and the price of the term it
// allows can never be moved apart by a parameter change.
func (d *DAG) maxTermEpochs() uint64 {
	return d.mustFeeParams().MaxTermEpochs
}

// graceEpochs returns the governed window past expiry a lease stays in the
// registry before the boundary sweep removes it, reserving its owner's
// exclusive renewal right until then.
func (d *DAG) graceEpochs() uint64 {
	return d.mustFeeParams().GraceEpochs
}

// mustFeeParams returns the governed fee parameters and panics when a DAG
// reaches a consensus decision that depends on them with none wired.
//
// The rule it enforces is a construction rule: every path builds the DAG with
// WithFeeParams (package tests use SetFeeSystem), so reaching this panic means
// a construction path forgot the parameters — and a node that applies domain
// leases against parameters no other node uses is not degraded, it is forked.
// The accessors used to fall back to the defaults instead, which is exactly
// how the listener path stayed silently broken: built with no fee system, it
// capped lease terms from a package constant while validators capped them from
// the governed value, so the day MaxTermEpochs moves it reverts operations
// every validator applies and anchors a domain tree no one else computes.
// Rejecting the operation instead of panicking would fork just as permanently,
// only more quietly. Stopping is the only outcome that cannot corrupt the
// committed log.
func (d *DAG) mustFeeParams() *FeeParams {
	if d.feeParams == nil {
		panic("consensus: domain lease parameters read from a DAG built with no fee parameters (WithFeeParams, or SetFeeSystem in tests)")
	}

	return d.feeParams
}

// domainExpiry computes the expiry a register or renew produces and reports
// whether it is allowed. A renewal runs from the later of the current expiry
// and the current epoch, so prepaying ahead and renewing after an expiry are
// the same operation. A zero term is rejected (it would buy no lease at no
// rent), and so is any result past maxTerm — reverted, never clamped, because
// the rent charged is rate x the term the header declared.
func domainExpiry(current, epoch uint64, term uint32, maxTerm uint64) (uint64, bool) {
	if term == 0 {
		return 0, false
	}

	base := current
	if epoch > base {
		base = epoch
	}

	expiry := safeAdd(base, uint64(term))
	if expiry > safeAdd(epoch, maxTerm) {
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
