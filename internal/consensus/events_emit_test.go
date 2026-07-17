package consensus

import (
	"bytes"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"

	flatbuffers "github.com/google/flatbuffers/go"

	"BluePods/internal/events"
	"BluePods/internal/types"
)

// =============================================================================
// Test infrastructure: capturing internal/events output.
// =============================================================================

// captureEvents swaps the default slog logger for a JSON handler writing into a
// fresh buffer, restoring the previous default on test cleanup. internal/logger
// also routes through slog.Default(), so the buffer carries both plain log lines
// (Info/Warn) and typed event records; eventsNamed filters for the latter.
func captureEvents(t *testing.T) *bytes.Buffer {
	t.Helper()

	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })

	return &buf
}

// eventsNamed decodes one JSON object per captured line and returns those whose
// reserved events.Key attribute equals name, in emission order. Non-event log
// lines (no events.Key attribute) are silently skipped.
func eventsNamed(t *testing.T, buf *bytes.Buffer, name string) []map[string]any {
	t.Helper()

	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}

		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("captured line is not JSON: %v (line=%q)", err, line)
		}

		if rec[events.Key] == name {
			out = append(out, rec)
		}
	}

	return out
}

// assertSingleTxCommitted asserts exactly one tx.committed event was captured,
// carrying the given round/success/reason and naming vertexHash — the
// propagation this task threads through executeTx as its new last parameter.
func assertSingleTxCommitted(t *testing.T, buf *bytes.Buffer, round uint64, success bool, reason string, vertexHash Hash) {
	t.Helper()

	recs := eventsNamed(t, buf, events.EvTxCommitted)
	if len(recs) != 1 {
		t.Fatalf("want 1 %s event, got %d: %v", events.EvTxCommitted, len(recs), recs)
	}

	rec := recs[0]
	if rec["round"] != float64(round) {
		t.Errorf("round = %v, want %d", rec["round"], round)
	}
	if rec["success"] != success {
		t.Errorf("success = %v, want %v", rec["success"], success)
	}
	if rec["reason"] != reason {
		t.Errorf("reason = %v, want %q", rec["reason"], reason)
	}
	if rec["vertex"] != hex.EncodeToString(vertexHash[:]) {
		t.Errorf("vertex = %v, want %s", rec["vertex"], hex.EncodeToString(vertexHash[:]))
	}
}

// buildATXWithOneProof builds a minimal ATX (zero-hash Transaction, no refs)
// carrying exactly one dummy QuorumProof entry, so atx.ProofsLength() > 0 and
// executeTx's proof gate calls the configured verifier instead of skipping it.
func buildATXWithOneProof(t *testing.T, funcName string) []byte {
	t.Helper()

	builder := flatbuffers.NewBuilder(512)

	hashVec := builder.CreateByteVector(make([]byte, 32))
	senderVec := builder.CreateByteVector(make([]byte, 32))
	podVec := builder.CreateByteVector(make([]byte, 32))
	funcNameOff := builder.CreateString(funcName)

	types.TransactionStart(builder)
	types.TransactionAddHash(builder, hashVec)
	types.TransactionAddSender(builder, senderVec)
	types.TransactionAddPod(builder, podVec)
	types.TransactionAddFunctionName(builder, funcNameOff)
	txOff := types.TransactionEnd(builder)

	types.AttestedTransactionStartObjectsVector(builder, 0)
	objVec := builder.EndVector(0)

	objIDVec := builder.CreateByteVector(make([]byte, 32))
	blsVec := builder.CreateByteVector(make([]byte, 96))
	bitmapVec := builder.CreateByteVector(make([]byte, 1))

	types.QuorumProofStart(builder)
	types.QuorumProofAddObjectId(builder, objIDVec)
	types.QuorumProofAddBlsSignature(builder, blsVec)
	types.QuorumProofAddSignerBitmap(builder, bitmapVec)
	proofOff := types.QuorumProofEnd(builder)

	types.AttestedTransactionStartProofsVector(builder, 1)
	builder.PrependUOffsetT(proofOff)
	prfVec := builder.EndVector(1)

	types.AttestedTransactionStart(builder)
	types.AttestedTransactionAddTransaction(builder, txOff)
	types.AttestedTransactionAddObjects(builder, objVec)
	types.AttestedTransactionAddProofs(builder, prfVec)
	atxOff := types.AttestedTransactionEnd(builder)

	builder.Finish(atxOff)

	return builder.FinishedBytes()
}

// stubExecutor is a minimal Executor test double whose Execute return value is
// configurable, so tests can drive both the success and error path of
// executeTx's executor-integration branch (tx.executed + the final tx.committed
// outcome).
type stubExecutor struct {
	err error // err is returned by every Execute call
}

// Execute implements Executor, always returning the configured error (nil for
// success).
func (s *stubExecutor) Execute(tx []byte) error {
	return s.err
}

