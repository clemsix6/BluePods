package client

import (
	"fmt"
	"time"

	"BluePods/internal/index"
	"BluePods/internal/network"
)

// FRESHNESS, the choice spec §5 leaves to the caller and this file exposes.
//
// A proved query answers at the SERVING NODE'S committed frontier, while the
// anchor bundle serves the highest frontier a stake quorum has been observed
// for — always the same or older, since a producer's vertex anchors a frontier
// a couple of rounds behind. The two therefore rarely name the same round, and
// a client that waited for the rounds to match would wait forever. What it
// matches is the INDEX ROOT: most committed rounds change no index entry, so
// the root at the bundle's frontier and the root at the answer's frontier are
// usually the same 32 bytes, and a proof folding to that root is attested.
// When the index has just moved, the two roots differ and the client waits for
// the bundle to catch up (WaitForFrontier) before reading again.
//
// So a client that just saw its own transaction finalize at round R has
// exactly two options, and both are here:
//
//   - WaitForFrontier(R, ...) then a proved read, sub-second and verifiable;
//   - the unproven live read (Client.DomainResolve and friends), which returns
//     the node's own word with no proof at all. It is the right choice only
//     when the answer is not worth verifying.
//
// A LightClient is not safe for concurrent use: it walks its own checkpoint
// forward, which is a write.

const (
	// anchorPollInterval is how often WaitForFrontier re-asks for a bundle.
	// The wait is for the network's next vertices to attest a frontier, which
	// takes a couple of rounds, not for anything this client can hurry.
	anchorPollInterval = 100 * time.Millisecond

	// readAnchorWait bounds the wait a proved read makes when the serving
	// node's index moved past the newest attested root. It covers the few
	// rounds it takes producers to anchor the new frontier.
	readAnchorWait = 5 * time.Second
)

// indexSource is the node surface a light client reads. The QUIC transport
// implements it; the seam exists so verification can be exercised against a
// scripted node without a network.
type indexSource interface {
	GetIndexAnchor() (*network.GetIndexAnchorResponse, error)
	GetValidatorTree(epoch uint64) (*network.GetValidatorTreeResponse, error)
	ResolveDomainProved(name string) (*network.DomainResolveResponse, error)
	ListChildren(parent [32]byte) (*network.ListChildrenResponse, error)
	GetAncestors(object [32]byte) (*network.GetAncestorsResponse, error)
}

// LightClient reads the index from one node and verifies every answer itself,
// trusting that node for nothing: proofs fold to component roots, the
// components combine to an index root, and a capped-stake quorum of
// producer-signed headers attests that root. Its only trust is the checkpoint
// it starts from, which it walks forward across epoch boundaries.
type LightClient struct {
	src indexSource // src is the node this client reads from

	checkpoint Checkpoint   // checkpoint is the trust anchor, advanced by the epoch walk
	set        ValidatorSet // set is the authenticated committee for checkpoint.Epoch, empty until first use
}

// NewLightClient returns a light client reading through c and trusting cp.
func NewLightClient(c *Client, cp Checkpoint) *LightClient {
	return &LightClient{src: c.transport, checkpoint: cp}
}

// Checkpoint returns the client's current trust anchor. It advances as the
// epoch walk crosses boundaries, so persisting it between runs is what saves a
// client from needing a fresh out-of-band pin.
func (lc *LightClient) Checkpoint() Checkpoint {
	return lc.checkpoint
}

// Anchor fetches the node's quorum bundle and verifies it against the
// authenticated committee, returning the attested (frontier, index root) pair
// every proof is checked against.
//
// It is also where the epoch walk happens. Under the spec §5 handoff rule a
// committee at epoch N weighs headers at N or N+1, so a bundle attested by
// N+1 headers still verifies here; that attested root then commits to the N+1
// committee, which Anchor reads back and re-pins the checkpoint to. The walk
// is OPPORTUNISTIC: if the new committee cannot be authenticated right now
// (the serving node's index has already moved past the attested root), the
// checkpoint stays where it is and the anchor this call verified is returned
// regardless. Declining to advance trust costs nothing immediately — the
// handoff window still covers the reads — and the client hard-fails only once
// it falls a full epoch behind that window, which is the weak-subjectivity
// boundary, not a bug to paper over.
func (lc *LightClient) Anchor() (VerifiedAnchor, error) {
	set, err := lc.validatorSet()
	if err != nil {
		return VerifiedAnchor{}, err
	}

	bundle, err := lc.src.GetIndexAnchor()
	if err != nil {
		return VerifiedAnchor{}, fmt.Errorf("anchor bundle:\n%w", err)
	}

	attested, err := VerifyAnchor(bundle, set)
	if err != nil {
		return VerifiedAnchor{}, err
	}

	if attested.Epoch > set.Epoch {
		lc.advance(attested)
	}

	return attested, nil
}

