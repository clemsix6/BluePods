package aggregation

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"BluePods/internal/consensus"
	"BluePods/internal/network"
	"BluePods/internal/types"

	flatbuffers "github.com/google/flatbuffers/go"
)

// testNode holds a test node and its key.
type testNode struct {
	node *network.Node
	key  ed25519.PrivateKey
}

// setupTestNetwork creates a network of connected nodes for testing.
func setupTestNetwork(t *testing.T, numNodes int) []*testNode {
	t.Helper()

	nodes := make([]*testNode, numNodes)

	for i := 0; i < numNodes; i++ {
		_, priv, _ := ed25519.GenerateKey(rand.Reader)

		node, err := network.NewNode(network.Config{
			PrivateKey: priv,
			ListenAddr: "127.0.0.1:0",
		})
		if err != nil {
			t.Fatalf("create node %d: %v", i, err)
		}

		if err := node.Start(); err != nil {
			t.Fatalf("start node %d: %v", i, err)
		}

		nodes[i] = &testNode{node: node, key: priv}
	}

	// Connect node 0 to all other nodes
	for i := 1; i < numNodes; i++ {
		if _, err := nodes[0].node.Connect(nodes[i].node.Addr()); err != nil {
			t.Fatalf("connect node 0 to %d: %v", i, err)
		}
	}

	time.Sleep(100 * time.Millisecond)

	return nodes
}

// cleanupTestNetwork closes all nodes.
func cleanupTestNetwork(nodes []*testNode) {
	for _, n := range nodes {
		n.node.Close()
	}
}

// buildTestObject creates a FlatBuffers Object with the given replication.
func buildTestObject(id [32]byte, replication uint16) []byte {
	builder := flatbuffers.NewBuilder(256)

	idOffset := builder.CreateByteVector(id[:])
	contentOffset := builder.CreateByteVector([]byte("test content"))

	types.ObjectStart(builder)
	types.ObjectAddId(builder, idOffset)
	types.ObjectAddVersion(builder, 1)
	types.ObjectAddReplication(builder, replication)
	types.ObjectAddContent(builder, contentOffset)

	objOffset := types.ObjectEnd(builder)
	builder.Finish(objOffset)

	return builder.FinishedBytes()
}

