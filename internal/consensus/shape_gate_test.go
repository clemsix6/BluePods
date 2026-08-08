package consensus

import (
	"testing"

	"BluePods/internal/genesis"
	"BluePods/internal/types"
	"BluePods/internal/validation"
)

// TestMalformedShapeChargesNothing is the network-wide half of the shape rule.
// A transaction mixing declared operations with created_objects_replication is
// refused at client ingress, but a byzantine producer includes what it likes
// and a forged submission arrives wrapped in an ATX, so the commit path must
// refuse the same shape on its own. Until it did, the hybrid was charged: the
// fee deduction withheld the storage deposit the replication entries price, the
// operations exit returned a split accounting only the consumed part, and the
// difference left the coin without ever reaching the pool — coin supply leaving
// accounted state on a shape no honest node would have accepted.
//
// The assertion is the supply identity itself: whatever the gas coin loses must
// be exactly what the returned split accounts. A shape no honest node accepts
// costs its sender nothing, so both sides are zero.
func TestMalformedShapeChargesNothing(t *testing.T) {
	env := newOpsFeeEnv(t)
	defer env.dag.Close()

	sender := Hash{0x01}
	gasCoin := Hash{0xCC}
	const startBalance = 1_000_000
	env.coins.SetObject(buildTestCoinObject(gasCoin, startBalance, sender, 0))

	obj := Hash{0x11}
	target := Hash{0x22}

	atxBytes := env.atx(t, opsFeeTx{
		sender:  sender,
		gasCoin: gasCoin,
		maxGas:  env.params.MinGas,
		ops: []genesis.DeclaredOp{
			{Kind: reparentOp, ObjectID: obj[:], TargetKind: keyRootKind, Target: target[:]},
		},
		replication: []uint16{0, 0},
	})
	atx := types.GetRootAsAttestedTransaction(atxBytes, 0)

	buf := captureEvents(t)
	split := env.dag.executeTx(atx, 1, env.producer.pubKey, nil, Hash{})

	debited := debitedFrom(t, env, gasCoin, startBalance)

	if debited != split.Total {
		t.Errorf("gas coin lost %d but the split accounts %d: the difference left the supply", debited, split.Total)
	}
	if debited != 0 {
		t.Errorf("a malformed shape was charged %d: no honest node would have accepted it", debited)
	}

	assertSingleTxCommitted(t, buf, 1, false, "malformed_shape", Hash{})
}

// TestOversizedOpsListChargesNothing holds the other rule the shared gate
// carries: the bound on the operation list. It cannot live at ingress alone
// either, and for a sharper reason than the exclusivity clauses — the work it
// caps (a leaf rewritten on every node per operation) is done before the fee
// that would price it is charged, so a list a producer sizes itself has to be
// refused where it is applied, not only where a client offers it.
func TestOversizedOpsListChargesNothing(t *testing.T) {
	env := newOpsFeeEnv(t)
	defer env.dag.Close()

	sender := Hash{0x01}
	gasCoin := Hash{0xCC}
	const startBalance = 100_000_000
	env.coins.SetObject(buildTestCoinObject(gasCoin, startBalance, sender, 0))

	atxBytes := env.atx(t, opsFeeTx{
		sender:  sender,
		gasCoin: gasCoin,
		maxGas:  env.params.MinGas,
		ops:     opsListPastTheBound(t),
	})
	atx := types.GetRootAsAttestedTransaction(atxBytes, 0)

	buf := captureEvents(t)
	split := env.dag.executeTx(atx, 1, env.producer.pubKey, nil, Hash{})

	debited := debitedFrom(t, env, gasCoin, startBalance)

	if debited != split.Total {
		t.Errorf("gas coin lost %d but the split accounts %d: the difference left the supply", debited, split.Total)
	}
	if debited != 0 {
		t.Errorf("an over-long operation list was charged %d: no honest node would have accepted it", debited)
	}

	assertSingleTxCommitted(t, buf, 1, false, "malformed_shape", Hash{})
}

// opsListPastTheBound returns the shortest operation list the shared gate
// refuses, grown one operation at a time and asked of the gate itself: the cap
// is internal/validation's to own, and a witness that restated it would keep
// passing after the two drifted apart.
func opsListPastTheBound(t *testing.T) []genesis.DeclaredOp {
	t.Helper()

	obj := Hash{0x11}
	target := Hash{0x22}

	// A runaway guard, not the cap: it only stops the growth if the gate turns
	// out to bound nothing at all, which is the failure reported below.
	const guard = 1024

	var ops []genesis.DeclaredOp
	for len(ops) < guard {
		ops = append(ops, genesis.DeclaredOp{Kind: reparentOp, ObjectID: obj[:], TargetKind: keyRootKind, Target: target[:]})

		atxBytes := buildOpsFeeATX(t, opsFeeTx{ops: ops})
		atx := types.GetRootAsAttestedTransaction(atxBytes, 0)

		if validation.ValidateShape(atx.Transaction(nil)) != nil {
			return ops
		}
	}

	t.Fatal("the shared gate accepts an operation list of any length")

	return nil
}

// TestPodCallWithReplicationStillCommits is the negative of the shape rule.
// The exclusivity is between DECLARED OPERATIONS and replication entries, never
// between a pod call and them: a pod call is precisely the transaction that
// creates the objects those entries replicate, and gating it would stop every
// object creation on the network. The gate is asked here to let one through and
// charge it.
func TestPodCallWithReplicationStillCommits(t *testing.T) {
	env := newOpsFeeEnv(t)
	defer env.dag.Close()

	sender := Hash{0x01}
	gasCoin := Hash{0xCC}
	const startBalance = 1_000_000
	env.coins.SetObject(buildTestCoinObject(gasCoin, startBalance, sender, 0))

	atxBytes := env.atx(t, opsFeeTx{
		sender:      sender,
		gasCoin:     gasCoin,
		maxGas:      env.params.MinGas,
		funcName:    "create",
		replication: []uint16{0, 0},
	})
	atx := types.GetRootAsAttestedTransaction(atxBytes, 0)

	buf := captureEvents(t)
	split := env.dag.executeTx(atx, 1, env.producer.pubKey, nil, Hash{})

	if split.Total == 0 {
		t.Error("a creating pod call must be charged: the shape gate rejected it")
	}
	if debited := debitedFrom(t, env, gasCoin, startBalance); debited == 0 {
		t.Error("a creating pod call must pay for the objects it declares")
	}

	assertSingleTxCommitted(t, buf, 1, true, "", Hash{})
}

// debitedFrom returns what the gas coin lost against the balance it started
// with.
func debitedFrom(t *testing.T, env *opsFeeEnv, gasCoin Hash, startBalance uint64) uint64 {
	t.Helper()

	balance, err := readCoinBalance(env.coins.GetObject(gasCoin))
	if err != nil {
		t.Fatalf("read gas coin: %v", err)
	}

	return startBalance - balance
}
