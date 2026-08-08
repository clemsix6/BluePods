package consensus

import (
	"crypto/ed25519"
	"encoding/binary"
	"testing"
	"time"

	"BluePods/internal/index"
)

// newBundleDAG builds a four-validator DAG with equal stake and a frozen
// genesis-epoch holder snapshot (setEqualStake, from anchor_decision_test.go),
// wired to a real index.Manager committed at receiverFrontier. It mirrors
// newAnchorReceiver's single-validator fixture (rootcheck_test.go) but with
// the four-member committee IndexAnchorBundle's quorum tests need: three of
// four is the standard 2/3-capped-stake majority under the default voting
// cap.
func newBundleDAG(t *testing.T) ([]testValidator, *DAG, Hash) {
	t.Helper()

	vals, vs := newTestValidatorSet(4)
	dag := New(newTestStorage(t), vs, nil, testSystemPod, 0, vals[0].privKey, nil)
	t.Cleanup(func() { dag.Close() })
	setEqualStake(dag, vals, 25)

	mgr := index.NewManager()
	mgr.ApplyEdge([32]byte{0x11}, index.KeyRootKind, [32]byte{0x01})
	mgr.SetFrontier(receiverFrontier)
	dag.SetIndexer(mgr)

	root, ok := mgr.RootAt(receiverFrontier)
	if !ok {
		t.Fatalf("bundle fixture: no root retained at frontier %d", receiverFrontier)
	}
	if root == (Hash{}) {
		t.Fatal("bundle fixture: committed root is the zero root, which would make quorum exclusion vacuous")
	}

	return vals, dag, root
}

// verifyHeaderRecord decodes and verifies one IndexAnchorBundle header record
// the way a light client must: recompute the header hash under its domain tag
// and check the producer's Ed25519 signature over it. It fails the test
// outright on any structural or cryptographic defect, and returns the
// producer and the (frontier_round, index_root) pair the header claims, so
// callers assert on those directly.
func verifyHeaderRecord(t *testing.T, record []byte) (producer Hash, frontier uint64, root Hash) {
	t.Helper()

	const wantSize = headerSize + ed25519.SignatureSize
	if len(record) != wantSize {
		t.Fatalf("header record is %d bytes, want %d (%d header + %d signature)",
			len(record), wantSize, headerSize, ed25519.SignatureSize)
	}

	header := record[:headerSize]
	signature := record[headerSize:]

	copy(producer[:], header[0:32])
	frontier = binary.BigEndian.Uint64(header[48:56])
	copy(root[:], header[56:88])

	identity := taggedHash(headerDomainTag, header)
	if !ed25519.Verify(producer[:], identity[:], signature) {
		t.Fatal("header record signature does not verify")
	}

	return producer, frontier, root
}

// TestIndexAnchorBundle_QuorumExcludesWrongRootMinority is the plan's core
// case: three of four validators anchor the receiver's own committed root at
// the same frontier and reach the 2/3 capped-stake quorum on their own; a
// fourth anchors a DIFFERENT root at the identical frontier. Its header never
// matches this node's own RootAt, so it is excluded from the bundle by the
// very comparison that would otherwise quarantine it at ingress — no
// separate quarantine check is needed in assembly.
func TestIndexAnchorBundle_QuorumExcludesWrongRootMinority(t *testing.T) {
	vals, dag, root := newBundleDAG(t)

	const round = receiverFrontier + 2

	for _, v := range vals[:3] {
		storeAnchoredVertex(t, dag, v, round, receiverFrontier, root)
	}

	wrongRoot := root
	wrongRoot[0] ^= 0xFF
	storeAnchoredVertex(t, dag, vals[3], round, receiverFrontier, wrongRoot)

	bundle, ok := dag.IndexAnchorBundle()
	if !ok {
		t.Fatal("three of four validators anchoring the real root must reach quorum")
	}

	if bundle.FrontierRound != receiverFrontier {
		t.Errorf("bundle frontier = %d, want %d", bundle.FrontierRound, receiverFrontier)
	}
	if bundle.IndexRoot != root {
		t.Errorf("bundle root = %x, want %x", bundle.IndexRoot[:4], root[:4])
	}
	if want := dag.commitEpochForRound(receiverFrontier); bundle.Epoch != want {
		t.Errorf("bundle epoch = %d, want %d (re-derived from the frontier)", bundle.Epoch, want)
	}

	if len(bundle.Headers) != 3 {
		t.Fatalf("bundle carries %d headers, want 3 (the wrong-root minority excluded)", len(bundle.Headers))
	}

	seen := make(map[Hash]bool)
	for _, record := range bundle.Headers {
		producer, frontier, claimedRoot := verifyHeaderRecord(t, record)

		if producer == vals[3].pubKey {
			t.Fatal("the wrong-root minority's header must not appear in the bundle")
		}
		if frontier != receiverFrontier {
			t.Errorf("header frontier = %d, want %d", frontier, receiverFrontier)
		}
		if claimedRoot != root {
			t.Errorf("header root = %x, want %x", claimedRoot[:4], root[:4])
		}

		seen[producer] = true
	}

	if len(seen) != 3 {
		t.Fatalf("bundle carries %d distinct producers, want 3 (a producer's stake must count once)", len(seen))
	}
}

