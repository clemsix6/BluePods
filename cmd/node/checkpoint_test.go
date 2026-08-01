package main

import (
	"bytes"
	"encoding/hex"
	"errors"
	"io"
	"strconv"
	"strings"
	"testing"
	"time"

	"BluePods/internal/consensus"
	"BluePods/internal/index"
	"BluePods/internal/logger"
)

// TestParseTrustCheckpoint_AcceptsTheOperatorPair round-trips the value an
// operator copies out of a trusted node's epoch.validators.frozen event.
func TestParseTrustCheckpoint_AcceptsTheOperatorPair(t *testing.T) {
	var want [32]byte
	want[0], want[31] = 0xAB, 0xCD

	cp, err := parseTrustCheckpoint("7:" + hex.EncodeToString(want[:]))
	if err != nil {
		t.Fatalf("parseTrustCheckpoint: %v", err)
	}

	if cp.epoch != 7 {
		t.Errorf("epoch = %d, want 7", cp.epoch)
	}
	if cp.validatorRoot != want {
		t.Errorf("root = %x, want %x", cp.validatorRoot[:4], want[:4])
	}
}

// TestParseTrustCheckpoint_RejectsMalformed keeps the parser strict: a
// checkpoint silently misread is a checkpoint silently not enforced, and every
// one of these shapes is a plausible operator slip.
func TestParseTrustCheckpoint_RejectsMalformed(t *testing.T) {
	full := strings.Repeat("ab", 32)

	cases := []struct {
		name  string
		value string
	}{
		{"empty", ""},
		{"no separator", "7" + full},
		{"epoch not a number", "seven:" + full},
		{"root not hex", "7:" + strings.Repeat("zz", 32)},
		{"root too short", "7:" + strings.Repeat("ab", 31)},
		{"root too long", "7:" + strings.Repeat("ab", 33)},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := parseTrustCheckpoint(tc.value); !errors.Is(err, errCheckpointFormat) {
				t.Fatalf("parseTrustCheckpoint(%q) error = %v, want errCheckpointFormat", tc.value, err)
			}
		})
	}
}

// TestValidateTrustAnchor_SyncingNodeMustPinSomething is the mandatory-flag
// rule: a node that will sync either pins a checkpoint or opts out loudly.
// There is no third option, because the third option is trusting the source's
// own validator set, which proves nothing.
func TestValidateTrustAnchor_SyncingNodeMustPinSomething(t *testing.T) {
	full := strings.Repeat("ab", 32)

	cases := []struct {
		name string
		cfg  Config
		want error
	}{
		{"genesis bootstrap needs nothing", Config{Bootstrap: true}, nil},
		{"joiner with a checkpoint", Config{BootstrapAddr: "127.0.0.1:9000", TrustCheckpoint: "3:" + full}, nil},
		{"joiner opting out", Config{BootstrapAddr: "127.0.0.1:9000", InsecureBootstrap: true}, nil},
		{"joiner with nothing", Config{BootstrapAddr: "127.0.0.1:9000"}, errNoTrustAnchor},
		{"joiner with a malformed checkpoint", Config{BootstrapAddr: "127.0.0.1:9000", TrustCheckpoint: "3:beef"}, errCheckpointFormat},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.cfg.validateTrustAnchor()

			if tc.want == nil && err != nil {
				t.Fatalf("validateTrustAnchor() = %v, want nil", err)
			}
			if tc.want != nil && !errors.Is(err, tc.want) {
				t.Fatalf("validateTrustAnchor() = %v, want %v", err, tc.want)
			}
		})
	}
}

// TestValidateTrustAnchor_RejectsBothAtOnce refuses an ambiguous
// configuration: verified and unverified are a choice, and a node started with
// both would leave an operator believing the checkpoint is being enforced.
func TestValidateTrustAnchor_RejectsBothAtOnce(t *testing.T) {
	cfg := Config{
		BootstrapAddr:     "127.0.0.1:9000",
		TrustCheckpoint:   "3:" + strings.Repeat("ab", 32),
		InsecureBootstrap: true,
	}

	if err := cfg.validateTrustAnchor(); err == nil {
		t.Fatal("--insecure-bootstrap beside --trust-checkpoint must be refused, not silently resolved")
	}
}

