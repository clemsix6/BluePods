package main

import (
	"crypto/ed25519"
	"fmt"
	"os"

	"github.com/zeebo/blake3"

	"BluePods/internal/aggregation"
	"BluePods/internal/consensus"
	"BluePods/internal/genesis"
	"BluePods/internal/logger"
	"BluePods/internal/network"
	"BluePods/internal/podvm"
	"BluePods/internal/state"
	"BluePods/internal/storage"
)

// initStorage initializes the Pebble storage.
func (n *Node) initStorage() error {
	dbPath := n.cfg.DataPath + "/db"

	if err := os.MkdirAll(n.cfg.DataPath, 0755); err != nil {
		return fmt.Errorf("create data directory:\n%w", err)
	}

	db, err := storage.New(dbPath)
	if err != nil {
		return fmt.Errorf("init storage:\n%w", err)
	}

	n.storage = db

	return nil
}

// initPodVM initializes the WASM runtime pool with the system pod.
func (n *Node) initPodVM() error {
	pool := podvm.New()

	wasmBytes, err := os.ReadFile(n.cfg.SystemPodPath)
	if err != nil {
		pool.Close()
		return fmt.Errorf("read system pod WASM:\n%w", err)
	}

	// Use blake3 hash as system pod ID
	n.systemPod = blake3.Sum256(wasmBytes)

	if _, err := pool.Load(wasmBytes, &n.systemPod); err != nil {
		pool.Close()
		return fmt.Errorf("load system pod:\n%w", err)
	}

	n.podPool = pool

	return nil
}

// initState initializes the transaction executor.
func (n *Node) initState() error {
	n.state = state.New(n.storage, n.podPool)
	return nil
}

// initNetwork initializes the P2P network node.
func (n *Node) initNetwork() error {
	netCfg := network.Config{
		PrivateKey: n.cfg.PrivateKey,
		ListenAddr: n.cfg.QUICAddress,
	}

	node, err := network.NewNode(netCfg)
	if err != nil {
		return fmt.Errorf("init network:\n%w", err)
	}

	n.network = node

	return nil
}

// initConsensus initializes the DAG consensus engine.
func (n *Node) initConsensus() error {
	validators := n.buildValidatorSet()
	opts := n.buildConsensusOpts()

	n.dag = consensus.New(
		n.storage,
		validators,
		n.network,
		n.systemPod,
		0, // epoch
		n.cfg.PrivateKey,
		n.state,
		opts...,
	)

	// Wire missing-ancestor recovery (bootstrap construction site). Every construction
	// path must do this, or a decided anchor whose ancestry never gossips stalls.
	n.dag.SetVertexFetcher(n.newVertexFetcher())

	n.setupValidatorCallback()
	n.initAggregation(validators)
	n.seedGenesisState()
	n.initIndex()

	return nil
}

// buildValidatorSet creates the initial validator set. On a restart over an existing
// data directory it rebuilds the LIVE set from the durable snapshot persisted with the
// commit cursor, so total_bonded and every validator's stake and reward-coin
// designation survive; only a fresh start falls back to the bare local identity.
func (n *Node) buildValidatorSet() *consensus.ValidatorSet {
	pubKey := n.cfg.PrivateKey.Public().(ed25519.PublicKey)

	var hash consensus.Hash
	copy(hash[:], pubKey)

	// Restart: rebuild the live validator set from durable state — the local
	// equivalent of the sync path's buildValidatorSetFromSnapshot.
	if restored := restoreValidatorSet(consensus.LoadLiveValidators(n.storage)); restored != nil {
		return restored
	}

	if n.cfg.Bootstrap {
		// Bootstrap validator must be in set BEFORE first vertex for validation
		return consensus.NewValidatorSet([]consensus.Hash{hash})
	}

	return consensus.NewValidatorSet(nil)
}

// restoreValidatorSet rebuilds a live validator set from validators persisted with the
// commit cursor, or nil when none were persisted (a fresh chain). Each validator is
// restored with every field the set carries, mirroring buildValidatorSetFromSnapshot:
// RewardCoin is set explicitly because AddWithStake omits it, and dropping it — or any
// other field — would fork the convergence fingerprint on restart.
func restoreValidatorSet(infos []*consensus.ValidatorInfo) *consensus.ValidatorSet {
	if len(infos) == 0 {
		return nil
	}

	vs := consensus.NewValidatorSet(nil)
	for _, v := range infos {
		vs.AddWithStake(v.Pubkey, v.QUICAddr, v.BLSPubkey, v.SelfStake, v.DelegatedTotal, v.Jailed)
		vs.SetRewardCoin(v.Pubkey, v.RewardCoin)
	}

	return vs
}

