package harness

import (
	"path/filepath"
	"testing"
)

// dumpTailSize is how many of a node's most recent events Dump prints.
const dumpTailSize = 15

// Dump prints, per node: its last dumpTailSize events, its live Status()
// fields (round, epoch, last committed round, validator count), and the
// path to its full stdout.log. It is called automatically by
// CheckInvariants and by the harness's own wait-timeout failure paths, so a
// failing scenario is diagnosable without rerunning it.
func (c *Cluster) Dump(t *testing.T) {
	t.Helper()

	for _, n := range c.Nodes() {
		if n == nil {
			continue
		}

		c.dumpNode(t, n)
	}
}

// dumpNode prints one node's diagnostic summary.
func (c *Cluster) dumpNode(t *testing.T, n *Node) {
	t.Helper()

	t.Logf("--- node %d (alive=%v) ---", n.Index, n.Alive())
	c.dumpTailEvents(t, n)
	c.dumpStatus(t, n)
	t.Logf("node %d stdout: %s", n.Index, filepath.Join(n.Dir, "stdout.log"))
}

// dumpTailEvents prints a node's last dumpTailSize journal events, of any
// name.
func (c *Cluster) dumpTailEvents(t *testing.T, n *Node) {
	t.Helper()

	events := n.Journal().all()
	if len(events) > dumpTailSize {
		events = events[len(events)-dumpTailSize:]
	}

	for _, e := range events {
		t.Logf("node %d event: seg=%d %s %v", n.Index, e.Seg, e.Name, e.Attrs)
	}
}

// dumpStatus prints a node's live consensus status, if it can still be
// reached. It builds its own client through newClientFor rather than
// Cluster.clientFor, which fails the test on error — Dump must never abort,
// since it runs on failure paths where reaching the node may itself fail.
func (c *Cluster) dumpStatus(t *testing.T, n *Node) {
	t.Helper()

	if !n.Alive() {
		t.Logf("node %d: not alive, no live status", n.Index)
		return
	}

	cli, err := c.newClientFor(n)
	if err != nil {
		t.Logf("node %d: status unavailable: %v", n.Index, err)
		return
	}

	status, err := cli.Status()
	if err != nil {
		t.Logf("node %d: status unavailable: %v", n.Index, err)
		return
	}

	t.Logf("node %d status: round=%d epoch=%d lastCommitted=%d validators=%d",
		n.Index, status.Round, status.Epoch, status.LastCommitted, status.Validators)
}
