package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"testing"
	"time"

	flatbuffers "github.com/google/flatbuffers/go"
	"github.com/zeebo/blake3"

	"BluePods/internal/consensus"
	"BluePods/internal/network"
	"BluePods/internal/podvm"
	"BluePods/internal/state"
	"BluePods/internal/storage"
	"BluePods/internal/types"
)

// newFetchTestNode assembles the minimal real Node scaffolding the vertex-fetch
// wiring needs: temp storage, a real QUIC network node, an empty state, and a
// private key. The consensus DAG is left for the caller to build via a real
// construction path.
func newFetchTestNode(t *testing.T) *Node {
	t.Helper()

	dir := t.TempDir()
	db, err := storage.New(dir)
	if err != nil {
		t.Fatalf("storage: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	_, priv, _ := ed25519.GenerateKey(rand.Reader)

	netNode, err := network.NewNode(network.Config{PrivateKey: priv, ListenAddr: "127.0.0.1:0"})
	if err != nil {
		t.Fatalf("network: %v", err)
	}
	t.Cleanup(func() { netNode.Close() })

	return &Node{
		cfg: &Config{
			PrivateKey:    priv,
			QUICAddress:   "127.0.0.1:0",
			InitialMint:   1_000_000,
			MinValidators: 2,
		},
		storage: db,
		network: netNode,
		state:   state.New(db, podvm.New()),
		stats:   newStats(),
		txIndex: newTxStatusIndex(),
	}
}

// buildRound0Vertex builds a validly-signed round-0, epoch-0 vertex (no parents, no
// transactions) so a listener DAG holding its producer accepts it through AddVertex.
// It returns the serialized bytes and the vertex hash.
func buildRound0Vertex(t *testing.T, priv ed25519.PrivateKey) ([]byte, consensus.Hash) {
	t.Helper()

	var producer [32]byte
	copy(producer[:], priv.Public().(ed25519.PublicKey))

	build := func(hash, sig []byte) []byte {
		b := flatbuffers.NewBuilder(256)

		types.VertexStartParentsVector(b, 0)
		parents := b.EndVector(0)
		types.VertexStartTransactionsVector(b, 0)
		txs := b.EndVector(0)

		var hashVec, sigVec flatbuffers.UOffsetT
		if hash != nil {
			hashVec = b.CreateByteVector(hash)
		}
		if sig != nil {
			sigVec = b.CreateByteVector(sig)
		}
		prodVec := b.CreateByteVector(producer[:])

		types.VertexStart(b)
		if hash != nil {
			types.VertexAddHash(b, hashVec)
		}
		types.VertexAddRound(b, 0)
		types.VertexAddProducer(b, prodVec)
		if sig != nil {
			types.VertexAddSignature(b, sigVec)
		}
		types.VertexAddParents(b, parents)
		types.VertexAddTransactions(b, txs)
		types.VertexAddEpoch(b, 0)
		b.Finish(types.VertexEnd(b))

		return b.FinishedBytes()
	}

	unsigned := build(nil, nil)
	hash := blake3.Sum256(unsigned)
	sig := ed25519.Sign(priv, hash[:])

	data := build(hash[:], sig)

	var h consensus.Hash
	copy(h[:], hash[:])

	return data, h
}

// startServingNode builds a server node whose DAG holds the given vertex and serves
// it through the REAL request-handler wiring (setupRequestHandlers ->
// handleClientMessage -> handleGetVertex), then starts its network listener.
func startServingNode(t *testing.T, producer ed25519.PrivateKey, vertex []byte) *Node {
	t.Helper()

	n := newFetchTestNode(t)

	var prod consensus.Hash
	copy(prod[:], producer.Public().(ed25519.PublicKey))

	vs := consensus.NewValidatorSet([]consensus.Hash{prod})
	n.dag = consensus.New(n.storage, vs, nil, n.systemPod, 0, n.cfg.PrivateKey, n.state)
	t.Cleanup(func() { n.dag.Close() })

	if !n.dag.AddVertex(vertex) {
		t.Fatal("server failed to store the vertex it must serve")
	}

	n.setupRequestHandlers()
	if err := n.network.Start(); err != nil {
		t.Fatalf("start server network: %v", err)
	}

	return n
}

// newListenerClient builds a client through the REAL synced-listener construction
// path (initConsensusForListener), which must wire the vertex fetcher, with the
// producer in its validator set so a fetched round-0 vertex passes AddVertex.
func newListenerClient(t *testing.T, producer ed25519.PrivateKey) *Node {
	t.Helper()

	n := newFetchTestNode(t)

	var prod consensus.Hash
	copy(prod[:], producer.Public().(ed25519.PublicKey))

	result := &snapshotResult{
		validators: []*consensus.ValidatorInfo{{Pubkey: prod}},
	}

	if err := n.initConsensusForListener(result); err != nil {
		t.Fatalf("listener init: %v", err)
	}
	t.Cleanup(func() { n.dag.Close() })

	if err := n.network.Start(); err != nil {
		t.Fatalf("start client network: %v", err)
	}

	return n
}

// waitForVertex polls the DAG until the vertex is present or the deadline passes.
func waitForVertex(n *Node, hash consensus.Hash) bool {
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if n.dag.VertexBytes(hash) != nil {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}

	return false
}

// TestVertexFetcherWiredOnEveryConstructionPath is the anti-regression guard for the
// bug this task fixes: the recovery was shipped with SetVertexFetcher never called, so
// every joiner stalled. Each real DAG construction path — bootstrap, synced validator,
// synced listener — must leave a fetcher wired.
func TestVertexFetcherWiredOnEveryConstructionPath(t *testing.T) {
	bootstrap := newFetchTestNode(t)
	bootstrap.cfg.Bootstrap = true
	if err := bootstrap.initConsensus(); err != nil {
		t.Fatalf("bootstrap init: %v", err)
	}
	if !bootstrap.dag.VertexFetcherWired() {
		t.Fatal("bootstrap construction path did not wire the vertex fetcher")
	}
	bootstrap.dag.Close()

	validator := newFetchTestNode(t)
	if err := validator.initConsensusForValidator(&snapshotResult{}); err != nil {
		t.Fatalf("synced-validator init: %v", err)
	}
	if !validator.dag.VertexFetcherWired() {
		t.Fatal("synced-validator construction path did not wire the vertex fetcher")
	}
	validator.dag.Close()

	listener := newFetchTestNode(t)
	if err := listener.initConsensusForListener(&snapshotResult{}); err != nil {
		t.Fatalf("synced-listener init: %v", err)
	}
	if !listener.dag.VertexFetcherWired() {
		t.Fatal("synced-listener construction path did not wire the vertex fetcher")
	}
	listener.dag.Close()
}

// TestVertexFetch_RealWiring_DeliversMissingVertex drives the fetcher over real QUIC
// against the real serving handler: a client missing a vertex requests it, receives
// it, and ingests it through AddVertex. This is the path the reverted attempt shipped
// but never reached, so joiners stalled.
func TestVertexFetch_RealWiring_DeliversMissingVertex(t *testing.T) {
	_, producer, _ := ed25519.GenerateKey(rand.Reader)
	vertex, want := buildRound0Vertex(t, producer)

	server := startServingNode(t, producer, vertex)
	client := newListenerClient(t, producer)

	if !client.dag.VertexFetcherWired() {
		t.Fatal("listener path did not wire the fetcher")
	}
	if client.dag.VertexBytes(want) != nil {
		t.Fatal("client already holds the vertex; scenario is void")
	}

	if _, err := client.network.Connect(server.network.Addr()); err != nil {
		t.Fatalf("connect: %v", err)
	}

	client.newVertexFetcher().FetchVertices([]consensus.Hash{want})

	if !waitForVertex(client, want) {
		t.Fatal("client never received the missing vertex through the real fetch wiring")
	}
}

// TestVertexFetch_RealWiring_DiscardsHashMismatchAndRetries verifies I5: a peer that
// answers with a DIFFERENT valid vertex is discarded (never ingested) and the request
// falls through to an honest peer. A liar alone cannot satisfy the request.
func TestVertexFetch_RealWiring_DiscardsHashMismatchAndRetries(t *testing.T) {
	_, producer, _ := ed25519.GenerateKey(rand.Reader)
	vertex, want := buildRound0Vertex(t, producer)

	// A different, unrelated valid vertex the liar returns for every request.
	_, otherProducer, _ := ed25519.GenerateKey(rand.Reader)
	otherVertex, otherHash := buildRound0Vertex(t, otherProducer)

	evil := newFetchTestNode(t)
	evil.network.OnRequest(func(_ *network.Peer, _ []byte) ([]byte, error) {
		return network.EncodeGetVertexResp(&network.GetVertexResponse{Found: true, Data: otherVertex}), nil
	})
	if err := evil.network.Start(); err != nil {
		t.Fatalf("start evil network: %v", err)
	}

	// A liar alone cannot satisfy the request, and its mismatched vertex is discarded.
	lonely := newListenerClient(t, producer)
	if _, err := lonely.network.Connect(evil.network.Addr()); err != nil {
		t.Fatalf("connect evil: %v", err)
	}
	lonely.newVertexFetcher().FetchVertices([]consensus.Hash{want})
	time.Sleep(500 * time.Millisecond)
	if lonely.dag.VertexBytes(want) != nil {
		t.Fatal("a mismatched response satisfied the request; the real hash must stay outstanding")
	}
	if lonely.dag.VertexBytes(otherHash) != nil {
		t.Fatal("the mismatched vertex was ingested; it must be discarded")
	}

	// With an honest peer also connected, the retry reaches it and lands the real vertex.
	honest := startServingNode(t, producer, vertex)
	client := newListenerClient(t, producer)
	if _, err := client.network.Connect(evil.network.Addr()); err != nil {
		t.Fatalf("connect evil: %v", err)
	}
	if _, err := client.network.Connect(honest.network.Addr()); err != nil {
		t.Fatalf("connect honest: %v", err)
	}

	client.newVertexFetcher().FetchVertices([]consensus.Hash{want})

	if !waitForVertex(client, want) {
		t.Fatal("retry did not reach the honest peer after the liar's mismatch")
	}
	if client.dag.VertexBytes(otherHash) != nil {
		t.Fatal("the liar's mismatched vertex was ingested; it must be discarded")
	}
}

// TestVertexHashIs_MalformedPayloadNeverPanics is the I3 guard: a Byzantine peer
// answering Found=true with a truncated or garbage payload must be discarded, never
// crash the node. vertexHashIs must return false for every malformed buffer, catching
// any FlatBuffers parse panic instead of propagating it.
func TestVertexHashIs_MalformedPayloadNeverPanics(t *testing.T) {
	var want consensus.Hash
	want[0] = 0xAB

	cases := [][]byte{
		nil,
		{},
		{0x01},
		{0x01, 0x02, 0x03},
		{0xFF, 0xFF, 0xFF, 0xFF},             // 4 bytes: a bogus root offset
		{0x08, 0x00, 0x00, 0x00, 0x00, 0x00}, // offset points past the buffer
	}

	for i, data := range cases {
		if vertexHashIs(data, want) {
			t.Fatalf("case %d: malformed payload matched the wanted hash", i)
		}
	}
}

// TestVertexRangeFetch_RealWiring_DeliversSpan drives the range fetcher over real QUIC
// against the real serving handler (handleClientMessage -> handleGetVertexRange): a
// client missing a vertex requests the round span holding it, receives the chunk, and
// ingests it through AddVertex. This exercises the deep-catch-up path end to end.
func TestVertexRangeFetch_RealWiring_DeliversSpan(t *testing.T) {
	_, producer, _ := ed25519.GenerateKey(rand.Reader)
	vertex, want := buildRound0Vertex(t, producer)

	server := startServingNode(t, producer, vertex)
	client := newListenerClient(t, producer)

	if client.dag.VertexBytes(want) != nil {
		t.Fatal("client already holds the vertex; scenario is void")
	}

	if _, err := client.network.Connect(server.network.Addr()); err != nil {
		t.Fatalf("connect: %v", err)
	}

	client.newVertexFetcher().FetchRange(0, 0)

	if !waitForVertex(client, want) {
		t.Fatal("client never received the vertex through the real range-fetch wiring")
	}
}
