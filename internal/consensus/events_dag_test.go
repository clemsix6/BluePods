package consensus

import (
	"bytes"
	"encoding/hex"
	"testing"
	"time"

	"BluePods/internal/events"
	"BluePods/internal/types"
)

// =============================================================================
// consensus.vertex.received / consensus.vertex.rejected (AddVertex)
// =============================================================================

// assertSingleVertexRejected asserts exactly one consensus.vertex.rejected event
// was captured, naming hash and reason.
func assertSingleVertexRejected(t *testing.T, buf *bytes.Buffer, hash Hash, reason string) {
	t.Helper()

	recs := eventsNamed(t, buf, events.EvVertexRejected)
	if len(recs) != 1 {
		t.Fatalf("want 1 %s event, got %d: %v", events.EvVertexRejected, len(recs), recs)
	}

	rec := recs[0]
	if rec["vertex"] != hex.EncodeToString(hash[:]) {
		t.Errorf("vertex = %v, want %s", rec["vertex"], hex.EncodeToString(hash[:]))
	}
	if rec["reason"] != reason {
		t.Errorf("reason = %v, want %q", rec["reason"], reason)
	}
}

// assertNoVertexEvents asserts neither consensus.vertex.received nor
// consensus.vertex.rejected was captured. Used for the buffer (retry-later)
// paths, which must emit nothing: a buffered vertex may be accepted later, and
// a rejected-then-received pair for the same hash would corrupt journal
// semantics.
func assertNoVertexEvents(t *testing.T, buf *bytes.Buffer) {
	t.Helper()

	if recs := eventsNamed(t, buf, events.EvVertexRejected); len(recs) != 0 {
		t.Fatalf("buffered vertex must emit no %s event, got %d: %v", events.EvVertexRejected, len(recs), recs)
	}
	if recs := eventsNamed(t, buf, events.EvVertexReceived); len(recs) != 0 {
		t.Fatalf("buffered vertex must emit no %s event, got %d: %v", events.EvVertexReceived, len(recs), recs)
	}
}

// TestAddVertex_EmitsVertexReceived checks a validly accepted vertex emits
// consensus.vertex.received with its hash, producer, and round.
func TestAddVertex_EmitsVertexReceived(t *testing.T) {
	db := newTestStorage(t)
	validators, vs := newTestValidatorSet(4)

	dag := New(db, vs, nil, testSystemPod, 1, validators[0].privKey, nil)
	defer dag.Close()

	data := buildTestVertex(t, validators[1], 0, nil, 1)
	vertex := types.GetRootAsVertex(data, 0)
	var hash Hash
	copy(hash[:], vertex.HashBytes())

	buf := captureEvents(t)
	if !dag.AddVertex(data) {
		t.Fatal("AddVertex should accept a valid round-0 vertex")
	}

	recs := eventsNamed(t, buf, events.EvVertexReceived)
	if len(recs) != 1 {
		t.Fatalf("want 1 %s event, got %d: %v", events.EvVertexReceived, len(recs), recs)
	}

	rec := recs[0]
	if rec["vertex"] != hex.EncodeToString(hash[:]) {
		t.Errorf("vertex = %v, want %s", rec["vertex"], hex.EncodeToString(hash[:]))
	}
	if rec["producer"] != hex.EncodeToString(validators[1].pubKey[:]) {
		t.Errorf("producer = %v, want %s", rec["producer"], hex.EncodeToString(validators[1].pubKey[:]))
	}
	if rec["round"] != float64(0) {
		t.Errorf("round = %v, want 0", rec["round"])
	}
}

