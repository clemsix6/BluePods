package network

import (
	"encoding/binary"
	"fmt"
)

// ListChildrenRequest requests the children of an owner key or an object ID.
type ListChildrenRequest struct {
	ParentID [32]byte // ParentID is the owner key or object ID to enumerate under
}

// EncodeListChildren encodes a children-enumeration request.
// Format: [1B tag] [32B parentID].
func EncodeListChildren(req *ListChildrenRequest) []byte {
	buf := make([]byte, 33)
	buf[0] = MsgTagListChildren
	copy(buf[1:33], req.ParentID[:])

	return buf
}

// DecodeListChildren decodes a children-enumeration request.
func DecodeListChildren(data []byte) (*ListChildrenRequest, error) {
	if len(data) < 33 || data[0] != MsgTagListChildren {
		return nil, fmt.Errorf("not a list-children message")
	}

	req := &ListChildrenRequest{}
	copy(req.ParentID[:], data[1:33])

	return req, nil
}

// ListChildrenResponse carries a proved enumeration: the parent's children
// subtree root proven against ChildrenRoot, plus the raw child-leaf stream.
// The stream is unauthenticated on purpose — the client rebuilds the subtree
// from it and checks that root against SubtreeRoot, which detects a withheld or
// invented leaf at every set size, with no threshold below which the stream is
// taken on trust. The whole set travels in one message, so a parent with more
// children than the transport's message limit can carry is not serveable
// through this verb: there is no continuation token, by design — a paginated
// stream would need a completeness argument per chunk, which is exactly what
// the single subtree-root check replaces.
type ListChildrenResponse struct {
	Anchor      ProvedIndexAnchor // Anchor is the index state Proof was taken against
	Found       bool              // Found reports whether the parent currently has any children
	SubtreeRoot [32]byte          // SubtreeRoot is the proven root of the parent's children subtree, zero when Found is false
	Proof       []byte            // Proof is the serialized top-tree inclusion or absence proof against Anchor.ChildrenRoot
	Children    [][32]byte        // Children are the raw child IDs, in no particular order
}

// EncodeListChildrenResp encodes a children-enumeration response.
// Format: [1B tag] [provedAnchorSize anchor] [1B found] [32B subtreeRoot]
// [4B proofLen] [proof] [4B childCount] then childCount 32-byte child IDs back
// to back. All integers are big-endian.
func EncodeListChildrenResp(resp *ListChildrenResponse) []byte {
	buf := make([]byte, 1+provedAnchorSize+1+32+4+len(resp.Proof)+4+len(resp.Children)*32)
	buf[0] = MsgTagListChildrenResp

	off := 1 + putProvedAnchor(buf[1:], resp.Anchor)

	if resp.Found {
		buf[off] = 1
	}
	off++

	copy(buf[off:off+32], resp.SubtreeRoot[:])
	off += 32

	off += putBlob(buf[off:], resp.Proof)

	binary.BigEndian.PutUint32(buf[off:off+4], uint32(len(resp.Children)))
	off += 4

	for _, c := range resp.Children {
		copy(buf[off:off+32], c[:])
		off += 32
	}

	return buf
}

// DecodeListChildrenResp decodes a children-enumeration response. A truncated
// child stream is an error rather than a shorter list: silently returning the
// leaves that happened to arrive would hand the completeness check a set the
// serving node never claimed.
func DecodeListChildrenResp(data []byte) (*ListChildrenResponse, error) {
	const fixed = 1 + provedAnchorSize + 1 + 32

	if len(data) < fixed || data[0] != MsgTagListChildrenResp {
		return nil, fmt.Errorf("not a list-children response")
	}

	resp := &ListChildrenResponse{Anchor: readProvedAnchor(data[1:])}

	off := 1 + provedAnchorSize
	resp.Found = data[off] == 1
	off++

	copy(resp.SubtreeRoot[:], data[off:off+32])
	off += 32

	proof, rest, ok := readBlob(data[off:])
	if !ok {
		return nil, fmt.Errorf("list-children response truncated in proof")
	}
	resp.Proof = proof

	children, err := readChildIDs(rest)
	if err != nil {
		return nil, err
	}
	resp.Children = children

	return resp, nil
}

// readChildIDs decodes the 4-byte count and that many 32-byte child IDs.
func readChildIDs(data []byte) ([][32]byte, error) {
	if len(data) < 4 {
		return nil, fmt.Errorf("list-children response truncated in child count")
	}

	count := int(binary.BigEndian.Uint32(data[:4]))
	data = data[4:]

	if len(data) < count*32 {
		return nil, fmt.Errorf("list-children response carries %d bytes of leaves, want %d", len(data), count*32)
	}

	children := make([][32]byte, count)
	for i := range children {
		copy(children[i][:], data[i*32:(i+1)*32])
	}

	return children, nil
}