// WaitForFrontier polls for a bundle attesting a frontier at or beyond round,
// which is what a client does after seeing its own transaction finalize there.
// It returns as soon as one exists, and fails at the bound rather than waiting
// forever on a node whose commit has stalled.
func (lc *LightClient) WaitForFrontier(round uint64, timeout time.Duration) (VerifiedAnchor, error) {
	deadline := time.Now().Add(timeout)

	var last error

	for {
		attested, err := lc.Anchor()
		if err == nil && attested.FrontierRound >= round {
			return attested, nil
		}

		last = err

		if time.Now().After(deadline) {
			if last != nil {
				return VerifiedAnchor{}, fmt.Errorf("no attested frontier reached %d before the bound:\n%w", round, last)
			}

			return VerifiedAnchor{}, fmt.Errorf("no attested frontier reached %d before the bound", round)
		}

		time.Sleep(anchorPollInterval)
	}
}

// ResolveDomain resolves a name and verifies the answer. found is false when
// the name provably has no leaf; the lease's expiry is the caller's to read
// off the returned leaf.
func (lc *LightClient) ResolveDomain(name string) (index.DomainLeaf, bool, error) {
	resp, err := lc.src.ResolveDomainProved(name)
	if err != nil {
		return index.DomainLeaf{}, false, err
	}

	attested, err := lc.attest(resp.Anchor)
	if err != nil {
		return index.DomainLeaf{}, false, err
	}

	return attested.VerifyDomain(resp, name)
}

// ListChildren enumerates a parent's children — an owner key or an object ID —
// and verifies the enumeration is complete against the attested root.
func (lc *LightClient) ListChildren(parent [32]byte) ([][32]byte, error) {
	resp, err := lc.src.ListChildren(parent)
	if err != nil {
		return nil, err
	}

	attested, err := lc.attest(resp.Anchor)
	if err != nil {
		return nil, err
	}

	return attested.VerifyChildren(resp, parent)
}

// Ancestors walks an object's parent chain and verifies every hop, including
// that each hop continues the previous one and that the walk truly terminates.
func (lc *LightClient) Ancestors(object [32]byte) ([]index.ParentLeaf, error) {
	resp, err := lc.src.GetAncestors(object)
	if err != nil {
		return nil, err
	}

	attested, err := lc.attest(resp.Anchor)
	if err != nil {
		return nil, err
	}

	return attested.VerifyAncestry(resp, object)
}

// attest returns the verified anchor covering one answer's anchoring block: a
// bundle whose attested index root is the root that answer's proofs fold to.
// Roots are matched, never rounds (see this file's freshness note). When the
// newest attested root is not the answer's, the index moved between the two
// reads and this waits for a bundle at or past the answer's own frontier; if
// the roots still differ, the answer is stale and the caller re-reads.
func (lc *LightClient) attest(anchor network.ProvedIndexAnchor) (VerifiedAnchor, error) {
	if !anchor.Anchored {
		return VerifiedAnchor{}, errUnanchored
	}

	attested, err := lc.Anchor()
	if err != nil {
		return VerifiedAnchor{}, err
	}

	if attested.IndexRoot == anchor.IndexRoot {
		return attested, nil
	}

	attested, err = lc.WaitForFrontier(anchor.FrontierRound, readAnchorWait)
	if err != nil {
		return VerifiedAnchor{}, err
	}

	if attested.IndexRoot != anchor.IndexRoot {
		return VerifiedAnchor{}, fmt.Errorf("the index moved under the read: answer at root %x, attested root %x",
			anchor.IndexRoot[:8], attested.IndexRoot[:8])
	}

	return attested, nil
}

// validatorSet returns the authenticated committee for the checkpoint's epoch,
// fetching and checking it against the checkpoint on first use.
func (lc *LightClient) validatorSet() (ValidatorSet, error) {
	if len(lc.set.Leaves) > 0 {
		return lc.set, nil
	}

	resp, err := lc.src.GetValidatorTree(lc.checkpoint.Epoch)
	if err != nil {
		return ValidatorSet{}, fmt.Errorf("validator tree:\n%w", err)
	}

	set, err := lc.checkpoint.authenticate(resp)
	if err != nil {
		return ValidatorSet{}, err
	}

	lc.set = set

	return set, nil
}

// advance re-pins the checkpoint to the committee the attested root commits
// to, the second half of the epoch walk. It gives up quietly on any failure:
// see Anchor for why declining to advance is safe.
func (lc *LightClient) advance(attested VerifiedAnchor) {
	resp, err := lc.src.GetValidatorTree(attested.Epoch)
	if err != nil || resp == nil || resp.Epoch != attested.Epoch {
		return
	}

	set, err := attested.VerifyValidatorSet(resp)
	if err != nil {
		return
	}

	lc.checkpoint = Checkpoint{
		Epoch:            set.Epoch,
		IndexRoot:        attested.IndexRoot,
		ValidatorSetHash: index.ValidatorRootOf(set.Leaves),
	}
	lc.set = set
}
