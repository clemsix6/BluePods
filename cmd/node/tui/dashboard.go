package tui

import (
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// Snapshot is the immutable view the dashboard renders. The node assembles it
// from its stats source and live DAG and network reads; the TUI computes nothing.
type Snapshot struct {
	NodeAddr       string     // NodeAddr is the node's QUIC listen address
	Round          uint64     // Round is the current consensus round
	Epoch          uint64     // Epoch is the current epoch
	LastCommitted  uint64     // LastCommitted is the last committed round
	Validators     int        // Validators is the active validator count
	ConnectedPeers int        // ConnectedPeers is the connected mesh-peer count
	TotalTx        uint64     // TotalTx is total committed transactions
	FailedTx       uint64     // FailedTx is the failed subset
	TPS            float64    // TPS is the current transactions-per-second
	Recent         []RecentTx // Recent is the most recent committed transactions
}

// RecentTx is one row of the dashboard activity list.
type RecentTx struct {
	Hash     string // Hash is the short hex transaction hash
	Function string // Function is the called function name
	Success  bool   // Success reports whether it applied
	Reason   string // Reason labels a failure
}

// Provider supplies a fresh Snapshot on each dashboard tick.
type Provider interface {
	Snapshot() Snapshot
}

var (
	titleStyle  = lipgloss.NewStyle().Bold(true)
	labelStyle  = lipgloss.NewStyle().Faint(true)
	boxStyle    = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).Padding(0, 1)
	failStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("9"))
	okStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("10"))
	refreshRate = time.Second
)

// renderDashboard renders a snapshot to a string. It is pure and unit-tested; the
// Bubble Tea model only calls it.
func renderDashboard(s Snapshot) string {
	var b strings.Builder

	fmt.Fprintf(&b, "%s  %s\n\n", titleStyle.Render("BluePods node"), s.NodeAddr)
	fmt.Fprintf(&b, "%s %-10d %s %d\n", labelStyle.Render("round"), s.Round, labelStyle.Render("epoch"), s.Epoch)
	fmt.Fprintf(&b, "%s %-10.1f %s %d\n", labelStyle.Render("tps  "), s.TPS, labelStyle.Render("validators"), s.Validators)
	fmt.Fprintf(&b, "%s %-10d %s %d\n", labelStyle.Render("peers"), s.ConnectedPeers, labelStyle.Render("tx total"), s.TotalTx)
	fmt.Fprintf(&b, "%s %-10d %s %d\n", labelStyle.Render("failed"), s.FailedTx, labelStyle.Render("last commit"), s.LastCommitted)

	b.WriteString("\n")
	for _, tx := range s.Recent {
		status := okStyle.Render("committed")
		if !tx.Success {
			status = failStyle.Render("failed " + tx.Reason)
		}
		fmt.Fprintf(&b, "%s  %-9s  %s\n", tx.Hash, tx.Function, status)
	}

	return boxStyle.Render(b.String())
}

// tickMsg drives a periodic refresh.
type tickMsg struct{}

// tick schedules the next refresh.
func tick() tea.Cmd {
	return tea.Tick(refreshRate, func(time.Time) tea.Msg { return tickMsg{} })
}

// model is the thin Bubble Tea wrapper: it holds the provider and the latest
// snapshot, refreshes on each tick, and renders with renderDashboard.
type model struct {
	provider Provider // provider supplies snapshots
	snap     Snapshot // snap is the latest snapshot
}

// New creates a dashboard model over a snapshot provider.
func New(p Provider) model {
	return model{provider: p, snap: p.Snapshot()}
}

// Init starts the refresh ticker.
func (m model) Init() tea.Cmd {
	return tick()
}

// Update refreshes the snapshot on a tick and quits on q or ctrl+c.
func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyPressMsg:
		if s := msg.String(); s == "q" || s == "ctrl+c" {
			return m, func() tea.Msg { return tea.Quit() }
		}
	case tickMsg:
		m.snap = m.provider.Snapshot()
		return m, tick()
	}

	return m, nil
}

// View renders the latest snapshot.
func (m model) View() tea.View {
	v := tea.NewView(renderDashboard(m.snap))
	v.AltScreen = true

	return v
}

// Run runs the dashboard program against a provider until the user quits.
func Run(p Provider) error {
	_, err := tea.NewProgram(New(p)).Run()

	return err
}
