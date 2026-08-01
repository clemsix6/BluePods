package client

import (
	"fmt"

	"BluePods/internal/genesis"
)

const (
	// domainRegisterOpKind mirrors internal/consensus's domainRegisterOp:
	// DeclaredOp.kind=2 binds an unregistered name to an object for a rental
	// term.
	domainRegisterOpKind byte = 2

	// domainRenewOpKind mirrors internal/consensus's domainRenewOp:
	// DeclaredOp.kind=3 extends a name's lease, leaving its object and owner
	// untouched.
	domainRenewOpKind byte = 3

	// domainTransferOpKind mirrors internal/consensus's domainTransferOp:
	// DeclaredOp.kind=5 hands a name to a new owner, leaving its object and
	// lease untouched.
	domainTransferOpKind byte = 5
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
