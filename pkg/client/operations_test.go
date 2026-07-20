package client

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"testing"

	"github.com/zeebo/blake3"

	"BluePods/internal/genesis"
	"BluePods/internal/types"
)

// =============================================================================
// Declared-Operation Builder Tests
// =============================================================================

// TestReparentOpTxTargetsObjectParent verifies a Reparent-style build carries a
// single kind-0 declared op targeting an ObjectParent, the object as the sole
// mutable_ref at its current version, no pod call, and the supplied gas coin.
func TestReparentOpTxTargetsObjectParent(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	w := &Wallet{privKey: priv, pubKey: priv.Public().(ed25519.PublicKey), coins: make(map[[32]byte]*CoinInfo)}

	var objectID, newParent, gasCoin [32]byte
	objectID[0] = 0x11
	newParent[0] = 0x22
	gasCoin[0] = 0x33

	mutableRefs := buildMutableRef(objectID, 7)
	op := reparentOpFor(objectID, objectParentKind, newParent[:])

	txBytes, _ := w.buildOpsTx(mutableRefs, gasCoin, op)

	assertPureOpsTx(t, txBytes)
	assertGasCoinTx(t, txBytes, gasCoin)
	assertSingleMutableRef(t, txBytes, objectID, 7)

	ops := extractOps(t, txBytes)
	if ops[0].Kind != reparentOpKind {
		t.Errorf("kind: got %d, want %d", ops[0].Kind, reparentOpKind)
	}
	if !bytes.Equal(ops[0].ObjectID, objectID[:]) {
		t.Errorf("object_id mismatch: got %x, want %x", ops[0].ObjectID, objectID[:])
	}
	if ops[0].TargetKind != objectParentKind {
		t.Errorf("target_kind: got %d, want %d (ObjectParent)", ops[0].TargetKind, objectParentKind)
	}
	if !bytes.Equal(ops[0].Target, newParent[:]) {
		t.Errorf("target mismatch: got %x, want %x", ops[0].Target, newParent[:])
	}
}

// TestTransferObjectOpTxTargetsKeyRoot verifies TransferObject's declared op
// (kind 0, target_kind KeyRoot) targets the recipient's key and pays gas from
// the caller-supplied singleton coin, not the transferred object.
func TestTransferObjectOpTxTargetsKeyRoot(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	w := &Wallet{privKey: priv, pubKey: priv.Public().(ed25519.PublicKey), coins: make(map[[32]byte]*CoinInfo)}

	var objectID, recipient, gasCoin [32]byte
	objectID[0] = 0x44
	recipient[0] = 0x55
	gasCoin[0] = 0x66

	mutableRefs := buildMutableRef(objectID, 2)
	op := reparentOpFor(objectID, keyRootKind, recipient[:])

	txBytes, _ := w.buildOpsTx(mutableRefs, gasCoin, op)

	assertPureOpsTx(t, txBytes)
	assertGasCoinTx(t, txBytes, gasCoin)
	assertSingleMutableRef(t, txBytes, objectID, 2)

	ops := extractOps(t, txBytes)
	if ops[0].Kind != reparentOpKind {
		t.Errorf("kind: got %d, want %d", ops[0].Kind, reparentOpKind)
	}
	if ops[0].TargetKind != keyRootKind {
		t.Errorf("target_kind: got %d, want %d (KeyRoot)", ops[0].TargetKind, keyRootKind)
	}
	if !bytes.Equal(ops[0].Target, recipient[:]) {
		t.Errorf("target mismatch: got %x, want %x", ops[0].Target, recipient[:])
	}
}

// TestCoinTransferOpTxUsesCoinAsGasAndTarget verifies a coin transfer (Wallet.
// Transfer) reparents the coin itself to the recipient's KeyRoot and pays gas
// from that same coin, mirroring the pre-existing self-paying coin convention.
func TestCoinTransferOpTxUsesCoinAsGasAndTarget(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	w := &Wallet{privKey: priv, pubKey: priv.Public().(ed25519.PublicKey), coins: make(map[[32]byte]*CoinInfo)}

	var coinID, recipient [32]byte
	coinID[0] = 0x77
	recipient[0] = 0x88

	mutableRefs := buildMutableRef(coinID, 9)
	op := reparentOpFor(coinID, keyRootKind, recipient[:])

	txBytes, _ := w.buildOpsTx(mutableRefs, coinID, op)

	assertPureOpsTx(t, txBytes)
	assertGasCoinTx(t, txBytes, coinID)
	assertSingleMutableRef(t, txBytes, coinID, 9)

	ops := extractOps(t, txBytes)
	if ops[0].Kind != reparentOpKind || ops[0].TargetKind != keyRootKind {
		t.Errorf("unexpected op: %+v", ops[0])
	}
	if !bytes.Equal(ops[0].Target, recipient[:]) {
		t.Errorf("target mismatch: got %x, want %x", ops[0].Target, recipient[:])
	}
}

