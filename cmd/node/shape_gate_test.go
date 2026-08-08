package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"testing"

	flatbuffers "github.com/google/flatbuffers/go"
	"github.com/zeebo/blake3"

	"BluePods/internal/events"
	"BluePods/internal/genesis"
	"BluePods/internal/network"
)

// TestSubmitATX_MalformedShapeRefused closes the seam a malformed transaction
// walked through: the raw-transaction path validates a submission fully, but a
// body that fails that validation falls through to the ATX path, which only
// checked that a transaction was present and carried a 32-byte hash. Wrapping
// the very shape the raw path rejects turned that fallthrough into an
// acceptance — the node included the transaction in its own vertex and gossiped
// it to the network. The ATX path must run the same shape gate on the
// transaction it carries.
func TestSubmitATX_MalformedShapeRefused(t *testing.T) {
	n := submitTestNode(t)

	body := genesis.WrapInATX(buildHybridShapeTx(t))
	req := network.EncodeSubmitTx(&network.SubmitTxRequest{Body: body})

	respBytes, err := n.handleSubmitTx(req)
	if err != nil {
		t.Fatalf("handleSubmitTx: %v", err)
	}

	resp, err := network.DecodeSubmitTxResp(respBytes)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}

	if resp.Err == "" {
		t.Fatal("a wrapped malformed shape was accepted, included and gossiped")
	}
}

// TestGossipedTx_MalformedShapeRefused holds the other half of the seam: a
// transaction arriving over gossip never passed this node's ingress at all, so
// forwarding it into the pending set makes an honest node the carrier of a
// shape it would have refused from a client. A refused body is neither queued
// for inclusion nor re-gossiped.
func TestGossipedTx_MalformedShapeRefused(t *testing.T) {
	n := submitTestNode(t)

	body := genesis.WrapInATX(buildHybridShapeTx(t))

	buf := captureEvents(t)
	n.ingestGossipedTx(body, network.EncodeGossipTx(body))

	recs := eventsNamed(t, buf, events.EvIngressTxRejected)
	if len(recs) != 1 {
		t.Fatalf("want 1 %s event, got %d: %v", events.EvIngressTxRejected, len(recs), recs)
	}
	if recs[0]["reason"] != "malformed_shape" {
		t.Errorf("reason = %v, want malformed_shape", recs[0]["reason"])
	}
}

// buildHybridShapeTx builds a signed transaction carrying BOTH declared
// operations and created_objects_replication: the shape whose replication
// entries price a storage deposit no created object can lock, which is why no
// node may accept it however it arrives.
func buildHybridShapeTx(t *testing.T) []byte {
	t.Helper()

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	var zeroPod, objectID, target [32]byte
	objectID[0] = 0xA1
	target[0] = 0x33

	ops := []genesis.DeclaredOp{{ObjectID: objectID[:], Target: target[:]}}
	reps := []uint16{0, 0}

	unsigned := genesis.BuildUnsignedTxBytesSponsored(
		pub, zeroPod, "", nil, reps, 1000, nil, nil, nil, genesis.Sponsorship{}, nil, ops,
	)
	hash := blake3.Sum256(unsigned)
	sig := ed25519.Sign(priv, hash[:])

	builder := flatbuffers.NewBuilder(1024)
	txOffset := genesis.BuildTxTableSponsored(
		builder, pub, zeroPod, "", nil, reps, 1000, nil, hash, sig, nil, nil, genesis.Sponsorship{}, nil, nil, ops,
	)
	builder.Finish(txOffset)

	return builder.FinishedBytes()
}