// =============================================================================
// tx.committed: one test per terminal reason, plus vertex-hash propagation.
// =============================================================================

// TestExecuteTx_EmitsTxCommitted_ProofFailed checks the proof-verification
// failure site emits reason=proof_failed.
func TestExecuteTx_EmitsTxCommitted_ProofFailed(t *testing.T) {
	db := newTestStorage(t)
	validators, vs := newTestValidatorSet(1)

	dag := New(db, vs, nil, testSystemPod, 0, validators[0].privKey, nil)
	defer dag.Close()
	disableTxAuth(dag)

	dag.SetATXProofVerifier(func(*types.AttestedTransaction, uint64) error {
		return errors.New("bad proof")
	})

	atxBytes := buildATXWithOneProof(t, "test_func")
	atx := types.GetRootAsAttestedTransaction(atxBytes, 0)
	vertexHash := Hash{0xA1}

	buf := captureEvents(t)
	dag.executeTx(atx, 5, Hash{}, nil, vertexHash)

	assertSingleTxCommitted(t, buf, 5, false, "proof_failed", vertexHash)
}

// TestExecuteTx_EmitsTxCommitted_AuthenticityFailed checks the commit-time
// authenticity failure site emits reason=authenticity_failed. Mirrors
// TestExecuteTx_RejectsForged (txauth_test.go): authenticity is always on, so
// no verifier needs to be wired.
func TestExecuteTx_EmitsTxCommitted_AuthenticityFailed(t *testing.T) {
	db := newTestStorage(t)
	validators, vs := newTestValidatorSet(3)

	dag := New(db, vs, nil, testSystemPod, 0, validators[0].privKey, nil)
	defer dag.Close()

	_, priv, _ := ed25519.GenerateKey(nil)
	tx := signedTestTx(t, priv)
	_, attacker, _ := ed25519.GenerateKey(nil)
	forged := resignTx(t, tx, tx.HashBytes(), ed25519.Sign(attacker, tx.HashBytes()), tx.SenderBytes())

	atxBytes := wrapTxInATX(t, forged)
	atx := types.GetRootAsAttestedTransaction(atxBytes, 0)
	vertexHash := Hash{0xA2}

	buf := captureEvents(t)
	dag.executeTx(atx, 0, validators[0].pubKey, nil, vertexHash)

	assertSingleTxCommitted(t, buf, 0, false, "authenticity_failed", vertexHash)
}

// TestExecuteTx_EmitsTxCommitted_ExpiredSponsorship checks a sponsored tx past
// its valid_until bound emits reason=expired_sponsorship.
func TestExecuteTx_EmitsTxCommitted_ExpiredSponsorship(t *testing.T) {
	db := newTestStorage(t)
	validators, vs := newTestValidatorSet(1)

	dag := New(db, vs, nil, testSystemPod, 0, validators[0].privKey, nil)
	defer dag.Close()

	_, senderKey, _ := ed25519.GenerateKey(nil)
	_, sponsorKey, _ := ed25519.GenerateKey(nil)

	var gasCoin Hash
	gasCoin[0] = 0x77

	tx := sponsoredTestTx(t, senderKey, sponsorKey, gasCoin, 0) // valid_until=0 -> always expired
	atxBytes := wrapTxInATX(t, tx)
	atx := types.GetRootAsAttestedTransaction(atxBytes, 0)
	vertexHash := Hash{0xA3}

	buf := captureEvents(t)
	dag.executeTx(atx, 5, validators[0].pubKey, nil, vertexHash)

	assertSingleTxCommitted(t, buf, 5, false, "expired_sponsorship", vertexHash)
}

// TestExecuteTx_EmitsTxCommitted_Duplicate checks the commit-once guard's
// second occurrence of a tx emits reason=duplicate, with no d.emitTransaction
// counterpart (unchanged behavior on that branch).
func TestExecuteTx_EmitsTxCommitted_Duplicate(t *testing.T) {
	db := newTestStorage(t)
	validators, vs := newTestValidatorSet(1)

	dag := New(db, vs, nil, testSystemPod, 0, validators[0].privKey, nil)
	defer dag.Close()
	disableTxAuth(dag)

	atxBytes := buildTestATX(t, "test_func", nil, nil, 0)
	atx := types.GetRootAsAttestedTransaction(atxBytes, 0)

	// First occurrence marks the (zero) tx hash committed.
	dag.executeTx(atx, 0, Hash{}, nil, Hash{})

	vertexHash := Hash{0xA4}
	buf := captureEvents(t)
	dag.executeTx(atx, 0, Hash{}, nil, vertexHash)

	assertSingleTxCommitted(t, buf, 0, false, "duplicate", vertexHash)
}

