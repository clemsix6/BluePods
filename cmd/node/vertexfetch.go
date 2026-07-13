package main

import (
	"context"
	"sync"
	"time"

	"BluePods/internal/consensus"
	"BluePods/internal/logger"
	"BluePods/internal/network"
	"BluePods/internal/types"
)

const (
	// vertexFetchTimeout bounds one mesh vertex-fetch round-trip.
	vertexFetchTimeout = 5 * time.Second
)

// meshVertexFetcher recovers a decided anchor's missing ancestry by requesting the
// absent vertices from mesh peers. It implements consensus.VertexFetcher. Fetches
// run asynchronously and are deduplicated by in-flight hash, so the commit loop —
// which re-triggers every tick while stalled — never blocks under the commit lock
// and issues at most one outstanding request per hash.
type meshVertexFetcher struct {
	node     *Node                       // node provides the peer set and the DAG ingest path
	mu       sync.Mutex                  // mu guards inFlight
	inFlight map[consensus.Hash]struct{} // inFlight holds only the hashes with a request outstanding
}

// newVertexFetcher builds the mesh vertex fetcher for this node. Every DAG
// construction path wires it via SetVertexFetcher; leaving it unset ships
// missing-ancestor recovery as dead code and stalls joiners.
func (n *Node) newVertexFetcher() *meshVertexFetcher {
	return &meshVertexFetcher{
		node:     n,
		inFlight: make(map[consensus.Hash]struct{}),
	}
}

// FetchVertices requests each not-already-in-flight hash from peers in its own
// goroutine and returns immediately, so the commit loop is never blocked on
// network I/O.
func (f *meshVertexFetcher) FetchVertices(hashes []consensus.Hash) {
	for _, h := range hashes {
		if f.begin(h) {
			go f.fetchOne(h)
		}
	}
}

// begin marks a hash in flight, returning false when a request is already
// outstanding for it. This bounds concurrency to one request per hash and keeps a
// permanently-unfetchable hash from spawning a goroutine on every stalled tick.
func (f *meshVertexFetcher) begin(h consensus.Hash) bool {
	f.mu.Lock()
	defer f.mu.Unlock()

	if _, busy := f.inFlight[h]; busy {
		return false
	}

	f.inFlight[h] = struct{}{}
	return true
}

// finish clears a hash's in-flight mark so a later stalled tick can retry it. The
// map only ever holds currently-outstanding hashes, so it never grows unbounded.
func (f *meshVertexFetcher) finish(h consensus.Hash) {
	f.mu.Lock()
	delete(f.inFlight, h)
	f.mu.Unlock()
}

// fetchOne asks each connected peer for the vertex until one returns it with a
// matching hash, then ingests it through the normal AddVertex path (picked up by the
// commit loop on a later tick). A hash mismatch is discarded and the next peer tried.
func (f *meshVertexFetcher) fetchOne(h consensus.Hash) {
	defer f.finish(h)

	if f.node.network == nil || f.node.dag == nil {
		return
	}

	req := network.EncodeGetVertex(&network.GetVertexRequest{Hash: h})

	for _, peer := range f.node.network.Peers() {
		data := f.requestVertexFrom(peer, req, h)
		if data == nil {
			continue
		}

		f.node.dag.AddVertex(data)
		return
	}
}

// requestVertexFrom performs one vertex request against a peer and returns the
// vertex bytes only when the peer holds it AND its embedded hash equals the
// requested hash. A mismatch is discarded (returns nil) so fetchOne tries another
// peer and the real hash stays outstanding — a malicious peer cannot pin the request
// with a different valid vertex (I5).
func (f *meshVertexFetcher) requestVertexFrom(peer *network.Peer, req []byte, want consensus.Hash) []byte {
	ctx, cancel := context.WithTimeout(context.Background(), vertexFetchTimeout)
	defer cancel()

	respBytes, err := peer.Request(ctx, req)
	if err != nil {
		return nil
	}

	resp, err := network.DecodeGetVertexResp(respBytes)
	if err != nil || !resp.Found {
		return nil
	}

	if !vertexHashIs(resp.Data, want) {
		logger.Debug("discarding mismatched vertex-fetch response", "want_prefix", want[:4])
		return nil
	}

	return resp.Data
}

// vertexHashIs reports whether the serialized vertex's own hash field equals want.
// The fetcher discards a response that does not, so a peer cannot answer a request
// for one hash with a different vertex. AddVertex then re-verifies the producer
// signature over that hash, so a forged hash field is rejected downstream.
func vertexHashIs(data []byte, want consensus.Hash) bool {
	v := types.GetRootAsVertex(data, 0)

	hb := v.HashBytes()
	if len(hb) != 32 {
		return false
	}

	var got consensus.Hash
	copy(got[:], hb)

	return got == want
}

// handleGetVertex serves the single stored vertex named by the request hash to a
// mesh peer, or a not-found response when it is absent. It returns exactly the
// requested vertex and enumerates nothing else.
func (n *Node) handleGetVertex(data []byte) ([]byte, error) {
	req, err := network.DecodeGetVertex(data)
	if err != nil {
		return nil, err
	}

	if n.dag == nil {
		return network.EncodeGetVertexResp(&network.GetVertexResponse{Found: false}), nil
	}

	vertex := n.dag.VertexBytes(req.Hash)
	if vertex == nil {
		return network.EncodeGetVertexResp(&network.GetVertexResponse{Found: false}), nil
	}

	return network.EncodeGetVertexResp(&network.GetVertexResponse{Found: true, Data: vertex}), nil
}
