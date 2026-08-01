package main

import (
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"BluePods/internal/consensus"
	"BluePods/internal/events"
	"BluePods/internal/index"
	"BluePods/internal/logger"
)

// THE VERIFICATION CHAIN a joining node walks before it goes live. Every link
// is checked in this file; a break in any of them aborts the join (spec §9's
// fail-closed sync, §5's checkpoint model).
//
//  1. OUT OF BAND. The operator obtains one pair from a node it already
//     trusts: an epoch and that epoch's validator-set root. Every node
//     publishes exactly that pair as epoch.validators.frozen whenever the
//     index's validator tree is (re)built. It is the ONLY input to a join that
//     does not come from the source being verified — standard weak
//     subjectivity, stated rather than assumed.
//
//  2. IMPORT. The snapshot carries state (tracker, domains), the regime (the
//     frozen holder snapshots per epoch) and vertex history. All of it is the
//     source's claim, none of it is evidence yet.
//
//  3. JUDGE AUTHENTICATION (trustedJudge). The joiner rebuilds the checkpointed
//     epoch's validator tree from the holder snapshot it just imported and
//     requires its root to equal the checkpoint's. The leaf hashes pubkey,
//     capped stake, BLS key and jail status, so equality means the imported
//     committee IS the checkpointed committee, member for member and weight
//     for weight. A source that invented a committee to judge its own snapshot
//     is refused here — which is the point: without this link the bootstrap
//     supplies both the state and the authority over it.
//
//  4. LOCAL RECOMPUTATION. The joiner rebuilds all four trees from the imported
//     state and records their combined root at the frontier that state
//     describes, then at every frontier its own commit loop decides (spec §9:
//     the trees are rebuilt, never shipped — nothing Merkle-shaped travels on
//     the wire).
//
//  5. ATTESTATION (awaitAnchorQuorum -> consensus.AnchorQuorumSince). The
//     joiner requires a capped-stake quorum OF THE AUTHENTICATED SET, each
//     member's own signature verified over the header identity, all reporting
//     the root the joiner itself recomputed, at a frontier at or after the one
//     its rebuilt state describes.
//
//  6. CONCLUSION. A stake quorum of a validator set pinned out of band attests
//     the state this node rebuilt for itself. Under the protocol's standing
//     honest-supermajority assumption that state is the network's, and only
//     then does the node go live.
//
// Links 3 and 5 are bound, not independent: the combined root the quorum
// matches has the validator tree as one of its four components
// (index.CombinedRoot), and for a frontier inside the checkpointed epoch that
// component IS the checkpointed root.
//
// WHAT THIS CHAIN DOES NOT CLOSE, precisely. Spec §5 describes a checkpoint as
// (epoch, indexRoot, validator set hash); only the last component is verifiable
// by a joiner today, so it is the one the flag carries. The joiner cannot
// recompute a PAST frontier's combined index root: the snapshot is pinned at
// the source's last committed round and carries no versioned history, and the
// per-epoch root checkpoints (index.Manager's epochCheckpoints) are in-memory
// per-node state that neither the snapshot nor any message carries. Pinning the
// index root instead would either fail every honest join under load (the root
// moves with every committed batch between publication and the snapshot being
// cut) or, if weakened to "the root appears somewhere in the attested history",
// be satisfied for free by a fabricating source that copies the public root
// into its own headers. Closing it would need the snapshot pinned AT the
// checkpointed frontier, or a validator-subtree proof against the checkpointed
// index root. Two consequences follow and are accepted: an attacker holding a
// stake quorum's signing keys is not caught here (that is the honest-majority
// assumption, not something this gate can add), and history older than the
// joiner's retained epochs cannot be checkpointed against (a stale checkpoint
// is refused, never silently accepted).
//
// FRESHNESS IS NOT CLOSED EITHER, and it is a separate gap from the ones
// above. minFrontier is derived from the snapshot itself (the committed round
// the rebuilt state describes), never from an independent "current round"
// signal, and the quorum requirement in link 5 asks only whether the
// checkpointed committee attested THAT frontier, not whether that frontier is
// recent. A source in a position to eclipse the joiner (control every peer it
// syncs and gossips through) can therefore serve a genuine snapshot from
// further back in the chain's history, with genuine matching attestations
// from the time it was cut, and the chain above accepts it: every link
// verifies authenticity, none of them verifies recency. This is the plan's
// accepted rule, not an oversight to close here — see Task 5.2's as-built
// note.

