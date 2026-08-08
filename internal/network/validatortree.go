package network

import (
	"encoding/binary"
	"fmt"
)

// GetValidatorTreeRequest asks for the leaf set of an epoch's validator tree.
type GetValidatorTreeRequest struct {
	Epoch uint64 // Epoch is the epoch whose validator set is wanted
}

// EncodeGetValidatorTree encodes a validator-tree request.
// Format: [1B tag] [8B epoch], big-endian.
func EncodeGetValidatorTree(req *GetValidatorTreeRequest) []byte {
	buf := make([]byte, 9)
	buf[0] = MsgTagGetValidatorTree
	binary.BigEndian.PutUint64(buf[1:9], req.Epoch)

	return buf
}

// DecodeGetValidatorTree decodes a validator-tree request.
func DecodeGetValidatorTree(data []byte) (*GetValidatorTreeRequest, error) {
	if len(data) < 9 || data[0] != MsgTagGetValidatorTree {
		return nil, fmt.Errorf("not a get-validator-tree message")
	}

	return &GetValidatorTreeRequest{Epoch: binary.BigEndian.Uint64(data[1:9])}, nil
}

// GetValidatorTreeResponse carries an epoch's validator leaf set, the light
// client's side of the spec §5 epoch walk: it authenticates the set that
// weighs the next quorum without trusting the node that served it.
//
// The WHOLE set travels, not a per-leaf inclusion proof, because what a
// quorum check needs is the capped-stake TOTAL, and no inclusion proof
// authenticates a total: it shows a member is in the tree and says nothing
// about how many others are. The client rebuilds the tree from these leaves
// (index.ValidatorRootOf), checks that root against Anchor.ValidatorRoot, and
// checks that Anchor's four component roots combine to an index root a quorum
// attested — the same one-mechanism-at-every-size check ListChildren uses for
// completeness.
//
// Found is false when Epoch is not the epoch the served tree currently
// describes: the index keeps no versioned validator trees, so only the current
// epoch's set is provable against a live anchor. Epoch always reports the
// epoch this node does hold, so a client one boundary behind learns where to
// walk to, and a client further behind than the handoff window learns it needs
// a fresh checkpoint (spec §5 weak subjectivity).
type GetValidatorTreeResponse struct {
	Anchor ProvedIndexAnchor // Anchor is the index state Leaves must rebuild the validator component of
	Found  bool              // Found reports whether the requested epoch is the one served
	Epoch  uint64            // Epoch is the epoch the served tree describes
	Leaves [][]byte          // Leaves are the raw validator leaf values, exactly as the tree hashed them; index.DecodeValidatorLeaf reads one
}

// EncodeGetValidatorTreeResp encodes a validator-tree response.
// Format: [1B tag] [provedAnchorSize anchor] [1B found] [8B epoch]
// [4B leafCount] then per leaf [4B len] [leaf bytes]. All integers are
// big-endian. Leaves are length-prefixed rather than fixed-width so this
// encoding carries no copy of the index package's leaf size.
func EncodeGetValidatorTreeResp(resp *GetValidatorTreeResponse) []byte {
	size := 1 + provedAnchorSize + 1 + 8 + 4
	for _, l := range resp.Leaves {
		size += 4 + len(l)
	}

	buf := make([]byte, size)
	buf[0] = MsgTagGetValidatorTreeResp

	off := 1 + putProvedAnchor(buf[1:], resp.Anchor)

	if resp.Found {
		buf[off] = 1
	}
	off++

	binary.BigEndian.PutUint64(buf[off:off+8], resp.Epoch)
	off += 8

	binary.BigEndian.PutUint32(buf[off:off+4], uint32(len(resp.Leaves)))
	off += 4

	for _, l := range resp.Leaves {
		off += putBlob(buf[off:], l)
	}

	return buf
}

// DecodeGetValidatorTreeResp decodes a validator-tree response. A truncated
// leaf list is an error rather than a shorter set: a silently shortened set
// would be weighed as if it were the whole epoch, which is exactly the
// inflated quorum the completeness check exists to catch.
func DecodeGetValidatorTreeResp(data []byte) (*GetValidatorTreeResponse, error) {
	const fixed = 1 + provedAnchorSize + 1 + 8 + 4

	if len(data) < fixed || data[0] != MsgTagGetValidatorTreeResp {
		return nil, fmt.Errorf("not a get-validator-tree response")
	}

	resp := &GetValidatorTreeResponse{Anchor: readProvedAnchor(data[1:])}

	off := 1 + provedAnchorSize
	resp.Found = data[off] == 1
	off++

	resp.Epoch = binary.BigEndian.Uint64(data[off : off+8])
	off += 8

	count := int(binary.BigEndian.Uint32(data[off : off+4]))
	rest := data[off+4:]

	for i := 0; i < count; i++ {
		leaf, remaining, ok := readBlob(rest)
		if !ok {
			return nil, fmt.Errorf("get-validator-tree response truncated in leaf %d", i)
		}

		resp.Leaves = append(resp.Leaves, leaf)
		rest = remaining
	}

	return resp, nil
}
