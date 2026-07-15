package harness

import (
	"context"
	"crypto/ed25519"
	"fmt"
	"os"
	"time"
)

// partitionTimeout bounds how long Partition/Heal wait for an affected node
// to confirm a blocklist change.
const partitionTimeout = 30 * time.Second

// Partition installs crossed blocklists between group a and group b: every
// alive node in a drops mesh traffic to and from every node in b, and vice
// versa. It waits for every affected node to confirm its own
// net.partition.applied before returning.
func (c *Cluster) Partition(a, b []int) {
	c.t.Helper()

	pubkeysA := c.pubkeysFor(a)
	pubkeysB := c.pubkeysFor(b)

	c.applyBlocklist(a, pubkeysB)
	c.applyBlocklist(b, pubkeysA)
}

// Heal clears the blocklist on every alive node, restoring normal mesh
// traffic everywhere. It waits for every alive node to confirm its own
// net.partition.cleared before returning.
func (c *Cluster) Heal() {
	c.t.Helper()

	for _, n := range c.Alive() {
		before := len(n.Journal().Events("net.partition.cleared"))

		if err := c.clientFor(n).ClearPartition(); err != nil {
			c.t.Fatalf("clear partition on node %d: %v", n.Index, err)
		}

		c.waitPartitionEventCount(n, "net.partition.cleared", before+1)
	}
}

// applyBlocklist calls SetPartition(blocked) on every alive node in group,
// waiting for each to confirm before moving on.
func (c *Cluster) applyBlocklist(group []int, blocked [][32]byte) {
	c.t.Helper()

	for _, i := range group {
		n := c.Node(i)
		if !n.Alive() {
			continue
		}

		before := len(n.Journal().Events("net.partition.applied"))

		if err := c.clientFor(n).SetPartition(blocked); err != nil {
			c.t.Fatalf("set partition on node %d: %v", i, err)
		}

		c.waitPartitionEventCount(n, "net.partition.applied", before+1)
	}
}

// waitPartitionEventCount blocks until n's journal holds at least min events
// named name. Unlike a plain WaitEvent, this does not risk matching a stale
// occurrence from an earlier Partition/Heal cycle in the same scenario,
// since net.partition.applied/cleared carry no attribute that would let a
// predicate distinguish one occurrence from the next.
func (c *Cluster) waitPartitionEventCount(n *Node, name string, min int) {
	c.t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), partitionTimeout)
	defer cancel()

	if _, err := n.Journal().waitCount(ctx, name, min); err != nil {
		c.Dump(c.t)
		c.t.Fatalf("node %d did not confirm %q: %v", n.Index, name, err)
	}
}

// pubkeysFor returns the Ed25519 public keys of the given node indices, read
// from each node's own key file.
func (c *Cluster) pubkeysFor(indices []int) [][32]byte {
	c.t.Helper()

	out := make([][32]byte, 0, len(indices))
	for _, i := range indices {
		pk, err := nodePubkey(c.Node(i))
		if err != nil {
			c.t.Fatalf("pubkey for node %d: %v", i, err)
		}
		out = append(out, pk)
	}

	return out
}

// nodePubkey reads a node's Ed25519 key file and derives its public key.
func nodePubkey(n *Node) ([32]byte, error) {
	data, err := os.ReadFile(n.KeyPath)
	if err != nil {
		return [32]byte{}, fmt.Errorf("read key for node %d:\n%w", n.Index, err)
	}
	if len(data) != ed25519.PrivateKeySize {
		return [32]byte{}, fmt.Errorf("node %d key file has unexpected size %d", n.Index, len(data))
	}

	pub := ed25519.PrivateKey(data).Public().(ed25519.PublicKey)

	var out [32]byte
	copy(out[:], pub)

	return out, nil
}