const (
	// anchorQuorumTimeout bounds how long a joining node waits for the network
	// to attest the state it rebuilt. It is a wait for gossip to deliver
	// enough headers, not a retry of anything: replay ends at the tip, and the
	// producers' next vertices anchor frontiers at or above it within a few
	// rounds.
	anchorQuorumTimeout = 30 * time.Second

	// anchorQuorumPoll is how often the wait re-tests the quorum. The test
	// reads the vertex store and the index seam only, never the commit lock.
	anchorQuorumPoll = 250 * time.Millisecond

	// reasonSyncUnverified is the node.stopping reason for a join aborted by
	// this gate: the node had state but no proof, and refused to serve it.
	reasonSyncUnverified = "sync_unverified"

	// checkpointSeparator splits the --trust-checkpoint value into its epoch
	// and its hex root.
	checkpointSeparator = ":"
)

var (
	// errNoTrustAnchor rejects a non-genesis join that pinned nothing. The
	// default cannot be "trust the snapshot's own validator set": that lets one
	// source supply both the state and the judge, which is the lie this whole
	// gate exists to catch.
	errNoTrustAnchor = errors.New("a non-genesis join requires --trust-checkpoint <epoch>:<validator-root hex> (or --insecure-bootstrap to join unverified)")

	// errCheckpointFormat rejects a malformed --trust-checkpoint value.
	errCheckpointFormat = errors.New("malformed --trust-checkpoint")

	// errCheckpointEpoch rejects a checkpoint naming an epoch this node holds
	// no frozen holder snapshot for: too old to bind, or from another network.
	errCheckpointEpoch = errors.New("no frozen validator set for the checkpointed epoch")

	// errCheckpointMismatch rejects a snapshot whose validator set is not the
	// checkpointed one.
	errCheckpointMismatch = errors.New("the snapshot's validator set does not match the trusted checkpoint")

	// errAnchorQuorum rejects state no stake quorum of the trusted set attests.
	errAnchorQuorum = errors.New("no stake quorum attests the rebuilt index root")
)

// trustCheckpoint is the out-of-band trust root a joining node pins: an epoch
// and the root of that epoch's frozen validator tree.
type trustCheckpoint struct {
	epoch         uint64   // epoch is the epoch whose frozen holder snapshot the root commits to
	validatorRoot [32]byte // validatorRoot is index.ValidatorRootOf that epoch's leaves
}

// parseTrustCheckpoint parses the --trust-checkpoint value, "<epoch>:<64 hex
// characters>". It is strict on purpose: a checkpoint silently misread is a
// checkpoint silently not enforced.
func parseTrustCheckpoint(value string) (trustCheckpoint, error) {
	epochText, rootText, found := strings.Cut(value, checkpointSeparator)
	if !found {
		return trustCheckpoint{}, fmt.Errorf("%w %q: want <epoch>:<validator-root hex>", errCheckpointFormat, value)
	}

	epoch, err := strconv.ParseUint(epochText, 10, 64)
	if err != nil {
		return trustCheckpoint{}, fmt.Errorf("%w %q: epoch is not a number", errCheckpointFormat, value)
	}

	root, err := hex.DecodeString(rootText)
	if err != nil {
		return trustCheckpoint{}, fmt.Errorf("%w %q: root is not hex", errCheckpointFormat, value)
	}

	if len(root) != 32 {
		return trustCheckpoint{}, fmt.Errorf("%w %q: root is %d bytes, want 32", errCheckpointFormat, value, len(root))
	}

	cp := trustCheckpoint{epoch: epoch}
	copy(cp.validatorRoot[:], root)

	return cp, nil
}

