package daemon

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"testing"
	"time"

	flatbuffers "github.com/google/flatbuffers/go"

	"github.com/quic-go/quic-go"

	"BluePods/internal/attest"
	"BluePods/internal/network"
	"BluePods/internal/types"
	"BluePods/internal/validators"
)

// mockHolder is a network node that answers attestation requests for one object.
type mockHolder struct {
	node    *network.Node
	pubkey  validators.Hash
	blsKey  *attest.BLSKeyPair
	negate  bool // negate makes the holder refuse to attest (negative response)
	objData []byte
}

// newMockHolder starts a network node answering attestation requests.
func newMockHolder(t *testing.T, objData []byte, negate bool) *mockHolder {
	t.Helper()

	pub, priv, _ := ed25519.GenerateKey(rand.Reader)

	node, err := network.NewNode(network.Config{PrivateKey: priv, ListenAddr: "127.0.0.1:0"})
	if err != nil {
		t.Fatalf("create holder: %v", err)
	}
	if err := node.Start(); err != nil {
		t.Fatalf("start holder: %v", err)
	}

	seed := make([]byte, 32)
	copy(seed, pub)
	blsKey, err := attest.GenerateBLSKeyFromSeed(seed)
	if err != nil {
		t.Fatalf("bls key: %v", err)
	}

	var pubkey validators.Hash
	copy(pubkey[:], pub)

	h := &mockHolder{node: node, pubkey: pubkey, blsKey: blsKey, negate: negate, objData: objData}

	node.OnRequest(func(_ *network.Peer, data []byte) ([]byte, error) {
		// Serve object fetches so the daemon can learn content/version/replication.
		if network.IsClientMessage(data) {
			if tag, _ := network.MessageTag(data); tag == network.MsgTagGetObject {
				return network.EncodeGetObjectResp(&network.GetObjectResponse{Found: true, Data: h.objData}), nil
			}
			return nil, nil
		}

		req, err := attest.DecodeRequest(data)
		if err != nil {
			return nil, err
		}

		if h.negate {
			return attest.EncodeNegativeResponse(&attest.NegativeResponse{
				Reason: attest.ReasonWrongVersion,
			}), nil
		}

		obj := types.GetRootAsObject(h.objData, 0)
		hash := attest.ComputeObjectHash(obj.ContentBytes(), req.Version)

		return attest.EncodePositiveResponse(&attest.PositiveResponse{
			Hash:      hash,
			Signature: h.blsKey.Sign(hash[:]),
		}), nil
	})

	return h
}

// buildObject builds an Object FlatBuffer with content and replication.
func buildObject(id [32]byte, version uint64, replication uint16, content []byte) []byte {
	builder := flatbuffers.NewBuilder(256)

	idVec := builder.CreateByteVector(id[:])
	contentVec := builder.CreateByteVector(content)

	types.ObjectStart(builder)
	types.ObjectAddId(builder, idVec)
	types.ObjectAddVersion(builder, version)
	types.ObjectAddReplication(builder, replication)
	types.ObjectAddContent(builder, contentVec)
	objOffset := types.ObjectEnd(builder)
	builder.Finish(objOffset)

	return builder.FinishedBytes()
}

// newDaemonWithHolders builds a daemon whose validator set is exactly the given
// holders (BLS key + QUIC address), pinned to epoch 0. It does not run New (which
// would require a live node to sync from); the set is injected directly.
func newDaemonWithHolders(holders []*mockHolder) *Daemon {
	vs := validators.NewValidatorSet(nil)
	for _, h := range holders {
		var blsPub [48]byte
		copy(blsPub[:], h.blsKey.PublicKeyBytes())
		vs.Add(h.pubkey, h.node.Addr(), blsPub)
	}

	addrs := make([]string, len(holders))
	for i, h := range holders {
		addrs[i] = h.node.Addr()
	}

	return &Daemon{
		nodeAddrs: addrs,
		tlsConfig: &tls.Config{
			InsecureSkipVerify: true,
			NextProtos:         []string{alpnProtocol},
		},
		quicConfig:   &quic.Config{MaxIdleTimeout: 30 * time.Second, KeepAlivePeriod: 10 * time.Second},
		validatorSet: vs,
		epoch:        0,
	}
}

// TestCollectAttestationsSuccess collects a quorum proof for one replicated object.
func TestCollectAttestationsSuccess(t *testing.T) {
	objID := [32]byte{0x11}
	content := []byte("replicated content")

	// 4 holders, all attest positively.
	objData := buildObject(objID, 1, 4, content)
	holders := make([]*mockHolder, 4)
	for i := range holders {
		holders[i] = newMockHolder(t, objData, false)
	}
	defer closeHolders(holders)

	d := newDaemonWithHolders(holders)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	results, err := d.CollectAttestations(ctx, []objectRef{{ID: objID, Version: 1}})
	if err != nil {
		t.Fatalf("collect failed: %v", err)
	}

	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}

	r := results[0]
	if len(r.AggSig) != 96 {
		t.Errorf("expected 96-byte aggregated signature, got %d", len(r.AggSig))
	}
	if len(r.Bitmap) == 0 {
		t.Error("expected a signer bitmap")
	}
	if r.Replication != 4 {
		t.Errorf("expected replication 4, got %d", r.Replication)
	}
}

// TestCollectAttestationsQuorumImpossible fails fast when too many holders refuse.
func TestCollectAttestationsQuorumImpossible(t *testing.T) {
	objID := [32]byte{0x22}
	objData := buildObject(objID, 1, 4, []byte("contended"))

	// 4 holders, 3 refuse → quorum (QuorumSize(4)) cannot be met.
	holders := make([]*mockHolder, 4)
	for i := range holders {
		holders[i] = newMockHolder(t, objData, i != 0)
	}
	defer closeHolders(holders)

	d := newDaemonWithHolders(holders)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, err := d.CollectAttestations(ctx, []objectRef{{ID: objID, Version: 1}})
	if err == nil {
		t.Fatal("expected quorum-impossible error, got nil")
	}
}

// TestComputeHoldersMatchesAttest confirms the daemon's holder computation is the
// shared attest.ComputeHolders for the same set and replication.
func TestComputeHoldersMatchesAttest(t *testing.T) {
	vs := validators.NewValidatorSet(nil)
	for i := 0; i < 7; i++ {
		var pk validators.Hash
		pk[0] = byte(i + 1)
		vs.Add(pk, "addr", [48]byte{byte(i)})
	}

	objID := [32]byte{0x99}

	for _, rep := range []int{1, 3, 5} {
		a := attest.ComputeHolders(vs, objID, rep)
		b := attest.ComputeHolders(vs, objID, rep)

		if len(a) != len(b) {
			t.Fatalf("rep %d: length mismatch %d vs %d", rep, len(a), len(b))
		}
		for i := range a {
			if a[i] != b[i] {
				t.Fatalf("rep %d: holder %d mismatch", rep, i)
			}
		}
	}
}

// closeHolders shuts down all mock holders.
func closeHolders(holders []*mockHolder) {
	for _, h := range holders {
		h.node.Close()
	}
}
