package main

import (
	"testing"

	"BluePods/internal/consensus"
)

// TestRunBootstrap_MarksOnlyGenuineGenesis pins the laundering sequence the
// security-boundary review found: runBootstrap is not only the genesis path,
// it is Run's own fallthrough — a non-genesis node started with neither
// --bootstrap nor --bootstrap-addr takes it too. A refused join leaves a
// directory holding committed residue (a cursor and a live validator set,
// applied by performSync before its verification gate decides anything) and
// no marker; committedDataDir mirrors that residue exactly (see its doc
// comment in resume_test.go). One start without the upstream flag — the
// operator "just retry without --bootstrap-addr" move — must not launder
// that residue into permanently-adopted state: every later start would then
// resume from it as if it had been this node's own history all along. The
// genesis case (cfg.Bootstrap) is the twin, positive shape: it is the one
// start runBootstrap exists to mark, and this test pins that it still does.
//
// runBootstrap is called directly rather than through Run/Start: n.network is
// deliberately left nil, so the call panics on network.Start() immediately
// after the marker write this test pins. The panic is recovered because only
// that write is under test — a bootstrap fallthrough never legitimately
// reaches the network path in this test, and nothing between the marker
// write and the panic can mutate storage.
func TestRunBootstrap_MarksOnlyGenuineGenesis(t *testing.T) {
	cases := []struct {
		name string
		cfg  *Config
		want bool
	}{
		// Bootstrap: false, BootstrapAddr: "" — the laundering shape: neither a
		// genesis start nor a join, so Run's dispatch falls through to
		// runBootstrap with nothing legitimizing it as the origin of this state.
		{"fallthrough, no upstream", &Config{}, false},
		// Bootstrap: true — the genuine genesis start runBootstrap exists for;
		// the one shape allowed to mark the state adopted.
		{"genesis", &Config{Bootstrap: true}, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := committedDataDir(t)
			n, db := resumeTestNode(t, tc.cfg, dir)

			func() {
				defer func() { recover() }() //nolint:errcheck // deliberate: see doc comment above
				n.runBootstrap()
			}()

			if got := consensus.HasAdoptedState(db); got != tc.want {
				t.Fatalf("HasAdoptedState after runBootstrap(cfg=%+v) = %v, want %v", tc.cfg, got, tc.want)
			}
		})
	}
}
