package consensus

import (
	"testing"

	"BluePods/internal/genesis"
	"BluePods/internal/types"
)

// feeDAG builds an ops-test DAG with a mock coin store and default fee params
// wired, so applyReparent has a consensus-side store to rewrite and the gas and
// mutable-ref ownership checks have bodies to read.
func feeDAG(t *testing.T) (*DAG, *mockCoinStore) {
	t.Helper()

	dag := opsTestDAG(t)
	coinStore := newMockCoinStore()
	coinStore.SetTotalSupply(1_000_000_000)
	coinStore.SetCoinsTotal(1_000_000_000)
	params := DefaultFeeParams()
	dag.SetFeeSystem(coinStore, &params, func([32]byte, int) []Hash { return nil })

	return dag, coinStore
}

// TestApplyReparent_RewritesSingletonCoinBodyOwner asserts a committed transfer
// (a reparent to a new KeyRoot) rewrites the singleton coin's stored body owner
// bytes to the recipient, read back through the same store the gas check uses.
func TestApplyReparent_RewritesSingletonCoinBodyOwner(t *testing.T) {
	dag, coinStore := feeDAG(t)
	defer dag.Close()

	sender := Hash{0xA1}
	recipient := Hash{0xB2}
	coin := Hash{0xC1}

	coinStore.SetObject(buildTestCoinObject(coin, 500_000, sender, 0))
	dag.tracker.trackObject(coin, 0, 0, 0, keyRootKind, sender)

	ops := []genesis.DeclaredOp{{Kind: reparentOp, ObjectID: coin[:], TargetKind: keyRootKind, Target: recipient[:]}}
	tx := opsTx(t, sender, "", nil, []objectRef{{id: coin, version: 0}}, ops, Hash{0x01})

	if !dag.handleDeclaredOps(tx) {
		t.Fatal("transfer of a controlled coin must succeed")
	}

	owner, err := readCoinOwner(coinStore.GetObject(coin))
	if err != nil {
		t.Fatalf("read coin owner: %v", err)
	}
	if owner != recipient {
		t.Fatalf("coin body owner = %x, want recipient %x", owner[:8], recipient[:8])
	}

	// Balance and other fields are preserved by the rewrite.
	if bal, _ := readCoinBalance(coinStore.GetObject(coin)); bal != 500_000 {
		t.Errorf("coin balance changed by the owner rewrite: %d, want 500000", bal)
	}
}

// TestApplyReparent_RewritesBodyOwnerToObjectParent asserts a reparent under an
// ObjectParent rewrites the body owner bytes to the new parent object's ID and
// sets the body parent_kind to ObjectParent, keeping the body consistent with
// the tracker's new parent reference.
func TestApplyReparent_RewritesBodyOwnerToObjectParent(t *testing.T) {
	dag, coinStore := feeDAG(t)
	defer dag.Close()

	sender := Hash{0xA1}
	obj := Hash{0xC2}
	parent := Hash{0xD3}

	coinStore.SetObject(buildTestCoinObject(obj, 0, sender, 0))
	dag.tracker.trackObject(obj, 0, 0, 0, keyRootKind, sender)
	dag.tracker.trackObject(parent, 0, 0, 0, keyRootKind, sender)

	ops := []genesis.DeclaredOp{{Kind: reparentOp, ObjectID: obj[:], TargetKind: objectParentKind, Target: parent[:]}}
	tx := opsTx(t, sender, "", nil, []objectRef{{id: obj, version: 0}}, ops, Hash{0x02})

	if !dag.handleDeclaredOps(tx) {
		t.Fatal("reparent under a controlled ObjectParent must succeed")
	}

	data := coinStore.GetObject(obj)
	owner, err := readCoinOwner(data)
	if err != nil {
		t.Fatalf("read body owner: %v", err)
	}
	if owner != parent {
		t.Fatalf("body owner = %x, want new parent %x", owner[:8], parent[:8])
	}
	if k := types.GetRootAsObject(data, 0).ParentKind(); k != objectParentKind {
		t.Errorf("body parent_kind = %d, want ObjectParent (%d)", k, objectParentKind)
	}
}