// verifySyncedState is the gate performSync closes a join on: it verifies the
// chain above and, on any failure, records the refusal as node.stopping with
// the "sync_unverified" reason before handing the error back. minFrontier is
// the committed frontier the rebuilt state describes.
//
// THE ONE RULE, enforced at all three sites that know both flags
// (parseFlags' validateTrustAnchor, the harness's buildArgs, and here): the
// checkpoint wins whenever both are set. parseFlags refuses the combination
// outright (an operator who typed both gets an immediate error, not a silent
// pick); the other two sites cannot refuse — a Config built in-process
// bypasses parseFlags entirely, and the harness only ever emits one of the
// two flags to begin with — so both take the checkpoint path rather than the
// hatch when a caller (by omission or otherwise) leaves InsecureBootstrap set
// alongside a non-empty TrustCheckpoint.
func (n *Node) verifySyncedState(minFrontier uint64) error {
	if n.cfg.TrustCheckpoint == "" && n.cfg.InsecureBootstrap {
		logger.Warn("INSECURE BOOTSTRAP: joining WITHOUT a trusted checkpoint",
			"consequence", "the bootstrap supplies both this node's state and the validator set that judges it; nothing here proves the state is the network's")

		return nil
	}

	if err := n.verifyJoin(minFrontier, anchorQuorumTimeout); err != nil {
		logger.Error("refusing to go live on unverified state", "error", err)
		events.NodeStopping(reasonSyncUnverified)

		return err
	}

	return nil
}

// verifyJoin runs links 3 and 5 of the chain: authenticate the judge against
// the pinned checkpoint, then require that judge's stake quorum to attest this
// node's own recomputed root at a frontier at or above minFrontier. It is
// separated from verifySyncedState so tests drive it with their own bound.
func (n *Node) verifyJoin(minFrontier uint64, timeout time.Duration) error {
	if n.cfg.TrustCheckpoint == "" {
		return errNoTrustAnchor
	}

	cp, err := parseTrustCheckpoint(n.cfg.TrustCheckpoint)
	if err != nil {
		return err
	}

	judge, err := n.trustedJudge(cp)
	if err != nil {
		return err
	}

	return n.awaitAnchorQuorum(judge, minFrontier, timeout)
}

// trustedJudge returns the validator set the checkpoint authenticates: the
// frozen holder snapshot this node imported for the checkpointed epoch, but
// only once its rebuilt validator tree hashes to the checkpointed root. The
// returned set is the ONLY authority the join is weighed against; everything
// else about the snapshot is still unproven at this point.
func (n *Node) trustedJudge(cp trustCheckpoint) (*consensus.ValidatorSet, error) {
	set, ok := n.dag.HoldersForEpoch(cp.epoch)
	if !ok || set.Len() == 0 {
		return nil, fmt.Errorf("%w: checkpoint names epoch %d, this node synced into epoch %d and retains only its neighbours",
			errCheckpointEpoch, cp.epoch, n.dag.Epoch())
	}

	root := index.ValidatorRootOf(n.dag.ValidatorLeaves(set.All()))
	if root != cp.validatorRoot {
		return nil, fmt.Errorf("%w: epoch %d holds %d validators rooted at %x, checkpoint pins %x",
			errCheckpointMismatch, cp.epoch, set.Len(), root[:8], cp.validatorRoot[:8])
	}

	logger.Info("trusted checkpoint matched",
		"epoch", cp.epoch,
		"validators", set.Len(),
		"root", hex.EncodeToString(root[:8]),
	)

	return set, nil
}

// awaitAnchorQuorum blocks until a stake quorum of judge attests this node's
// own recomputed root at a frontier at or above minFrontier, or the bound
// expires. Waiting is not leniency: replay ends at the tip and the network's
// next vertices anchor it within a few rounds, so the wait covers gossip
// latency only. When it expires, nothing attested the state and the join is
// refused.
func (n *Node) awaitAnchorQuorum(judge *consensus.ValidatorSet, minFrontier uint64, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)

	for {
		if bundle, ok := n.dag.AnchorQuorumSince(minFrontier, judge); ok {
			logger.Info("synced state verified against the anchored root",
				"frontier", bundle.FrontierRound,
				"root", hex.EncodeToString(bundle.IndexRoot[:8]),
				"attesting_validators", len(bundle.Headers),
			)

			return nil
		}

		if time.Now().After(deadline) {
			frontier, root := n.idxManager.CommittedFrontier()

			return fmt.Errorf("%w: rebuilt state at frontier %d roots at %x, no quorum of the %d checkpointed validators matched it at or above frontier %d within %s",
				errAnchorQuorum, frontier, root[:8], judge.Len(), minFrontier, timeout)
		}

		time.Sleep(anchorQuorumPoll)
	}
}
