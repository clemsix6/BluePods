package tui

import (
	"encoding/hex"
	"time"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"

	"BluePods/internal/consensus"
	"BluePods/internal/network"
	"BluePods/pkg/client"
)

// pollRate is how often the console polls status, balances, and tracked tx.
const pollRate = time.Second

// statusMsg carries a polled snapshot of node status and wallet balance.
type statusMsg struct {
	state consoleState // state is the freshly polled view (without Activity/Input)
}

// trackMsg carries a polled status for one tracked transaction.
type trackMsg struct {
	hash   [32]byte // hash is the tracked transaction
	state  uint8    // state is the polled tx state
	reason uint8    // reason is the failure reason; zero for non-failed states
	label  string   // label is a short hex of the hash for display
}

// pollMsg drives periodic polling.
type pollMsg struct{}

// consoleModel is the thin Bubble Tea wrapper for the interactive console.
type consoleModel struct {
	client  *client.Client    // client is the SDK connection
	wallet  *client.Wallet    // wallet holds the key and known coins
	state   consoleState      // state is the rendered view
	input   textinput.Model   // input is the footer command line
	tracked map[[32]byte]bool // tracked is the set of in-flight tx hashes
}

// NewConsole creates a console model bound to a client and wallet.
func NewConsole(c *client.Client, w *client.Wallet, nodeAddr string) consoleModel {
	in := textinput.New()
	in.Placeholder = "type a command (help, quit)"
	in.Focus()

	return consoleModel{
		client:  c,
		wallet:  w,
		state:   consoleState{NodeAddr: nodeAddr},
		input:   in,
		tracked: make(map[[32]byte]bool),
	}
}

// Init starts polling.
func (m consoleModel) Init() tea.Cmd {
	return poll()
}

// poll schedules the next poll tick.
func poll() tea.Cmd {
	return tea.Tick(pollRate, func(time.Time) tea.Msg { return pollMsg{} })
}

// Update handles input, command submission, polling, and tx tracking.
func (m consoleModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyPressMsg:
		switch msg.String() {
		case "ctrl+c":
			return m, func() tea.Msg { return tea.Quit() }
		case "enter":
			return m.submit()
		}
	case pollMsg:
		return m, tea.Batch(m.fetchStatus(), m.fetchTracked(), poll())
	case statusMsg:
		msg.state.Activity = m.state.Activity
		msg.state.Input = m.input.Value()
		m.state = msg.state
		return m, nil
	case trackMsg:
		if msg.state == network.TxStateFinalized || msg.state == network.TxStateFailed {
			delete(m.tracked, msg.hash)
			var activity string
			if msg.state == network.TxStateFailed {
				activity = msg.label + " FAILED (" + consensus.FailReason(msg.reason).String() + ")"
			} else {
				activity = msg.label + " FINALIZED"
			}
			m.appendActivity(activity)
		}
		return m, nil
	}

	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)

	return m, cmd
}

// submit parses and dispatches the input line, then tracks any returned hash.
func (m consoleModel) submit() (tea.Model, tea.Cmd) {
	line := m.input.Value()
	m.input.SetValue("")

	parsed, err := parseCommand(line)
	if err != nil {
		return m, nil
	}

	if parsed.verb == "quit" {
		return m, func() tea.Msg { return tea.Quit() }
	}

	result, track, derr := dispatch(m.client, m.wallet, parsed)
	if derr != nil {
		m.appendActivity("error: " + derr.Error())
		return m, nil
	}

	m.appendActivity(result)

	if track != ([32]byte{}) {
		m.tracked[track] = true
		return m, m.fetchTrack(track)
	}

	return m, nil
}

// fetchStatus polls node status and the summed balance off the UI goroutine.
func (m consoleModel) fetchStatus() tea.Cmd {
	return func() tea.Msg {
		st := consoleState{NodeAddr: m.state.NodeAddr}

		status, err := m.client.Status()
		if err == nil {
			st.Connected = true
			st.Round = status.Round
			st.Epoch = status.Epoch
			st.Validators = int(status.Validators)
			st.Peers = int(status.ConnectedPeers)
			st.TPS = float64(status.TPSMilli) / 1000
		}

		coinIDs := m.wallet.CoinIDs()
		st.CoinCount = len(coinIDs)

		for _, id := range coinIDs {
			if err := m.wallet.RefreshCoin(m.client, id); err == nil {
				if ci := m.wallet.GetCoin(id); ci != nil {
					st.Balance += ci.Balance
				}
			}
		}

		return statusMsg{state: st}
	}
}

// fetchTracked emits a fetchTrack command for each in-flight tx hash.
func (m consoleModel) fetchTracked() tea.Cmd {
	if len(m.tracked) == 0 {
		return nil
	}

	cmds := make([]tea.Cmd, 0, len(m.tracked))
	for hash := range m.tracked {
		cmds = append(cmds, m.fetchTrack(hash))
	}

	return tea.Batch(cmds...)
}

// fetchTrack polls one tracked transaction's status.
func (m consoleModel) fetchTrack(hash [32]byte) tea.Cmd {
	return func() tea.Msg {
		resp, err := m.client.GetTxStatus(hash)
		state := network.TxStateUnknown
		var reason uint8
		if err == nil {
			state = resp.State
			reason = resp.Reason
		}

		return trackMsg{hash: hash, state: state, reason: reason, label: hex.EncodeToString(hash[:4])}
	}
}

// appendActivity adds a line to the activity log, bounded to the last 100 lines.
func (m *consoleModel) appendActivity(line string) {
	m.state.Activity = append(m.state.Activity, line)
	if len(m.state.Activity) > 100 {
		m.state.Activity = m.state.Activity[len(m.state.Activity)-100:]
	}
}

// View renders the console into an alt-screen view.
func (m consoleModel) View() tea.View {
	m.state.Input = m.input.View()
	v := tea.NewView(renderConsole(m.state))
	v.AltScreen = true

	return v
}

// RunConsole runs the interactive console until the user quits.
func RunConsole(c *client.Client, w *client.Wallet, nodeAddr string) error {
	_, err := tea.NewProgram(NewConsole(c, w, nodeAddr)).Run()

	return err
}