// joinedNode builds the joiner a snapshot produces and stops its loops, so
// every assertion below reads a settled node. The DAG stays usable: the gate
// reads the vertex store and the index seam, never the commit loop.
func joinedNode(t *testing.T, src syncSource) *Node {
	t.Helper()

	n := syncedJoiner(t, src.result)
	if err := n.initConsensusForValidator(src.result); err != nil {
		t.Fatalf("initConsensusForValidator: %v", err)
	}
	n.dag.Close()

	return n
}

// publishedCheckpoint is the pair the SOURCE node publishes as
// epoch.validators.frozen: the epoch it is in and the root of that epoch's
// frozen validator tree. An operator reads exactly this off a node it trusts
// and hands it to the joiner, so deriving it from the source (never from the
// joiner) is what makes the assertions below mean anything.
func publishedCheckpoint(t *testing.T, src syncSource) string {
	t.Helper()

	dag := src.live.dag
	root := index.ValidatorRootOf(dag.ValidatorLeaves(dag.EpochHolders().All()))

	return formatCheckpoint(dag.Epoch(), root)
}

// formatCheckpoint renders a --trust-checkpoint value.
func formatCheckpoint(epoch uint64, root [32]byte) string {
	return strconv.FormatUint(epoch, 10) + ":" + hex.EncodeToString(root[:])
}

// TestTrustedJudge_AcceptsTheSourcesPublishedSet is the checkpoint's pass
// case: the joiner rebuilds the checkpointed epoch's validator tree from the
// holder snapshot the source shipped and reaches the same root the source
// publishes. Only then does that set become the authority weighing the join.
func TestTrustedJudge_AcceptsTheSourcesPublishedSet(t *testing.T) {
	src := syncSnapshotFromLiveNode(t)
	n := joinedNode(t, src)

	n.cfg.TrustCheckpoint = publishedCheckpoint(t, src)

	cp, err := parseTrustCheckpoint(n.cfg.TrustCheckpoint)
	if err != nil {
		t.Fatalf("parseTrustCheckpoint: %v", err)
	}

	judge, err := n.trustedJudge(cp)
	if err != nil {
		t.Fatalf("the source's own published checkpoint must match the set it shipped: %v", err)
	}

	if judge.Len() == 0 {
		t.Fatal("the authenticated judge is empty: a quorum weighed against it would be vacuous")
	}
}

// TestTrustedJudge_RejectsASetTheCheckpointDoesNotPin is the substitution the
// whole flag exists to catch: the committee the snapshot carries is not the
// committee the operator pinned. Here the pinned root belongs to a set with one
// extra member, which is exactly what a bootstrap inventing validators to
// approve its own snapshot would produce.
func TestTrustedJudge_RejectsASetTheCheckpointDoesNotPin(t *testing.T) {
	src := syncSnapshotFromLiveNode(t)
	n := joinedNode(t, src)

	leaves := n.dag.ValidatorLeaves(n.dag.EpochHolders().All())
	invented := append(leaves, index.ValidatorLeaf{Pubkey: [32]byte{0xFF}, CappedStake: 1_000_000})

	n.cfg.TrustCheckpoint = formatCheckpoint(n.dag.Epoch(), index.ValidatorRootOf(invented))

	cp, err := parseTrustCheckpoint(n.cfg.TrustCheckpoint)
	if err != nil {
		t.Fatalf("parseTrustCheckpoint: %v", err)
	}

	if _, err := n.trustedJudge(cp); !errors.Is(err, errCheckpointMismatch) {
		t.Fatalf("trustedJudge error = %v, want errCheckpointMismatch: a committee the checkpoint does not pin must never judge this join", err)
	}
}

// TestTrustedJudge_RejectsAnEpochItCannotBind covers a checkpoint too old (or
// from another network) to bind anything: the joiner holds no frozen set for
// it, so there is nothing to authenticate and the join must be refused rather
// than fall back to the snapshot's own set.
func TestTrustedJudge_RejectsAnEpochItCannotBind(t *testing.T) {
	src := syncSnapshotFromLiveNode(t)
	n := joinedNode(t, src)

	cp := trustCheckpoint{epoch: n.dag.Epoch() + 99}

	if _, err := n.trustedJudge(cp); !errors.Is(err, errCheckpointEpoch) {
		t.Fatalf("trustedJudge error = %v, want errCheckpointEpoch", err)
	}
}

