package consensus

import (
	"bytes"
	"sort"
	"testing"

	"BluePods/internal/storage"
)

// sortedSetKeys returns a validator set's pubkeys sorted ascending, so two frozen
// committees compare independently of set iteration order.
func sortedSetKeys(set *ValidatorSet) []Hash {
	if set == nil {
		return nil
	}

	keys := make([]Hash, 0, set.Len())
	for _, v := range set.All() {
		keys = append(keys, v.Pubkey)
	}

	sort.Slice(keys, func(i, j int) bool {
		return bytes.Compare(keys[i][:], keys[j][:]) < 0
	})

	return keys
}

// restoreLiveSet rebuilds a live validator set from durably persisted infos, the
// test stand-in for the node's restart rebuild (every field including the reward
// coin), so a restarted DAG starts from the same live set the node would.
func restoreLiveSet(infos []*ValidatorInfo) *ValidatorSet {
	if len(infos) == 0 {
		return nil
	}

	vs := NewValidatorSet(nil)
	for _, v := range infos {
		vs.AddWithStake(v.Pubkey, v.QUICAddr, v.BLSPubkey, v.SelfStake, v.DelegatedTotal, v.Jailed)
		vs.SetRewardCoin(v.Pubkey, v.RewardCoin)
	}

	return vs
}

// TestRestartRefreezesFullCommitteePastBoundary asserts that a node restarted in an
// epoch past the genesis epoch refreezes the SAME committee at the next boundary as
// it held before the restart. The committed member set drives the freeze, so it must
// survive a restart: without durable persistence it comes back empty, the bootstrap
// path re-seeds only the founder, and the next boundary freezes a founder-only
// committee — a durable fork of the execution shard, routed reads, and BLS quorum
// resolution.
func TestRestartRefreezesFullCommitteePastBoundary(t *testing.T) {
	vals, vs := seededValidatorSet(4)
	const epochLen = 4

	dir := t.TempDir()

	// Phase 1: drive commits across the first boundary (epoch 1), freezing the full
	// four-member committee, and persist everything to the data dir.
	db1, err := storage.New(dir)
	if err != nil {
		t.Fatalf("open storage: %v", err)
	}

	dag1 := newPersistDAG(t, db1, vals, vs, WithEpochLength(epochLen))
	spine := buildTaggedSpine(t, vals, 5) // crosses the round-4 boundary
	ingest(dag1, spine, roundIndices(0, 6, len(vals)))
	dag1.checkCommits()

	if dag1.Epoch() != 1 {
		t.Fatalf("pre-restart node did not cross the first boundary: epoch=%d", dag1.Epoch())
	}

	committeeBefore := sortedSetKeys(dag1.epochHolders)
	if len(committeeBefore) != len(vals) {
		t.Fatalf("pre-restart committee is not the full set: got %d want %d", len(committeeBefore), len(vals))
	}
	dag1.Close()
	db1.Close()

	// Phase 2: restart over the same data dir. The live set is rebuilt from durable
	// state exactly as the node does (LoadLiveValidators); NO test helper re-seeds the
	// committed set, so committedMembers comes only from restore plus the bootstrap
	// founder re-seed below.
	db2, err := storage.New(dir)
	if err != nil {
		t.Fatalf("reopen storage: %v", err)
	}
	defer db2.Close()

	restored := restoreLiveSet(LoadLiveValidators(db2))
	if restored == nil || restored.Len() != len(vals) {
		t.Fatalf("live validator set not restored from durable state: %+v", restored)
	}

	dag2 := New(db2, restored, nil, testSystemPod, 0, vals[0].privKey, nil,
		WithEpochLength(epochLen), WithListenerMode())
	t.Cleanup(func() { dag2.Close() })

	// The bootstrap path re-seeds ONLY the founder into the committed set on every
	// start (SeedGenesisValidator). Mimic that so committedMembers reflects what a
	// real restart holds after boot.
	dag2.commitMu.Lock()
	dag2.recordCommittedMember(vals[0].pubKey, dag2.lastCommitted)

	// Cross the next boundary and read the frozen committee under the same lock.
	dag2.transitionEpoch(2 * epochLen)
	committeeAfter := sortedSetKeys(dag2.epochHolders)
	dag2.commitMu.Unlock()

	if !equalHashes(committeeBefore, committeeAfter) {
		t.Fatalf("restart in epoch >= 1 froze a divergent committee past the boundary:\n before=%x\n after=%x",
			committeeBefore, committeeAfter)
	}
}

// TestSyncAdoptsFullCommitteePastBoundary asserts that a node synced past the genesis
// epoch adopts the source's network-uniform committed member set and freezes the SAME
// committee at the next boundary. The sync snapshot must carry the committed set: a
// joiner that instead rebuilds it locally from the first committed registration it
// observes holds a partial set and freezes a joiner-only committee at the boundary —
// a durable fork from the rest of the network.
func TestSyncAdoptsFullCommitteePastBoundary(t *testing.T) {
	vals, vs := seededValidatorSet(4)
	const epochLen = 4

	// A source already past the first boundary (epoch 1, latched) with the full
	// four-member committee committed.
	src := New(newTestStorage(t), vs, nil, testSystemPod, 0, vals[0].privKey, nil, WithEpochLength(epochLen))
	t.Cleanup(func() { src.Close() })
	setEqualStake(src, vals, 25) // freezes genesis: committedMembers = all four

	src.commitMu.Lock()
	src.currentEpoch = 1
	src.epochHolders = snapshotOf(src.validators)
	src.prevEpochHolders = snapshotOf(src.validators)
	src.strictLatched = true
	src.strictStartRound = 2
	committeeSource := sortedSetKeys(src.epochHolders)
	src.commitMu.Unlock()

	_, blob := src.ExportRegimeState()

	// A joiner boots fresh and installs the sync-carried regime state.
	joiner := New(newTestStorage(t), NewValidatorSet(keysOf(vals)), nil, testSystemPod, 0, vals[0].privKey, nil,
		WithEpochLength(epochLen), WithSyncedRegimeState(blob), WithLastCommittedRound(5))
	t.Cleanup(func() { joiner.Close() })

	// Give the joiner the live set a real synced node rebuilds from the snapshot.
	for _, v := range vals {
		joiner.validators.SetSelfStake(v.pubKey, 25)
	}

	joiner.commitMu.Lock()
	// A synced node past the genesis epoch rebuilt its committed set from the first
	// committed registration it observed, holding a partial set. Mimic that single
	// observation so the boundary freeze reflects what such a node would hold.
	joiner.recordCommittedMember(vals[1].pubKey, joiner.lastCommitted)

	// Cross the next boundary and read the frozen committee under the same lock.
	joiner.transitionEpoch(2 * epochLen)
	committeeAfter := sortedSetKeys(joiner.epochHolders)
	joiner.commitMu.Unlock()

	if len(committeeSource) != len(vals) {
		t.Fatalf("source committee is not the full set: got %d want %d", len(committeeSource), len(vals))
	}
	if !equalHashes(committeeSource, committeeAfter) {
		t.Fatalf("sync in epoch >= 1 froze a divergent committee past the boundary:\n source=%x\n joiner=%x",
			committeeSource, committeeAfter)
	}
}