// TestIndexAnchorBundle_WindowAnchoredAtOwnFrontier is the availability-DoS
// regression the plan pins: a stored header claiming an absurd future
// frontier must never drag the served window there. The window is anchored
// solely at this node's own CommittedFrontier(), so injecting one such
// vertex beside a genuine quorum must not change what gets served.
func TestIndexAnchorBundle_WindowAnchoredAtOwnFrontier(t *testing.T) {
	vals, dag, root := newBundleDAG(t)

	const round = receiverFrontier + 2

	for _, v := range vals[:3] {
		storeAnchoredVertex(t, dag, v, round, receiverFrontier, root)
	}

	const absurdFrontier = 1_000_000_000
	storeAnchoredVertex(t, dag, vals[3], round, absurdFrontier, Hash{0xEE})

	bundle, ok := dag.IndexAnchorBundle()
	if !ok {
		t.Fatal("the genuine quorum must still be served despite the absurd-frontier vertex")
	}

	if bundle.FrontierRound != receiverFrontier {
		t.Fatalf("bundle frontier = %d, want %d: an absurd claimed frontier moved the served window",
			bundle.FrontierRound, receiverFrontier)
	}

	if len(bundle.Headers) != 3 {
		t.Fatalf("bundle carries %d headers, want 3", len(bundle.Headers))
	}
}

// TestIndexAnchorBundle_NoIndexerUnavailable covers a DAG with no indexer
// wired: it can verify nothing, so it must report no bundle rather than
// panic on a nil seam.
func TestIndexAnchorBundle_NoIndexerUnavailable(t *testing.T) {
	vals, vs := newTestValidatorSet(4)
	dag := New(newTestStorage(t), vs, nil, testSystemPod, 0, vals[0].privKey, nil)
	t.Cleanup(func() { dag.Close() })

	if _, ok := dag.IndexAnchorBundle(); ok {
		t.Fatal("a DAG with no indexer wired must report no bundle")
	}
}

// TestIndexAnchorBundle_AbsurdStoredRoundBounded is the store-walk
// regression: collectAnchorTallies must never scan up to a maliciously
// inflated store.latestRound (the producer's own claim, not a value this
// node computed). Unlike TestIndexAnchorBundle_WindowAnchoredAtOwnFrontier,
// which pins an absurd CLAIMED frontier, this pins an absurd PRODUCTION
// round: three honest validators anchor the real root at committed+2 — well
// inside anchorScanSlack, so the in-slack case still works — and a fourth
// vertex is stored at round 1<<40. The genuine quorum must still be served,
// unaffected by the absurd vertex, and IndexAnchorBundle must return well
// within the bound asserted here: before the fix, the tally walk had no
// upper cap and would have counted from windowFloor up to 1<<40, wedging for
// as long as that takes — and every GetIndexAnchor request queued behind
// cmd/node's anchorCache.mu with it.
func TestIndexAnchorBundle_AbsurdStoredRoundBounded(t *testing.T) {
	vals, dag, root := newBundleDAG(t)

	const inSlackRound = receiverFrontier + 2
	for _, v := range vals[:3] {
		storeAnchoredVertex(t, dag, v, inSlackRound, receiverFrontier, root)
	}

	const absurdRound = 1 << 40
	storeAnchoredVertex(t, dag, vals[3], absurdRound, receiverFrontier, Hash{0xEE})

	type result struct {
		bundle AnchorBundle
		ok     bool
	}
	done := make(chan result, 1)

	go func() {
		bundle, ok := dag.IndexAnchorBundle()
		done <- result{bundle, ok}
	}()

	select {
	case r := <-done:
		if !r.ok {
			t.Fatal("the in-slack quorum at committed+2 must still be served despite the absurd-round vertex")
		}
		if r.bundle.FrontierRound != receiverFrontier {
			t.Fatalf("bundle frontier = %d, want %d: the absurd stored round changed the served bundle",
				r.bundle.FrontierRound, receiverFrontier)
		}

		seen := make(map[Hash]bool)
		for _, record := range r.bundle.Headers {
			producer, frontier, claimedRoot := verifyHeaderRecord(t, record)
			if producer == vals[3].pubKey {
				t.Fatal("the absurd-round vertex must not be collected into the bundle")
			}
			if frontier != receiverFrontier || claimedRoot != root {
				t.Errorf("header (frontier, root) = (%d, %x), want (%d, %x)",
					frontier, claimedRoot[:4], receiverFrontier, root[:4])
			}
			seen[producer] = true
		}
		if len(seen) != 3 {
			t.Fatalf("bundle carries %d distinct producers, want 3 (the committed+2 in-slack vertices)", len(seen))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("IndexAnchorBundle did not return within 5s: the store walk is wedged by the absurd stored round")
	}
}
