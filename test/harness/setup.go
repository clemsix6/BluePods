package harness

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"os"
	"time"

	"BluePods/pkg/client"
)

// bondFeeMargin is added to the faucet amount above the target stake so the
// staked coin also covers its own bond transaction's gas fee (Bond uses the
// staked coin as its own gas coin, debiting both the stake and the fee from
// the same balance). It comfortably covers DefaultFeeParams' MinGas (100)
// with headroom to spare.
const bondFeeMargin = 10_000

// stakeSetupTimeout bounds each non-founder's faucet-then-bond round trip.
const stakeSetupTimeout = 30 * time.Second

// setupStakes bonds an equal stake behind every non-founder validator so
// consensus weight is distributed across the cluster: the founder already
// holds InitialMint/10 in genesis self-stake (cmd/node's genesis config), so
// matching that per non-founder gives equal weight everywhere. It is
// NewCluster's default behavior, skipped by WithoutStakeSetup.
func (c *Cluster) setupStakes() {
	c.t.Helper()

	nonFounders := len(c.Nodes()) - 1
	if nonFounders <= 0 {
		return
	}

	stake := c.opts.stake
	if stake == 0 {
		stake = defaultStakeTarget(c.opts.initialMint, nonFounders)
	}

	for i := 1; i < len(c.Nodes()); i++ {
		c.bondValidator(i, stake)
	}

	c.t.Logf("stake setup: founder self-stake %d, %d non-founder(s) bonded %d each",
		c.opts.initialMint/10, nonFounders, stake)
}

// defaultStakeTarget aims for a stake equal to the founder's genesis
// self-stake (initialMint/10). The genesis reserve available to the faucet
// is initialMint-initialMint/10 (0.9 x initialMint): equal stakes fit
// exactly up to 9 non-founders. If matching the founder exactly, plus every
// non-founder's bondFeeMargin, would overdraw the reserve — the tight
// boundary at 9 non-founders, and any larger cluster — the per-node stake is
// shrunk so the total draw fits the reserve exactly, by construction: still
// equal across every non-founder, just no longer exactly equal to the
// founder.
func defaultStakeTarget(initialMint uint64, nonFounders int) uint64 {
	founderStake := initialMint / 10
	reserve := initialMint - founderStake

	target := founderStake
	draw := uint64(nonFounders) * (target + bondFeeMargin)

	if draw > reserve {
		target = reserve/uint64(nonFounders) - bondFeeMargin
	}

	return target
}

// bondValidator faucets a stake coin to node i's own validator key,
// registers that coin as its reward coin, and bonds stake behind it. The
// faucet request specifically goes through the FOUNDER's client: a node's
// handleFaucet derives the genesis reserve coin ID from ITS OWN key (there is
// no user-callable mint, only a split of the bootstrap node's reserve), so
// only the bootstrap node resolves it to the coin that actually exists.
func (c *Cluster) bondValidator(i int, stake uint64) {
	c.t.Helper()

	n := c.Node(i)
	founder := c.clientFor(c.Node(0))
	w := c.loadNodeWallet(n)

	coinID, faucetHash, err := founder.Faucet(w.Pubkey(), stake+bondFeeMargin)
	if err != nil {
		c.t.Fatalf("faucet stake coin for node %d: %v", i, err)
	}
	c.waitTxCommittedAll(faucetHash)

	cli := c.clientFor(n)

	if err := w.RefreshCoin(cli, coinID); err != nil {
		c.t.Fatalf("refresh stake coin for node %d: %v", i, err)
	}

	// blsPubkey is nil: the validator already carries its real BLS key from
	// its own self-registration at startup, and validators.Add only fills an
	// empty field, never overwrites one — this re-registration exists purely
	// to designate the reward coin.
	if err := w.RegisterValidator(cli, n.QUICAddr, nil, coinID); err != nil {
		c.t.Fatalf("register validator for node %d: %v", i, err)
	}

	coin := w.GetCoin(coinID)
	bondHash, err := w.Bond(cli, coinID, coin.Version, stake)
	if err != nil {
		c.t.Fatalf("bond for node %d: %v", i, err)
	}
	c.waitBondCommittedAll(bondHash)
}

// loadNodeWallet builds a Wallet from node n's own Ed25519 key file, so the
// stake is bonded behind the validator's real on-chain identity.
func (c *Cluster) loadNodeWallet(n *Node) *client.Wallet {
	c.t.Helper()

	data, err := os.ReadFile(n.KeyPath)
	if err != nil {
		c.t.Fatalf("read key for node %d: %v", n.Index, err)
	}
	if len(data) != ed25519.PrivateKeySize {
		c.t.Fatalf("node %d key file has unexpected size %d", n.Index, len(data))
	}

	return client.NewWalletFromKey(ed25519.PrivateKey(data))
}

// waitTxCommittedAll blocks until every alive node's journal records
// tx.committed for hash, regardless of success, bounded by
// stakeSetupTimeout. Waiting on every node (not just the one that submitted
// it) matters: consensus replicates the commit, but each node logs it on its
// own schedule, so a caller that only watched one node could race ahead of
// the others and observe a stale event count immediately after.
func (c *Cluster) waitTxCommittedAll(hash [32]byte) {
	c.t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), stakeSetupTimeout)
	defer cancel()

	if err := c.WaitAll(ctx, "tx.committed", Attr("tx", hex.EncodeToString(hash[:]))); err != nil {
		c.Dump(c.t)
		c.t.Fatalf("wait tx.committed(%x) on every node: %v", hash[:4], err)
	}
}

// waitBondCommittedAll blocks until every alive node's journal records a
// successful tx.committed for hash, bounded by stakeSetupTimeout.
func (c *Cluster) waitBondCommittedAll(hash [32]byte) {
	c.t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), stakeSetupTimeout)
	defer cancel()

	preds := []Pred{Attr("tx", hex.EncodeToString(hash[:])), Attr("success", true)}
	if err := c.WaitAll(ctx, "tx.committed", preds...); err != nil {
		c.Dump(c.t)
		c.t.Fatalf("wait successful bond tx.committed(%x) on every node: %v", hash[:4], err)
	}
}
