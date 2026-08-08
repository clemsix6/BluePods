package consensus

import (
	"crypto/ed25519"
)

// AnchorQuorumSince is the joiner's side of the anchor: the highest frontier
// at or above minFrontier for which the headers this node holds, signed by
// members of judge and carrying a capped-stake quorum of it, report exactly
// the root this node RECOMPUTED for itself at that frontier. ok is false when
// no such frontier exists, which is the fail-closed outcome a syncing node
// must refuse to go live on.
//
// It differs from IndexAnchorBundle, which serves clients, in the two ways
// that matter to a node deciding whether to trust its own imported state:
//
//   - the quorum is weighed against judge, an EXPLICIT validator set the
//     caller has authenticated out of band, never against the set the frontier's
//     own epoch resolves to. A joiner's epoch state comes from the same snapshot
//     as its state, so letting the frontier pick the judge would let one source
//     supply both the claim and the authority over it;
//   - every header's signature is verified here. Ingress verifies the
//     signature of a gossiped vertex, but snapshot-imported vertices enter the
//     store through WithImportData without passing ingress at all, so a lying
//     bootstrap can plant records naming honest producers over signatures they
//     never made. Counting one would let the source forge the quorum weighing
//     its own snapshot.
//
// The root comparison itself is inherited from collectAnchorTallies: a header
// counts only when its claimed (frontier, root) matches this node's own
// RootAt, so what the returned quorum attests is always the caller's
// recomputation, never the caller's import.
func (d *DAG) AnchorQuorumSince(minFrontier uint64, judge *ValidatorSet) (AnchorBundle, bool) {
	if d.indexer == nil || judge == nil || judge.Len() == 0 {
		return AnchorBundle{}, false
	}

	committed, _ := d.indexer.CommittedFrontier()
	if committed < minFrontier {
		return AnchorBundle{}, false
	}

	floor := anchorQuorumFloor(minFrontier, committed)
	tallies := d.collectAnchorTallies(floor, committed)

	for frontier := committed; ; frontier-- {
		if bundle, ok := d.verifiedBundleAt(frontier, tallies[frontier], judge); ok {
			return bundle, true
		}

		if frontier == floor {
			return AnchorBundle{}, false
		}
	}
}

// anchorQuorumFloor is the lowest frontier a joiner's gate scans: never below
// minFrontier (a quorum at an older frontier says nothing about the state the
// joiner rebuilt, and accepting one would let a source satisfy the gate with
// genuinely attested ancient history), and never more than bundleWindow rounds
// below this node's own committed frontier, which bounds the scan while the
// gate polls a live, advancing DAG.
func anchorQuorumFloor(minFrontier, committed uint64) uint64 {
	if committed < bundleWindow {
		return minFrontier
	}

	if windowFloor := committed - (bundleWindow - 1); windowFloor > minFrontier {
		return windowFloor
	}

	return minFrontier
}

// verifiedBundleAt reports the bundle at one candidate frontier, counting only
// the tallied records that are signed by a member of judge with that member's
// own key, and only when those members carry judge's capped-stake quorum. The
// root it reports is read back from this node's own retained history, so a
// caller never learns a root from the headers it just weighed.
func (d *DAG) verifiedBundleAt(frontier uint64, group map[Hash][]byte, judge *ValidatorSet) (AnchorBundle, bool) {
	if len(group) == 0 {
		return AnchorBundle{}, false
	}

	producers := make(map[Hash]bool, len(group))
	headers := make([][]byte, 0, len(group))

	for producer, record := range group {
		if judge.Get(producer) == nil || !verifiedHeaderRecord(producer, record) {
			continue
		}

		producers[producer] = true
		headers = append(headers, record)
	}

	if !d.reachesStrictQuorum(judge, producers) {
		return AnchorBundle{}, false
	}

	root, ok := d.indexer.RootAt(frontier)
	if !ok {
		return AnchorBundle{}, false
	}

	return AnchorBundle{
		FrontierRound: frontier,
		IndexRoot:     root,
		Epoch:         d.commitEpochForRound(frontier),
		Headers:       headers,
	}, true
}

// verifiedHeaderRecord reports whether a {header ‖ signature} record carries
// producer's own Ed25519 signature over the vertex identity its header
// implies — the same check a light client runs, and the same one recordAnchorFault
// runs before storing evidence.
func verifiedHeaderRecord(producer Hash, record []byte) bool {
	if len(record) != headerSize+ed25519.SignatureSize {
		return false
	}

	identity := taggedHash(headerDomainTag, record[:headerSize])

	return ed25519.Verify(producer[:], identity[:], record[headerSize:])
}
