package consensus

import (
	"crypto/ed25519"
	"encoding/hex"
	"testing"

	"BluePods/internal/events"
)

// commitLiar puts a vertex anchoring root at a frontier the node has committed
// straight into its store and applies it as a committed batch, the way a crafted
// store is the only route a wrong-root vertex has into committed history (stage
// 1 rejects it at the door, stage 2 refuses to reference it). It returns the
// vertex identity and the node's own root at that frontier.
func commitLiar(t *testing.T, node *DAG, producer testValidator, root Hash, applies int) Hash {
	t.Helper()

	const round = 5

	hash := storeAnchoredVertex(t, node, producer, round, receiverFrontier, root)

	node.commitMu.Lock()
	defer node.commitMu.Unlock()

	for i := 0; i < applies; i++ {
		node.applyBatch(round, []Hash{hash})
	}

	return hash
}

// storedFaults returns every persisted fault record, keyed by the vertex
// identity it convicts.
func storedFaults(t *testing.T, node *DAG) map[Hash][]byte {
	t.Helper()

	out := make(map[Hash][]byte)

	err := node.store.db.IteratePrefix(prefixFault, func(key, value []byte) error {
		if len(key) != len(prefixFault)+32 {
			t.Fatalf("fault key is %d bytes, want %d", len(key), len(prefixFault)+32)
		}

		var hash Hash
		copy(hash[:], key[len(prefixFault):])

		out[hash] = append([]byte(nil), value...)

		return nil
	})
	if err != nil {
		t.Fatalf("iterate fault records: %v", err)
	}

	return out
}

// TestRecheckCommittedAnchor_RecordsFaultForCommittedLiar is stage 3, the
// record: a wrong-root vertex that reached committed history anyway (a lagging
// producer referenced it before it could verify it) is convicted as the commit
// cursor passes it, with one fault record and one event.
func TestRecheckCommittedAnchor_RecordsFaultForCommittedLiar(t *testing.T) {
	validators, vs := newTestValidatorSet(2)
	node, committed := newAnchorReceiver(t, vs, validators[0])

	claimed := committed
	claimed[0] ^= 0xFF

	buf := captureEvents(t)
	hash := commitLiar(t, node, validators[1], claimed, 1)

	faults := storedFaults(t, node)
	if len(faults) != 1 {
		t.Fatalf("want exactly 1 fault record, got %d", len(faults))
	}
	if _, ok := faults[hash]; !ok {
		t.Fatalf("the fault record is not keyed by the lying vertex %x", hash[:8])
	}

	recs := eventsNamed(t, buf, events.EvAnchorFault)
	if len(recs) != 1 {
		t.Fatalf("want exactly 1 %s event, got %d", events.EvAnchorFault, len(recs))
	}

	rec := recs[0]
	if rec["producer"] != hex.EncodeToString(validators[1].pubKey[:]) {
		t.Errorf("producer = %v, want the lying producer", rec["producer"])
	}
	if rec["round"] != float64(5) {
		t.Errorf("round = %v, want 5", rec["round"])
	}
	if rec["claimed"] != hex.EncodeToString(claimed[:]) {
		t.Errorf("claimed = %v, want the root the vertex anchored", rec["claimed"])
	}
	if rec["computed"] != hex.EncodeToString(committed[:]) {
		t.Errorf("computed = %v, want this node's own root at the frontier", rec["computed"])
	}
}

