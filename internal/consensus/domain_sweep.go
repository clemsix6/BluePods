package consensus

import (
	"sort"

	"BluePods/internal/events"
)

// sweepExpiredDomains removes every name whose lease has run past its grace
// window as of newEpoch — the epoch the boundary now landing transitions
// INTO. A name is swept when newEpoch > expiry + grace; grace only reserves
// the owner's exclusive right to renew (spec: an expired-but-in-grace name
// already stops resolving at execution and in queries), so the sweep is the
// only thing that ever removes a leaf once it is registered.
//
// It reads the committed domain registry through the DomainStore seam, never
// any in-memory residue, so the swept set is a pure function of committed
// state — identical on every node, including one that restarted mid-epoch.
// Names are swept in sorted order (map iteration is not deterministic), so
// the sequence of store writes, index removals, and emitted events is
// byte-identical everywhere. A DAG with no domain store wired sweeps
// nothing, the same no-op every other domain feed point takes when unset.
func (d *DAG) sweepExpiredDomains(newEpoch uint64) {
	if d.domains == nil {
		return
	}

	grace := d.graceEpochs()

	var names []string
	for _, entry := range d.domains.ExportDomains() {
		if newEpoch > safeAdd(entry.ExpiryEpoch, grace) {
			names = append(names, entry.Name)
		}
	}

	sort.Strings(names)

	for _, name := range names {
		d.domains.DeleteDomainLeaf(name)

		if d.indexer != nil {
			d.indexer.RemoveDomain(name)
		}

		events.DomainDeleted(name, Hash{}, "expired")
	}
}
