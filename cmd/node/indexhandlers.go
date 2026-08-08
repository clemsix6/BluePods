package main

import (
	gosync "sync"

	"BluePods/internal/consensus"
	"BluePods/internal/network"
)

// indexAnchorCache holds the most recently assembled quorum bundle, keyed by
// the committed frontier it was built at, so repeated GetIndexAnchor requests
// at the same frontier are served without re-walking the vertex store — the
// bundle is reassembled only when this node's own commit cursor has advanced
// past what is cached, never once per request.
type indexAnchorCache struct {
	mu     gosync.Mutex
	seeded bool                   // seeded distinguishes "never built" from a genuine committed frontier 0
	cursor uint64                 // cursor is the committed frontier the cached bundle was built at
	bundle consensus.AnchorBundle // bundle is the cached bundle
	ok     bool                   // ok reports whether bundle is quorate
}

// handleGetIndexAnchor serves the cached quorum-attested index anchor bundle
// to a light client: the highest recent frontier for which this node found a
// capped-stake quorum of producer-signed headers agreeing with its own index
// root, ready to verify with no further trust in this node. The response
// carries Found=false when no indexer is wired yet or no frontier in the
// serving window currently reaches quorum.
func (n *Node) handleGetIndexAnchor() ([]byte, error) {
	bundle, ok := n.cachedAnchorBundle()
	if !ok {
		return network.EncodeGetIndexAnchorResp(&network.GetIndexAnchorResponse{}), nil
	}

	return network.EncodeGetIndexAnchorResp(&network.GetIndexAnchorResponse{
		Found:         true,
		FrontierRound: bundle.FrontierRound,
		IndexRoot:     bundle.IndexRoot,
		Epoch:         bundle.Epoch,
		Headers:       bundle.Headers,
	}), nil
}

// cachedAnchorBundle returns the quorum bundle for this node's current
// committed frontier, reassembling it only when that frontier has advanced
// past what is cached. It never runs on the commit path and never takes
// commitMu: both the frontier check (idxManager.CommittedFrontier) and the
// reassembly it may trigger (dag.IndexAnchorBundle) read only the indexer
// seam and the vertex store, exactly like ingress anchor validation does.
func (n *Node) cachedAnchorBundle() (consensus.AnchorBundle, bool) {
	if n.dag == nil || n.idxManager == nil {
		return consensus.AnchorBundle{}, false
	}

	committed, _ := n.idxManager.CommittedFrontier()

	n.anchorCache.mu.Lock()
	defer n.anchorCache.mu.Unlock()

	if n.anchorCache.seeded && n.anchorCache.cursor == committed {
		return n.anchorCache.bundle, n.anchorCache.ok
	}

	n.anchorCache.bundle, n.anchorCache.ok = n.dag.IndexAnchorBundle()
	n.anchorCache.cursor = committed
	n.anchorCache.seeded = true

	return n.anchorCache.bundle, n.anchorCache.ok
}
