package consensus

import (
	"crypto/ed25519"
	"testing"

	flatbuffers "github.com/google/flatbuffers/go"
	"github.com/zeebo/blake3"

	"BluePods/internal/genesis"
	"BluePods/internal/types"
)

// signedTestTx builds a correctly-signed Transaction whose canonical body hash
// and Ed25519 signature verify against the given key, using the SAME builder
// (genesis.BuildUnsignedTxBytes) the production path recomputes against.
func signedTestTx(t *testing.T, priv ed25519.PrivateKey) *types.Transaction {
	t.Helper()

	pub := priv.Public().(ed25519.PublicKey)
	atxBytes := genesis.BuildAttestedTx(priv, testSystemPod, "noop", []byte("x"), nil, 0, 0, nil)

	atx := types.GetRootAsAttestedTransaction(atxBytes, 0)
	tx := atx.Transaction(nil)

	// Sanity: the builder produced a body that matches its own hash for this pubkey.
	body := genesis.BuildUnsignedTxBytes(pub, testSystemPod, "noop", []byte("x"), nil, 0, 0, nil)
	want := blake3.Sum256(body)
	if string(tx.HashBytes()) != string(want[:]) {
		t.Fatalf("test setup: builder hash does not match recomputed body hash")
	}

	return tx
}

// TestVerifyTxAuthenticity_ValidPasses confirms a correctly-signed transaction
// is accepted by the commit-time authenticity check.
func TestVerifyTxAuthenticity_ValidPasses(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(nil)
	tx := signedTestTx(t, priv)

	if err := verifyTxAuthenticity(tx); err != nil {
		t.Fatalf("valid signed tx rejected: %v", err)
	}
}

// TestVerifyTxAuthenticity_ForgedSignature confirms a transaction whose
// signature does not verify against sender over the recomputed body hash is
// rejected.
func TestVerifyTxAuthenticity_ForgedSignature(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(nil)
	tx := signedTestTx(t, priv)

	// Re-sign with a DIFFERENT key while keeping the original sender → forgery.
	_, attacker, _ := ed25519.GenerateKey(nil)
	forged := resignTx(t, tx, tx.HashBytes(), ed25519.Sign(attacker, tx.HashBytes()), tx.SenderBytes())

	if err := verifyTxAuthenticity(forged); err == nil {
		t.Fatal("forged-signature tx accepted, want rejection")
	}
}

// TestVerifyTxAuthenticity_TamperedHash confirms a transaction whose hash field
// does not equal the recomputed body hash is rejected.
func TestVerifyTxAuthenticity_TamperedHash(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(nil)
	tx := signedTestTx(t, priv)

	wrong := make([]byte, 32)
	wrong[0] = 0xFF
	tampered := resignTx(t, tx, wrong, tx.SignatureBytes(), tx.SenderBytes())

	if err := verifyTxAuthenticity(tampered); err == nil {
		t.Fatal("tampered-hash tx accepted, want rejection")
	}
}

// TestExecuteTx_RejectsForged confirms executeTx rejects a forged tx
// (success=false, no fees) without poisoning the tracker. Commit-time
// authenticity is always on, so no verifier needs to be wired.
func TestExecuteTx_RejectsForged(t *testing.T) {
	db := newTestStorage(t)
	validators, vs := newTestValidatorSet(3)
	mock := &mockBroadcaster{}

	dag := New(db, vs, mock, testSystemPod, 0, validators[0].privKey, nil)
	defer dag.Close()

	_, priv, _ := ed25519.GenerateKey(nil)
	tx := signedTestTx(t, priv)
	_, attacker, _ := ed25519.GenerateKey(nil)
	forged := resignTx(t, tx, tx.HashBytes(), ed25519.Sign(attacker, tx.HashBytes()), tx.SenderBytes())

	atxBytes := wrapTxInATX(t, forged)
	atx := types.GetRootAsAttestedTransaction(atxBytes, 0)

	feeSplit := dag.executeTx(atx, 0, validators[0].pubKey, nil, Hash{})

	if feeSplit.Total != 0 {
		t.Errorf("forged tx produced fees: total=%d", feeSplit.Total)
	}

	committed := <-dag.Committed()
	if committed.Success {
		t.Error("forged tx emitted success=true, want false")
	}
}

// resignTx rebuilds a Transaction with the given hash, signature, and sender,
// preserving the rest of the body. Used to forge or tamper a test tx.
func resignTx(t *testing.T, tx *types.Transaction, hash, sig, sender []byte) *types.Transaction {
	t.Helper()

	builder := flatbuffers.NewBuilder(512)

	hashVec := builder.CreateByteVector(hash)
	sigVec := builder.CreateByteVector(sig)
	senderVec := builder.CreateByteVector(sender)
	podVec := builder.CreateByteVector(tx.PodBytes())
	funcOff := builder.CreateString(string(tx.FunctionName()))
	argsVec := builder.CreateByteVector(tx.ArgsBytes())

	types.TransactionStart(builder)
	types.TransactionAddHash(builder, hashVec)
	types.TransactionAddSender(builder, senderVec)
	types.TransactionAddSignature(builder, sigVec)
	types.TransactionAddPod(builder, podVec)
	types.TransactionAddFunctionName(builder, funcOff)
	types.TransactionAddArgs(builder, argsVec)
	off := types.TransactionEnd(builder)
	builder.Finish(off)

	return types.GetRootAsTransaction(builder.FinishedBytes(), 0)
}

// wrapTxInATX wraps a raw Transaction in an AttestedTransaction with empty
// objects and proofs, returning a standalone buffer.
func wrapTxInATX(t *testing.T, tx *types.Transaction) []byte {
	t.Helper()

	builder := flatbuffers.NewBuilder(512)
	txOff := genesis.RebuildTxInBuilder(builder, tx)

	types.AttestedTransactionStartObjectsVector(builder, 0)
	objVec := builder.EndVector(0)
	types.AttestedTransactionStartProofsVector(builder, 0)
	prfVec := builder.EndVector(0)

	types.AttestedTransactionStart(builder)
	types.AttestedTransactionAddTransaction(builder, txOff)
	types.AttestedTransactionAddObjects(builder, objVec)
	types.AttestedTransactionAddProofs(builder, prfVec)
	atxOff := types.AttestedTransactionEnd(builder)
	builder.Finish(atxOff)

	return builder.FinishedBytes()
}
