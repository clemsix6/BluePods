package daemon

import (
	"crypto/ed25519"
	"testing"

	flatbuffers "github.com/google/flatbuffers/go"
	"github.com/zeebo/blake3"

	"BluePods/internal/genesis"
	"BluePods/internal/types"
)

// TestBuildATX_PreservesOperations reproduces the attested-submission path a
// TransferObject of a REPLICATED object takes: the client signs a declared-op
// transaction, the daemon collects holder attestations (empty here — quorum
// collection is orthogonal to this bug) and wraps the signed raw tx into an
// ATX via BuildATX, exactly as collectBuildSubmit does. It confirms the ATX's
// embedded transaction still carries the operations the sender signed, and
// that recomputing the canonical body from that embedded transaction — the
// same recompute commit's verifyTxAuthenticity performs on every node —
// reproduces the sender-signed hash.
//
// Before the fix, BuildATX rebuilt the transaction table through a hand-rolled
// field list that never carried the operations vector: OperationsLength()
// came back 0 and the recomputed body hash no longer matched the signed hash,
// which is exactly TestScenarioConsensusBasics/object_create_transfer's
// authenticity_failed on every node.
func TestBuildATX_PreservesOperations(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	var objID, gasCoin, newOwner [32]byte
	objID[0] = 0xAB
	gasCoin[0] = 0xCD
	newOwner[0] = 0xEF

	ops := []genesis.DeclaredOp{{
		Kind:       0,
		ObjectID:   objID[:],
		TargetKind: 0,
		Target:     newOwner[:],
	}}
	mutableRefs := []genesis.ObjectRefData{{ID: objID, Version: 1}}

	body := genesis.BuildUnsignedTxBytesSponsored(
		pub, [32]byte{}, "", nil, nil, 0, 1000, gasCoin[:], mutableRefs, nil, genesis.Sponsorship{}, nil, ops,
	)
	hash := blake3.Sum256(body)
	sig := ed25519.Sign(priv, hash[:])

	builder := flatbuffers.NewBuilder(512)
	txOff := genesis.BuildTxTableSponsored(
		builder, pub, [32]byte{}, "", nil, nil, 0, 1000, gasCoin[:], hash, sig, mutableRefs, nil,
		genesis.Sponsorship{}, nil, nil, ops,
	)
	builder.Finish(txOff)
	rawTx := builder.FinishedBytes()

	d := &Daemon{}
	atxBytes := d.BuildATX(rawTx, nil)

	atx := types.GetRootAsAttestedTransaction(atxBytes, 0)
	tx := atx.Transaction(nil)

	if got := tx.OperationsLength(); got != len(ops) {
		t.Fatalf("operations length after BuildATX = %d, want %d", got, len(ops))
	}

	var op types.DeclaredOp
	tx.Operations(&op, 0)

	if string(op.ObjectIdBytes()) != string(ops[0].ObjectID) {
		t.Errorf("object_id after BuildATX = %x, want %x", op.ObjectIdBytes(), ops[0].ObjectID)
	}
	if string(op.TargetBytes()) != string(ops[0].Target) {
		t.Errorf("target after BuildATX = %x, want %x", op.TargetBytes(), ops[0].Target)
	}

	// Recompute the canonical body exactly as commit's verifyTxAuthenticity
	// does from the ATX's embedded transaction, and confirm it reproduces the
	// sender-signed hash — this is the check every node runs before applying
	// the transferred object's reparent.
	rebuilt := genesis.BuildUnsignedTxBytesSponsored(
		tx.SenderBytes(), [32]byte{}, string(tx.FunctionName()), tx.ArgsBytes(), nil,
		tx.MaxCreateDomains(), tx.MaxGas(), tx.GasCoinBytes(), mutableRefs, nil,
		genesis.Sponsorship{}, tx.DeletedObjectsBytes(), genesis.ExtractOperations(tx),
	)
	rebuiltHash := blake3.Sum256(rebuilt)

	if rebuiltHash != hash {
		t.Fatalf("commit-time recomputed hash != sender-signed hash: ATX assembly altered the canonical body")
	}
}
