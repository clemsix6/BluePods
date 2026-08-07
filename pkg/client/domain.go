package client

import (
	"fmt"
	"time"

	"BluePods/internal/genesis"
	"BluePods/internal/network"
)

// domainRegisterCommitTimeout bounds how long RegisterNewObjectDomain's saga
// waits for the creating transaction to commit before giving up on the
// register half.
const domainRegisterCommitTimeout = 30 * time.Second

const (
	// domainRegisterOpKind mirrors internal/consensus's domainRegisterOp:
	// DeclaredOp.kind=2 binds an unregistered name to an object for a rental
	// term.
	domainRegisterOpKind byte = 2

	// domainRenewOpKind mirrors internal/consensus's domainRenewOp:
	// DeclaredOp.kind=3 extends a name's lease, leaving its object and owner
	// untouched.
	domainRenewOpKind byte = 3

	// domainUpdateOpKind mirrors internal/consensus's domainUpdateOp:
	// DeclaredOp.kind=4 repoints a name at another object, leaving its owner
	// and lease untouched.
	domainUpdateOpKind byte = 4

	// domainTransferOpKind mirrors internal/consensus's domainTransferOp:
	// DeclaredOp.kind=5 hands a name to a new owner, leaving its object and
	// lease untouched.
	domainTransferOpKind byte = 5

	// domainDeleteOpKind mirrors internal/consensus's domainDeleteOp:
	// DeclaredOp.kind=6 removes a name from the registry.
	domainDeleteOpKind byte = 6
)

// DomainRegister binds name to objectID under the sender's ownership for
// termEpochs, via the protocol's declared domain_register operation. The
// sender must control objectID; a dotted name additionally requires the
// sender to own its namespace (everything after the first label). Domain ops
// carry no object refs and touch no object version — the name and its lease
// live in the domain registry, not the tracker. The caller supplies an owned
// singleton coin (gasCoinID) to pay gas and the term's rent. Returns the
// transaction hash.
func (w *Wallet) DomainRegister(c *Client, name string, objectID [32]byte, termEpochs uint32, gasCoinID [32]byte) ([32]byte, error) {
	op := genesis.DeclaredOp{
		Kind:       domainRegisterOpKind,
		Name:       name,
		ObjectID:   objectID[:],
		TermEpochs: termEpochs,
	}

	txBytes, txHash := w.buildOpsTx(nil, gasCoinID, op)

	if err := c.submit(txBytes); err != nil {
		return [32]byte{}, fmt.Errorf("submit domain_register tx:\n%w", err)
	}

	return txHash, nil
}

// DomainRenew extends name's lease by termEpochs from its current expiry (or
// from the current epoch, whichever is later), via the protocol's declared
// domain_renew operation. Only the name's owner may renew, which holds during
// the grace window past expiry too. The caller supplies an owned singleton
// coin (gasCoinID) to pay gas and the term's rent. Returns the transaction
// hash.
func (w *Wallet) DomainRenew(c *Client, name string, termEpochs uint32, gasCoinID [32]byte) ([32]byte, error) {
	op := genesis.DeclaredOp{
		Kind:       domainRenewOpKind,
		Name:       name,
		TermEpochs: termEpochs,
	}

	txBytes, txHash := w.buildOpsTx(nil, gasCoinID, op)

	if err := c.submit(txBytes); err != nil {
		return [32]byte{}, fmt.Errorf("submit domain_renew tx:\n%w", err)
	}

	return txHash, nil
}

// DomainTransfer hands name to newOwner, via the protocol's declared
// domain_transfer operation. Only the name's current owner may transfer it;
// the new owner may be any key and need not consent, like an object transfer.
// The caller supplies an owned singleton coin (gasCoinID) to pay gas. Returns
// the transaction hash.
func (w *Wallet) DomainTransfer(c *Client, name string, newOwner [32]byte, gasCoinID [32]byte) ([32]byte, error) {
	op := genesis.DeclaredOp{
		Kind:   domainTransferOpKind,
		Name:   name,
		Target: newOwner[:],
	}

	txBytes, txHash := w.buildOpsTx(nil, gasCoinID, op)

	if err := c.submit(txBytes); err != nil {
		return [32]byte{}, fmt.Errorf("submit domain_transfer tx:\n%w", err)
	}

	return txHash, nil
}

