package harness

import "testing"

// CheckInvariants runs the cardinal checks over all alive nodes. It is
// registered by NewCluster in t.Cleanup (running BEFORE node teardown)
// unless WithoutInvariants was given.
//
// This lands the schema-drift check only; convergence, zero rollback and
// supply are added alongside it in a later task.
func (c *Cluster) CheckInvariants(t *testing.T) {
	t.Helper()

	c.checkSchemaDrift(t)
}

// checkSchemaDrift fails the test if any node ever produced an unparsable
// JSON log line: the schema-drift detector.
func (c *Cluster) checkSchemaDrift(t *testing.T) {
	t.Helper()

	for _, n := range c.Nodes() {
		if n == nil {
			continue
		}

		if err := n.ParseError(); err != nil {
			c.Dump(t)
			t.Fatalf("node %d: schema drift: %v", n.Index, err)
		}
	}
}
