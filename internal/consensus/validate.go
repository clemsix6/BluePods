package consensus

import (
	"crypto/ed25519"
	"encoding/hex"
	"errors"
	"fmt"

	"BluePods/internal/logger"
	"BluePods/internal/types"
)

// Sentinel errors for the non-buffer validateVertex failure paths.
// classifyRejection maps each to its consensus.vertex.rejected reason code via
// errors.Is, rather than fragile error-string prefix matching (which is used
// only for the two BUFFER cases: isMissingParentError/isUnknownProducerError).
//
// errIndexRoot is the one sentinel that is NOT a rejection: it carries the
// quarantine verdict (the vertex is stored, withheld from relay and reference),
// so it never reaches classifyRejection — isQuarantineVerdict intercepts it at
// both admission sites first.
var (
	errBadSignature = errors.New("bad_signature")
	errWrongEpoch   = errors.New("wrong_epoch")
	errParentRound  = errors.New("parent_round")
	errParentQuorum = errors.New("parent_quorum")
	errFeeSummary   = errors.New("fee_summary")
	errIndexRoot    = errors.New("index_root")
)

// classifyRejection maps a terminal validateVertex error to its
// consensus.vertex.rejected reason code. It must only be called on an error
// that is NOT a buffer case (isMissingParentError/isUnknownProducerError were
// already checked and returned false) and NOT the quarantine verdict — every
// remaining validateVertex error path wraps exactly one of the sentinels below.
func classifyRejection(err error) string {
	switch {
	case errors.Is(err, errBadSignature):
		return "bad_signature"
	case errors.Is(err, errWrongEpoch):
		return "wrong_epoch"
	case errors.Is(err, errParentRound):
		return "parent_round"
	case errors.Is(err, errParentQuorum):
		return "parent_quorum"
	case errors.Is(err, errFeeSummary):
		return "fee_summary"
	default:
		return "unknown" // defensive fallback; unreachable in practice — a forgotten case must surface as "unknown", never masquerade as a real reason such as "fee_summary"
	}
}

// validateVertex performs full validation of a vertex before accepting it.
// This is the single entry point for all vertex validation (external and local).
func (d *DAG) validateVertex(v *types.Vertex, data []byte) error {
	// 1. Producer must be a known validator
	if err := d.validateProducer(v); err != nil {
		return err
	}

	// 2. Signature must be valid
	if err := d.validateSignature(v); err != nil {
		return err
	}

	// 3. Epoch must match current epoch
	if err := d.validateEpoch(v); err != nil {
		return err
	}

	// 4. Parents must exist and form quorum.
	// Use the vertex's round (not the node's current round) to determine if
	// validation should be relaxed. A vertex produced during the transition/buffer
	// window must always be accepted, even if it arrives via gossip after the
	// node's current round has moved past the buffer.
	if !d.isRoundInTransitionOrBuffer(v.Round()) {
		if err := d.validateParents(v); err != nil {
			return err
		}
	}

	// 5. Parents must represent quorum of validators from round-1
	if err := d.validateParentsQuorum(v); err != nil {
		return err
	}

	// 6. Fee summary must match recalculation from tx headers
	if err := d.validateFeeSummary(v); err != nil {
		return err
	}

	// 7. The anchored index root must match this node's own history, when this
	// node has the history to check it against (stage 1; unverifiable anchors
	// pass untouched).
	//
	// It runs LAST because its verdict is the only one that does not stop the
	// vertex from being STORED: a proven liar is quarantined, not dropped. Every
	// other check that ran must therefore have passed before it is reached, or
	// the quarantine door would be a way to put structurally invalid vertices
	// into every honest node's store. That claim is scoped, not absolute: check
	// 4 (validateParents) is skipped for every vertex, quarantined or not,
	// inside the transition/buffer window, so a quarantined vertex reached from
	// there may carry unvalidated parents. That is not exposure quarantine
	// introduces — any vertex, quarantined or clean, is storable on those same
	// terms in that window. A vertex that is both wrong-root and malformed
	// comes back with the malformed verdict and stays terminally rejected.
	return d.validateIndexAnchor(v)
}