// TestExecuteTx_EmitsTxCommitted_VersionConflict checks a stale mutable-ref
// version emits reason=version_conflict.
func TestExecuteTx_EmitsTxCommitted_VersionConflict(t *testing.T) {
	db := newTestStorage(t)
	validators, vs := newTestValidatorSet(1)

	dag := New(db, vs, nil, testSystemPod, 0, validators[0].privKey, nil)
	defer dag.Close()
	disableTxAuth(dag)

	objID := Hash{0x10}
	dag.tracker.trackObject(objID, 3, 0, 0) // current version 3

	atxBytes := buildTestATX(t, "test_func", nil, []objectRef{{id: objID, version: 0}}, 0)
	atx := types.GetRootAsAttestedTransaction(atxBytes, 0)
	vertexHash := Hash{0xA5}

	buf := captureEvents(t)
	dag.executeTx(atx, 0, Hash{}, nil, vertexHash)

	assertSingleTxCommitted(t, buf, 0, false, "version_conflict", vertexHash)
}

// TestExecuteTx_EmitsTxCommitted_FeeRejected checks a min_gas violation
// (fee deduction rejects the tx) emits reason=fee_rejected.
func TestExecuteTx_EmitsTxCommitted_FeeRejected(t *testing.T) {
	db := newTestStorage(t)
	validators, vs := newTestValidatorSet(3)

	dag := New(db, vs, nil, testSystemPod, 0, validators[0].privKey, nil)
	defer dag.Close()
	disableTxAuth(dag)

	coinStore := newMockCoinStore()
	params := DefaultFeeParams() // MinGas=100
	dag.SetFeeSystem(coinStore, &params, nil)

	sender := Hash{0x01}
	gasCoinID := Hash{0xCC}

	atxBytes := buildFeeTestATX(t, sender, gasCoinID, 50, nil) // max_gas=50 < MinGas=100
	atx := types.GetRootAsAttestedTransaction(atxBytes, 0)
	vertexHash := Hash{0xA6}

	buf := captureEvents(t)
	dag.executeTx(atx, 1, validators[0].pubKey, nil, vertexHash)

	assertSingleTxCommitted(t, buf, 1, false, "fee_rejected", vertexHash)
}

// TestExecuteTx_EmitsTxCommitted_Ownership checks a mutable_ref owned by
// someone other than the sender emits reason=ownership.
func TestExecuteTx_EmitsTxCommitted_Ownership(t *testing.T) {
	db := newTestStorage(t)
	validators, vs := newTestValidatorSet(1)

	dag := New(db, vs, nil, testSystemPod, 0, validators[0].privKey, nil)
	defer dag.Close()
	disableTxAuth(dag)

	coinStore := newMockCoinStore()
	dag.SetFeeSystem(coinStore, nil, nil)

	sender := Hash{0x01}
	owner := Hash{0x02}
	objID := Hash{0xAA}
	coinStore.SetObject(buildTestCoinObject(objID, 1000, owner, 0))

	atxBytes := buildOwnershipTestATX(t, sender, []objectRef{{id: objID, version: 0}})
	atx := types.GetRootAsAttestedTransaction(atxBytes, 0)
	vertexHash := Hash{0xA7}

	buf := captureEvents(t)
	dag.executeTx(atx, 1, validators[0].pubKey, nil, vertexHash)

	assertSingleTxCommitted(t, buf, 1, false, "ownership", vertexHash)
}

// TestExecuteTx_EmitsTxCommitted_NonHolderSkip checks the execution-sharding
// skip (committed without local execution) emits success=true, reason="".
func TestExecuteTx_EmitsTxCommitted_NonHolderSkip(t *testing.T) {
	db := newTestStorage(t)
	validators, vs := newTestValidatorSet(1)

	dag := New(db, vs, nil, testSystemPod, 0, validators[0].privKey, nil)
	defer dag.Close()
	disableTxAuth(dag)

	dag.isHolder = func(objectID [32]byte, replication uint16) bool { return false }

	otherObj := Hash{0x20}
	atxBytes := buildTestATXWithObjects(t, "test_func",
		[]objectRef{{id: otherObj, version: 0}},
		[]testObject{{id: otherObj, replication: 50}},
	)
	atx := types.GetRootAsAttestedTransaction(atxBytes, 0)
	vertexHash := Hash{0xA8}

	buf := captureEvents(t)
	dag.executeTx(atx, 1, validators[0].pubKey, nil, vertexHash)

	assertSingleTxCommitted(t, buf, 1, true, "", vertexHash)
}

