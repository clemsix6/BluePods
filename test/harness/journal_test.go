package harness

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"
)

// eventLine builds a minimal JSON event record line, as the node's JSON log
// handler would emit it.
func eventLine(t *testing.T, name string, attrs map[string]any) []byte {
	t.Helper()

	rec := map[string]any{
		"time":  time.Now().Format(time.RFC3339Nano),
		"level": "INFO",
		"msg":   name,
		"event": name,
		"node":  "abcd1234",
	}
	for k, v := range attrs {
		rec[k] = v
	}

	data, err := json.Marshal(rec)
	if err != nil {
		t.Fatalf("marshal event line: %v", err)
	}

	return append(data, '\n')
}

// TestAppendIgnoresHumanLines asserts a line without the reserved "event" key
// is ignored, not stored and not an error.
func TestAppendIgnoresHumanLines(t *testing.T) {
	j := NewJournal("n0")

	line := []byte(`{"time":"2024-01-01T00:00:00Z","level":"INFO","msg":"starting up"}` + "\n")
	if err := j.Append(line); err != nil {
		t.Fatalf("human line: %v", err)
	}

	if got := j.Events("node.started"); len(got) != 0 {
		t.Fatalf("expected no events, got %d", len(got))
	}
}

// TestAppendStoresEvent asserts an event line is parsed and retrievable.
func TestAppendStoresEvent(t *testing.T) {
	j := NewJournal("n0")

	if err := j.Append(eventLine(t, "node.started", map[string]any{"quic_addr": "127.0.0.1:9000"})); err != nil {
		t.Fatalf("append: %v", err)
	}

	got := j.Events("node.started")
	if len(got) != 1 {
		t.Fatalf("expected 1 event, got %d", len(got))
	}
	if got[0].Attrs["quic_addr"] != "127.0.0.1:9000" {
		t.Fatalf("bad attrs: %v", got[0].Attrs)
	}
	if got[0].Node != "abcd1234" {
		t.Fatalf("bad node: %q", got[0].Node)
	}
}

// TestAppendGarbageErrors asserts a line that is not valid JSON returns an
// error, the schema-drift detector.
func TestAppendGarbageErrors(t *testing.T) {
	j := NewJournal("n0")

	err := j.Append([]byte("not json at all\n"))
	if err == nil {
		t.Fatal("expected an error for unparsable line")
	}
	if j.ParseError() == nil {
		t.Fatal("expected ParseError to record the failure")
	}
}

// TestWaitReleasedByLaterAppend asserts Wait unblocks when a later Append
// satisfies it.
func TestWaitReleasedByLaterAppend(t *testing.T) {
	j := NewJournal("n0")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	done := make(chan Event, 1)
	errCh := make(chan error, 1)

	go func() {
		e, err := j.Wait(ctx, "consensus.round.advanced")
		if err != nil {
			errCh <- err
			return
		}
		done <- e
	}()

	// Give the waiter a moment to register by polling Events until this
	// goroutine has plausibly entered Wait; a short sleep here would be a
	// flakiness risk, so instead we just append immediately — Wait handles
	// both "already recorded" and "recorded later" cases identically.
	if err := j.Append(eventLine(t, "consensus.round.advanced", map[string]any{"round": 3})); err != nil {
		t.Fatalf("append: %v", err)
	}

	select {
	case e := <-done:
		if e.Attrs["round"] != float64(3) {
			t.Fatalf("bad round: %v", e.Attrs["round"])
		}
	case err := <-errCh:
		t.Fatalf("wait error: %v", err)
	case <-ctx.Done():
		t.Fatal("wait did not release in time")
	}
}

