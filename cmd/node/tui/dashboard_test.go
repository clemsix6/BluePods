package tui

import (
	"strings"
	"testing"
)

func TestRenderDashboard(t *testing.T) {
	out := renderDashboard(Snapshot{
		NodeAddr: "127.0.0.1:9000", Round: 1428, Epoch: 3,
		Validators: 7, ConnectedPeers: 6, TotalTx: 18392, FailedTx: 12, TPS: 12.4,
		Recent: []RecentTx{{Hash: "a1b2c3d4", Function: "transfer", Success: true, Reason: "none"}},
	})

	for _, want := range []string{"127.0.0.1:9000", "1428", "18392", "12.4", "transfer"} {
		if !strings.Contains(out, want) {
			t.Fatalf("dashboard missing %q in:\n%s", want, out)
		}
	}
}
