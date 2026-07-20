package consensus

import (
	"testing"

	"BluePods/internal/index"
)

// TestGenesisValidatorTreeRestartInvariant_Critical is the regression for the
// critical genesis-epoch fork: the index's validator tree must observe a
// committed registration or bond the moment it commits, not only at the first
// epoch boundary (transitionEpoch's RebuildValidators call, which epoch 0
// never reaches until it ends). Before the fix, a node's index tree built
// once at boot (cmd/node/indexing.go initIndex) stayed frozen at that
// snapshot for the rest of epoch 0, while a restarted twin (or any node fed
// the committed stream directly) rebuilds it from the CURRENT committed
// holders -- a different root the moment a genesis registration or bond
// commits, forking the anchored root across the network the instant batch 3
// anchors it. The existing restart tests (cmd/node/index_test.go) never
// mutated a validator between boot and restart, which is exactly why this
// slipped through.
func TestGenesisValidatorTreeRestartInvariant_Critical(t *testing.T) {
	founder := newTestValidator()
	second := newTestValidator()

	dag := New(newTestStorage(t), NewValidatorSet(nil), nil, testSystemPod, 0, founder.privKey, nil)
	t.Cleanup(func() { dag.Close() })

	// Genesis: the founder registers and bonds, exactly as SeedGenesisValidator
	// does at boot.
	dag.validators.AddWithStake(founder.pubKey, "", [48]byte{}, 25, 0, false)
	dag.commitMu.Lock()
	dag.recordCommittedMember(founder.pubKey, 0)
	dag.commitMu.Unlock()

	// initIndex's genesis-time build: a validator-tree snapshot taken once at
	// boot from the frozen genesis holders, wired via SetIndexer afterward --
	// mirroring cmd/node's initIndex -> SetIndexer sequence.
	live := index.NewManager()
	live.BuildFromState(nil, nil, dag.ValidatorLeaves(dag.epochHolders.All()))
	dag.SetIndexer(live)

	// A second validator registers mid-genesis (epoch 0), AFTER the initial
	// index build -- exactly the case the existing restart tests never
	// exercised.
	dag.validators.Add(second.pubKey, "", [48]byte{})
	dag.commitMu.Lock()
	dag.recordCommittedMember(second.pubKey, 5)
	dag.commitMu.Unlock()

	// ...then bonds, a separate committed event (mirroring the registration vs.
	// bond split handleRegisterValidator / the bond path each commit).
	dag.validators.SetSelfStake(second.pubKey, 10)
	dag.commitMu.Lock()
	dag.refreezeGenesisRegime(6)
	dag.commitMu.Unlock()

	// The "restarted twin": rebuilds fresh from the CURRENT committed holders --
	// exactly what a restart (or any node fed the committed stream directly)
	// observes. It never saw the live feed calls above.
	restarted := index.NewManager()
	restarted.BuildFromState(nil, nil, dag.ValidatorLeaves(dag.epochHolders.All()))

	if got, want := live.Root(), restarted.Root(); got != want {
		t.Fatalf("live (never-restarted) index root %x != restarted-twin root %x: "+
			"the genesis validator tree did not observe the mid-genesis registration+bond",
			got[:4], want[:4])
	}
}