// TestDeleteObjectOpTxCarriesDeleteKind verifies DeleteObject's declared op is
// kind 1, targets the object, is the transaction's sole mutable_ref, and pays
// (and is refunded to) the caller-supplied gas coin.
func TestDeleteObjectOpTxCarriesDeleteKind(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	w := &Wallet{privKey: priv, pubKey: priv.Public().(ed25519.PublicKey), coins: make(map[[32]byte]*CoinInfo)}

	var objectID, gasCoin [32]byte
	objectID[0] = 0x99
	gasCoin[0] = 0xAA

	mutableRefs := buildMutableRef(objectID, 3)
	op := genesis.DeclaredOp{Kind: deleteOpKind, ObjectID: objectID[:]}

	txBytes, _ := w.buildOpsTx(mutableRefs, gasCoin, op)

	assertPureOpsTx(t, txBytes)
	assertGasCoinTx(t, txBytes, gasCoin)
	assertSingleMutableRef(t, txBytes, objectID, 3)

	ops := extractOps(t, txBytes)
	if ops[0].Kind != deleteOpKind {
		t.Errorf("kind: got %d, want %d", ops[0].Kind, deleteOpKind)
	}
	if !bytes.Equal(ops[0].ObjectID, objectID[:]) {
		t.Errorf("object_id mismatch: got %x, want %x", ops[0].ObjectID, objectID[:])
	}
}

// TestOpsTxRoundTripsAuthenticity confirms a declared-op transaction's
// canonical body — reconstructed the same way commit-time authenticity does —
// hashes back to the declared hash and verifies the sender's signature. This
// proves the operations vector is actually covered by what the sender signs,
// not merely carried alongside it.
func TestOpsTxRoundTripsAuthenticity(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	w := &Wallet{privKey: priv, pubKey: priv.Public().(ed25519.PublicKey), coins: make(map[[32]byte]*CoinInfo)}

	var objectID, newParent, gasCoin [32]byte
	objectID[0] = 0xBB
	newParent[0] = 0xCC
	gasCoin[0] = 0xDD

	mutableRefs := buildMutableRef(objectID, 5)
	op := reparentOpFor(objectID, objectParentKind, newParent[:])

	txBytes, txHash := w.buildOpsTx(mutableRefs, gasCoin, op)

	tx := types.GetRootAsTransaction(txBytes, 0)
	if !bytes.Equal(tx.HashBytes(), txHash[:]) {
		t.Fatalf("returned hash does not match hash in FlatBuffer")
	}

	verifyOpsTxRoundTrip(t, txBytes, w.pubKey)
}

// TestSponsoredOpsTxVerifies confirms a sponsored declared-operation
// transaction — the sender authorizes a reparent, the sponsor co-signs and
// pays gas — round-trips authenticity for both signatures over the same body,
// proving the sponsored path carries operations end-to-end.
func TestSponsoredOpsTxVerifies(t *testing.T) {
	_, senderPriv, _ := ed25519.GenerateKey(rand.Reader)
	_, sponsorPriv, _ := ed25519.GenerateKey(rand.Reader)

	sender := &Wallet{privKey: senderPriv, pubKey: senderPriv.Public().(ed25519.PublicKey), coins: map[[32]byte]*CoinInfo{}}
	sponsor := &Wallet{privKey: sponsorPriv, pubKey: sponsorPriv.Public().(ed25519.PublicKey), coins: map[[32]byte]*CoinInfo{}}

	var objectID, recipient, gasCoin [32]byte
	objectID[0] = 0xEE
	recipient[0] = 0xFF
	gasCoin[0] = 0x01

	op := SponsoredOp{
		MutableRefs: buildMutableRef(objectID, 4),
		Operations:  []genesis.DeclaredOp{reparentOpFor(objectID, keyRootKind, recipient[:])},
	}

	signed := sender.SignSponsoredOp(op, sponsor.Pubkey(), gasCoin, 12)
	txBytes := assembleSponsored(sponsor, signed)

	assertPureOpsTx(t, txBytes)

	ops := extractOps(t, txBytes)
	if len(ops) != 1 || ops[0].Kind != reparentOpKind || ops[0].TargetKind != keyRootKind {
		t.Fatalf("unexpected operations: %+v", ops)
	}
	if !bytes.Equal(ops[0].Target, recipient[:]) {
		t.Errorf("target mismatch: got %x, want %x", ops[0].Target, recipient[:])
	}

	verifyOpsTxRoundTrip(t, txBytes, sender.pubKey)

	tx := types.GetRootAsTransaction(txBytes, 0)
	if !ed25519.Verify(sponsor.pubKey, tx.HashBytes(), tx.SponsorSignatureBytes()) {
		t.Error("sponsor signature does not verify")
	}
}

