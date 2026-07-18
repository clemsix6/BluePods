package consensus

import (
	"fmt"
	"os"
	"testing"

	"BluePods/internal/genesis"
	"BluePods/internal/storage"
)

// blsKeyFor builds a distinct non-zero BLS key for validator i, so the persisted
// blob carries a value that a dropped field would visibly lose on restore.
func blsKeyFor(i int) [48]byte {
	var bls [48]byte
	bls[0] = byte(0xB0 + i)
	bls[47] = byte(i + 1)
	return bls
}

// coinFor builds a distinct non-zero reward-coin id for validator i.
func coinFor(i int) Hash {
	var coin Hash
	coin[0] = byte(0xC0 + i)
	coin[31] = byte(i + 7)
	return coin
}

// TestLiveValidatorSetRestoredOnRestart is co-factor (a) of the bootstrap-restart bug:
// a restarting node must rebuild its LIVE validator set from durable state, not start
// from the bare local identity. A node commits a round with a fully-populated live set
// (distinct per-validator stake, delegated total, jail flag, BLS key, QUIC address, and
// reward coin), crashes, and reopens over the same storage. LoadLiveValidators must
// return every validator with every field intact — dropping any one forks total_bonded
// or the convergence fingerprint on restart. Without the persist-with-cursor path the
// key is never written and the reopen restores nothing.
func TestLiveValidatorSetRestoredOnRestart(t *testing.T) {
	vals, _ := seededValidatorSet(4)
	n := len(vals)

	dir, err := os.MkdirTemp("", "consensus_live_validators_*")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	defer os.RemoveAll(dir)

	db1, err := storage.New(dir)
	if err != nil {
		t.Fatalf("open storage: %v", err)
	}

	_, vs1 := reuseSet(vals)
	dag1 := newPersistDAG(t, db1, vals, vs1)

	// Give the live set fields that diverge from any frozen/genesis snapshot, so a
	// restart restoring only a frozen committee (or nothing) would visibly lose them.
	want := make(map[Hash]ValidatorInfo, n)
	for i, v := range vals {
		quic := fmt.Sprintf("10.0.0.%d:90%02d", i+1, i)
		dag1.validators.Add(v.pubKey, quic, blsKeyFor(i)) // back-fills the empty addr/BLS
		dag1.validators.SetSelfStake(v.pubKey, uint64(1_000_000*(i+1)))
		dag1.validators.AddDelegated(v.pubKey, uint64(250_000*i))
		dag1.validators.SetRewardCoin(v.pubKey, coinFor(i))
	}
	dag1.validators.Jail(vals[2].pubKey)
	for _, v := range vals {
		want[v.pubKey] = *dag1.validators.Get(v.pubKey)
	}

	// Persist through the normal path: a committed round advances the cursor, which
	// writes the live validator set in the same atomic batch.
	spine := buildTaggedSpine(t, vals, 3)
	ingest(dag1, spine, roundIndices(0, 4, n))
	dag1.checkCommits()
	if dag1.LastCommittedRound() == 0 {
		t.Fatal("no round committed; live set never persisted through the normal path")
	}
	dag1.Close()
	db1.Close()

	// Restart: reopen the storage and rebuild the live set the way buildValidatorSet does.
	db2, err := storage.New(dir)
	if err != nil {
		t.Fatalf("reopen storage: %v", err)
	}
	defer db2.Close()

	restored := LoadLiveValidators(db2)
	if len(restored) != n {
		t.Fatalf("restored %d validators, want %d (membership lost across restart)", len(restored), n)
	}

	got := make(map[Hash]*ValidatorInfo, len(restored))
	for _, v := range restored {
		got[v.Pubkey] = v
	}

	for pk, w := range want {
		g := got[pk]
		if g == nil {
			t.Fatalf("validator %x absent after restart", pk[:4])
		}
		if g.SelfStake != w.SelfStake {
			t.Errorf("validator %x self-stake = %d, want %d", pk[:4], g.SelfStake, w.SelfStake)
		}
		if g.DelegatedTotal != w.DelegatedTotal {
			t.Errorf("validator %x delegated = %d, want %d", pk[:4], g.DelegatedTotal, w.DelegatedTotal)
		}
		if g.RewardCoin != w.RewardCoin {
			t.Errorf("validator %x reward coin = %x, want %x", pk[:4], g.RewardCoin[:4], w.RewardCoin[:4])
		}
		if g.Jailed != w.Jailed {
			t.Errorf("validator %x jailed = %v, want %v", pk[:4], g.Jailed, w.Jailed)
		}
		if g.BLSPubkey != w.BLSPubkey {
			t.Errorf("validator %x BLS key not restored", pk[:4])
		}
		if g.QUICAddr != w.QUICAddr {
			t.Errorf("validator %x QUIC = %q, want %q", pk[:4], g.QUICAddr, w.QUICAddr)
		}
	}
}

// TestSeedGenesisValidatorMergesOnRestart is co-factor (b): SeedGenesisValidator runs on
// every bootstrap start, and on a restart it must not regress the founder's live
// self-stake and reward coin — restored from durable state before it runs — back to
// their genesis values. It seeds those values only on a fresh chain, where the founder
// has no live stake yet.
func TestSeedGenesisValidatorMergesOnRestart(t *testing.T) {
	const genesisStake = uint64(100_000_000_000)
	genesisCoin := coinFor(0)

	newDAG := func(t *testing.T) (*DAG, Hash) {
		t.Helper()
		vals, vs := seededValidatorSet(1)
		dag := New(newTestStorage(t), vs, nil, testSystemPod, 0, vals[0].privKey, nil, WithListenerMode())
		t.Cleanup(dag.Close)
		return dag, vals[0].pubKey
	}

	is := func(founder Hash, stake uint64, coin Hash) genesis.InitialState {
		return genesis.InitialState{
			Pubkey:    [32]byte(founder),
			QUIC:      "10.0.0.1:9001",
			BLS:       make([]byte, blsKeyLen),
			SelfStake: stake,
			CoinID:    [32]byte(coin),
		}
	}

	t.Run("restart_does_not_overwrite_live", func(t *testing.T) {
		dag, founder := newDAG(t)

		// Simulate the durable restore: the founder is back with a self-stake bonded
		// above genesis and a reward coin that differs from its genesis coin.
		const liveStake = genesisStake + 7_000_000
		liveCoin := coinFor(3)
		dag.validators.SetSelfStake(founder, liveStake)
		dag.validators.SetRewardCoin(founder, liveCoin)

		dag.SeedGenesisValidator(is(founder, genesisStake, genesisCoin))

		info := dag.validators.Get(founder)
		if info.SelfStake != liveStake {
			t.Errorf("founder self-stake regressed to %d, want live %d preserved", info.SelfStake, liveStake)
		}
		if info.RewardCoin != liveCoin {
			t.Errorf("founder reward coin regressed to %x, want live %x preserved", info.RewardCoin[:4], liveCoin[:4])
		}
	})

	t.Run("fresh_chain_seeds_genesis", func(t *testing.T) {
		dag, founder := newDAG(t)

		// Fresh chain: the founder carries no live self-stake yet, so the genesis
		// values must be installed.
		dag.SeedGenesisValidator(is(founder, genesisStake, genesisCoin))

		info := dag.validators.Get(founder)
		if info.SelfStake != genesisStake {
			t.Errorf("founder self-stake = %d, want genesis %d installed", info.SelfStake, genesisStake)
		}
		if info.RewardCoin != genesisCoin {
			t.Errorf("founder reward coin = %x, want genesis %x installed", info.RewardCoin[:4], genesisCoin[:4])
		}
	})
}