// TestAddVertex_TerminalRejection_BadSignature checks a corrupted signature is a
// terminal rejection with reason=bad_signature.
func TestAddVertex_TerminalRejection_BadSignature(t *testing.T) {
	db := newTestStorage(t)
	validators, vs := newTestValidatorSet(4)

	dag := New(db, vs, nil, testSystemPod, 1, validators[0].privKey, nil)
	defer dag.Close()

	data := buildTestVertex(t, validators[1], 0, nil, 1)
	vertex := types.GetRootAsVertex(data, 0)

	// SignatureBytes aliases data's backing array, so mutating it in place
	// corrupts the signature the same way TestValidateSignature_InvalidSig does.
	sigBytes := vertex.SignatureBytes()
	sigBytes[0] ^= 0xFF

	var hash Hash
	copy(hash[:], vertex.HashBytes())

	buf := captureEvents(t)
	if dag.AddVertex(data) {
		t.Fatal("AddVertex should reject a corrupt signature")
	}

	assertSingleVertexRejected(t, buf, hash, "bad_signature")
}

// TestAddVertex_TerminalRejection_WrongEpoch checks an epoch mismatch is a
// terminal rejection with reason=wrong_epoch.
func TestAddVertex_TerminalRejection_WrongEpoch(t *testing.T) {
	db := newTestStorage(t)
	validators, vs := newTestValidatorSet(4)

	dag := New(db, vs, nil, testSystemPod, 1, validators[0].privKey, nil)
	defer dag.Close()

	data := buildTestVertex(t, validators[1], 0, nil, 99) // dag epoch is 1
	vertex := types.GetRootAsVertex(data, 0)
	var hash Hash
	copy(hash[:], vertex.HashBytes())

	buf := captureEvents(t)
	if dag.AddVertex(data) {
		t.Fatal("AddVertex should reject an epoch mismatch")
	}

	assertSingleVertexRejected(t, buf, hash, "wrong_epoch")
}

// TestAddVertex_TerminalRejection_ParentRound checks a round with zero parents
// (validateParents' "no parents for round" path, distinct from the buffered
// "parent not found" case) is a terminal rejection with reason=parent_round.
func TestAddVertex_TerminalRejection_ParentRound(t *testing.T) {
	db := newTestStorage(t)
	validators, vs := newTestValidatorSet(4)

	dag := New(db, vs, nil, testSystemPod, 1, validators[0].privKey, nil)
	defer dag.Close()

	data := buildTestVertexWithParentLinks(t, validators[1], 1, 1, nil)
	vertex := types.GetRootAsVertex(data, 0)
	var hash Hash
	copy(hash[:], vertex.HashBytes())

	buf := captureEvents(t)
	if dag.AddVertex(data) {
		t.Fatal("AddVertex should reject a round >0 vertex with no parents")
	}

	assertSingleVertexRejected(t, buf, hash, "parent_round")
}

// TestAddVertex_TerminalRejection_ParentQuorum checks a vertex whose only parent
// link points to an absent hash from an UNKNOWN producer (validateParentLink
// skips it, so validateParents passes) leaves zero known parent producers, a
// terminal rejection with reason=parent_quorum.
func TestAddVertex_TerminalRejection_ParentQuorum(t *testing.T) {
	db := newTestStorage(t)
	validators, vs := newTestValidatorSet(4)

	dag := New(db, vs, nil, testSystemPod, 1, validators[0].privKey, nil)
	defer dag.Close()

	stranger := newTestValidator()
	data := buildTestVertexWithParentLinks(t, validators[1], 1, 1,
		[]parentLink{{hash: Hash{0xAB}, producer: stranger.pubKey}},
	)
	vertex := types.GetRootAsVertex(data, 0)
	var hash Hash
	copy(hash[:], vertex.HashBytes())

	buf := captureEvents(t)
	if dag.AddVertex(data) {
		t.Fatal("AddVertex should reject a vertex with no known parent producers")
	}

	assertSingleVertexRejected(t, buf, hash, "parent_quorum")
}