// TestRecheckCommittedAnchor_FaultRecordVerifiesStandalone is what makes the
// evidence slashing-grade: a third party holding the record alone — no vertex,
// no DAG, no access to this node — reproduces the conviction. It recomputes the
// vertex identity from the normative 120-byte header, checks the producer's own
// signature over it, and reads the claimed root out of the same header. Nothing
// in the record is taken on trust.
func TestRecheckCommittedAnchor_FaultRecordVerifiesStandalone(t *testing.T) {
	validators, vs := newTestValidatorSet(2)
	node, committed := newAnchorReceiver(t, vs, validators[0])

	claimed := committed
	claimed[1] ^= 0xFF

	commitLiar(t, node, validators[1], claimed, 1)

	var evidence []byte
	for _, value := range storedFaults(t, node) {
		evidence = value
	}

	record, ok := decodeFaultRecord(evidence)
	if !ok {
		t.Fatalf("stored fault evidence does not decode: %d bytes", len(evidence))
	}

	producer := record.producer()
	if producer != validators[1].pubKey {
		t.Fatalf("evidence names producer %x, want the liar", producer[:8])
	}

	header := record.identity()
	identity := header.hash()

	if !ed25519.Verify(producer[:], identity[:], record.signature) {
		t.Fatal("the producer's signature does not verify over the header the evidence carries")
	}

	if got := record.claimed(); got != claimed {
		t.Fatalf("evidence claims root %x, want the anchored root %x", got[:8], claimed[:8])
	}

	if record.computed != committed {
		t.Fatalf("evidence computes root %x, want this node's own root %x", record.computed[:8], committed[:8])
	}

	if record.claimed() == record.computed {
		t.Fatal("evidence that does not exhibit a mismatch convicts nobody")
	}

	if got := record.round(); got != 5 {
		t.Fatalf("evidence carries round %d, want 5", got)
	}
}

// TestRecheckCommittedAnchor_OneRecordPerLyingVertex pins the dedup: the same
// vertex re-applied — a crash between the fault write and the committed flag,
// or a replay of a decided round after a restart — convicts it once, not once
// per pass. Evidence that grows with retries is a log, not a record.
func TestRecheckCommittedAnchor_OneRecordPerLyingVertex(t *testing.T) {
	validators, vs := newTestValidatorSet(2)
	node, committed := newAnchorReceiver(t, vs, validators[0])

	claimed := committed
	claimed[2] ^= 0xFF

	buf := captureEvents(t)
	commitLiar(t, node, validators[1], claimed, 3)

	if faults := storedFaults(t, node); len(faults) != 1 {
		t.Fatalf("want exactly 1 fault record after 3 applies, got %d", len(faults))
	}

	if recs := eventsNamed(t, buf, events.EvAnchorFault); len(recs) != 1 {
		t.Fatalf("want exactly 1 %s event after 3 applies, got %d", events.EvAnchorFault, len(recs))
	}
}

// TestRecheckCommittedAnchor_NoFaultWithoutProof holds stage 3 to the same
// verdict rule as stages 1 and 2: only a PROVEN lie is recorded. An honest
// anchor, an anchor at a frontier this node has not committed (the ordinary
// state of a peer that commits faster), and a genesis-epoch zero anchor all
// pass through commit leaving no evidence — a fault record is an accusation,
// and this node must not make one it cannot prove.
func TestRecheckCommittedAnchor_NoFaultWithoutProof(t *testing.T) {
	const epochLength = 10

	validators, vs := newTestValidatorSet(2)
	node, committed := newAnchorReceiver(t, vs, validators[0], WithEpochLength(epochLength))

	build := newAnchoredVertices(t, validators[1])

	honestData, honest := build(5, receiverFrontier, committed, nil)
	node.store.add(honestData, honest, 5, validators[1].pubKey)

	aheadData, ahead := build(6, receiverFrontier+9, Hash{0xAB}, nil)
	node.store.add(aheadData, ahead, 6, validators[1].pubKey)

	if got := node.commitEpochForRound(7); got != 0 {
		t.Fatalf("round 7 commits in epoch %d, want the genesis epoch", got)
	}
	genesisData, genesis := build(7, receiverFrontier, Hash{}, nil)
	node.store.add(genesisData, genesis, 7, validators[1].pubKey)

	buf := captureEvents(t)

	node.commitMu.Lock()
	node.applyBatch(5, []Hash{honest})
	node.applyBatch(6, []Hash{ahead})
	node.applyBatch(7, []Hash{genesis})
	node.commitMu.Unlock()

	if faults := storedFaults(t, node); len(faults) != 0 {
		t.Fatalf("want no fault records, got %d", len(faults))
	}

	if recs := eventsNamed(t, buf, events.EvAnchorFault); len(recs) != 0 {
		t.Fatalf("want no %s events, got %d", events.EvAnchorFault, len(recs))
	}
}
