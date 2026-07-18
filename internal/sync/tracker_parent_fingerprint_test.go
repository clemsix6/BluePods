package sync

import (
	"testing"

	"github.com/zeebo/blake3"

	"BluePods/internal/consensus"
)

// TestHashTrackerEntries_ParentFieldsDistinguish verifies a tracker entry's
// parent kind, parent reference, and child count are each part of the
// per-entry convergence digest: two entries identical in ID, version,
// replication, and fees but differing in exactly one of these fields must
// hash differently. Without this, a joined node's parent hierarchy could
// silently diverge from the founder's while the fingerprint stayed unchanged.
func TestHashTrackerEntries_ParentFieldsDistinguish(t *testing.T) {
	base := consensus.ObjectTrackerEntry{
		ID:          consensus.Hash{0x01},
		Version:     1,
		Replication: 5,
		Fees:        100,
		ParentKind:  0,
		Parent:      consensus.Hash{0xAA},
		ChildCount:  3,
	}

	cases := []struct {
		name    string
		variant consensus.ObjectTrackerEntry
	}{
		{"parent_kind", func() consensus.ObjectTrackerEntry { e := base; e.ParentKind = 1; return e }()},
		{"parent", func() consensus.ObjectTrackerEntry { e := base; e.Parent = consensus.Hash{0xBB}; return e }()},
		{"child_count", func() consensus.ObjectTrackerEntry { e := base; e.ChildCount = 4; return e }()},
	}

	baseSum := hashTrackerEntriesSum(base)

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			variantSum := hashTrackerEntriesSum(tc.variant)
			if baseSum == variantSum {
				t.Errorf("hashTrackerEntries ignores %s: distinct values hash identically", tc.name)
			}
		})
	}
}

// hashTrackerEntriesSum returns the BLAKE3 digest of hashTrackerEntries over a
// single tracker entry, for comparing two entries' contribution to the
// fingerprint.
func hashTrackerEntriesSum(e consensus.ObjectTrackerEntry) [32]byte {
	h := blake3.New()
	hashTrackerEntries(h, []consensus.ObjectTrackerEntry{e})

	var sum [32]byte
	h.Sum(sum[:0])

	return sum
}
