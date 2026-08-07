package scenarios

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"BluePods/internal/index"
	"BluePods/pkg/client"
)

// mintCheckpoint pins a light client's trust anchor by reading it off the
// node behind transport through the exact public primitives a real light
// client uses: GetIndexAnchor for the bundle's frontier and index root, then
// GetValidatorTree for the epoch the bundle names. The validator-set hash is
// rebuilt from the served leaves with the same function Checkpoint.
// authenticate itself uses to check it (index.ValidatorRootOf), so the pin
// is exactly what an operator publishing epoch.validators.frozen would hand
// a real light client out of band — never a value picked by hand (mirroring
// how test/harness's Cluster.trustCheckpointFrom mints a joiner's checkpoint
// from a founder's own published state, and how cmd/node/checkpoint.go
// verifies one).
//
// GetValidatorTree only answers Found for the epoch its own index tree
// currently describes (cmd/node/indexqueries.go), so when the bundle's
// epoch hint has already gone stale this re-asks once with the epoch the
// node reports actually serving — the same opportunistic re-ask a light
// client's own epoch walk performs (pkg/client/lightclient.go's advance).
func mintCheckpoint(t *testing.T, transport *client.QUICTransport) client.Checkpoint {
	t.Helper()

	bundle, err := transport.GetIndexAnchor()
	requireNoErr(t, err)
	if !bundle.Found {
		t.Fatalf("no quorate index anchor bundle to mint a checkpoint from")
	}

	vt, err := transport.GetValidatorTree(bundle.Epoch)
	requireNoErr(t, err)
	if !vt.Found {
		vt, err = transport.GetValidatorTree(vt.Epoch)
		requireNoErr(t, err)
	}
	if !vt.Found {
		t.Fatalf("node serves no validator tree for its own reported epoch %d", vt.Epoch)
	}

	leaves := make([]index.ValidatorLeaf, 0, len(vt.Leaves))
	for i, raw := range vt.Leaves {
		leaf, ok := index.DecodeValidatorLeaf(raw)
		if !ok {
			t.Fatalf("validator tree epoch %d: leaf %d does not decode", vt.Epoch, i)
		}
		leaves = append(leaves, leaf)
	}

	return client.Checkpoint{
		Epoch:            vt.Epoch,
		IndexRoot:        bundle.IndexRoot,
		ValidatorSetHash: index.ValidatorRootOf(leaves),
	}
}

// retryUnanchored calls read until it returns a nil error, retrying only on
// client.ErrUnanchored: the narrow race between a tree mutation and the
// SetFrontier that closes its commit batch (pkg/client/verifyproof.go),
// gone within the next commit. Any other error, or the bound expiring
// first, fails the test — this is a bounded retry of a transient RPC race,
// never a substitute for the corpus's event-driven waits.
func retryUnanchored(ctx context.Context, t *testing.T, what string, read func() error) {
	t.Helper()

	ticker := time.NewTicker(eventPollInterval)
	defer ticker.Stop()

	for {
		err := read()
		if err == nil {
			return
		}
		if !errors.Is(err, client.ErrUnanchored) {
			t.Fatalf("%s: %v", what, err)
		}

		select {
		case <-ticker.C:
			continue
		case <-ctx.Done():
			t.Fatalf("%s: still unanchored after the bound: %v", what, err)
			return
		}
	}
}

// resolveDomainProved reads name through lc, verified end to end against its
// checkpoint, bounded and retried per retryUnanchored.
func resolveDomainProved(ctx context.Context, t *testing.T, lc *client.LightClient, name string) (index.DomainLeaf, bool) {
	t.Helper()

	var leaf index.DomainLeaf
	var found bool

	retryUnanchored(ctx, t, fmt.Sprintf("proved resolve of %q", name), func() error {
		var err error
		leaf, found, err = lc.ResolveDomain(name)
		return err
	})

	return leaf, found
}

// listChildrenProved enumerates parent's children through lc, verified end
// to end against its checkpoint (a completeness proof, not the node's bare
// word), bounded and retried per retryUnanchored.
func listChildrenProved(ctx context.Context, t *testing.T, lc *client.LightClient, parent [32]byte) [][32]byte {
	t.Helper()

	var children [][32]byte

	retryUnanchored(ctx, t, fmt.Sprintf("proved children of %x", parent[:8]), func() error {
		var err error
		children, err = lc.ListChildren(parent)
		return err
	})

	return children
}

// ancestorsProved walks object's ancestry through lc, verified end to end
// against its checkpoint, bounded and retried per retryUnanchored.
func ancestorsProved(ctx context.Context, t *testing.T, lc *client.LightClient, object [32]byte) []index.ParentLeaf {
	t.Helper()

	var edges []index.ParentLeaf

	retryUnanchored(ctx, t, fmt.Sprintf("proved ancestors of %x", object[:8]), func() error {
		var err error
		edges, err = lc.Ancestors(object)
		return err
	})

	return edges
}

// containsID reports whether ids includes want.
func containsID(ids [][32]byte, want [32]byte) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}

	return false
}