// validateEpoch checks the epoch a vertex's header claims against a
// receiver-independent window derived from the vertex's OWN ANCHORED FRONTIER: the
// epoch that committed round belongs to (commitEpochForRound), plus or minus one.
//
// The two fields are read off the SAME state. A producer stamps the live epoch its
// commit path maintains (productionEpoch, the liveEpoch mirror) and anchors the
// frontier its commit path recorded (committedFrontier), and both move inside one
// commitNextRound call: advanceCommitCursor transitions the epoch at a boundary,
// setIndexFrontier records the round it just decided. An honest header is therefore
// internally consistent however far the node's commit cursor trails its production
// round — the property this rule needs, and the one the vertex's ROUND cannot give:
// the round comes from the PRODUCTION clock, and the gap between the two clocks is
// unbounded (a partition-healed node produces hundreds of rounds above the cursor
// it is stuck on, a cold-restarted one resumes production below its cursor, and
// sustained load lifts the lag past an epoch length with no fault at all). Bounding
// a commit-clock field by a production-clock window rejected all three network-wide.
//
// The window is one epoch either side of the frontier's own.
//
//   - Plus one is reached on every boundary: at round R = k*epochLength
//     transitionEpoch moves the live epoch to k BEFORE setIndexFrontier records R,
//     and commitEpochForRound maps that boundary round back to k-1 (it is committed
//     under the outgoing epoch's holders). The same edge covers a producer that
//     read the lock-free epoch mirror after a transition the frontier seam had not
//     caught up to yet.
//   - Minus one is the mirror-image tolerance. Production reads the frontier first
//     and the epoch second, so today the epoch can only be the fresher of the two;
//     the low side keeps the rule from depending on that read order, and the field
//     must be bounded on BOTH sides regardless: from the quorum-bundle work on it
//     names the validator tree a header's quorum is weighed against, so an
//     unbounded-below claim is a stale-validator-set attack.
//
// The receiver's own currentEpoch is deliberately NOT the reference. It is not
// network-uniform at any instant: the producer of a vertex in flight across a
// boundary is one epoch behind a receiver that already transitioned (the boundary
// window the anchor rule already tolerates in HoldersForEpoch), and a node that
// crashed, joined, or stalled trails the live epoch by however far its commit
// cursor trails — while it needs exactly those ahead-of-its-epoch tip vertices to
// buffer, trigger deep-gap recovery and catch up at all. Comparing against
// currentEpoch in either direction would reject them and wedge the node.
//
// A producer with no indexer wired anchors frontier 0 forever (committedFrontier's
// zero pair), so its window stays 0..1 however far its live epoch runs. That is not
// a carve-out this rule owes anything: stage 1 already ends that configuration one
// epoch earlier, since a zero index root is tolerated only while the vertex's own
// round is in the genesis epoch (validateIndexAnchor). Every production path wires
// the index before the DAG produces or verifies a vertex (cmd/node's fresh-chain,
// restart and sync constructions all pass WithIndexer), so an indexer-less DAG is a
// test and tooling configuration, and a test that needs a nonzero epoch anchors a
// frontier that reaches it.
//
// With epochs disabled (epochLength 0) no boundary is ever crossed, so the only
// epoch any header may claim is 0.
func (d *DAG) validateEpoch(v *types.Vertex) error {
	if d.epochLength == 0 {
		if v.Epoch() != 0 {
			return fmt.Errorf("epoch mismatch: epochs disabled, only epoch 0 is valid, got %d:\n%w",
				v.Epoch(), errWrongEpoch)
		}

		return nil
	}

	low, high := epochWindow(d.commitEpochForRound(v.FrontierRound()))

	if v.Epoch() < low || v.Epoch() > high {
		return fmt.Errorf("epoch mismatch: frontier %d allows epochs %d..%d, got %d:\n%w",
			v.FrontierRound(), low, high, v.Epoch(), errWrongEpoch)
	}

	return nil
}

// epochWindow returns the inclusive epoch window around the epoch the anchored
// frontier commits in: one below to one above (see validateEpoch for what each
// edge is), clamped at 0.
func epochWindow(frontierEpoch uint64) (low, high uint64) {
	if frontierEpoch > 0 {
		low = frontierEpoch - 1
	}

	return low, frontierEpoch + 1
}

// validateProducer checks the producer is in the validator set.
// During init phase (before minValidators), accepts any producer to allow
// observing the bootstrap chain and learning about new validators.
func (d *DAG) validateProducer(v *types.Vertex) error {
	producer := extractProducer(v)

	// During init, accept vertices from any producer.
	// This allows nodes to observe bootstrap's chain and commit registrations.
	if d.minValidators > 0 && d.validators.Len() < d.minValidators {
		return nil
	}

	if !d.validators.Contains(producer) {
		return fmt.Errorf("unknown producer: %x", producer)
	}

	return nil
}