// =============================================================================
// Test Helpers
// =============================================================================

// assertPureOpsTx asserts the built tx carries no pod call: empty function
// name and an all-zero pod, the two conditions commit's txHasPodCall checks.
func assertPureOpsTx(t *testing.T, txBytes []byte) {
	t.Helper()

	tx := types.GetRootAsTransaction(txBytes, 0)

	if len(tx.FunctionName()) != 0 {
		t.Errorf("function_name: got %q, want empty", tx.FunctionName())
	}

	for _, b := range tx.PodBytes() {
		if b != 0 {
			t.Fatalf("pod: got non-zero byte, want all-zero")
		}
	}

	if tx.OperationsLength() == 0 {
		t.Fatalf("expected at least one declared operation")
	}
}

// assertSingleMutableRef asserts the built tx carries exactly one mutable_ref
// matching id and version.
func assertSingleMutableRef(t *testing.T, txBytes []byte, id [32]byte, version uint64) {
	t.Helper()

	tx := types.GetRootAsTransaction(txBytes, 0)

	if tx.MutableRefsLength() != 1 {
		t.Fatalf("mutable_refs: got %d, want 1", tx.MutableRefsLength())
	}

	var ref types.ObjectRef
	tx.MutableRefs(&ref, 0)

	var got [32]byte
	copy(got[:], ref.IdBytes())
	if got != id {
		t.Errorf("mutable_ref id: got %x, want %x", got[:8], id[:8])
	}
	if ref.Version() != version {
		t.Errorf("mutable_ref version: got %d, want %d", ref.Version(), version)
	}
}

// extractOps parses txBytes and extracts its declared operations, failing the
// test if there are none.
func extractOps(t *testing.T, txBytes []byte) []genesis.DeclaredOp {
	t.Helper()

	tx := types.GetRootAsTransaction(txBytes, 0)
	ops := genesis.ExtractOperations(tx)
	if len(ops) == 0 {
		t.Fatalf("expected at least one declared operation")
	}

	return ops
}

// extractMutableRefs reads a tx's mutable_refs into ObjectRefData, in order.
func extractMutableRefs(tx *types.Transaction) []genesis.ObjectRefData {
	count := tx.MutableRefsLength()
	if count == 0 {
		return nil
	}

	refs := make([]genesis.ObjectRefData, count)
	var ref types.ObjectRef

	for i := 0; i < count; i++ {
		tx.MutableRefs(&ref, i)
		copy(refs[i].ID[:], ref.IdBytes())
		refs[i].Version = ref.Version()
	}

	return refs
}

// verifyOpsTxRoundTrip reconstructs the canonical unsigned body from the
// parsed tx exactly as commit-time authenticity does (same shared genesis
// primitives, same field set) and confirms the recomputed hash matches the
// declared hash and senderPub's signature verifies against it.
func verifyOpsTxRoundTrip(t *testing.T, txBytes []byte, senderPub ed25519.PublicKey) {
	t.Helper()

	tx := types.GetRootAsTransaction(txBytes, 0)

	var pod [32]byte
	copy(pod[:], tx.PodBytes())

	sponsor := genesis.Sponsorship{FeePayer: tx.FeePayerBytes(), ValidUntil: tx.ValidUntil()}

	body := genesis.BuildUnsignedTxBytesSponsored(
		tx.SenderBytes(), pod, string(tx.FunctionName()), tx.ArgsBytes(), nil,
		tx.MaxCreateDomains(), tx.MaxGas(), tx.GasCoinBytes(),
		extractMutableRefs(tx), nil, sponsor, tx.DeletedObjectsBytes(), genesis.ExtractOperations(tx),
	)

	hash := blake3.Sum256(body)
	if !bytes.Equal(hash[:], tx.HashBytes()) {
		t.Fatalf("recomputed body hash does not match declared hash")
	}

	if !ed25519.Verify(senderPub, hash[:], tx.SignatureBytes()) {
		t.Fatalf("sender signature does not verify against recomputed hash")
	}
}
