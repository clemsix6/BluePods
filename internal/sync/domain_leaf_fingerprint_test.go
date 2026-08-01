package sync

import (
	"testing"

	"github.com/zeebo/blake3"

	"BluePods/internal/state"
)

// TestHashDomains_OwnerAndExpiryDistinguish verifies a domain leaf's owner
// and expiry epoch — not just its name and object — are each part of the
// per-entry convergence digest: two entries identical in name and object but
// differing in exactly one of these fields must hash differently. Without
// this, a joined node's domain registry could silently diverge from the
// founder's (a different owner, or a lease swept on one node and not the
// other) while the fingerprint stayed unchanged.
func TestHashDomains_OwnerAndExpiryDistinguish(t *testing.T) {
	base := state.DomainEntry{
		Name:        "alpha.pod",
		ObjectID:    [32]byte{0xAA},
		Owner:       [32]byte{0x11},
		ExpiryEpoch: 5,
	}

	cases := []struct {
		name    string
		variant state.DomainEntry
	}{
		{"owner", func() state.DomainEntry { e := base; e.Owner = [32]byte{0x22}; return e }()},
		{"expiry_epoch", func() state.DomainEntry { e := base; e.ExpiryEpoch = 6; return e }()},
	}

	baseSum := hashDomainsSum(base)

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			variantSum := hashDomainsSum(tc.variant)
			if baseSum == variantSum {
				t.Errorf("hashDomains ignores %s: distinct values hash identically", tc.name)
			}
		})
	}
}

// hashDomainsSum returns the BLAKE3 digest of hashDomains over a single
// domain entry, for comparing two entries' contribution to the fingerprint.
func hashDomainsSum(e state.DomainEntry) [32]byte {
	h := blake3.New()
	hashDomains(h, []state.DomainEntry{e})

	var sum [32]byte
	h.Sum(sum[:0])

	return sum
}