// TestExecuteTx_EmitsTxCommittedAndExecuted_Success checks the final-outcome
// site after a successful executor call emits BOTH tx.executed (success=true,
// error_code="") and tx.committed (success=true, reason="").
func TestExecuteTx_EmitsTxCommittedAndExecuted_Success(t *testing.T) {
	db := newTestStorage(t)
	validators, vs := newTestValidatorSet(1)

	dag := New(db, vs, nil, testSystemPod, 0, validators[0].privKey, &stubExecutor{err: nil})
	defer dag.Close()
	disableTxAuth(dag)

	atxBytes := buildTestATX(t, "test_func", nil, nil, 0)
	atx := types.GetRootAsAttestedTransaction(atxBytes, 0)
	vertexHash := Hash{0xA9}

	buf := captureEvents(t)
	dag.executeTx(atx, 2, Hash{}, nil, vertexHash)

	execRecs := eventsNamed(t, buf, events.EvTxExecuted)
	if len(execRecs) != 1 {
		t.Fatalf("want 1 %s event, got %d: %v", events.EvTxExecuted, len(execRecs), execRecs)
	}
	if execRecs[0]["success"] != true || execRecs[0]["error_code"] != "" {
		t.Errorf("tx.executed = %v, want success=true error_code=\"\"", execRecs[0])
	}

	assertSingleTxCommitted(t, buf, 2, true, "", vertexHash)
}

// TestExecuteTx_EmitsTxCommittedAndExecuted_Error checks the final-outcome
// site after a failing executor call emits BOTH tx.executed (success=false,
// error_code=<the executor error>) and tx.committed (success=false,
// reason=execution_error).
func TestExecuteTx_EmitsTxCommittedAndExecuted_Error(t *testing.T) {
	db := newTestStorage(t)
	validators, vs := newTestValidatorSet(1)

	execErr := errors.New("boom")
	dag := New(db, vs, nil, testSystemPod, 0, validators[0].privKey, &stubExecutor{err: execErr})
	defer dag.Close()
	disableTxAuth(dag)

	atxBytes := buildTestATX(t, "test_func", nil, nil, 0)
	atx := types.GetRootAsAttestedTransaction(atxBytes, 0)
	vertexHash := Hash{0xAB}

	buf := captureEvents(t)
	dag.executeTx(atx, 3, Hash{}, nil, vertexHash)

	execRecs := eventsNamed(t, buf, events.EvTxExecuted)
	if len(execRecs) != 1 {
		t.Fatalf("want 1 %s event, got %d: %v", events.EvTxExecuted, len(execRecs), execRecs)
	}
	if execRecs[0]["success"] != false || execRecs[0]["error_code"] != execErr.Error() {
		t.Errorf("tx.executed = %v, want success=false error_code=%q", execRecs[0], execErr.Error())
	}

	assertSingleTxCommitted(t, buf, 3, false, "execution_error", vertexHash)
}

// =============================================================================
// fees.deducted
// =============================================================================

// TestDeductFees_EmitsFeesDeducted_Covered checks a fully-covered fee
// deduction emits covered=true.
func TestDeductFees_EmitsFeesDeducted_Covered(t *testing.T) {
	db := newTestStorage(t)
	validators, vs := newTestValidatorSet(3)

	dag := New(db, vs, nil, testSystemPod, 0, validators[0].privKey, nil)
	defer dag.Close()
	disableTxAuth(dag)

	coinStore := newMockCoinStore()
	params := DefaultFeeParams()
	dag.SetFeeSystem(coinStore, &params, nil)

	sender := Hash{0x01}
	gasCoinID := Hash{0xCC}
	coinStore.SetObject(buildTestCoinObject(gasCoinID, 1_000_000, sender, 0))

	atxBytes := buildFeeTestATX(t, sender, gasCoinID, 500, []uint16{0})
	atx := types.GetRootAsAttestedTransaction(atxBytes, 0)
	tx := atx.Transaction(nil)

	buf := captureEvents(t)
	_, _, proceed := dag.deductFees(tx, atx, validators[0].pubKey)
	if !proceed {
		t.Fatal("fee deduction should proceed: gas coin fully covers the fee")
	}

	recs := eventsNamed(t, buf, events.EvFeesDeducted)
	if len(recs) != 1 {
		t.Fatalf("want 1 %s event, got %d: %v", events.EvFeesDeducted, len(recs), recs)
	}
	rec := recs[0]
	if rec["covered"] != true {
		t.Errorf("covered = %v, want true", rec["covered"])
	}
	if amt, _ := rec["amount"].(float64); amt <= 0 {
		t.Errorf("amount = %v, want > 0", rec["amount"])
	}
	if rec["coin"] != hex.EncodeToString(gasCoinID[:]) {
		t.Errorf("coin = %v, want %s", rec["coin"], hex.EncodeToString(gasCoinID[:]))
	}
}