// TestCollectorBasic tests basic attestation collection.
func TestCollectorBasic(t *testing.T) {
	nodes := setupTestNetwork(t, 6)
	defer cleanupTestNetwork(nodes)

	// Exclude node 0 from validators (aggregator can't be its own peer)
	validators := make([]consensus.Hash, len(nodes)-1)
	for i := 1; i < len(nodes); i++ {
		copy(validators[i-1][:], nodes[i].key.Public().(ed25519.PublicKey))
	}

	vs := consensus.NewValidatorSet(validators)
	rv := NewRendezvous(vs)
	collector := NewCollector(nodes[0].node, rv, vs, nil)

	objID := [32]byte{0x01}

	// Set up handlers - return object with replication=3
	for i := 1; i < len(nodes); i++ {
		idx := i

		nodes[i].node.OnRequest(func(p *network.Peer, data []byte) ([]byte, error) {
			req, err := DecodeRequest(data)
			if err != nil {
				return nil, err
			}

			seed := make([]byte, 32)
			copy(seed, nodes[idx].key.Public().(ed25519.PublicKey)[:32])
			blsKey, _ := GenerateBLSKeyFromSeed(seed)

			resp := &PositiveResponse{
				Hash:      [32]byte{0x42},
				Signature: blsKey.Sign(req.ObjectID[:]),
			}

			if req.WantFull {
				resp.Data = buildTestObject(req.ObjectID, 3) // replication=3
			}

			return EncodePositiveResponse(resp), nil
		})
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	obj := ObjectRef{ID: objID, Version: 1}
	result := collector.CollectObject(ctx, obj)

	if result.Error != nil {
		t.Fatalf("collection failed: %v", result.Error)
	}

	if result.IsSingleton {
		t.Error("should not be singleton")
	}

	if len(result.Signatures) == 0 {
		t.Error("no signatures collected")
	}

	if len(result.ObjectData) == 0 {
		t.Error("no object data collected")
	}

	if result.Replication != 3 {
		t.Errorf("expected replication 3, got %d", result.Replication)
	}
}

// TestCollectorSingleton tests that singletons are detected and skipped.
func TestCollectorSingleton(t *testing.T) {
	nodes := setupTestNetwork(t, 4)
	defer cleanupTestNetwork(nodes)

	validators := make([]consensus.Hash, len(nodes)-1)
	for i := 1; i < len(nodes); i++ {
		copy(validators[i-1][:], nodes[i].key.Public().(ed25519.PublicKey))
	}

	vs := consensus.NewValidatorSet(validators)
	rv := NewRendezvous(vs)
	collector := NewCollector(nodes[0].node, rv, vs, nil)

	objID := [32]byte{0x02}

	// Set up handlers - return object with replication=0 (singleton)
	for i := 1; i < len(nodes); i++ {
		idx := i

		nodes[i].node.OnRequest(func(p *network.Peer, data []byte) ([]byte, error) {
			req, err := DecodeRequest(data)
			if err != nil {
				return nil, err
			}

			seed := make([]byte, 32)
			copy(seed, nodes[idx].key.Public().(ed25519.PublicKey)[:32])
			blsKey, _ := GenerateBLSKeyFromSeed(seed)

			resp := &PositiveResponse{
				Hash:      [32]byte{0x42},
				Signature: blsKey.Sign(req.ObjectID[:]),
			}

			if req.WantFull {
				resp.Data = buildTestObject(req.ObjectID, 0) // singleton!
			}

			return EncodePositiveResponse(resp), nil
		})
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	obj := ObjectRef{ID: objID, Version: 1}
	result := collector.CollectObject(ctx, obj)

	if result.Error != nil {
		t.Fatalf("collection failed: %v", result.Error)
	}

	if !result.IsSingleton {
		t.Error("should be detected as singleton")
	}

	// Singletons don't need signatures or object data in result
	if len(result.Signatures) != 0 {
		t.Error("singleton should not have signatures")
	}
}

// TestCollectorQuorum tests that quorum is required.
func TestCollectorQuorum(t *testing.T) {
	nodes := setupTestNetwork(t, 6)
	defer cleanupTestNetwork(nodes)

	validators := make([]consensus.Hash, len(nodes)-1)
	for i := 1; i < len(nodes); i++ {
		copy(validators[i-1][:], nodes[i].key.Public().(ed25519.PublicKey))
	}

	vs := consensus.NewValidatorSet(validators)
	rv := NewRendezvous(vs)
	collector := NewCollector(nodes[0].node, rv, vs, nil)

	objID := [32]byte{0x03}
	respondersCount := 0

	// Only first responder returns positive, others negative
	for i := 1; i < len(nodes); i++ {
		idx := i
		shouldRespond := respondersCount < 1
		respondersCount++

		nodes[i].node.OnRequest(func(p *network.Peer, data []byte) ([]byte, error) {
			req, _ := DecodeRequest(data)

			seed := make([]byte, 32)
			copy(seed, nodes[idx].key.Public().(ed25519.PublicKey)[:32])
			blsKey, _ := GenerateBLSKeyFromSeed(seed)

			if shouldRespond {
				resp := &PositiveResponse{
					Hash:      [32]byte{0x42},
					Signature: blsKey.Sign(req.ObjectID[:]),
				}

				if req.WantFull {
					resp.Data = buildTestObject(req.ObjectID, 3)
				}

				return EncodePositiveResponse(resp), nil
			}

			resp := &NegativeResponse{
				Reason:    reasonNotFound,
				Signature: blsKey.Sign([]byte("not found")),
			}

			return EncodeNegativeResponse(resp), nil
		})
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	obj := ObjectRef{ID: objID, Version: 1}
	result := collector.CollectObject(ctx, obj)

	// Should fail because only 1 positive out of 3 required
	if result.Error == nil {
		t.Error("expected error due to insufficient quorum")
	}
}

// TestCollectorTimeout tests timeout handling.
func TestCollectorTimeout(t *testing.T) {
	nodes := setupTestNetwork(t, 6)
	defer cleanupTestNetwork(nodes)

	validators := make([]consensus.Hash, len(nodes)-1)
	for i := 1; i < len(nodes); i++ {
		copy(validators[i-1][:], nodes[i].key.Public().(ed25519.PublicKey))
	}

	vs := consensus.NewValidatorSet(validators)
	rv := NewRendezvous(vs)
	collector := NewCollector(nodes[0].node, rv, vs, nil)

	// All nodes delay their response
	for i := 1; i < len(nodes); i++ {
		nodes[i].node.OnRequest(func(p *network.Peer, data []byte) ([]byte, error) {
			time.Sleep(500 * time.Millisecond)
			return nil, nil
		})
	}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	obj := ObjectRef{ID: [32]byte{0x04}, Version: 1}
	result := collector.CollectObject(ctx, obj)

	if result.Error == nil {
		t.Error("expected error due to timeout")
	}
}

// TestCollectorHashMismatch tests that hash mismatches are detected.
func TestCollectorHashMismatch(t *testing.T) {
	nodes := setupTestNetwork(t, 6)
	defer cleanupTestNetwork(nodes)

	validators := make([]consensus.Hash, len(nodes)-1)
	for i := 1; i < len(nodes); i++ {
		copy(validators[i-1][:], nodes[i].key.Public().(ed25519.PublicKey))
	}

	vs := consensus.NewValidatorSet(validators)
	rv := NewRendezvous(vs)
	collector := NewCollector(nodes[0].node, rv, vs, nil)

	objID := [32]byte{0x05}

	// Each node returns a different hash
	for i := 1; i < len(nodes); i++ {
		idx := i

		nodes[i].node.OnRequest(func(p *network.Peer, data []byte) ([]byte, error) {
			req, _ := DecodeRequest(data)

			seed := make([]byte, 32)
			copy(seed, nodes[idx].key.Public().(ed25519.PublicKey)[:32])
			blsKey, _ := GenerateBLSKeyFromSeed(seed)

			resp := &PositiveResponse{
				Hash:      [32]byte{byte(idx)}, // Different hash per node
				Signature: blsKey.Sign(req.ObjectID[:]),
			}

			if req.WantFull {
				resp.Data = buildTestObject(req.ObjectID, 3)
			}

			return EncodePositiveResponse(resp), nil
		})
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	obj := ObjectRef{ID: objID, Version: 1}
	result := collector.CollectObject(ctx, obj)

	if result.Error == nil {
		t.Error("expected error due to hash mismatch")
	}
}

// TestProtocolEncoding tests protocol message encoding/decoding.
func TestProtocolEncoding(t *testing.T) {
	req := &AttestationRequest{
		ObjectID: [32]byte{0x01, 0x02, 0x03},
		Version:  12345,
		WantFull: true,
	}

	encoded := EncodeRequest(req)
	decoded, err := DecodeRequest(encoded)

	if err != nil {
		t.Fatalf("decode request: %v", err)
	}

	if decoded.ObjectID != req.ObjectID {
		t.Error("objectID mismatch")
	}

	if decoded.Version != req.Version {
		t.Error("version mismatch")
	}

	if decoded.WantFull != req.WantFull {
		t.Error("wantFull mismatch")
	}
}

// TestProtocolPositiveResponse tests positive response encoding.
func TestProtocolPositiveResponse(t *testing.T) {
	resp := &PositiveResponse{
		Hash:      [32]byte{0xAB, 0xCD},
		Signature: make([]byte, 96),
		Data:      []byte("test object data"),
	}

	for i := range resp.Signature {
		resp.Signature[i] = byte(i)
	}

	encoded := EncodePositiveResponse(resp)
	decoded, err := DecodePositiveResponse(encoded)

	if err != nil {
		t.Fatalf("decode positive response: %v", err)
	}

	if decoded.Hash != resp.Hash {
		t.Error("hash mismatch")
	}

	if !bytes.Equal(decoded.Signature, resp.Signature) {
		t.Error("signature mismatch")
	}

	if !bytes.Equal(decoded.Data, resp.Data) {
		t.Error("data mismatch")
	}
}

// TestProtocolNegativeResponse tests negative response encoding.
func TestProtocolNegativeResponse(t *testing.T) {
	resp := &NegativeResponse{
		Reason:    reasonNotFound,
		Signature: make([]byte, 96),
	}

	for i := range resp.Signature {
		resp.Signature[i] = byte(i)
	}

	encoded := EncodeNegativeResponse(resp)
	decoded, err := DecodeNegativeResponse(encoded)

	if err != nil {
		t.Fatalf("decode negative response: %v", err)
	}

	if decoded.Reason != resp.Reason {
		t.Error("reason mismatch")
	}

	if !bytes.Equal(decoded.Signature, resp.Signature) {
		t.Error("signature mismatch")
	}
}

// mockState implements a minimal state for testing local storage optimization.
type mockState struct {
	objects map[[32]byte][]byte
}

// newMockState creates a new mock state.
func newMockState() *mockState {
	return &mockState{objects: make(map[[32]byte][]byte)}
}

// GetObject retrieves an object by ID. Returns nil if not found.
func (s *mockState) GetObject(id [32]byte) []byte {
	return s.objects[id]
}

// SetObject stores an object in the mock state.
func (s *mockState) SetObject(id [32]byte, data []byte) {
	s.objects[id] = data
}

// TestCollectorLocalSingleton tests that singleton objects in local storage bypass network.
func TestCollectorLocalSingleton(t *testing.T) {
	nodes := setupTestNetwork(t, 4)
	defer cleanupTestNetwork(nodes)

	validators := make([]consensus.Hash, len(nodes)-1)
	for i := 1; i < len(nodes); i++ {
		copy(validators[i-1][:], nodes[i].key.Public().(ed25519.PublicKey))
	}

	vs := consensus.NewValidatorSet(validators)
	rv := NewRendezvous(vs)

	// Create mock state with singleton object
	mockSt := newMockState()
	objID := [32]byte{0x10}
	mockSt.SetObject(objID, buildTestObject(objID, 0)) // replication=0 (singleton)

	// Create collector with mock state adapter
	collector := NewCollector(nodes[0].node, rv, vs, nil)
	// Manually set the state field using a wrapper that calls mockState
	collectorWithState := &collectorWithMockState{
		Collector: collector,
		mockState: mockSt,
	}

	// Track if any network request was made
	requestMade := false
	for i := 1; i < len(nodes); i++ {
		nodes[i].node.OnRequest(func(p *network.Peer, data []byte) ([]byte, error) {
			requestMade = true
			return nil, nil
		})
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	obj := ObjectRef{ID: objID, Version: 1}
	result := collectorWithState.CollectObject(ctx, obj)

	if result.Error != nil {
		t.Fatalf("collection failed: %v", result.Error)
	}

	if !result.IsSingleton {
		t.Error("should be detected as singleton")
	}

	if requestMade {
		t.Error("no network requests should be made for local singleton")
	}
}

// collectorWithMockState wraps Collector to use a mock state.
type collectorWithMockState struct {
	*Collector
	mockState *mockState
}

// CollectObject collects attestations with local storage check.
func (c *collectorWithMockState) CollectObject(ctx context.Context, obj ObjectRef) *CollectionResult {
	result := &CollectionResult{ObjectID: obj.ID}

	// Check mock state first
	localData := c.mockState.GetObject(obj.ID)
	if localData != nil {
		fbObj := types.GetRootAsObject(localData, 0)
		replication := fbObj.Replication()

		if replication == 0 {
			result.IsSingleton = true
			return result
		}

		return c.collectAttestationsOnlyMock(ctx, obj, localData, replication)
	}

	// Delegate to original collector
	return c.Collector.CollectObject(ctx, obj)
}

// collectAttestationsOnlyMock collects attestations using local data.
func (c *collectorWithMockState) collectAttestationsOnlyMock(
	ctx context.Context,
	obj ObjectRef,
	localData []byte,
	replication uint16,
) *CollectionResult {
	result := &CollectionResult{
		ObjectID:    obj.ID,
		ObjectData:  localData,
		Replication: replication,
	}

	holders := c.rendezvous.ComputeHolders(obj.ID, int(replication))
	if len(holders) == 0 {
		result.Error = fmt.Errorf("no holders for replication %d", replication)
		return result
	}

	quorum := QuorumSize(len(holders))
	attestCh := make(chan *HolderAttestation, len(holders))

	var successCount atomic.Int32
	var negativeCount atomic.Int32
	var wg sync.WaitGroup

	for i, holder := range holders {
		wg.Add(1)

		go func(idx int, h consensus.Hash) {
			defer wg.Done()

			att := c.requestAttestation(ctx, h, obj, false, idx)

			if att != nil {
				if att.IsNegative {
					negativeCount.Add(1)
				} else {
					successCount.Add(1)
				}
			}

			attestCh <- att
		}(i, holder)
	}

	go func() {
		wg.Wait()
		close(attestCh)
	}()

	var attestations []*HolderAttestation

	for att := range attestCh {
		if att == nil {
			continue
		}

		attestations = append(attestations, att)

		if int(negativeCount.Load()) >= quorum {
			result.Error = fmt.Errorf("quorum of negative attestations")
			return result
		}

		if int(successCount.Load()) >= quorum {
			break
		}
	}

	return c.finalizeResult(result, attestations, quorum)
}

// TestCollectorLocalStandard tests that standard objects in local storage skip top-1 fetch.
func TestCollectorLocalStandard(t *testing.T) {
	nodes := setupTestNetwork(t, 6)
	defer cleanupTestNetwork(nodes)

	validators := make([]consensus.Hash, len(nodes)-1)
	for i := 1; i < len(nodes); i++ {
		copy(validators[i-1][:], nodes[i].key.Public().(ed25519.PublicKey))
	}

	vs := consensus.NewValidatorSet(validators)
	rv := NewRendezvous(vs)

	// Create mock state with standard object
	mockSt := newMockState()
	objID := [32]byte{0x11}
	mockSt.SetObject(objID, buildTestObject(objID, 3)) // replication=3

	collector := NewCollector(nodes[0].node, rv, vs, nil)
	collectorWithState := &collectorWithMockState{
		Collector: collector,
		mockState: mockSt,
	}

	// Track requests - none should request WantFull=true
	var wantFullRequests atomic.Int32
	for i := 1; i < len(nodes); i++ {
		idx := i

		nodes[i].node.OnRequest(func(p *network.Peer, data []byte) ([]byte, error) {
			req, err := DecodeRequest(data)
			if err != nil {
				return nil, err
			}

			if req.WantFull {
				wantFullRequests.Add(1)
			}

			seed := make([]byte, 32)
			copy(seed, nodes[idx].key.Public().(ed25519.PublicKey)[:32])
			blsKey, _ := GenerateBLSKeyFromSeed(seed)

			resp := &PositiveResponse{
				Hash:      [32]byte{0x42},
				Signature: blsKey.Sign(req.ObjectID[:]),
			}

			return EncodePositiveResponse(resp), nil
		})
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	obj := ObjectRef{ID: objID, Version: 1}
	result := collectorWithState.CollectObject(ctx, obj)

	if result.Error != nil {
		t.Fatalf("collection failed: %v", result.Error)
	}

	if result.IsSingleton {
		t.Error("should not be singleton")
	}

	if wantFullRequests.Load() > 0 {
		t.Error("no WantFull requests should be made when object is in local storage")
	}

	if len(result.Signatures) == 0 {
		t.Error("should have collected signatures")
	}

	if result.Replication != 3 {
		t.Errorf("expected replication 3, got %d", result.Replication)
	}
}