// validateSignature verifies the Ed25519 signature over the vertex identity, and
// first re-derives that identity from the vertex's own content: a valid signature
// over a hash nothing binds to the body would let a producer sign one vertex and
// ship another.
func (d *DAG) validateSignature(v *types.Vertex) error {
	sig := v.SignatureBytes()
	if len(sig) != ed25519.SignatureSize {
		return fmt.Errorf("invalid signature size: %d:\n%w", len(sig), errBadSignature)
	}

	pubkey := v.ProducerBytes()
	if len(pubkey) != ed25519.PublicKeySize {
		return fmt.Errorf("invalid pubkey size: %d:\n%w", len(pubkey), errBadSignature)
	}

	hashBytes := v.HashBytes()
	if len(hashBytes) != 32 {
		return fmt.Errorf("invalid hash size: %d:\n%w", len(hashBytes), errBadSignature)
	}

	if err := validateVertexHash(v); err != nil {
		return err
	}

	if !ed25519.Verify(pubkey, hashBytes, sig) {
		return fmt.Errorf("invalid signature:\n%w", errBadSignature)
	}

	return nil
}

// validateVertexHash re-derives the vertex identity from the body it carries and
// the header it declares, and requires both to match what the vertex claims. It is
// what binds the detached header to the body: the declared body_hash must be the
// body's real hash (or the header a light verifier is served would describe a
// different vertex), and the identity must be the hash of the declared header (or
// the parent links and store keys would point at something else entirely).
func validateVertexHash(v *types.Vertex) error {
	identity, bodyHash := vertexIdentity(v)

	if bodyHash == malformedBody {
		return fmt.Errorf("unreadable vertex body:\n%w", errBadSignature)
	}

	if declared := hashFrom(v.BodyHashBytes()); declared != bodyHash {
		return fmt.Errorf("body_hash mismatch: declared %x, computed %x:\n%w",
			declared[:8], bodyHash[:8], errBadSignature)
	}

	if declared := hashFrom(v.HashBytes()); declared != identity {
		return fmt.Errorf("header hash mismatch: declared %x, computed %x:\n%w",
			declared[:8], identity[:8], errBadSignature)
	}

	return nil
}

// validateParents checks all parents exist and are from round N-1.
func (d *DAG) validateParents(v *types.Vertex) error {
	round := v.Round()

	if round == 0 {
		return nil
	}

	parentCount := v.ParentsLength()
	if parentCount == 0 {
		return fmt.Errorf("no parents for round %d:\n%w", round, errParentRound)
	}

	var link types.VertexLink
	for i := 0; i < parentCount; i++ {
		if !v.Parents(&link, i) {
			return fmt.Errorf("failed to read parent %d:\n%w", i, errParentRound)
		}

		if err := d.validateParentLink(&link, round); err != nil {
			return err
		}
	}

	return nil
}

// validateParentLink checks a single parent link.
// If the parent is not found and its producer is unknown, the parent is skipped.
// This allows vertices from known validators to be accepted even when they
// reference parents from validators not yet registered on this node.
func (d *DAG) validateParentLink(link *types.VertexLink, round uint64) error {
	parentHash := extractLinkHash(link)
	parentProducer := extractLinkProducer(link)
	parent := d.store.get(parentHash)

	if parent == nil {
		// If the parent's producer is unknown, skip this parent.
		// The vertex producer (a known validator) has already validated it.
		// The parent will be fully validated when its producer registers.
		if !d.validators.Contains(parentProducer) {
			return nil
		}

		// The parent is a known validator's vertex that is not yet stored (it may be
		// sitting in the pending buffer). Do NOT admit this child with an absent
		// parent: a stored vertex whose round-(R-1) parent is missing lets the commit
		// loop read a different round-R candidate/citation set than a peer that has the
		// parent, so the causal batch composition — and the round at which a
		// registration commits, hence when the committee grows — becomes arrival-order
		// dependent and forks the committed log during bootstrap. Return the missing-
		// parent error so AddVertex buffers this child and reprocesses it once the
		// parent is stored, keeping the store causally closed.
		logger.Debug("missing parent",
			"parentHash", hex.EncodeToString(parentHash[:8]),
			"parentProducer", hex.EncodeToString(parentProducer[:8]),
			"forRound", round,
		)
		return fmt.Errorf("parent not found: %x", parentHash)
	}

	if parent.Round() != round-1 {
		return fmt.Errorf("parent round mismatch: expected %d, got %d:\n%w", round-1, parent.Round(), errParentRound)
	}

	return nil
}