// TestAddVertex_TerminalRejection_FeeSummary checks a wrong declared fee summary
// is a terminal rejection with reason=fee_summary.
func TestAddVertex_TerminalRejection_FeeSummary(t *testing.T) {
	db := newTestStorage(t)
	validators, vs := newTestValidatorSet(3)

	dag := New(db, vs, nil, testSystemPod, 1, validators[0].privKey, nil)
	defer dag.Close()

	params := DefaultFeeParams()
	dag.SetFeeSystem(newMockCoinStore(), &params, nil)

	sender := Hash{0x01}
	gasCoin := Hash{0xCC}
	atxBytes := buildFeeTestATX(t, sender, gasCoin, 500, []uint16{0})

	// Deliberately wrong total_fees so validateFeeSummary rejects it.
	data := buildVertexWithFeeSummary(t, validators[0], 0, 1,
		&feeSummaryValues{totalFees: 999_999},
		[][]byte{atxBytes},
	)
	vertex := types.GetRootAsVertex(data, 0)
	var hash Hash
	copy(hash[:], vertex.HashBytes())

	buf := captureEvents(t)
	if dag.AddVertex(data) {
		t.Fatal("AddVertex should reject a wrong fee summary")
	}

	assertSingleVertexRejected(t, buf, hash, "fee_summary")
}

// TestAddVertex_BufferedMissingParent_EmitsNothing checks a vertex whose parent
// hash is absent but whose parent PRODUCER is known (the missing-parent buffer
// case, retried once the parent arrives) emits no vertex event at all.
func TestAddVertex_BufferedMissingParent_EmitsNothing(t *testing.T) {
	db := newTestStorage(t)
	validators, vs := newTestValidatorSet(4)

	dag := New(db, vs, nil, testSystemPod, 1, validators[0].privKey, nil)
	defer dag.Close()

	data := buildTestVertexWithParentLinks(t, validators[1], 1, 1,
		[]parentLink{{hash: Hash{0xDE, 0xAD}, producer: validators[0].pubKey}},
	)

	buf := captureEvents(t)
	if dag.AddVertex(data) {
		t.Fatal("AddVertex should buffer (not accept) a vertex with a missing parent")
	}

	assertNoVertexEvents(t, buf)
}

// TestAddVertex_BufferedUnknownProducer_EmitsNothing checks a vertex from an
// unregistered producer (the unknown-producer buffer case, retried once the
// producer registers) emits no vertex event at all.
func TestAddVertex_BufferedUnknownProducer_EmitsNothing(t *testing.T) {
	db := newTestStorage(t)
	validators, vs := newTestValidatorSet(4)
	unknown := newTestValidator()

	dag := New(db, vs, nil, testSystemPod, 1, validators[0].privKey, nil)
	defer dag.Close()

	data := buildTestVertex(t, unknown, 0, nil, 1)

	buf := captureEvents(t)
	if dag.AddVertex(data) {
		t.Fatal("AddVertex should buffer (not accept) a vertex from an unknown producer")
	}

	assertNoVertexEvents(t, buf)
}

// =============================================================================
// consensus.vertex.produced (tryProduceVertex)
// =============================================================================

// TestTryProduceVertex_EmitsVertexProduced checks a locally produced vertex
// emits consensus.vertex.produced with its round and transaction count.
func TestTryProduceVertex_EmitsVertexProduced(t *testing.T) {
	db := newTestStorage(t)
	validators, vs := newTestValidatorSet(4)
	mock := &mockBroadcaster{}

	dag := New(db, vs, mock, testSystemPod, 1, validators[0].privKey, nil)
	defer dag.Close()

	buf := captureEvents(t)
	dag.SubmitTx([]byte("test tx"))

	deadline := time.Now().Add(2 * time.Second)
	for len(mock.vertices) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if len(mock.vertices) == 0 {
		t.Fatal("no vertex was broadcast")
	}

	recs := eventsNamed(t, buf, events.EvVertexProduced)
	if len(recs) != 1 {
		t.Fatalf("want 1 %s event, got %d: %v", events.EvVertexProduced, len(recs), recs)
	}

	rec := recs[0]
	if rec["round"] != float64(0) {
		t.Errorf("round = %v, want 0", rec["round"])
	}
	if rec["txs"] != float64(1) {
		t.Errorf("txs = %v, want 1", rec["txs"])
	}
}

