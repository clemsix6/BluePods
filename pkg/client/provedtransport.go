package client

import (
	"fmt"

	"BluePods/internal/network"
)

// The proved read verbs. Each one performs the wire round-trip and the decode
// only: every answer carries the anchoring block and the Merkle proof a client
// checks for itself, and nothing here checks anything — verification is the
// caller's, through VerifiedAnchor's methods, which is the whole point of a
// light client that trusts no node.

// GetValidatorTree fetches an epoch's validator leaf set over QUIC. The
// response is Found only for the epoch the serving node's index tree currently
// describes; otherwise it still reports the epoch that node does hold, which
// tells a client one boundary behind where to walk to.
func (t *QUICTransport) GetValidatorTree(epoch uint64) (*network.GetValidatorTreeResponse, error) {
	resp, err := t.roundTrip(network.EncodeGetValidatorTree(&network.GetValidatorTreeRequest{Epoch: epoch}))
	if err != nil {
		return nil, fmt.Errorf("get validator tree:\n%w", err)
	}

	return network.DecodeGetValidatorTreeResp(resp)
}

// ResolveDomainProved resolves a name over QUIC and returns the whole proved
// answer: the raw domain leaf, its inclusion or absence proof, and the
// anchoring block they were taken against. It is the verifiable counterpart of
// DomainResolve, which returns the node's unproven word.
func (t *QUICTransport) ResolveDomainProved(name string) (*network.DomainResolveResponse, error) {
	resp, err := t.roundTrip(network.EncodeDomainResolve(&network.DomainResolveRequest{Name: name}))
	if err != nil {
		return nil, fmt.Errorf("resolve domain:\n%w", err)
	}

	return network.DecodeDomainResolveResp(resp)
}

// ListChildren enumerates a parent's children over QUIC — an owner key or an
// object ID — returning the top-tree proof of the parent's subtree root
// together with the raw child-leaf stream the client rebuilds that root from.
func (t *QUICTransport) ListChildren(parent [32]byte) (*network.ListChildrenResponse, error) {
	resp, err := t.roundTrip(network.EncodeListChildren(&network.ListChildrenRequest{ParentID: parent}))
	if err != nil {
		return nil, fmt.Errorf("list children:\n%w", err)
	}

	return network.DecodeListChildrenResp(resp)
}

// GetAncestors walks an object's parent edges upward over QUIC, one proof per
// hop, and returns them in walk order.
func (t *QUICTransport) GetAncestors(object [32]byte) (*network.GetAncestorsResponse, error) {
	resp, err := t.roundTrip(network.EncodeGetAncestors(&network.GetAncestorsRequest{ObjectID: object}))
	if err != nil {
		return nil, fmt.Errorf("get ancestors:\n%w", err)
	}

	return network.DecodeGetAncestorsResp(resp)
}