// TestDeductFees_EmitsFeesDeducted_Partial checks an under-funded gas coin
// emits covered=false with the partial (drained) amount.
func TestDeductFees_EmitsFeesDeducted_Partial(t *testing.T) {
	db := newTestStorage(t)
	validators, vs := newTestValidatorSet(3)

	dag := New(db, vs, nil, testSystemPod, 0, validators[0].privKey, nil)
	defer dag.Close()
	disableTxAuth(dag)

	coinStore := newMockCoinStore()
	params := DefaultFeeParams()
	dag.SetFeeSystem(coinStore, &params, nil)

	sender := Hash{0x01}
	gasCoinID := Hash{0xCC}
	coinStore.SetObject(buildTestCoinObject(gasCoinID, 10, sender, 0)) // far below the fee

	atxBytes := buildFeeTestATX(t, sender, gasCoinID, 500, []uint16{0})
	atx := types.GetRootAsAttestedTransaction(atxBytes, 0)
	tx := atx.Transaction(nil)

	buf := captureEvents(t)
	_, _, proceed := dag.deductFees(tx, atx, validators[0].pubKey)
	if proceed {
		t.Fatal("fee deduction should reject: gas coin does not fully cover the fee")
	}

	recs := eventsNamed(t, buf, events.EvFeesDeducted)
	if len(recs) != 1 {
		t.Fatalf("want 1 %s event, got %d: %v", events.EvFeesDeducted, len(recs), recs)
	}
	rec := recs[0]
	if rec["covered"] != false {
		t.Errorf("covered = %v, want false", rec["covered"])
	}
	if rec["amount"] != float64(10) {
		t.Errorf("amount = %v, want 10 (whole drained balance)", rec["amount"])
	}
}

// =============================================================================
// stake.bonded / stake.unbonded / stake.delegated / stake.undelegated
// =============================================================================

// TestHandleBond_EmitsStakeBonded checks a valid bond emits stake.bonded.
func TestHandleBond_EmitsStakeBonded(t *testing.T) {
	dag, _, sender, coinID := bondTestDAG(t, 1000)
	defer dag.Close()

	buf := captureEvents(t)
	if !dag.handleBond(buildStakeTx(t, sender, coinID, "bond", 300)) {
		t.Fatal("bond should be applied")
	}

	recs := eventsNamed(t, buf, events.EvStakeBonded)
	if len(recs) != 1 {
		t.Fatalf("want 1 %s event, got %d: %v", events.EvStakeBonded, len(recs), recs)
	}
	rec := recs[0]
	if rec["validator"] != hex.EncodeToString(sender[:]) {
		t.Errorf("validator = %v, want %s", rec["validator"], hex.EncodeToString(sender[:]))
	}
	if rec["coin"] != hex.EncodeToString(coinID[:]) {
		t.Errorf("coin = %v, want %s", rec["coin"], hex.EncodeToString(coinID[:]))
	}
	if rec["amount"] != float64(300) {
		t.Errorf("amount = %v, want 300", rec["amount"])
	}
}

// TestHandleUnbond_EmitsStakeUnbonded checks a valid unbond emits
// stake.unbonded.
func TestHandleUnbond_EmitsStakeUnbonded(t *testing.T) {
	dag, _, sender, coinID := bondTestDAG(t, 1000)
	defer dag.Close()

	if !dag.handleBond(buildStakeTx(t, sender, coinID, "bond", 600)) {
		t.Fatal("bond should be applied")
	}

	buf := captureEvents(t)
	if !dag.handleUnbond(buildStakeTx(t, sender, coinID, "unbond", 200)) {
		t.Fatal("unbond should be applied")
	}

	recs := eventsNamed(t, buf, events.EvStakeUnbonded)
	if len(recs) != 1 {
		t.Fatalf("want 1 %s event, got %d: %v", events.EvStakeUnbonded, len(recs), recs)
	}
	rec := recs[0]
	if rec["validator"] != hex.EncodeToString(sender[:]) {
		t.Errorf("validator = %v, want %s", rec["validator"], hex.EncodeToString(sender[:]))
	}
	if rec["amount"] != float64(200) {
		t.Errorf("amount = %v, want 200", rec["amount"])
	}
}

// TestHandleDelegate_EmitsStakeDelegated checks a valid delegation emits
// stake.delegated, naming the deterministic delegation position ID.
func TestHandleDelegate_EmitsStakeDelegated(t *testing.T) {
	dag, _, delegator, coinID := bondTestDAG(t, 1000)
	defer dag.Close()

	validator := delegator // the registered validator is the delegation target
	posID := DelegationID(delegator, validator)

	buf := captureEvents(t)
	tx := buildDelegateTx(t, delegator, coinID, validator, "delegate", 300, true)
	if !dag.handleDelegate(tx) {
		t.Fatal("delegate should be applied")
	}

	recs := eventsNamed(t, buf, events.EvStakeDelegated)
	if len(recs) != 1 {
		t.Fatalf("want 1 %s event, got %d: %v", events.EvStakeDelegated, len(recs), recs)
	}
	rec := recs[0]
	if rec["validator"] != hex.EncodeToString(validator[:]) {
		t.Errorf("validator = %v, want %s", rec["validator"], hex.EncodeToString(validator[:]))
	}
	if rec["position"] != hex.EncodeToString(posID[:]) {
		t.Errorf("position = %v, want %s", rec["position"], hex.EncodeToString(posID[:]))
	}
	if rec["amount"] != float64(300) {
		t.Errorf("amount = %v, want 300", rec["amount"])
	}
}

