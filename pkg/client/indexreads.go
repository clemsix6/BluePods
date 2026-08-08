package client

import (
	"fmt"
	"time"

	"BluePods/internal/index"
	"BluePods/internal/network"
)

// The UNPROVEN index reads: thin Client wrappers around the QUIC transport,
// returning only the node's own word. ListChildren and Parent wrap the
// transport's proved verbs (pkg/client/provedtransport.go), discarding the
// proof and the anchoring block those carry; DomainResolve wraps the
// transport's own already-unproven verb (QUICTransport.DomainResolve,
// pkg/client/quic.go), which has no proof to discard in the first place. They
// exist for callers with no trusted checkpoint to verify against — bpctl and
// the interactive console are the two today, since neither acquires one (no
// --checkpoint flag, no persisted trust-on-first-use pin). A caller that
// holds a Checkpoint should read through a LightClient instead
// (pkg/client/lightclient.go), which checks every answer against a
// quorum-attested root; nothing here checks a proof against one — Parent
// still rejects an edge or leaf that names a different object (see its own
// doc comment), but that is self-consistency, not cryptographic
// verification.

// txPollInterval is how often WaitForTx re-polls a transaction's status.
const txPollInterval = 100 * time.Millisecond

// DomainResolve resolves a name over QUIC and returns the node's unproven
// word: the resolved object ID and whether the name currently resolves, with
// no inclusion or absence proof checked. See LightClient.ResolveDomain for
// the verified counterpart.
func (c *Client) DomainResolve(name string) ([32]byte, bool, error) {
	return c.transport.DomainResolve(name)
}

// ListChildren enumerates a parent's children over QUIC — an owner key or an
// object ID — and returns the node's unproven word: the raw child ID list,
// with no completeness guarantee. A parent whose child set exceeds the
// transport's single-frame ceiling (spec §10; 6.1's as-built) surfaces as a
// plain error, not a truncated list. See LightClient.ListChildren for the
// verified counterpart.
func (c *Client) ListChildren(parent [32]byte) ([][32]byte, error) {
	resp, err := c.transport.ListChildren(parent)
	if err != nil {
		return nil, fmt.Errorf("list children:\n%w", err)
	}

	if resp == nil || !resp.Found {
		return nil, nil
	}

	return resp.Children, nil
}

// Parent returns an object's immediate parent edge over QUIC: kind reports
// whether the object is rooted directly under an owner key (index.KeyRootKind)
// or nested under another object (index.ObjectParentKind), and hasParent is
// false only when the object carries no edge at all. This is the unproven,
// single-hop counterpart of LightClient.Ancestors, which walks and verifies
// the whole chain up to a KeyRoot.
//
// No proof is checked here — there is none to check — but the two identity
// checks that don't need one still run, mirroring verifyEdge
// (provedanswers.go): the served edge must actually be objectID's, and the
// leaf it carries must name the same child, so a node cannot answer with a
// neighbor's edge and have it printed as objectID's parent.
func (c *Client) Parent(objectID [32]byte) (kind byte, parent [32]byte, hasParent bool, err error) {
	resp, err := c.transport.GetAncestors(objectID)
	if err != nil {
		return 0, [32]byte{}, false, fmt.Errorf("get ancestors:\n%w", err)
	}

	if resp == nil || len(resp.Edges) == 0 || len(resp.Edges[0].Leaf) == 0 {
		return 0, [32]byte{}, false, nil
	}

	edge := resp.Edges[0]
	if edge.ChildID != objectID {
		return 0, [32]byte{}, false, fmt.Errorf("node answered with the edge for %x, not the requested %x", edge.ChildID[:8], objectID[:8])
	}

	leaf, ok := index.DecodeParentLeaf(edge.Leaf)
	if !ok {
		return 0, [32]byte{}, false, fmt.Errorf("parent leaf does not decode")
	}

	if leaf.ChildID != edge.ChildID {
		return 0, [32]byte{}, false, fmt.Errorf("leaf belongs to %x, not the requested %x", leaf.ChildID[:8], objectID[:8])
	}

	return leaf.ParentKind, leaf.Parent, true, nil
}

// WaitForTx blocks until hash leaves the pending state — finalized or failed
// at commit — or timeout elapses. It is the synchronous counterpart of the
// interactive console's polling loop (cmd/cli/tui/model.go's fetchTrack):
// same transport call, same terminal states, blocking instead of driving a UI
// tick. Non-interactive callers that need to know a transaction landed before
// acting on its effect (the domain-registration saga in domain.go) use this
// instead of racing the console's tick.
func (c *Client) WaitForTx(hash [32]byte, timeout time.Duration) (*network.GetTxStatusResponse, error) {
	deadline := time.Now().Add(timeout)

	for {
		resp, err := c.GetTxStatus(hash)
		if err != nil {
			return nil, fmt.Errorf("poll tx %x:\n%w", hash[:8], err)
		}

		if resp.State == network.TxStateFinalized || resp.State == network.TxStateFailed {
			return resp, nil
		}

		if time.Now().After(deadline) {
			return nil, fmt.Errorf("tx %x still in state %d after %s", hash[:8], resp.State, timeout)
		}

		time.Sleep(txPollInterval)
	}
}
