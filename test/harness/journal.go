package harness

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"
)

// Journal is a thread-safe, appendable, waitable event log for one node. It
// spans process runs: each restart opens a new segment (Node.Restart calls
// NewSegment), and every recorded event carries the segment it landed in.
type Journal struct {
	node string // node is the identity used for events whose record lacks one

	mu     sync.Mutex    // mu guards every field below
	seg    int           // seg is the current process-run segment
	events []Event       // events is every event recorded so far, in append order
	err    error         // err is the first schema-drift (unparsable line) error, if any
	notify chan struct{} // notify is closed and replaced on every append, waking blocked Waiters
}

// NewJournal creates an empty journal for node, the identity attributed to
// records that do not carry their own "node" attribute.
func NewJournal(node string) *Journal {
	return &Journal{node: node, notify: make(chan struct{})}
}

// Append parses one complete stdout line. A line without the reserved
// "event" key (a human log line) is ignored, not an error. A line that is
// not valid JSON returns an error (schema-drift detection); the caller
// decides whether it is exempt (for example the final, possibly truncated,
// line of a killed node).
func (j *Journal) Append(line []byte) error {
	trimmed := bytes.TrimSpace(line)
	if len(trimmed) == 0 {
		return nil
	}

	var rec map[string]any
	if err := json.Unmarshal(trimmed, &rec); err != nil {
		err = fmt.Errorf("unparsable journal line:\n%w", err)

		j.mu.Lock()
		if j.err == nil {
			j.err = err
		}
		j.mu.Unlock()

		return err
	}

	name, ok := rec["event"].(string)
	if !ok {
		return nil
	}

	j.recordEvent(name, rec)

	return nil
}

// recordEvent builds an Event from a parsed record and appends it, waking
// every blocked Wait call.
func (j *Journal) recordEvent(name string, rec map[string]any) {
	ev := Event{Name: name, Node: j.node, Attrs: rec}

	if ts, ok := rec["time"].(string); ok {
		if parsed, err := time.Parse(time.RFC3339Nano, ts); err == nil {
			ev.Time = parsed
		}
	}

	if node, ok := rec["node"].(string); ok && node != "" {
		ev.Node = node
	}

	delete(rec, "event")
	delete(rec, "time")
	delete(rec, "node")

	j.mu.Lock()
	ev.Seg = j.seg
	j.events = append(j.events, ev)
	closed := j.notify
	j.notify = make(chan struct{})
	j.mu.Unlock()

	close(closed)
}

// NewSegment records a process restart boundary: every event recorded after
// this call belongs to the next segment.
func (j *Journal) NewSegment() {
	j.mu.Lock()
	j.seg++
	j.mu.Unlock()
}

// currentSegment returns the segment new events are currently tagged with.
// It is internal bookkeeping for the harness's own orchestration (Cluster.Restart
// targets the segment about to open, rather than risk matching a stale
// pre-restart event of the same name).
func (j *Journal) currentSegment() int {
	j.mu.Lock()
	defer j.mu.Unlock()

	return j.seg
}

// all returns every event recorded so far, across all segments, in append
// order, regardless of name. It is internal to the harness (Cluster.Dump
// uses it for diagnostics); scenario code goes through Events for a specific
// name.
func (j *Journal) all() []Event {
	j.mu.Lock()
	defer j.mu.Unlock()

	return append([]Event{}, j.events...)
}

// Events returns matching events across all segments, in append order.
func (j *Journal) Events(name string, preds ...Pred) []Event {
	j.mu.Lock()
	defer j.mu.Unlock()

	var out []Event
	for _, e := range j.events {
		if e.Name == name && matchAll(e, preds) {
			out = append(out, e)
		}
	}

	return out
}

// ParseError returns the first schema-drift error recorded by Append, if any.
func (j *Journal) ParseError() error {
	j.mu.Lock()
	defer j.mu.Unlock()

	return j.err
}

// find returns the first recorded event matching name and preds, and whether
// one exists.
func (j *Journal) find(name string, preds []Pred) (Event, bool) {
	j.mu.Lock()
	defer j.mu.Unlock()

	for _, e := range j.events {
		if e.Name == name && matchAll(e, preds) {
			return e, true
		}
	}

	return Event{}, false
}

// Wait blocks until an event matching name and preds is recorded (including
// one already recorded before the call) or ctx ends.
func (j *Journal) Wait(ctx context.Context, name string, preds ...Pred) (Event, error) {
	for {
		if e, ok := j.find(name, preds); ok {
			return e, nil
		}

		j.mu.Lock()
		notify := j.notify
		j.mu.Unlock()

		select {
		case <-notify:
			continue
		case <-ctx.Done():
			// notify closing and ctx ending can become ready in the same instant;
			// select picks between ready cases pseudo-randomly. Recheck once before
			// reporting a timeout, so an event that landed exactly at the deadline is
			// not missed (this is the exit-vs-event race).
			if e, ok := j.find(name, preds); ok {
				return e, nil
			}

			return Event{}, fmt.Errorf("wait for event %q:\n%w", name, ctx.Err())
		}
	}
}

// waitCount blocks until at least min events named name (matching preds) are
// recorded, or ctx ends, returning the last one that satisfied the count.
// Unlike Wait, which returns on ANY match including one already recorded
// before the call, waitCount confirms an operation the caller just performed
// actually landed, for events that carry no attribute distinguishing one
// occurrence from the next (for example net.partition.applied/cleared) and
// so may otherwise be satisfied by a stale, pre-existing occurrence.
func (j *Journal) waitCount(ctx context.Context, name string, min int, preds ...Pred) (Event, error) {
	for {
		j.mu.Lock()
		var last Event
		count := 0
		for _, e := range j.events {
			if e.Name == name && matchAll(e, preds) {
				count++
				last = e
			}
		}
		if count >= min {
			j.mu.Unlock()
			return last, nil
		}
		notify := j.notify
		j.mu.Unlock()

		select {
		case <-notify:
			continue
		case <-ctx.Done():
			return Event{}, fmt.Errorf("wait for %d %q events (have %d):\n%w", min, name, count, ctx.Err())
		}
	}
}
