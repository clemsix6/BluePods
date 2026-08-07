package network

import (
	"bytes"
	"testing"
)

// testAnchor is a fully populated anchoring block: every field distinct, so a
// round trip that drops or transposes one is caught.
func testAnchor() ProvedIndexAnchor {
	return ProvedIndexAnchor{
		Anchored:      true,
		FrontierRound: 4242,
		IndexRoot:     [32]byte{0x01, 0xFF},
		DomainRoot:    [32]byte{0x02, 0xFE},
		ParentRoot:    [32]byte{0x03, 0xFD},
		ChildrenRoot:  [32]byte{0x04, 0xFC},
		ValidatorRoot: [32]byte{0x05, 0xFB},
	}
}

func TestDomainResolveRespCarriesProof(t *testing.T) {
	in := &DomainResolveResponse{
		Anchor:   testAnchor(),
		Found:    true,
		ObjectID: [32]byte{0x11, 0x22},
		Leaf:     []byte("leaf-bytes"),
		Proof:    []byte{0xAA, 0xBB, 0xCC},
	}

	out, err := DecodeDomainResolveResp(EncodeDomainResolveResp(in))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}

	if out.Anchor != in.Anchor {
		t.Fatalf("anchor lost: %+v", out.Anchor)
	}

	if !out.Found || out.ObjectID != in.ObjectID {
		t.Fatalf("resolution lost: %+v", out)
	}

	if !bytes.Equal(out.Leaf, in.Leaf) || !bytes.Equal(out.Proof, in.Proof) {
		t.Fatalf("leaf or proof lost: %+v", out)
	}
}

func TestListChildrenRoundTrip(t *testing.T) {
	req, err := DecodeListChildren(EncodeListChildren(&ListChildrenRequest{ParentID: [32]byte{0x09}}))
	if err != nil || req.ParentID != ([32]byte{0x09}) {
		t.Fatalf("request round-trip failed: %v %x", err, req.ParentID)
	}

	in := &ListChildrenResponse{
		Anchor:      testAnchor(),
		Found:       true,
		SubtreeRoot: [32]byte{0x77},
		Proof:       []byte{0x01, 0x02},
		Children:    [][32]byte{{0xA1}, {0xB2}, {0xC3}},
	}

	out, err := DecodeListChildrenResp(EncodeListChildrenResp(in))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}

	if out.Anchor != in.Anchor || !out.Found || out.SubtreeRoot != in.SubtreeRoot {
		t.Fatalf("header lost: %+v", out)
	}

	if !bytes.Equal(out.Proof, in.Proof) || len(out.Children) != len(in.Children) {
		t.Fatalf("proof or children lost: %+v", out)
	}

	for i, c := range in.Children {
		if out.Children[i] != c {
			t.Fatalf("child %d = %x, want %x", i, out.Children[i][:4], c[:4])
		}
	}
}

func TestGetAncestorsRoundTrip(t *testing.T) {
	req, err := DecodeGetAncestors(EncodeGetAncestors(&GetAncestorsRequest{ObjectID: [32]byte{0x0B}}))
	if err != nil || req.ObjectID != ([32]byte{0x0B}) {
		t.Fatalf("request round-trip failed: %v %x", err, req.ObjectID)
	}

	in := &GetAncestorsResponse{
		Anchor: testAnchor(),
		Edges: []AncestorEdge{
			{ChildID: [32]byte{0xA1}, Leaf: []byte("edge-one"), Proof: []byte{0x01}},
			{ChildID: [32]byte{0xB2}, Leaf: nil, Proof: []byte{0x02, 0x03}},
		},
	}

	out, err := DecodeGetAncestorsResp(EncodeGetAncestorsResp(in))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}

	if out.Anchor != in.Anchor || len(out.Edges) != len(in.Edges) {
		t.Fatalf("anchor or edge count lost: %+v", out)
	}

	for i, want := range in.Edges {
		got := out.Edges[i]
		if got.ChildID != want.ChildID || !bytes.Equal(got.Leaf, want.Leaf) || !bytes.Equal(got.Proof, want.Proof) {
			t.Fatalf("edge %d round-tripped as %+v, want %+v", i, got, want)
		}
	}
}

// TestIndexQueryTagsRoute checks the new request tags are classified as client
// messages: a tag missing from clientRequestTags is a handler no client can
// ever reach.
func TestIndexQueryTagsRoute(t *testing.T) {
	for _, tag := range []byte{MsgTagListChildren, MsgTagGetAncestors} {
		if !IsClientMessage([]byte{tag}) {
			t.Fatalf("request tag 0x%02x not classified as a client message", tag)
		}
	}
}