// TestWaitSatisfiedByEarlierEvent asserts Wait returns immediately for an
// event already recorded before the call.
func TestWaitSatisfiedByEarlierEvent(t *testing.T) {
	j := NewJournal("n0")

	if err := j.Append(eventLine(t, "node.ready", map[string]any{"round": 0})); err != nil {
		t.Fatalf("append: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	e, err := j.Wait(ctx, "node.ready")
	if err != nil {
		t.Fatalf("wait: %v", err)
	}
	if e.Name != "node.ready" {
		t.Fatalf("bad event: %v", e)
	}
}

// TestWaitContextTimeout asserts Wait returns an error once ctx ends without
// a match.
func TestWaitContextTimeout(t *testing.T) {
	j := NewJournal("n0")

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, err := j.Wait(ctx, "never.happens")
	if err == nil {
		t.Fatal("expected a timeout error")
	}
}

// TestAttrMatchesAcrossNumericTypes asserts Attr coerces both sides to
// float64, so a predicate built with a Go int matches a JSON-decoded float64.
func TestAttrMatchesAcrossNumericTypes(t *testing.T) {
	j := NewJournal("n0")

	if err := j.Append(eventLine(t, "consensus.round.advanced", map[string]any{"round": 7})); err != nil {
		t.Fatalf("append: %v", err)
	}

	got := j.Events("consensus.round.advanced", Attr("round", 7))
	if len(got) != 1 {
		t.Fatalf("Attr(int) should match float64 round, got %d", len(got))
	}

	got = j.Events("consensus.round.advanced", Attr("round", uint64(7)))
	if len(got) != 1 {
		t.Fatalf("Attr(uint64) should match float64 round, got %d", len(got))
	}

	got = j.Events("consensus.round.advanced", Attr("round", 8))
	if len(got) != 0 {
		t.Fatalf("Attr(8) should not match round 7, got %d", len(got))
	}
}

// TestAttrGEMatchesNumeric asserts AttrGE matches values at or above min.
func TestAttrGEMatchesNumeric(t *testing.T) {
	j := NewJournal("n0")

	if err := j.Append(eventLine(t, "consensus.round.advanced", map[string]any{"round": 5})); err != nil {
		t.Fatalf("append: %v", err)
	}

	if got := j.Events("consensus.round.advanced", AttrGE("round", 5)); len(got) != 1 {
		t.Fatalf("AttrGE(5) should match round 5, got %d", len(got))
	}
	if got := j.Events("consensus.round.advanced", AttrGE("round", 1)); len(got) != 1 {
		t.Fatalf("AttrGE(1) should match round 5, got %d", len(got))
	}
	if got := j.Events("consensus.round.advanced", AttrGE("round", 6)); len(got) != 0 {
		t.Fatalf("AttrGE(6) should not match round 5, got %d", len(got))
	}
}

// TestAttrStringAndBool asserts string and bool attribute equality without
// numeric coercion.
func TestAttrStringAndBool(t *testing.T) {
	j := NewJournal("n0")

	if err := j.Append(eventLine(t, "tx.committed", map[string]any{
		"success": true,
		"reason":  "version_conflict",
	})); err != nil {
		t.Fatalf("append: %v", err)
	}

	if got := j.Events("tx.committed", Attr("success", true)); len(got) != 1 {
		t.Fatalf("bool match failed, got %d", len(got))
	}
	if got := j.Events("tx.committed", Attr("reason", "version_conflict")); len(got) != 1 {
		t.Fatalf("string match failed, got %d", len(got))
	}
	if got := j.Events("tx.committed", Attr("reason", "duplicate")); len(got) != 0 {
		t.Fatalf("string mismatch should not match, got %d", len(got))
	}
}

// TestSegmentIncrements asserts NewSegment tags subsequent events with the
// next segment while earlier events keep their original segment.
func TestSegmentIncrements(t *testing.T) {
	j := NewJournal("n0")

	if err := j.Append(eventLine(t, "node.ready", nil)); err != nil {
		t.Fatalf("append: %v", err)
	}

	j.NewSegment()

	if err := j.Append(eventLine(t, "node.ready", nil)); err != nil {
		t.Fatalf("append: %v", err)
	}

	got := j.Events("node.ready")
	if len(got) != 2 {
		t.Fatalf("expected 2 events, got %d", len(got))
	}
	if got[0].Seg != 0 {
		t.Fatalf("first event should be segment 0, got %d", got[0].Seg)
	}
	if got[1].Seg != 1 {
		t.Fatalf("second event should be segment 1, got %d", got[1].Seg)
	}
}

// TestJournalConcurrentAppendAndWait exercises concurrent append and wait
// under -race.
func TestJournalConcurrentAppendAndWait(t *testing.T) {
	j := NewJournal("n0")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var wg sync.WaitGroup

	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(round int) {
			defer wg.Done()
			_ = j.Append(eventLine(t, "consensus.round.advanced", map[string]any{"round": round}))
		}(i)
	}

	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = j.Wait(ctx, "consensus.round.advanced", AttrGE("round", 15))
		}()
	}

	wg.Wait()

	if got := j.Events("consensus.round.advanced"); len(got) != 20 {
		t.Fatalf("expected 20 events, got %d", len(got))
	}
}

// TestAppendTrimsBlankLines asserts a blank line (bufio artifact) is ignored.
func TestAppendTrimsBlankLines(t *testing.T) {
	j := NewJournal("n0")

	if err := j.Append([]byte("\n")); err != nil {
		t.Fatalf("blank line should not error: %v", err)
	}
	if err := j.Append([]byte("   \n")); err != nil {
		t.Fatalf("whitespace line should not error: %v", err)
	}
}