// buildConsensusOpts creates consensus options for the bootstrap node.
// Genesis is seeded as state after the fee system is wired (seedGenesisState),
// not injected as transactions.
func (n *Node) buildConsensusOpts() []consensus.Option {
	if !n.cfg.Bootstrap {
		return nil
	}

	// Derive BLS key early so it's available for genesis seeding.
	blsKey, err := aggregation.DeriveFromED25519(n.cfg.PrivateKey)
	if err != nil {
		logger.Warn("failed to derive BLS key for genesis", "error", err)
		return nil
	}

	n.blsKey = blsKey

	opts := []consensus.Option{
		consensus.WithBootstrap(),
		consensus.WithMinValidators(n.cfg.MinValidators),
	}

	if n.cfg.GossipFanout > 0 {
		opts = append(opts, consensus.WithGossipFanout(n.cfg.GossipFanout))
	}

	return n.appendEpochOpts(opts)
}

// genesisConfig builds the genesis configuration for the bootstrap node.
// GenesisStake defaults to a tenth of the mint (with a small floor) so the
// founding validator has non-zero stake-weighted quorum weight.
func (n *Node) genesisConfig() genesis.Config {
	stake := n.cfg.InitialMint / 10
	if stake == 0 {
		stake = n.cfg.InitialMint
	}

	return genesis.Config{
		PrivateKey:   n.cfg.PrivateKey,
		InitialMint:  n.cfg.InitialMint,
		GenesisStake: stake,
		QUICAddress:  n.cfg.QUICAddress,
		SystemPodID:  n.systemPod,
		BLSPubkey:    n.blsKey.PublicKeyBytes(),
	}
}

// seedGenesisState seeds the initial ledger state on the bootstrap node. It must
// run after initAggregation wires SetFeeSystem so the coin store is available.
//
// The ledger (the reserve coin, total_supply, coins_total) is seeded only once:
// a bootstrap node restarting over its own data directory must not re-seed it,
// or it overwrites every balance change committed since genesis. The guard is
// the genesis coin's presence in state. On a restart the live validator set is
// rebuilt from durable state by buildValidatorSet, so SeedGenesisValidator runs
// merge-safe: it back-fills the founder's address and committed membership but
// leaves the restored self-stake and reward coin untouched, seeding their genesis
// values only on a fresh chain where the founder has no live stake yet.
func (n *Node) seedGenesisState() {
	if !n.cfg.Bootstrap {
		return
	}

	owner := deriveOwner(n.cfg.PrivateKey)
	is := genesis.BuildInitialState(n.genesisConfig(), owner)

	if n.state.GetObject(genesis.GenesisCoinID(owner)) != nil {
		n.dag.SeedGenesisValidator(is)
		logger.Info("genesis ledger already seeded; skipped re-seed on restart",
			"supply", n.state.TotalSupply(),
		)
		return
	}

	n.dag.SeedGenesis(is)

	logger.Info("seeded genesis state",
		"supply", is.Supply,
		"self_stake", is.SelfStake,
	)
}

// deriveOwner returns the 32-byte owner pubkey from an Ed25519 private key.
func deriveOwner(priv ed25519.PrivateKey) [32]byte {
	var owner [32]byte
	copy(owner[:], priv.Public().(ed25519.PublicKey))

	return owner
}

// appendEpochOpts adds epoch and transition options if configured.
func (n *Node) appendEpochOpts(opts []consensus.Option) []consensus.Option {
	if n.cfg.EpochLength > 0 {
		opts = append(opts, consensus.WithEpochLength(n.cfg.EpochLength))
	}

	if n.cfg.MaxChurnPerEpoch > 0 {
		opts = append(opts, consensus.WithMaxChurnPerEpoch(n.cfg.MaxChurnPerEpoch))
	}

	if n.cfg.TransitionGrace > 0 {
		opts = append(opts, consensus.WithTransitionGrace(n.cfg.TransitionGrace))
	}

	if n.cfg.TransitionBuffer > 0 {
		opts = append(opts, consensus.WithTransitionBuffer(n.cfg.TransitionBuffer))
	}

	return opts
}