// TestVerifyJoin_TamperedSnapshotNeverReachesQuorum is the plan's tampered
// case at the node level: ONE flipped tracker parent in the imported state.
// The checkpoint still matches (the committee was not touched — a lying
// bootstrap has no reason to break the part that is checked out of band), so
// the refusal has to come from the anchor quorum: the joiner recomputes a root
// no validator ever attested, and no wait makes that root appear.
func TestVerifyJoin_TamperedSnapshotNeverReachesQuorum(t *testing.T) {
	src := syncSnapshotFromLiveNode(t)

	tampered := *src.result
	tampered.trackerEntries = append([]consensus.ObjectTrackerEntry(nil), src.result.trackerEntries...)
	if len(tampered.trackerEntries) == 0 {
		t.Fatal("fixture: the snapshot carries no tracker entries to tamper with")
	}
	tampered.trackerEntries[0].Parent[0] ^= 0xFF

	n := joinedNode(t, syncSource{live: src.live, result: &tampered, frontier: src.frontier, root: src.root})

	if got := n.idxManager.Root(); got == src.root {
		t.Fatal("fixture: the flipped tracker parent rebuilt the source's root, so the case is vacuous")
	}

	n.cfg.TrustCheckpoint = publishedCheckpoint(t, src)

	err := n.verifyJoin(src.frontier, 300*time.Millisecond)
	if !errors.Is(err, errAnchorQuorum) {
		t.Fatalf("verifyJoin error = %v, want errAnchorQuorum: a tampered snapshot must never go live", err)
	}
}

// TestVerifySyncedState_RefusalIsObservable pins the operator-facing half of
// the refusal: the typed error comes back AND the node records why it is
// stopping, so an operator (and the scenario harness) sees "sync_unverified"
// rather than a node that quietly never became ready.
func TestVerifySyncedState_RefusalIsObservable(t *testing.T) {
	src := syncSnapshotFromLiveNode(t)
	n := joinedNode(t, src)

	// A checkpoint pinning a committee this snapshot does not carry: refused
	// before any waiting, which is what keeps this test fast.
	n.cfg.TrustCheckpoint = formatCheckpoint(n.dag.Epoch(), [32]byte{0xDE, 0xAD})

	logs := captureLogs(t)

	if err := n.verifySyncedState(src.frontier); !errors.Is(err, errCheckpointMismatch) {
		t.Fatalf("verifySyncedState error = %v, want errCheckpointMismatch", err)
	}

	if !strings.Contains(logs.String(), `"event":"node.stopping"`) ||
		!strings.Contains(logs.String(), `"reason":"`+reasonSyncUnverified+`"`) {
		t.Fatalf("a refused join must emit node.stopping with reason %q, got:\n%s", reasonSyncUnverified, logs.String())
	}
}

// TestVerifySyncedState_InsecureBootstrapSkipsLoudly covers the escape hatch:
// it verifies nothing, and it says so in terms an operator cannot mistake for
// a healthy join.
func TestVerifySyncedState_InsecureBootstrapSkipsLoudly(t *testing.T) {
	src := syncSnapshotFromLiveNode(t)
	n := joinedNode(t, src)

	n.cfg.InsecureBootstrap = true

	logs := captureLogs(t)

	if err := n.verifySyncedState(src.frontier); err != nil {
		t.Fatalf("--insecure-bootstrap must skip verification, got: %v", err)
	}

	if !strings.Contains(logs.String(), "INSECURE BOOTSTRAP") {
		t.Fatalf("the escape hatch must warn loudly, got:\n%s", logs.String())
	}
}

// captureLogs redirects the process logger into a buffer for the duration of
// one test, restoring a silent logger afterwards.
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()

	buf := &bytes.Buffer{}
	logger.UseJSON(buf)
	t.Cleanup(func() { logger.SetOutput(io.Discard) })

	return buf
}