// TestHandleUndelegate_EmitsStakeUndelegated checks a valid undelegate emits
// stake.undelegated, naming the same deterministic delegation position ID.
func TestHandleUndelegate_EmitsStakeUndelegated(t *testing.T) {
	dag, _, delegator, coinID := bondTestDAG(t, 1000)
	defer dag.Close()

	validator := delegator
	posID := DelegationID(delegator, validator)

	if !dag.handleDelegate(buildDelegateTx(t, delegator, coinID, validator, "delegate", 400, true)) {
		t.Fatal("delegate should be applied")
	}

	buf := captureEvents(t)
	if !dag.handleUndelegate(buildDelegateTx(t, delegator, coinID, validator, "undelegate", 0, false)) {
		t.Fatal("undelegate should be applied")
	}

	recs := eventsNamed(t, buf, events.EvStakeUndelegated)
	if len(recs) != 1 {
		t.Fatalf("want 1 %s event, got %d: %v", events.EvStakeUndelegated, len(recs), recs)
	}
	rec := recs[0]
	if rec["position"] != hex.EncodeToString(posID[:]) {
		t.Errorf("position = %v, want %s", rec["position"], hex.EncodeToString(posID[:]))
	}
	if rec["amount"] != float64(400) {
		t.Errorf("amount = %v, want 400", rec["amount"])
	}
}

// =============================================================================
// epoch.validator.registered / epoch.validator.deregistered
// =============================================================================

// TestHandleRegisterValidator_EmitsValidatorRegistered checks a registration
// emits epoch.validator.registered, and that a RE-registration of the same
// pubkey fires it again (unconditional, not gated on isNew).
func TestHandleRegisterValidator_EmitsValidatorRegistered(t *testing.T) {
	db := newTestStorage(t)
	validators, vs := newTestValidatorSet(2)

	dag := New(db, vs, nil, testSystemPod, 0, validators[0].privKey, nil)
	defer dag.Close()

	newVal := newTestValidator()
	blsKey := [48]byte{0xAA}
	atxBytes := buildRegisterATX(t, newVal.pubKey, testSystemPod, "quic://new:9090", blsKey)
	tx := types.GetRootAsAttestedTransaction(atxBytes, 0).Transaction(nil)

	buf := captureEvents(t)
	dag.handleRegisterValidator(tx, 5)

	recs := eventsNamed(t, buf, events.EvValidatorRegistered)
	if len(recs) != 1 {
		t.Fatalf("want 1 %s event, got %d: %v", events.EvValidatorRegistered, len(recs), recs)
	}
	rec := recs[0]
	if rec["validator"] != hex.EncodeToString(newVal.pubKey[:]) {
		t.Errorf("validator = %v, want %s", rec["validator"], hex.EncodeToString(newVal.pubKey[:]))
	}
	if rec["quic_addr"] != "quic://new:9090" {
		t.Errorf("quic_addr = %v, want quic://new:9090", rec["quic_addr"])
	}

	// Re-registering the same validator must still fire the event.
	dag.handleRegisterValidator(tx, 6)
	if recs := eventsNamed(t, buf, events.EvValidatorRegistered); len(recs) != 2 {
		t.Fatalf("want 2 %s events after re-registration, got %d", events.EvValidatorRegistered, len(recs))
	}
}

// TestHandleDeregisterValidator_EmitsValidatorDeregistered checks a
// deregistration emits epoch.validator.deregistered.
func TestHandleDeregisterValidator_EmitsValidatorDeregistered(t *testing.T) {
	db := newTestStorage(t)
	validators, vs := newTestValidatorSet(1)

	dag := New(db, vs, nil, testSystemPod, 0, validators[0].privKey, nil)
	defer dag.Close()

	sender := validators[0].pubKey
	atxBytes := buildDeregisterATX(t, sender, testSystemPod)
	tx := types.GetRootAsAttestedTransaction(atxBytes, 0).Transaction(nil)

	buf := captureEvents(t)
	dag.handleDeregisterValidator(tx, 5)

	recs := eventsNamed(t, buf, events.EvValidatorDeregistered)
	if len(recs) != 1 {
		t.Fatalf("want 1 %s event, got %d: %v", events.EvValidatorDeregistered, len(recs), recs)
	}
	if recs[0]["validator"] != hex.EncodeToString(sender[:]) {
		t.Errorf("validator = %v, want %s", recs[0]["validator"], hex.EncodeToString(sender[:]))
	}
}

