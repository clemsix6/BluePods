package tui

import (
	"fmt"
	"strings"

	"charm.land/lipgloss/v2"
)

// consoleState is the immutable view the console renders. The model assembles it
// from the client, the wallet, and the tracked-tx map.
type consoleState struct {
	NodeAddr   string   // NodeAddr is the connected node address
	Pubkey     string   // Pubkey is this wallet's public key (hex)
	Connected  bool     // Connected reports node reachability
	Round      uint64   // Round is the latest consensus round
	Epoch      uint64   // Epoch is the latest epoch
	Validators int      // Validators is the active validator count
	Peers      int      // Peers is the connected mesh-peer count
	TPS        float64  // TPS is the node's transactions-per-second
	Balance    uint64   // Balance is the summed balance of known coins
	CoinCount  int      // CoinCount is the number of known coins
	Activity   []string // Activity is the scrolling action and tx-status log
	Input      string   // Input is the current command-line text
}

var (
	headerStyle = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).Padding(0, 1)
	dotOK       = lipgloss.NewStyle().Foreground(lipgloss.Color("10")).Render("connected")
	dotDown     = lipgloss.NewStyle().Foreground(lipgloss.Color("9")).Render("disconnected")
)

// renderConsole renders the header, the activity body, and the input footer. It is
// pure and unit-tested.
func renderConsole(s consoleState) string {
	conn := dotDown
	if s.Connected {
		conn = dotOK
	}

	header := fmt.Sprintf("bpctl  node %s  %s\nround %d  epoch %d  val %d  peers %d  %.1f tps\nbalance %d  (%d coins)\nyou %s",
		s.NodeAddr, conn, s.Round, s.Epoch, s.Validators, s.Peers, s.TPS, s.Balance, s.CoinCount, s.Pubkey)

	body := strings.Join(s.Activity, "\n")

	return headerStyle.Render(header) + "\n" + body + "\n> " + s.Input
}
