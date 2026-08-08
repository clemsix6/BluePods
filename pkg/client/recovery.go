package client

import "fmt"

// recoveryDepthLimit bounds how many ObjectParent hops RecoverObjects follows
// below the wallet's own key, mirroring internal/consensus's walkDepthLimit —
// the same bound the protocol enforces on these edges at commit time (a
// reparent that would close a cycle or exceed it is rejected), so a genuine
// object tree can never be deeper than this walk already covers. A source
// that never terminates (a bug, or a hostile node with no proof to check) is
// stopped here rather than recursed into forever.
const recoveryDepthLimit = 256

// childrenSource is what wallet recovery needs from its data source: the
// children of an owner key or object ID. *LightClient (pkg/client/
// lightclient.go) satisfies it with a verified enumeration; *Client
// (pkg/client/indexreads.go) satisfies it with the node's unproven word —
// RecoverObjects takes whichever the caller wires in and checks nothing about
// which guarantee it got, since that choice belongs entirely to the caller
// (see indexreads.go's doc comment for why bpctl and the console pass the
// unproven Client today).
type childrenSource interface {
	ListChildren(parent [32]byte) ([][32]byte, error)
}

// RecoverObjects repopulates the wallet's tracked object set from the index:
// it walks src.ListChildren from the wallet's own key and recurses into every
// discovered object's own children — an object can itself be an
// ObjectParent, per spec §10's "ListChildren(pubkey) at the top level plus
// recursion into object-parented subtrees" — bounded to recoveryDepthLimit
// levels. Discovered IDs are MERGED into whatever the wallet already tracks,
// never overwriting it: an object created locally but not yet visible in the
// index (its creating transaction has not committed, or the read landed
// between a mutation and the commit that anchors it) must not be dropped just
// because this walk did not reach it. Returns every ID the walk discovered,
// whether or not it was already tracked.
func (w *Wallet) RecoverObjects(src childrenSource) ([][32]byte, error) {
	discovered, err := walkChildren(src, w.Pubkey(), recoveryDepthLimit)
	if err != nil {
		return nil, fmt.Errorf("recover objects from index:\n%w", err)
	}

	for _, id := range discovered {
		w.TrackObject(id)
	}

	return discovered, nil
}

// walkChildren enumerates root's children and recurses into each one's own
// children, consuming one level of depth per hop. A ListChildren failure —
// including the transport's single-frame ceiling being exceeded (spec §10;
// 6.1's as-built) — surfaces as an error here rather than a silently
// truncated result: a partial recovery must never be mistaken for a complete
// one.
func walkChildren(src childrenSource, root [32]byte, depth int) ([][32]byte, error) {
	if depth <= 0 {
		return nil, fmt.Errorf("recursion below %x exceeded %d levels", root[:8], recoveryDepthLimit)
	}

	children, err := src.ListChildren(root)
	if err != nil {
		return nil, fmt.Errorf("children of %x:\n%w", root[:8], err)
	}

	all := make([][32]byte, 0, len(children))

	for _, id := range children {
		all = append(all, id)

		nested, err := walkChildren(src, id, depth-1)
		if err != nil {
			return nil, err
		}

		all = append(all, nested...)
	}

	return all, nil
}

// EnumerateSubtree walks src.ListChildren from root and recurses into every
// discovered child's own children, bounded the same way RecoverObjects is —
// but without touching any wallet's tracked set, for enumerating a root that
// need not be the caller's own key (bpctl's `objects <owner-or-parent-id>`).
// src is whichever childrenSource the caller wants the walk verified against:
// *Client for the node's unproven word, *LightClient for a proved walk
// against a trust checkpoint.
func EnumerateSubtree(src childrenSource, root [32]byte) ([][32]byte, error) {
	return walkChildren(src, root, recoveryDepthLimit)
}

// The two data sources recovery is meant to be wired with — checked here so a
// signature drift on either fails the build instead of surfacing as a runtime
// type error at the call site.
var (
	_ childrenSource = (*Client)(nil)
	_ childrenSource = (*LightClient)(nil)
)