// =============================================================================
// consensus.anchor.committed / consensus.round.skipped
// =============================================================================

// TestCommitAnchorBatch_EmitsAnchorCommitted checks a successfully applied
// anchor batch emits consensus.anchor.committed with the anchor's producer and
// vertex count.
func TestCommitAnchorBatch_EmitsAnchorCommitted(t *testing.T) {
	db := newTestStorage(t)
	validators, vs := newTestValidatorSet(1)
	v := validators[0]

	dag := New(db, vs, nil, testSystemPod, 0, v.privKey, nil)
	defer dag.Close()

	const round = 3
	anchor := addDagVertex(t, dag, v, round, nil)

	buf := captureEvents(t)
	if !dag.commitAnchorBatch(round, anchor) {
		t.Fatal("commitAnchorBatch should succeed: the anchor's causal history is fully local")
	}

	recs := eventsNamed(t, buf, events.EvAnchorCommitted)
	if len(recs) != 1 {
		t.Fatalf("want 1 %s event, got %d: %v", events.EvAnchorCommitted, len(recs), recs)
	}
	rec := recs[0]
	if rec["round"] != float64(round) {
		t.Errorf("round = %v, want %d", rec["round"], round)
	}
	if rec["anchor"] != hex.EncodeToString(anchor[:]) {
		t.Errorf("anchor = %v, want %s", rec["anchor"], hex.EncodeToString(anchor[:]))
	}
	if rec["producer"] != hex.EncodeToString(v.pubKey[:]) {
		t.Errorf("producer = %v, want %s", rec["producer"], hex.EncodeToString(v.pubKey[:]))
	}
	if rec["vertices"] != float64(1) {
		t.Errorf("vertices = %v, want 1", rec["vertices"])
	}
}

// TestCommitNextRound_EmitsRoundSkipped checks a blamed (skipped) round emits
// consensus.round.skipped. Setup mirrors TestAnchorStatusDirectBlame: a
// round-N+1 stake quorum citing nothing of the designated producer.
func TestCommitNextRound_EmitsRoundSkipped(t *testing.T) {
	vals, dag := newDecisionDAG(t)

	const round = 3
	producer := designatedProducer(t, dag, vals, round)
	addDagVertex(t, dag, producer, round, nil) // present but ignored by the blame quorum

	for _, c := range others(vals, producer)[:3] {
		addDagVertex(t, dag, c, round+1, nil)
	}

	buf := captureEvents(t)

	dag.commitMu.Lock()
	dag.lastCommitted = round
	advanced := dag.commitNextRound()
	dag.commitMu.Unlock()

	if !advanced {
		t.Fatal("commitNextRound should advance past a skipped round")
	}

	recs := eventsNamed(t, buf, events.EvRoundSkipped)
	if len(recs) != 1 {
		t.Fatalf("want 1 %s event, got %d: %v", events.EvRoundSkipped, len(recs), recs)
	}
	if recs[0]["round"] != float64(round) {
		t.Errorf("round = %v, want %d", recs[0]["round"], round)
	}
}

// =============================================================================
// epoch.rewards.distributed / epoch.transitioned / supply.issued
// =============================================================================

// TestDistributeEpochRewards_EmitsRewardsDistributed checks a real
// distribution pass emits epoch.rewards.distributed with the pool and
// validator count.
func TestDistributeEpochRewards_EmitsRewardsDistributed(t *testing.T) {
	db := newTestStorage(t)
	validators, vs := newTestValidatorSet(1)
	pk := validators[0].pubKey

	store := newMockCoinStore()
	store.SetTotalSupply(1_000_000)
	rewardCoin := Hash{0xAA}
	store.SetObject(buildTestCoinObject(rewardCoin, 0, pk, 0))

	dag := New(db, vs, nil, testSystemPod, 0, validators[0].privKey, nil, WithEpochLength(10))
	params := DefaultFeeParams()
	dag.SetFeeSystem(store, &params, nil)
	defer dag.Close()

	dag.validators.SetSelfStake(pk, 100)
	dag.validators.SetRewardCoin(pk, rewardCoin)
	dag.epochRoundsProduced[pk] = 5
	dag.epochFees = 1000

	buf := captureEvents(t)
	dag.distributeEpochRewards(0)

	recs := eventsNamed(t, buf, events.EvRewardsDistributed)
	if len(recs) != 1 {
		t.Fatalf("want 1 %s event, got %d: %v", events.EvRewardsDistributed, len(recs), recs)
	}
	rec := recs[0]
	if rec["epoch"] != float64(dag.Epoch()) {
		t.Errorf("epoch = %v, want %d (the ending epoch)", rec["epoch"], dag.Epoch())
	}
	if rec["pool"] != float64(1000) {
		t.Errorf("pool = %v, want 1000", rec["pool"])
	}
	if rec["validators"] != float64(1) {
		t.Errorf("validators = %v, want 1", rec["validators"])
	}
}