// DomainUpdate repoints name at a different object, via the protocol's
// declared domain_update operation. Only the name's owner may repoint it, and
// only onto an object the sender controls (the same rule domain_register
// applies): without it, a name could alias a victim's object and reach it
// mutably through the domain-reference ownership exemption. The lease and
// owner are untouched. The caller supplies an owned singleton coin
// (gasCoinID) to pay gas. Returns the transaction hash.
func (w *Wallet) DomainUpdate(c *Client, name string, objectID [32]byte, gasCoinID [32]byte) ([32]byte, error) {
	op := domainUpdateOpFor(name, objectID)

	txBytes, txHash := w.buildOpsTx(nil, gasCoinID, op)

	if err := c.submit(txBytes); err != nil {
		return [32]byte{}, fmt.Errorf("submit domain_update tx:\n%w", err)
	}

	return txHash, nil
}

// DomainDelete removes name from the registry, via the protocol's declared
// domain_delete operation. Only the name's owner may delete it. The rent
// already consumed is not refunded — a lease buys epochs, not an asset — and
// the name becomes registrable again immediately. The caller supplies an
// owned singleton coin (gasCoinID) to pay gas. Returns the transaction hash.
func (w *Wallet) DomainDelete(c *Client, name string, gasCoinID [32]byte) ([32]byte, error) {
	op := domainDeleteOpFor(name)

	txBytes, txHash := w.buildOpsTx(nil, gasCoinID, op)

	if err := c.submit(txBytes); err != nil {
		return [32]byte{}, fmt.Errorf("submit domain_delete tx:\n%w", err)
	}

	return txHash, nil
}

// domainUpdateOpFor builds a kind-4 declared operation repointing name at
// objectID, factored out of DomainUpdate so tests can exercise the exact
// op-construction logic the real call runs, without a live node to submit
// against.
func domainUpdateOpFor(name string, objectID [32]byte) genesis.DeclaredOp {
	return genesis.DeclaredOp{
		Kind:     domainUpdateOpKind,
		Name:     name,
		ObjectID: objectID[:],
	}
}

// domainDeleteOpFor builds a kind-6 declared operation removing name from the
// registry, factored out of DomainDelete for the same reason
// domainUpdateOpFor is factored out of DomainUpdate.
func domainDeleteOpFor(name string) genesis.DeclaredOp {
	return genesis.DeclaredOp{Kind: domainDeleteOpKind, Name: name}
}

// RegisterNewObjectDomain runs the spec §8 two-transaction saga for naming a
// brand-new object: create it, wait for that transaction to commit, register
// name against the created ID, then wait for THAT transaction to commit too.
// Both waits are required, not a caution — spec §8's "wait for commit" holds
// for either half of the saga, since domain_register can itself revert at
// commit (the name was already taken, the term exceeds the registry's cap, or
// a dotted name's namespace is not owned by the sender) after the object has
// already been created and paid for; a caller that only waited on the create
// half could be told the saga "succeeded" while the name never bound. The
// either-ops-or-pod rule forbids folding the create pod call and the declared
// register op into a single transaction, so there is no way to shrink this to
// one round trip. A failure creating the object leaves nothing registered; a
// failure at either wait or while submitting the register call leaves the
// object created but unnamed (or its registration unconfirmed), and the
// caller can retry DomainRegister directly with the returned object ID rather
// than re-running the whole saga. Returns the created object ID and the
// register transaction's hash.
func (w *Wallet) RegisterNewObjectDomain(c *Client, name string, replication uint16, metadata []byte, termEpochs uint32, gasCoinID [32]byte) ([32]byte, [32]byte, error) {
	objectID, createHash, err := w.CreateObject(c, replication, metadata, gasCoinID)
	if err != nil {
		return [32]byte{}, [32]byte{}, fmt.Errorf("create object:\n%w", err)
	}

	createStatus, err := c.WaitForTx(createHash, domainRegisterCommitTimeout)
	if err != nil {
		return objectID, [32]byte{}, fmt.Errorf("wait for object creation to commit:\n%w", err)
	}

	if createStatus.State != network.TxStateFinalized {
		return objectID, [32]byte{}, fmt.Errorf("object creation did not finalize (state %d, reason %d)", createStatus.State, createStatus.Reason)
	}

	registerHash, err := w.DomainRegister(c, name, objectID, termEpochs, gasCoinID)
	if err != nil {
		return objectID, [32]byte{}, fmt.Errorf("register domain on already-created object %x:\n%w", objectID[:8], err)
	}

	registerStatus, err := c.WaitForTx(registerHash, domainRegisterCommitTimeout)
	if err != nil {
		return objectID, registerHash, fmt.Errorf("wait for domain registration to commit on already-created object %x (retry DomainRegister directly rather than re-running the saga):\n%w", objectID[:8], err)
	}

	if registerStatus.State != network.TxStateFinalized {
		return objectID, registerHash, fmt.Errorf("domain registration did not finalize on already-created object %x (state %d, reason %d; retry DomainRegister directly rather than re-running the saga)", objectID[:8], registerStatus.State, registerStatus.Reason)
	}

	return objectID, registerHash, nil
}
