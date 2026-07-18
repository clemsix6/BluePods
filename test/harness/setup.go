package harness

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"os"
	"time"

	"BluePods/internal/network"
	"BluePods/pkg/client"
)

// bondFeeMargin is added to the faucet amount above the target stake so the
// staked coin also covers its own bond transaction's gas fee (Bond uses the
// staked coin as its own gas coin, debiting both the stake and the fee from
// the same balance). It comfortably covers DefaultFeeParams' MinGas (100)
// with headroom to spare.
const bondFeeMargin = 10_000

// faucetFeeAllowance is the reserve-side budget for one setup faucet's own
// protocol charges: the faucet transaction's fee and the created coin's
// storage deposit are debited from the GENESIS RESERVE coin (2000 total at
// the default fee params), on top of the fauceted amount. Sizing the stake
// target without this allowance drains the reserve to the exact fauceted
// total and the LAST faucet fails execution, deterministically, on any
// cluster large enough for the reserve-fitted stake computation to bind.
const faucetFeeAllowance = 10_000

// reserveHeadroom is the slice of the genesis reserve the stake setup leaves
// unspent so the scenario itself can faucet traffic afterwards. Without it,
// the reserve-fitted stake computation drains the reserve to change and
// every post-setup faucet fails execution. It is orders of magnitude above
// any scenario's traffic needs and three orders below the default mint's
// reserve, so stake weights stay effectively equal.
const reserveHeadroom = 1_000_000_000

// stakeSetupTimeout bounds each non-founder's faucet-then-bond round trip.
const stakeSetupTimeout = 30 * time.Second

// refreshCoinRetryInterval paces refreshCoinWithRetry's retries, instead of
// hammering the node with a tight, delay-free retry loop.
const refreshCoinRetryInterval = 200 * time.Millisecond

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

	c.waitStrictRegime()
}

// strictRegimeTimeout bounds the post-bond wait for the strict latch to arm and
// every commit cursor to pass strictStartRound.
const strictRegimeTimeout = 90 * time.Second

// waitStrictRegime blocks until every node reports the strict latch armed and its
// commit cursor past strictStartRound, so the scenario's traffic runs entirely
// under strict quorum decisions. The relaxed bootstrap certificate is
// view-dependent under delivery skew: a transaction committed inside its window
// can be swept on one node and skipped on another, permanently forking derived
// state at equal totals. The default setup just bonded every validator and
// minValidators defaults to the cluster size, so the latch is guaranteed to arm;
// not reaching it within the bound is a hard failure, never silently tolerated.
func (c *Cluster) waitStrictRegime() {
	c.t.Helper()

	deadline := time.Now().Add(strictRegimeTimeout)

	for _, n := range c.Nodes() {
		for {
			status, err := c.nodeStatus(n)
			if err == nil && status.StrictLatched && status.LastCommitted >= status.StrictStart {
				break
			}

			if time.Now().After(deadline) {
				c.t.Fatalf("node %d never entered the strict regime: status=%+v err=%v", n.Index, status, err)
			}

			time.Sleep(200 * time.Millisecond)
		}
	}
}

// nodeStatus fetches one node's status tolerantly: any client or transport error
// is returned for the caller to retry rather than failing the test.
func (c *Cluster) nodeStatus(n *Node) (*network.StatusResponse, error) {
	cli, err := c.newClientFor(n)
	if err != nil {
		return nil, err
	}

	return cli.Status()
}

// defaultStakeTarget aims for a stake equal to the founder's genesis
// self-stake (initialMint/10). The budget available to the setup faucets is
// the genesis reserve (0.9 x initialMint) minus reserveHeadroom (kept for
// the scenario's own traffic). Each non-founder draws its stake plus
// bondFeeMargin (the fauceted amount) plus faucetFeeAllowance (the
// reserve-side protocol charges of the faucet itself). If matching the
// founder exactly would overdraw that budget — the boundary sits at 9
// non-founders, and any larger cluster — the per-node stake is shrunk so the
// total draw fits the budget by construction: still equal across every
// non-founder, just no longer exactly equal to the founder.
func defaultStakeTarget(initialMint uint64, nonFounders int) uint64 {
	founderStake := initialMint / 10
	budget := initialMint - founderStake - reserveHeadroom

	perNodeOverhead := uint64(bondFeeMargin + faucetFeeAllowance)

	target := founderStake
	draw := uint64(nonFounders) * (target + perNodeOverhead)

	if draw > budget {
		target = budget/uint64(nonFounders) - perNodeOverhead
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

	c.refreshCoinWithRetry(i, cli, w, coinID)

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
	c.waitTxCommittedAll(bondHash)
}

// refreshCoinWithRetry refreshes the stake coin, retrying within
// stakeSetupTimeout: on a large cluster the last bonds run at peak vertex
// load, where a single QUIC read can transiently exceed its own request
// deadline even though the coin is committed everywhere. One transient
// timeout must not kill the whole scenario's setup.
func (c *Cluster) refreshCoinWithRetry(i int, cli *client.Client, w *client.Wallet, coinID [32]byte) {
	c.t.Helper()

	deadline := time.Now().Add(stakeSetupTimeout)

	ticker := time.NewTicker(refreshCoinRetryInterval)
	defer ticker.Stop()

	var err error
	for {
		if err = w.RefreshCoin(cli, coinID); err == nil {
			return
		}
		if !time.Now().Before(deadline) {
			c.t.Fatalf("refresh stake coin for node %d: %v", i, err)
			return
		}
		<-ticker.C
	}
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

// waitTxCommittedAll blocks until every alive node's journal records a
// SUCCESSFUL tx.committed for hash, bounded by stakeSetupTimeout. Requiring
// success matters twice over: a setup transaction that commits-but-fails
// must kill the setup HERE with the transaction named, not surface later as
// a mystifying read timeout; and waiting on every node (not just the
// submitter) keeps the caller from racing ahead of the slower nodes.
func (c *Cluster) waitTxCommittedAll(hash [32]byte) {
	c.t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), stakeSetupTimeout)
	defer cancel()

	preds := []Pred{Attr("tx", hex.EncodeToString(hash[:])), Attr("success", true)}
	if err := c.WaitAll(ctx, "tx.committed", preds...); err != nil {
		c.Dump(c.t)
		c.t.Fatalf("wait successful tx.committed(%x) on every node: %v", hash[:4], err)
	}
}
