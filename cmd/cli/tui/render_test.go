package tui

import (
	"strings"
	"testing"
)

func TestRenderConsole(t *testing.T) {
	out := renderConsole(consoleState{
		NodeAddr: "127.0.0.1:9000", Connected: true,
		Round: 1428, Epoch: 3, Validators: 7, Peers: 6, TPS: 12.4, Balance: 4200, CoinCount: 3,
		Activity: []string{"14:21:03 faucet 1000 -> a1b2 PENDING", "14:21:04 a1b2 FINALIZED"},
		Input:    "transfer 500 9f3c",
	})

	for _, want := range []string{"127.0.0.1:9000", "1428", "4200", "FINALIZED", "transfer 500 9f3c"} {
		if !strings.Contains(out, want) {
			t.Fatalf("console missing %q in:\n%s", want, out)
		}
	}
}
