// Package harness drives a cluster of real BluePods node processes for
// scenario tests: it starts, stops, kills, restarts and partitions nodes,
// reads their typed event streams, injects transactions through the real
// client path, and checks the cardinal invariants (convergence, zero
// rollback, supply) at teardown.
package harness

import "time"

// Event is one parsed typed event from a node's JSON log stream. It carries
// the reserved "event" name, the record timestamp, the emitting node's
// identity, the process-run segment it was observed in, and every other
// attribute the record carried.
type Event struct {
	Name  string         // Name is the dotted event name (for example "tx.committed")
	Time  time.Time      // Time is the record timestamp
	Node  string         // Node is the emitting node's identity attribute
	Seg   int            // Seg is the process-run segment (increments on restart)
	Attrs map[string]any // Attrs holds the remaining attributes (JSON-decoded)
}

// Pred filters events during matching. A Pred returning false excludes the
// event from a match.
type Pred func(Event) bool

// Attr matches an attribute by equality. Numeric want values are compared
// against the attribute as float64 (the type every JSON number decodes to),
// so callers may pass any Go integer type; string and bool values compare
// directly.
func Attr(key string, want any) Pred {
	return func(e Event) bool {
		got, ok := e.Attrs[key]
		if !ok {
			return false
		}

		if wf, ok := toFloat64(want); ok {
			gf, ok := toFloat64(got)
			return ok && gf == wf
		}

		return got == want
	}
}

// AttrGE matches a numeric attribute greater than or equal to min.
func AttrGE(key string, min uint64) Pred {
	return func(e Event) bool {
		got, ok := e.Attrs[key]
		if !ok {
			return false
		}

		gf, ok := toFloat64(got)
		return ok && gf >= float64(min)
	}
}

// matchAll reports whether every predicate accepts e.
func matchAll(e Event, preds []Pred) bool {
	for _, p := range preds {
		if !p(e) {
			return false
		}
	}

	return true
}

// toFloat64 converts any Go numeric type to float64, for the numeric matching
// AttrGE and Attr perform. It returns ok=false for non-numeric values.
func toFloat64(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int8:
		return float64(n), true
	case int16:
		return float64(n), true
	case int32:
		return float64(n), true
	case int64:
		return float64(n), true
	case uint:
		return float64(n), true
	case uint8:
		return float64(n), true
	case uint16:
		return float64(n), true
	case uint32:
		return float64(n), true
	case uint64:
		return float64(n), true
	default:
		return 0, false
	}
}
