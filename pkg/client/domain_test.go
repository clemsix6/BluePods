package client

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"testing"

	"BluePods/internal/genesis"
	"BluePods/internal/types"
)

// TestDomainRegisterOpTxCarriesNameObjectAndTerm verifies a domain_register
// build carries a single kind-2 declared op with the name, object, and term,
// no pod call, the supplied gas coin, and no mutable ref — a domain op moves
// no object version, unlike reparent/delete.
func TestDomainRegisterOpTxCarriesNameObjectAndTerm(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	w := &Wallet{privKey: priv, pubKey: priv.Public().(ed25519.PublicKey), coins: make(map[[32]byte]*CoinInfo)}

	var objectID, gasCoin [32]byte
	objectID[0] = 0x11
	gasCoin[0] = 0x22

	op := genesis.DeclaredOp{Kind: domainRegisterOpKind, Name: "alpha", ObjectID: objectID[:], TermEpochs: 10}
	txBytes, _ := w.buildOpsTx(nil, gasCoin, op)

	assertPureOpsTx(t, txBytes)
	assertGasCoinTx(t, txBytes, gasCoin)
	assertNoMutableRefs(t, txBytes)

	ops := extractOps(t, txBytes)
	if ops[0].Kind != domainRegisterOpKind {
		t.Errorf("kind: got %d, want %d", ops[0].Kind, domainRegisterOpKind)
	}
	if ops[0].Name != "alpha" {
		t.Errorf("name: got %q, want alpha", ops[0].Name)
	}
	if !bytes.Equal(ops[0].ObjectID, objectID[:]) {
		t.Errorf("object_id mismatch: got %x, want %x", ops[0].ObjectID, objectID[:])
	}
	if ops[0].TermEpochs != 10 {
		t.Errorf("term_epochs: got %d, want 10", ops[0].TermEpochs)
	}
}

// TestDomainRenewOpTxCarriesNameAndTerm verifies a domain_renew build carries
// a single kind-3 declared op with the name and term, no object_id or target
// (a renewal touches neither), and no mutable ref.
func TestDomainRenewOpTxCarriesNameAndTerm(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	w := &Wallet{privKey: priv, pubKey: priv.Public().(ed25519.PublicKey), coins: make(map[[32]byte]*CoinInfo)}

	var gasCoin [32]byte
	gasCoin[0] = 0x33

	op := genesis.DeclaredOp{Kind: domainRenewOpKind, Name: "alpha", TermEpochs: 5}
	txBytes, _ := w.buildOpsTx(nil, gasCoin, op)

	assertPureOpsTx(t, txBytes)
	assertGasCoinTx(t, txBytes, gasCoin)
	assertNoMutableRefs(t, txBytes)

	ops := extractOps(t, txBytes)
	if ops[0].Kind != domainRenewOpKind {
		t.Errorf("kind: got %d, want %d", ops[0].Kind, domainRenewOpKind)
	}
	if ops[0].Name != "alpha" {
		t.Errorf("name: got %q, want alpha", ops[0].Name)
	}
	if ops[0].TermEpochs != 5 {
		t.Errorf("term_epochs: got %d, want 5", ops[0].TermEpochs)
	}
}

// TestDomainTransferOpTxCarriesNameAndNewOwner verifies a domain_transfer
// build carries a single kind-5 declared op with the name and the new owner
// as target, and no mutable ref.
func TestDomainTransferOpTxCarriesNameAndNewOwner(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	w := &Wallet{privKey: priv, pubKey: priv.Public().(ed25519.PublicKey), coins: make(map[[32]byte]*CoinInfo)}

	var newOwner, gasCoin [32]byte
	newOwner[0] = 0x44
	gasCoin[0] = 0x55

	op := genesis.DeclaredOp{Kind: domainTransferOpKind, Name: "alpha", Target: newOwner[:]}
	txBytes, _ := w.buildOpsTx(nil, gasCoin, op)

	assertPureOpsTx(t, txBytes)
	assertGasCoinTx(t, txBytes, gasCoin)
	assertNoMutableRefs(t, txBytes)

	ops := extractOps(t, txBytes)
	if ops[0].Kind != domainTransferOpKind {
		t.Errorf("kind: got %d, want %d", ops[0].Kind, domainTransferOpKind)
	}
	if ops[0].Name != "alpha" {
		t.Errorf("name: got %q, want alpha", ops[0].Name)
	}
	if !bytes.Equal(ops[0].Target, newOwner[:]) {
		t.Errorf("target mismatch: got %x, want %x", ops[0].Target, newOwner[:])
	}
}

// TestDomainRegisterOpTxRoundTripsAuthenticity confirms a domain-op
// transaction's canonical body — reconstructed the same way commit-time
// authenticity does — hashes back to the declared hash and verifies the
// sender's signature, proving the domain fields (name, object, term) are
// actually covered by what the sender signs.
func TestDomainRegisterOpTxRoundTripsAuthenticity(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	w := &Wallet{privKey: priv, pubKey: priv.Public().(ed25519.PublicKey), coins: make(map[[32]byte]*CoinInfo)}

	var objectID, gasCoin [32]byte
	objectID[0] = 0x66
	gasCoin[0] = 0x77

	op := genesis.DeclaredOp{Kind: domainRegisterOpKind, Name: "beta", ObjectID: objectID[:], TermEpochs: 20}
	txBytes, txHash := w.buildOpsTx(nil, gasCoin, op)

	tx := types.GetRootAsTransaction(txBytes, 0)
	if !bytes.Equal(tx.HashBytes(), txHash[:]) {
		t.Fatalf("returned hash does not match hash in FlatBuffer")
	}

	verifyOpsTxRoundTrip(t, txBytes, w.pubKey)
}

// assertNoMutableRefs asserts the built tx carries zero mutable refs: a
// domain op names its target through the op's own fields, never through a
// version-tracked ref.
func assertNoMutableRefs(t *testing.T, txBytes []byte) {
	t.Helper()

	tx := types.GetRootAsTransaction(txBytes, 0)
	if n := tx.MutableRefsLength(); n != 0 {
		t.Errorf("mutable_refs: got %d, want 0 (domain ops touch no object version)", n)
	}
}