// GetAncestorsRequest requests an object's proved ancestry walk.
type GetAncestorsRequest struct {
	ObjectID [32]byte // ObjectID is the object to walk upward from
}

// EncodeGetAncestors encodes an ancestry-walk request.
// Format: [1B tag] [32B objectID].
func EncodeGetAncestors(req *GetAncestorsRequest) []byte {
	buf := make([]byte, 33)
	buf[0] = MsgTagGetAncestors
	copy(buf[1:33], req.ObjectID[:])

	return buf
}

// DecodeGetAncestors decodes an ancestry-walk request.
func DecodeGetAncestors(data []byte) (*GetAncestorsRequest, error) {
	if len(data) < 33 || data[0] != MsgTagGetAncestors {
		return nil, fmt.Errorf("not a get-ancestors message")
	}

	req := &GetAncestorsRequest{}
	copy(req.ObjectID[:], data[1:33])

	return req, nil
}

// AncestorEdge is one proved hop of an ancestry walk. The parent kind and
// reference live INSIDE Leaf, which Proof authenticates, so a client reads the
// hop from bytes it has verified and can tell a genuine KeyRoot terminus from a
// withheld edge. An empty Leaf means the object has no edge at all, and Proof
// is then an absence proof.
type AncestorEdge struct {
	ChildID [32]byte // ChildID is the object this hop's edge belongs to
	Leaf    []byte   // Leaf is the raw parent-tree leaf, empty when ChildID has no edge
	Proof   []byte   // Proof is the serialized inclusion or absence proof against the response's ParentRoot
}

// GetAncestorsResponse carries the walk, ordered from the queried object
// upward, every edge proven against the same ParentRoot.
type GetAncestorsResponse struct {
	Anchor ProvedIndexAnchor // Anchor is the index state every edge's proof was taken against
	Edges  []AncestorEdge    // Edges are the walk's hops, the queried object's own edge first
}

// EncodeGetAncestorsResp encodes an ancestry-walk response.
// Format: [1B tag] [provedAnchorSize anchor] [4B edgeCount] then per edge
// [32B childID] [4B leafLen] [leaf] [4B proofLen] [proof]. All integers are
// big-endian.
func EncodeGetAncestorsResp(resp *GetAncestorsResponse) []byte {
	size := 1 + provedAnchorSize + 4
	for _, e := range resp.Edges {
		size += 32 + 4 + len(e.Leaf) + 4 + len(e.Proof)
	}

	buf := make([]byte, size)
	buf[0] = MsgTagGetAncestorsResp

	off := 1 + putProvedAnchor(buf[1:], resp.Anchor)

	binary.BigEndian.PutUint32(buf[off:off+4], uint32(len(resp.Edges)))
	off += 4

	for _, e := range resp.Edges {
		copy(buf[off:off+32], e.ChildID[:])
		off += 32

		off += putBlob(buf[off:], e.Leaf)
		off += putBlob(buf[off:], e.Proof)
	}

	return buf
}

// DecodeGetAncestorsResp decodes an ancestry-walk response. A truncated edge
// list is an error rather than a shorter walk: a walk silently cut short would
// look exactly like one that legitimately terminated.
func DecodeGetAncestorsResp(data []byte) (*GetAncestorsResponse, error) {
	const fixed = 1 + provedAnchorSize + 4

	if len(data) < fixed || data[0] != MsgTagGetAncestorsResp {
		return nil, fmt.Errorf("not a get-ancestors response")
	}

	resp := &GetAncestorsResponse{Anchor: readProvedAnchor(data[1:])}

	off := 1 + provedAnchorSize
	count := int(binary.BigEndian.Uint32(data[off : off+4]))
	rest := data[off+4:]

	for i := 0; i < count; i++ {
		edge, remaining, err := readAncestorEdge(rest)
		if err != nil {
			return nil, fmt.Errorf("get-ancestors response edge %d:\n%w", i, err)
		}

		resp.Edges = append(resp.Edges, edge)
		rest = remaining
	}

	return resp, nil
}

// readAncestorEdge decodes one edge record and returns the remaining bytes.
func readAncestorEdge(data []byte) (AncestorEdge, []byte, error) {
	if len(data) < 32 {
		return AncestorEdge{}, nil, fmt.Errorf("truncated child ID")
	}

	var edge AncestorEdge
	copy(edge.ChildID[:], data[:32])

	leaf, rest, ok := readBlob(data[32:])
	if !ok {
		return AncestorEdge{}, nil, fmt.Errorf("truncated leaf")
	}

	proof, rest, ok := readBlob(rest)
	if !ok {
		return AncestorEdge{}, nil, fmt.Errorf("truncated proof")
	}

	edge.Leaf, edge.Proof = leaf, proof

	return edge, rest, nil
}