// =============================================================================
// consensus.round.advanced (checkCommits / announceRoundAdvances)
// =============================================================================

// TestCheckCommits_EmitsRoundAdvanced_OncePerRound verifies checkCommits emits
// consensus.round.advanced for every round between the last announced round and
// the current production-round cursor (d.Round()), that a tick with no round
// progress announces nothing more, and that resuming progress announces only the
// newly crossed round. announceRoundAdvances runs under commitMu specifically so
// it can safely call anchorProducerFor, which reads commitMu-guarded epoch and
// eligibility state — this is the race-critical site the task calls out, so this
// test is run under `go test -race` as part of verification (the DAG's own
// commitLoop background goroutine is concurrently calling checkCommits too,
// exactly the concurrency this locking discipline must survive).
func TestCheckCommits_EmitsRoundAdvanced_OncePerRound(t *testing.T) {
	_, dag := newDecisionDAG(t)

	dag.updateRound(3)

	buf := captureEvents(t)
	dag.checkCommits()

	recs := eventsNamed(t, buf, events.EvRoundAdvanced)
	if len(recs) != 3 {
		t.Fatalf("want 3 %s events (rounds 1..3), got %d: %v", events.EvRoundAdvanced, len(recs), recs)
	}
	for i, want := range []uint64{1, 2, 3} {
		if recs[i]["round"] != float64(want) {
			t.Errorf("event %d round = %v, want %d", i, recs[i]["round"], want)
		}
	}

	// A tick with no round progress must not re-announce already-announced rounds.
	dag.checkCommits()
	if recs := eventsNamed(t, buf, events.EvRoundAdvanced); len(recs) != 3 {
		t.Fatalf("second tick re-announced rounds: want still 3 events, got %d", len(recs))
	}

	// Progressing further announces only the newly crossed round.
	dag.updateRound(4)
	dag.checkCommits()

	recs = eventsNamed(t, buf, events.EvRoundAdvanced)
	if len(recs) != 4 {
		t.Fatalf("want 4 %s events after advancing to round 4, got %d: %v", events.EvRoundAdvanced, len(recs), recs)
	}
	if recs[3]["round"] != float64(4) {
		t.Errorf("4th event round = %v, want 4", recs[3]["round"])
	}
}

// TestWithLastCommittedRound_SeedsLastAnnouncedRound verifies a DAG resumed
// from a restored commit cursor (WithLastCommittedRound, simulating a synced
// or restarted node) does not retroactively flood consensus.round.advanced
// for rounds already reached before the restore (I1): once production
// catches up past the restored round, only the genuinely new rounds are
// announced.
func TestWithLastCommittedRound_SeedsLastAnnouncedRound(t *testing.T) {
	vals, vs := newTestValidatorSet(4)
	const restoredRound = 50

	dag := New(newTestStorage(t), vs, nil, testSystemPod, 0, vals[0].privKey, nil,
		WithLastCommittedRound(restoredRound))
	t.Cleanup(func() { dag.Close() })
	setEqualStake(dag, vals, 25)

	// Jump production ahead, as a synced or restarted node's round quickly does
	// once vertices start arriving again.
	dag.updateRound(restoredRound + 2)

	buf := captureEvents(t)
	dag.checkCommits()

	recs := eventsNamed(t, buf, events.EvRoundAdvanced)
	for _, rec := range recs {
		if round, ok := rec["round"].(float64); ok && uint64(round) <= restoredRound {
			t.Fatalf("got a retroactive %s for round %v (restored cursor at %d)",
				events.EvRoundAdvanced, rec["round"], restoredRound)
		}
	}

	if len(recs) != 2 {
		t.Fatalf("want 2 %s events (rounds %d..%d), got %d: %v",
			events.EvRoundAdvanced, restoredRound+1, restoredRound+2, len(recs), recs)
	}
}