// TestDistributeEpochRewards_NoFeeSystem_NoRewardsEvent checks the early
// return (no fee system wired) does NOT emit epoch.rewards.distributed.
func TestDistributeEpochRewards_NoFeeSystem_NoRewardsEvent(t *testing.T) {
	db := newTestStorage(t)
	validators, vs := newTestValidatorSet(1)

	dag := New(db, vs, nil, testSystemPod, 0, validators[0].privKey, nil, WithEpochLength(10))
	defer dag.Close()
	// No SetFeeSystem call: feeParams and coinStore stay nil.

	buf := captureEvents(t)
	dag.distributeEpochRewards(0)

	if recs := eventsNamed(t, buf, events.EvRewardsDistributed); len(recs) != 0 {
		t.Fatalf("no fee system wired: want 0 %s events, got %d", events.EvRewardsDistributed, len(recs))
	}
}

// TestTransitionEpoch_EmitsEpochTransitioned checks a boundary transition
// emits epoch.transitioned with the incremented epoch and the hex-encoded
// added/removed validator sets.
func TestTransitionEpoch_EmitsEpochTransitioned(t *testing.T) {
	db := newTestStorage(t)
	validators, vs := newTestValidatorSet(4)

	dag := New(db, vs, nil, testSystemPod, 0, validators[0].privKey, nil, WithEpochLength(10))
	defer dag.Close()

	added := validators[1].pubKey
	removed := validators[3].pubKey
	dag.epochAdditions = append(dag.epochAdditions, added)
	dag.pendingRemovals[removed] = true

	buf := captureEvents(t)
	dag.transitionEpoch(10)

	recs := eventsNamed(t, buf, events.EvEpochTransitioned)
	if len(recs) != 1 {
		t.Fatalf("want 1 %s event, got %d: %v", events.EvEpochTransitioned, len(recs), recs)
	}
	rec := recs[0]
	if rec["epoch"] != float64(1) {
		t.Errorf("epoch = %v, want 1 (incremented)", rec["epoch"])
	}

	addedList, _ := rec["added"].([]any)
	if len(addedList) != 1 || addedList[0] != hex.EncodeToString(added[:]) {
		t.Errorf("added = %v, want [%s]", rec["added"], hex.EncodeToString(added[:]))
	}

	removedList, _ := rec["removed"].([]any)
	if len(removedList) != 1 || removedList[0] != hex.EncodeToString(removed[:]) {
		t.Errorf("removed = %v, want [%s]", rec["removed"], hex.EncodeToString(removed[:]))
	}
}

// TestRunThermostat_EmitsSupplyIssued checks a distributable epoch with
// bonded stake below the target band emits supply.issued with the minted
// amount and the (post-adjustment) issuance rate.
func TestRunThermostat_EmitsSupplyIssued(t *testing.T) {
	dag, _, pk := thermostatTestDAG(t, 1_000_000)
	defer dag.Close()

	dag.validators.SetSelfStake(pk, 10_000) // far below the target band
	dag.epochRoundsProduced[pk] = 5

	buf := captureEvents(t)
	issuance := dag.runThermostat(true)
	if issuance == 0 {
		t.Fatal("test misconfigured: expected nonzero issuance")
	}

	recs := eventsNamed(t, buf, events.EvSupplyIssued)
	if len(recs) != 1 {
		t.Fatalf("want 1 %s event, got %d: %v", events.EvSupplyIssued, len(recs), recs)
	}
	rec := recs[0]
	if rec["epoch"] != float64(dag.Epoch()) {
		t.Errorf("epoch = %v, want %d", rec["epoch"], dag.Epoch())
	}
	if rec["amount"] != float64(issuance) {
		t.Errorf("amount = %v, want %d", rec["amount"], issuance)
	}
	if rec["rate"] != float64(dag.issuanceRateMicro) {
		t.Errorf("rate = %v, want %d", rec["rate"], dag.issuanceRateMicro)
	}
}

// TestRunThermostat_ZeroIssuance_NoSupplyEvent checks a non-distributable
// epoch (nothing minted) does NOT emit supply.issued.
func TestRunThermostat_ZeroIssuance_NoSupplyEvent(t *testing.T) {
	dag, _, pk := thermostatTestDAG(t, 1_000_000)
	defer dag.Close()

	dag.validators.SetSelfStake(pk, 10_000)

	buf := captureEvents(t)
	issuance := dag.runThermostat(false) // not distributable -> mints nothing
	if issuance != 0 {
		t.Fatalf("distributable=false must mint nothing, got %d", issuance)
	}

	if recs := eventsNamed(t, buf, events.EvSupplyIssued); len(recs) != 0 {
		t.Fatalf("want 0 %s events when nothing minted, got %d", events.EvSupplyIssued, len(recs))
	}
}