// validateParentsQuorum ensures parents reference at least 1 known validator.
// This is intentionally a presence check, not the authoritative quorum. The
// authoritative quorum is stake-weighted (3*cappedSum >= 2*total over the epoch
// holder snapshot) and is enforced at production in hasQuorumFromRound and at
// commit by the anchor rule (directAnchorVerdict). A receiving node cannot recompute another node's
// stake-weighted quorum during convergence (validator sets and stakes may differ
// transiently), so here it only confirms the vertex links at least one producer
// it recognizes; the producing validator already enforced the real quorum.
func (d *DAG) validateParentsQuorum(v *types.Vertex) error {
	round := v.Round()

	// Round 0 has no parents requirement
	if round == 0 {
		return nil
	}

	// During init phase, skip quorum check.
	// Only bootstrap produces, so we can't have quorum yet.
	if d.minValidators > 0 && d.validators.Len() < d.minValidators {
		return nil
	}

	// Count unique known validator producers from parents
	knownParents := 0
	var link types.VertexLink

	for i := 0; i < v.ParentsLength(); i++ {
		if !v.Parents(&link, i) {
			continue
		}

		producer := extractLinkProducer(&link)
		if d.validators.Contains(producer) {
			knownParents++
			break // At least 1 known parent is sufficient
		}
	}

	if knownParents == 0 {
		return fmt.Errorf("no known parent producers for round %d:\n%w", round, errParentQuorum)
	}

	return nil
}

// validateFeeSummary verifies the vertex fee_summary by recalculating from tx
// headers, over the consumed portion only (in lockstep with buildFeeSummary).
// The storage deposit is locked in the object, not pooled, so it is not summarized.
// Skipped if fee system is not active (feeParams nil).
func (d *DAG) validateFeeSummary(v *types.Vertex) error {
	if d.feeParams == nil {
		return nil
	}

	declared := v.FeeSummary(nil)
	if declared == nil {
		// No fee summary declared and fees are enabled: only ok if no transactions
		if v.TransactionsLength() == 0 {
			return nil
		}
		return fmt.Errorf("missing fee_summary with %d transactions:\n%w", v.TransactionsLength(), errFeeSummary)
	}

	// Recalculate from transaction headers
	var totalFees, totalBurned, totalEpoch uint64
	var atx types.AttestedTransaction

	for i := 0; i < v.TransactionsLength(); i++ {
		if !v.Transactions(&atx, i) {
			continue
		}

		tx := atx.Transaction(nil)
		if tx == nil || len(tx.GasCoinBytes()) != 32 {
			continue
		}

		// Summarize the consumed portion only, in lockstep with buildFeeSummary;
		// the storage deposit is locked in the object and is not pooled.
		consumed, _ := d.calculateTxFeeSplit(tx, &atx)
		split := SplitFee(consumed, *d.feeParams)

		totalFees += split.Total
		totalBurned += split.Burned
		totalEpoch += split.Epoch
	}

	if declared.TotalFees() != totalFees {
		return fmt.Errorf("fee_summary.total_fees mismatch: declared %d, computed %d:\n%w",
			declared.TotalFees(), totalFees, errFeeSummary)
	}
	if declared.TotalBurned() != totalBurned {
		return fmt.Errorf("fee_summary.total_burned mismatch: declared %d, computed %d:\n%w",
			declared.TotalBurned(), totalBurned, errFeeSummary)
	}
	if declared.TotalEpoch() != totalEpoch {
		return fmt.Errorf("fee_summary.total_epoch mismatch: declared %d, computed %d:\n%w",
			declared.TotalEpoch(), totalEpoch, errFeeSummary)
	}

	return nil
}

// extractLinkProducer extracts the producer hash from a vertex link.
func extractLinkProducer(link *types.VertexLink) Hash {
	var h Hash
	if b := link.ProducerBytes(); len(b) == 32 {
		copy(h[:], b)
	}
	return h
}
