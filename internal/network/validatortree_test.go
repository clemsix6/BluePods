package network

import (
	"bytes"
	"testing"
)

// TestGetValidatorTreeRoundTrip pins the request encoding and the response's
// full payload: anchor block, found flag, epoch, and the leaf set in order.
func TestGetValidatorTreeRoundTrip(t *testing.T) {
	req, err := DecodeGetValidatorTree(EncodeGetValidatorTree(&GetValidatorTreeRequest{Epoch: 7}))
	if err != nil || req.Epoch != 7 {
		t.Fatalf("request round-trip failed: %v %d", err, req.Epoch)
	}

	if _, ok := clientRequestTags[MsgTagGetValidatorTree]; !ok {
		t.Fatal("get-validator-tree is not classified as a client request, so a node would never route it")
	}

	in := &GetValidatorTreeResponse{
		Anchor: testAnchor(),
		Found:  true,
		Epoch:  7,
		Leaves: [][]byte{{0xA1, 0xA2}, {0xB1}, {0xC1, 0xC2, 0xC3}},
	}

	out, err := DecodeGetValidatorTreeResp(EncodeGetValidatorTreeResp(in))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}

	if out.Anchor != in.Anchor {
		t.Fatalf("anchor lost: %+v", out.Anchor)
	}

	if !out.Found || out.Epoch != in.Epoch {
		t.Fatalf("found/epoch lost: %+v", out)
	}

	if len(out.Leaves) != len(in.Leaves) {
		t.Fatalf("leaf count: got %d, want %d", len(out.Leaves), len(in.Leaves))
	}

	for i := range in.Leaves {
		if !bytes.Equal(out.Leaves[i], in.Leaves[i]) {
			t.Fatalf("leaf %d: got %x, want %x", i, out.Leaves[i], in.Leaves[i])
		}
	}
}

// TestGetValidatorTreeRespTruncatedLeafIsAnError verifies a cut-short leaf
// stream fails to decode instead of yielding a shorter set: a set silently
// missing members would be weighed as if it were the whole epoch, inflating
// every signer's share of the quorum.
func TestGetValidatorTreeRespTruncatedLeafIsAnError(t *testing.T) {
	enc := EncodeGetValidatorTreeResp(&GetValidatorTreeResponse{
		Anchor: testAnchor(),
		Found:  true,
		Epoch:  3,
		Leaves: [][]byte{{0x01, 0x02, 0x03}, {0x04, 0x05, 0x06}},
	})

	if _, err := DecodeGetValidatorTreeResp(enc[:len(enc)-4]); err == nil {
		t.Fatal("truncated leaf stream decoded cleanly")
	}
}

// TestGetValidatorTreeRespNotFoundCarriesTheServedEpoch verifies a refusal
// still reports which epoch the node holds, which is what tells a client one
// boundary behind where to walk to.
func TestGetValidatorTreeRespNotFoundCarriesTheServedEpoch(t *testing.T) {
	out, err := DecodeGetValidatorTreeResp(EncodeGetValidatorTreeResp(&GetValidatorTreeResponse{Epoch: 9}))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}

	if out.Found || out.Epoch != 9 || len(out.Leaves) != 0 {
		t.Fatalf("refusal lost its epoch: %+v", out)
	}
}
