package harness

import (
	"context"
	"encoding/hex"
	"testing"
	"time"

	"BluePods/pkg/client"
)

// TestClusterBasics starts a 3-node cluster, asserts the default stake setup
// completed (every node's journal shows both non-founder bonds, since
// consensus replicates the commit stream to every node), drives a coin
// split through Client(0) to commitment on every node, and round-trips a
// partition and heal.
func TestClusterBasics(t *testing.T) {
	c := NewCluster(t, 3)

	for _, n := range c.Nodes() {
		got := n.Journal().Events("stake.bonded")
		if len(got) != 2 {
			t.Fatalf("node %d: expected 2 stake.bonded events (one per non-founder), got %d", n.Index, len(got))
		}
	}

	w := client.NewWallet()
	cli0 := c.Client(0)

	coinID, faucetHash, err := cli0.Faucet(w.Pubkey(), 1_000_000)
	if err != nil {
		t.Fatalf("faucet: %v", err)
	}

	waitCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := c.WaitAll(waitCtx, "tx.committed", Attr("tx", hex.EncodeToString(faucetHash[:]))); err != nil {
		t.Fatalf("wait faucet commit on every node: %v", err)
	}

	if err := w.RefreshCoin(cli0, coinID); err != nil {
		t.Fatalf("refresh coin: %v", err)
	}

	recipient := client.NewWallet().Pubkey()

	_, splitHash, err := w.Split(cli0, coinID, 100, recipient)
	if err != nil {
		t.Fatalf("split: %v", err)
	}

	splitCtx, cancel2 := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel2()

	splitPreds := []Pred{Attr("tx", hex.EncodeToString(splitHash[:])), Attr("success", true)}
	if err := c.WaitAll(splitCtx, "tx.committed", splitPreds...); err != nil {
		t.Fatalf("wait split commit on every node: %v", err)
	}

	c.Partition([]int{0}, []int{1, 2})
	c.Heal()
}
