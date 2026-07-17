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

	// vertexRangeChunk caps how many vertices a range response carries. A vertex is
	// small, so a few hundred stay well under the network message-size limit while
	// still closing a deep gap in a few round-trips.
	vertexRangeChunk = 256
)

// meshVertexFetcher recovers a stalled commit cursor by requesting absent vertices
// from mesh peers, either by hash (a decided anchor's missing ancestry) or by round
// range (a far-behind node's deep gap). It implements consensus.VertexFetcher.
// Fetches run asynchronously and are deduplicated by in-flight hash and in-flight
// range, so the commit loop — which re-triggers every tick while stalled — never
// blocks under the commit lock and issues at most one outstanding request per key.
type meshVertexFetcher struct {
	node           *Node                       // node provides the peer set and the DAG ingest path
	mu             sync.Mutex                  // mu guards inFlight and inFlightRanges
	inFlight       map[consensus.Hash]struct{} // inFlight holds only the hashes with a request outstanding
	inFlightRanges map[[2]uint64]struct{}      // inFlightRanges holds only the round spans with a request outstanding
}

// newVertexFetcher builds the mesh vertex fetcher for this node. Every DAG
// construction path wires it via SetVertexFetcher; leaving it unset ships
// missing-ancestor recovery as dead code and stalls joiners.
func (n *Node) newVertexFetcher() *meshVertexFetcher {
	return &meshVertexFetcher{
		node:           n,
		inFlight:       make(map[consensus.Hash]struct{}),
		inFlightRanges: make(map[[2]uint64]struct{}),
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

// FetchRange requests the round span [from, to] from peers in its own goroutine and
// returns immediately, so the commit loop is never blocked on network I/O. An identical
// span already in flight is skipped, so a stalled cursor re-triggering each tick issues
// at most one outstanding request per span.
func (f *meshVertexFetcher) FetchRange(from, to uint64) {
	if f.beginRange(from, to) {
		go f.fetchRange(from, to)
	}
}

// beginRange marks a span in flight, returning false when a request is already
// outstanding for it.
func (f *meshVertexFetcher) beginRange(from, to uint64) bool {
	f.mu.Lock()
	defer f.mu.Unlock()

	key := [2]uint64{from, to}
	if _, busy := f.inFlightRanges[key]; busy {
		return false
	}

	f.inFlightRanges[key] = struct{}{}
	return true
}

// finishRange clears a span's in-flight mark so a later stalled tick can retry it.
func (f *meshVertexFetcher) finishRange(from, to uint64) {
	f.mu.Lock()
	delete(f.inFlightRanges, [2]uint64{from, to})
	f.mu.Unlock()
}

// fetchRange asks each connected peer for the vertices in [from, to] until one returns
// a non-empty chunk, then ingests every vertex through the normal AddVertex path. Each
// vertex is re-validated there (producer signature, parents), so a peer cannot inject a
// forged or out-of-range vertex; the cursor promotes its pending buffer as the gap
// fills, and a later stalled tick re-requests the still-open remainder of the span.
func (f *meshVertexFetcher) fetchRange(from, to uint64) {
	defer f.finishRange(from, to)

	if f.node.network == nil || f.node.dag == nil {
		return
	}

	req := network.EncodeGetVertexRange(&network.GetVertexRangeRequest{From: from, To: to})

	for _, peer := range f.node.network.Peers() {
		vertices := f.requestRangeFrom(peer, req)
		if len(vertices) == 0 {
			continue
		}

		for _, data := range vertices {
			f.node.dag.AddVertex(data)
		}
		return
	}
}

// requestRangeFrom performs one range request against a peer and returns the vertices
// it served, or nil on any transport or decode error so fetchRange tries another peer.
func (f *meshVertexFetcher) requestRangeFrom(peer *network.Peer, req []byte) [][]byte {
	ctx, cancel := context.WithTimeout(context.Background(), vertexFetchTimeout)
	defer cancel()

	respBytes, err := peer.Request(ctx, req)
	if err != nil {
		return nil
	}

	resp, err := network.DecodeGetVertexRangeResp(respBytes)
	if err != nil {
		return nil
	}

	return resp.Vertices
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
//
// A Byzantine peer may answer Found=true with a truncated or malformed payload; the
// length guard rejects the obvious cases and the deferred recover contains any
// FlatBuffers parse panic on a crafted buffer, discarding the response (ok stays
// false) so the node tries the next peer instead of crashing (I3).
func vertexHashIs(data []byte, want consensus.Hash) (ok bool) {
	defer func() {
		if r := recover(); r != nil {
			ok = false
		}
	}()

	if len(data) < 4 {
		return false
	}

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

// handleGetVertexRange serves a mesh peer the vertices this node holds in the requested
// round span, ascending and bounded to one chunk (vertexRangeChunk) so the response
// stays within the message-size limit. It enumerates only the asked-for span; a
// far-behind peer marches up a deep gap over successive requests as its cursor advances.
func (n *Node) handleGetVertexRange(data []byte) ([]byte, error) {
	req, err := network.DecodeGetVertexRange(data)
	if err != nil {
		return nil, err
	}

	if n.dag == nil {
		return network.EncodeGetVertexRangeResp(&network.GetVertexRangeResponse{}), nil
	}

	vertices := n.dag.VertexRange(req.From, req.To, vertexRangeChunk)

	return network.EncodeGetVertexRangeResp(&network.GetVertexRangeResponse{Vertices: vertices}), nil
}