// TestExecuteTx_TransferThenNewOwnerPaysGas is the killer case for gas
// ownership: after a coin is transferred A -> B, B can pay gas with it (fee
// deduction succeeds) and the old owner A can no longer use it as a gas coin.
func TestExecuteTx_TransferThenNewOwnerPaysGas(t *testing.T) {
	dag, coinStore := feeDAG(t)
	defer dag.Close()

	a := Hash{0xA1}
	b := Hash{0xB2}
	coin := Hash{0xC1}

	coinStore.SetObject(buildTestCoinObject(coin, 1_000_000, a, 0))
	dag.tracker.trackObject(coin, 0, 0, 0, keyRootKind, a)

	// Transfer the coin A -> B (no fees on the direct handleDeclaredOps path).
	ops := []genesis.DeclaredOp{{Kind: reparentOp, ObjectID: coin[:], TargetKind: keyRootKind, Target: b[:]}}
	transfer := opsTx(t, a, "", nil, []objectRef{{id: coin, version: 0}}, ops, Hash{0x10})
	if !dag.handleDeclaredOps(transfer) {
		t.Fatal("transfer must succeed")
	}

	// B pays gas with the coin: fee deduction proceeds.
	atxB := types.GetRootAsAttestedTransaction(buildFeeTestATX(t, b, coin, 100, nil), 0)
	if _, _, proceed := dag.deductFees(atxB.Transaction(nil), atxB, Hash{0xEE}); !proceed {
		t.Error("new owner B could not pay gas with the transferred coin")
	}

	// The old owner A can no longer use it as a gas coin.
	atxA := types.GetRootAsAttestedTransaction(buildFeeTestATX(t, a, coin, 100, nil), 0)
	if _, _, proceed := dag.deductFees(atxA.Transaction(nil), atxA, Hash{0xEE}); proceed {
		t.Error("old owner A must be rejected paying gas with the transferred coin")
	}
}

// TestExecuteTx_TransferObjectThenNewOwnerMutates is the killer case for the
// pod-call mutable-ref ownership check: after an object is transferred A -> B,
// the mutable-ref ownership check passes for B (where it previously failed) and
// is rejected for the old owner A.
func TestExecuteTx_TransferObjectThenNewOwnerMutates(t *testing.T) {
	dag, coinStore := feeDAG(t)
	defer dag.Close()

	a := Hash{0xA1}
	b := Hash{0xB2}
	obj := Hash{0xC3}

	coinStore.SetObject(buildTestCoinObject(obj, 0, a, 0))
	dag.tracker.trackObject(obj, 0, 0, 0, keyRootKind, a)

	ops := []genesis.DeclaredOp{{Kind: reparentOp, ObjectID: obj[:], TargetKind: keyRootKind, Target: b[:]}}
	transfer := opsTx(t, a, "", nil, []objectRef{{id: obj, version: 0}}, ops, Hash{0x20})
	if !dag.handleDeclaredOps(transfer) {
		t.Fatal("object transfer must succeed")
	}

	// B mutates the object via a pod call: ownership check passes.
	atxB := types.GetRootAsAttestedTransaction(
		buildOpsATX(t, b, "mutate", nil, []objectRef{{id: obj, version: 0}}, nil, Hash{0x21}), 0)
	if !dag.validateMutableRefOwnership(atxB, atxB.Transaction(nil)) {
		t.Error("new owner B failed the mutable-ref ownership check on the transferred object")
	}

	// The old owner A is rejected.
	atxA := types.GetRootAsAttestedTransaction(
		buildOpsATX(t, a, "mutate", nil, []objectRef{{id: obj, version: 0}}, nil, Hash{0x22}), 0)
	if dag.validateMutableRefOwnership(atxA, atxA.Transaction(nil)) {
		t.Error("old owner A must fail the mutable-ref ownership check after transfer")
	}
}
