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
// resume from it as if it had been this node's own history all along.
//
// runBootstrap is called directly rather than through Run/Start: n.network is
// deliberately left nil, so the call panics on network.Start() immediately
// after the marker write this test pins. The panic is recovered because only
// that write is under test — a bootstrap fallthrough never legitimately
// reaches the network path in this test, and nothing between the marker
// write and the panic can mutate storage.
func TestRunBootstrap_MarksOnlyGenuineGenesis(t *testing.T) {
	dir := committedDataDir(t)

	// Bootstrap: false, BootstrapAddr: "" — the laundering shape: neither a
	// genesis start nor a join, so Run's dispatch falls through to
	// runBootstrap with nothing legitimizing it as the origin of this state.
	n, db := resumeTestNode(t, &Config{}, dir)

	func() {
		defer func() { recover() }() //nolint:errcheck // deliberate: see doc comment above
		n.runBootstrap()
	}()

	if consensus.HasAdoptedState(db) {
		t.Fatal("a non-genesis fallthrough start (no --bootstrap, no --bootstrap-addr) marked committed residue as adopted: a refused join's directory would be laundered into permanently-trusted state by one misconfigured restart")
	}
}
