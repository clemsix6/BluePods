package consensus

import "BluePods/internal/genesis"

// domainLeafView is a staged registry leaf: what a name would hold once the
// operations validated so far in this transaction applied.
type domainLeafView struct {
	objectID Hash   // objectID is the object the name resolves to
	owner    Hash   // owner is the key holding the name's rights
	expiry   uint64 // expiry is the last epoch the lease resolves in
}

// validateDomainOp checks and stages one declared domain operation. Every rule
// reads through the staged view, so a list registers a namespace and a name
// under it in one transaction, and a rejected list stages nothing.
func (s *stagedView) validateDomainOp(sender Hash, op genesis.DeclaredOp) bool {
	if s.ds == nil {
		return false
	}

	switch op.Kind {
	case domainRegisterOp:
		return s.validateDomainRegister(sender, op)
	case domainRenewOp:
		return s.validateDomainRenew(sender, op)
	case domainUpdateOp:
		return s.validateDomainUpdate(sender, op)
	case domainTransferOp:
		return s.validateDomainTransfer(sender, op)
	case domainDeleteOp:
		return s.validateDomainDelete(sender, op)
	default:
		return false
	}
}

// validateDomainRegister checks and stages a registration (kind 2): the name
// must be well-formed and unregistered, a dotted name's namespace must be a
// current lease the sender owns, the sender must control the object being
// named, and the term must fit under the cap.
func (s *stagedView) validateDomainRegister(sender Hash, op genesis.DeclaredOp) bool {
	if !validDomainName(op.Name) {
		return false
	}

	if _, taken := s.getDomain(op.Name); taken {
		return false
	}

	if !s.ownsDomainNamespace(sender, op.Name) {
		return false
	}

	objectID, ok := hash32(op.ObjectID)
	if !ok || !s.controls(sender, objectID) {
		return false
	}

	expiry, ok := domainExpiry(0, s.epoch, op.TermEpochs, s.maxTerm)
	if !ok {
		return false
	}

	s.stageDomain(op.Name, domainLeafView{objectID: objectID, owner: sender, expiry: expiry})

	return true
}

// validateDomainRenew checks and stages a renewal (kind 3). Only the owner may
// renew, which holds during the grace window too — that is the one right an
// expired lease keeps — and the extended lease must fit under the cap.
func (s *stagedView) validateDomainRenew(sender Hash, op genesis.DeclaredOp) bool {
	leaf, ok := s.ownedDomain(sender, op.Name)
	if !ok {
		return false
	}

	expiry, ok := domainExpiry(leaf.expiry, s.epoch, op.TermEpochs, s.maxTerm)
	if !ok {
		return false
	}

	leaf.expiry = expiry
	s.stageDomain(op.Name, leaf)

	return true
}

// validateDomainUpdate checks and stages a repoint (kind 4). Only the owner may
// repoint, and only onto an object it controls: without that rule a name could
// alias a victim's object and reach it mutably through the domain-reference
// ownership exemption.
func (s *stagedView) validateDomainUpdate(sender Hash, op genesis.DeclaredOp) bool {
	leaf, ok := s.ownedDomain(sender, op.Name)
	if !ok {
		return false
	}

	objectID, ok := hash32(op.ObjectID)
	if !ok || !s.controls(sender, objectID) {
		return false
	}

	leaf.objectID = objectID
	s.stageDomain(op.Name, leaf)

	return true
}

// validateDomainTransfer checks and stages a transfer (kind 5). The new owner
// may be any key — handing over a name needs no consent, like an object
// transfer — but never the zero key, which would freeze the name until it
// expires with nobody able to renew it.
func (s *stagedView) validateDomainTransfer(sender Hash, op genesis.DeclaredOp) bool {
	leaf, ok := s.ownedDomain(sender, op.Name)
	if !ok {
		return false
	}

	owner, ok := hash32(op.Target)
	if !ok || owner == (Hash{}) {
		return false
	}

	leaf.owner = owner
	s.stageDomain(op.Name, leaf)

	return true
}

// validateDomainDelete checks and stages a deletion (kind 6): the owner drops
// the name, which is registrable again immediately.
func (s *stagedView) validateDomainDelete(sender Hash, op genesis.DeclaredOp) bool {
	if _, ok := s.ownedDomain(sender, op.Name); !ok {
		return false
	}

	s.domains[op.Name] = domainLeafView{}
	s.domainRemoved[op.Name] = true

	return true
}

// ownedDomain returns a registered name's staged leaf when sender owns it.
// Expiry is not consulted: an expired lease still answers to its owner until
// the sweep removes it.
func (s *stagedView) ownedDomain(sender Hash, name string) (domainLeafView, bool) {
	leaf, ok := s.getDomain(name)
	if !ok || leaf.owner != sender || sender == (Hash{}) {
		return domainLeafView{}, false
	}

	return leaf, true
}

// ownsDomainNamespace reports whether sender may register name: a bare root is
// first come, first served, while a dotted name needs its namespace — the name
// after the first label — to be a lease sender owns and that has not expired.
// An expired namespace mints no sub-names; grace reserves renewal, not
// continued authority.
func (s *stagedView) ownsDomainNamespace(sender Hash, name string) bool {
	parent, dotted := parentDomainName(name)
	if !dotted {
		return true
	}

	leaf, ok := s.ownedDomain(sender, parent)

	return ok && leaf.expiry >= s.epoch
}

// stageDomain records a name's staged leaf, clearing any staged removal so a
// delete followed by a registration in the same list resolves to the
// registration.
func (s *stagedView) stageDomain(name string, leaf domainLeafView) {
	s.domains[name] = leaf
	delete(s.domainRemoved, name)
}

// getDomain returns a name's staged leaf: absent when a prior operation deleted
// it, the staged leaf when one wrote it, else the committed registry's.
func (s *stagedView) getDomain(name string) (domainLeafView, bool) {
	if s.domainRemoved[name] {
		return domainLeafView{}, false
	}

	if leaf, ok := s.domains[name]; ok {
		return leaf, true
	}

	objectID, owner, expiry, ok := s.ds.DomainLeaf(name)
	if !ok {
		return domainLeafView{}, false
	}

	return domainLeafView{objectID: objectID, owner: owner, expiry: expiry}, true
}
